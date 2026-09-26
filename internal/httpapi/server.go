// Package httpapi exposes the REST control plane. Middleware order is the
// security contract: recover → request-id → access-log → body-limit →
// authenticate → rate-limit → handler. Business logic lives in service.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/config"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/gov"
	"sandcastle.dev/sandcastle/internal/service"
)

const requestIDHeader = "X-Request-Id"

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyAPIKey
)

type Server struct {
	Svc *service.Service
	Cfg *config.Config
	mux *http.ServeMux
}

func New(svc *service.Service, cfg *config.Config) *Server {
	s := &Server{Svc: svc, Cfg: cfg}
	mux := http.NewServeMux()
	s.routes(mux)
	s.mux = mux
	return s
}

// Handler exposes the routed stack to the http.Server.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	// sandboxes
	mux.Handle("POST /v1/sandboxes", s.chain(domain.GroupCreate, s.createSandbox))
	mux.Handle("GET /v1/sandboxes", s.chain(domain.GroupRead, s.listSandboxes))
	mux.Handle("GET /v1/sandboxes/{id}", s.chain(domain.GroupRead, s.getSandbox))
	mux.Handle("POST /v1/sandboxes/{id}/pause", s.chain(domain.GroupRead, s.pauseSandbox))
	mux.Handle("POST /v1/sandboxes/{id}/resume", s.chain(domain.GroupRead, s.resumeSandbox))
	mux.Handle("POST /v1/sandboxes/{id}/stop", s.chain(domain.GroupRead, s.stopSandbox))
	mux.Handle("POST /v1/sandboxes/{id}/timeout", s.chain(domain.GroupRead, s.extendSandbox))
	mux.Handle("DELETE /v1/sandboxes/{id}", s.chain(domain.GroupCreate, s.deleteSandbox))
	mux.Handle("POST /v1/sandboxes/{id}/exec", s.chain(domain.GroupExec, s.exec))
	mux.Handle("PUT /v1/sandboxes/{id}/files", s.chain(domain.GroupFiles, s.putFile))
	mux.Handle("GET /v1/sandboxes/{id}/files", s.chain(domain.GroupFiles, s.getFile))
	mux.Handle("GET /v1/sandboxes/{id}/files/list", s.chain(domain.GroupFiles, s.listFiles))
	mux.Handle("DELETE /v1/sandboxes/{id}/files", s.chain(domain.GroupFiles, s.deleteFile))

	// snapshots & templates
	mux.Handle("POST /v1/snapshots", s.chain(domain.GroupSnapshot, s.createSnapshot))
	mux.Handle("GET /v1/snapshots", s.chain(domain.GroupRead, s.listSnapshots))
	mux.Handle("GET /v1/snapshots/{id}", s.chain(domain.GroupRead, s.getSnapshot))
	mux.Handle("DELETE /v1/snapshots/{id}", s.chain(domain.GroupSnapshot, s.deleteSnapshot))
	mux.Handle("GET /v1/templates", s.chain(domain.GroupRead, s.listTemplates))

	// events
	mux.Handle("GET /v1/events", s.chain(domain.GroupRead, s.listEvents))

	// admin: tenants & keys
	mux.Handle("POST /v1/tenants", s.chain(domain.GroupAdmin, s.createTenant))
	mux.Handle("GET /v1/tenants", s.chain(domain.GroupAdmin, s.listTenants))
	mux.Handle("GET /v1/tenants/{id}", s.chain(domain.GroupAdmin, s.getTenant))
	mux.Handle("PATCH /v1/tenants/{id}", s.chain(domain.GroupAdmin, s.updateTenant))
	mux.Handle("POST /v1/tenants/{id}/keys", s.chain(domain.GroupAdmin, s.mintKey))
	mux.Handle("GET /v1/tenants/{id}/keys", s.chain(domain.GroupAdmin, s.listKeys))
	mux.Handle("DELETE /v1/tenants/{id}/keys/{key_id}", s.chain(domain.GroupAdmin, s.revokeKey))
	mux.Handle("POST /v1/templates/import", s.chain(domain.GroupAdmin, s.importTemplate))
	mux.Handle("POST /v1/templates/compose", s.chain(domain.GroupAdmin, s.composeTemplate))
}

// chain composes the middleware stack. Order is load-bearing: the rate
// limiter runs INSIDE authentication because it needs the authenticated
// tenant — putting it outside silently disables limiting (a bug the
// HTTP-level rate-limit test caught).
func (s *Server) chain(group string, h http.HandlerFunc) http.Handler {
	return s.accessLog(s.recover(s.requestID(s.bodyLimit(s.authenticate(s.rateLimit(group, h))))))
}

// ---- middleware ---------------------------------------------------------------

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.writeError(w, r, apperr.InternalErr("internal error: %v", rec))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if id == "" || len(id) > 64 {
			var b [8]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.Cfg.LogAccessLog {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		logJSON("info", "access", map[string]any{
			"method": r.Method, "path": r.URL.Path, "status": sw.status,
			"duration_ms": time.Since(start).Milliseconds(),
			"request_id":  w.Header().Get(requestIDHeader),
		})
		s.Svc.Reg.Counter("sandboxd_http_requests_total", "HTTP requests", []string{"status"}, 1)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) bodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := s.Cfg.MaxRequestBody
		if strings.HasSuffix(r.URL.Path, "/files") && r.Method == http.MethodPut {
			limit = s.Cfg.MaxFileBytes + 4096 // raw upload path allows large bodies
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret := gov.ExtractSecret(r.Header)
		if secret == "" {
			s.writeError(w, r, apperr.Auth("authentication required (Authorization: Bearer or X-API-Key)"))
			return
		}
		key, err := s.Svc.Auth.Authenticate(secret)
		if err != nil {
			_ = s.Svc.Audit.Record("", "", "unknown", domain.ActKeyAuthFail, map[string]string{"path": r.URL.Path})
			s.writeError(w, r, err)
			return
		}
		// LastUsedAt persists at most once per key per minute: fresh enough
		// for ops, without an fsync on every request
		lastUsedMu.Lock()
		last, seen := lastUsedSeen[key.ID]
		now := time.Now()
		if !seen || now.Sub(last) > time.Minute {
			lastUsedSeen[key.ID] = now
			lastUsedMu.Unlock()
			_, _ = s.Svc.St.UpdateKey(key.ID, -1, func(k *domain.APIKey) error {
				ts := now.UTC()
				k.LastUsedAt = &ts
				return nil
			})
		} else {
			lastUsedMu.Unlock()
		}
		next.ServeHTTP(w, withKey(r, key))
	})
}

func (s *Server) rateLimit(group string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := keyFrom(r)
		if key == nil {
			next.ServeHTTP(w, r)
			return
		}
		tenant, ok := s.Svc.St.GetTenant(key.TenantID)
		if !ok && key.Scope == domain.ScopeTenant {
			s.writeError(w, r, apperr.Auth("tenant no longer exists"))
			return
		}
		if tenant == nil {
			tenant = &domain.Tenant{ID: "", Quota: s.Cfg.Defaults} // admin key: platform defaults
		}
		rate := s.Svc.Quotas.RateFor(tenant, group)
		if ok, retryAfter := s.Svc.Limiter.Allow(key.TenantID, group, rate); !ok {
			w.Header().Set("Retry-After", fmt.Sprint(retryAfter))
			w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%v", rate.PerSecond))
			e := apperr.RateLimit(retryAfter)
			e.Details = map[string]any{"group": group}
			s.writeError(w, r, e)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- helpers ---------------------------------------------------------------------

func withKey(r *http.Request, k *domain.APIKey) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKeyAPIKey, k))
}

func keyFrom(r *http.Request) *domain.APIKey {
	k, _ := r.Context().Value(ctxKeyAPIKey).(*domain.APIKey)
	return k
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	e := apperr.From(err)
	if e == nil {
		e = apperr.InternalErr("nil error")
	}
	rid := w.Header().Get(requestIDHeader)
	code := e.Code.HTTPStatus()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":       e.Code,
			"message":    e.Message,
			"retryable":  e.Retryable,
			"request_id": rid,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return apperr.Invalid("invalid JSON body: %v", err)
	}
	return nil
}

func logJSON(level, msg string, fields map[string]any) {
	entry := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano), "level": level, "msg": msg}
	for k, v := range fields {
		entry[k] = v
	}
	blob, _ := json.Marshal(entry)
	fmt.Println(string(blob))
}
