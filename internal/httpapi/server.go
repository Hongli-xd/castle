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
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
	// authFails throttles audit writes on the unauthenticated path.
	authFails *authFailureRecorder
}

func New(svc *service.Service, cfg *config.Config) *Server {
	s := &Server{Svc: svc, Cfg: cfg, authFails: newAuthFailureRecorder()}
	mux := http.NewServeMux()
	s.routes(mux)
	s.mux = mux
	return s
}

// Handler exposes the routed stack to the http.Server.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes(mux *http.ServeMux) {
	// Liveness and readiness stay unauthenticated so a load balancer can probe
	// them. They are cheap by construction: no store access beyond a mutex, no
	// audit write, no fsync.
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	// /metrics is NOT in that group. In v1.1.0 it was registered with
	// mux.HandleFunc, which bypasses s.chain() entirely — so it answered 200
	// to anyone, on the same port as the control plane. Operational data about
	// every tenant belongs behind admin auth.
	mux.Handle("GET /metrics", s.chainAdmin(s.handleMetrics))

	// The self-check inventory is admin-only for the same reason /metrics is:
	// it enumerates the controls actually in force, which is reconnaissance
	// for anyone planning an escape.
	mux.Handle("GET /v1/isolation", s.chainAdmin(s.isolationReport))

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

	// metering
	mux.Handle("GET /v1/metering", s.chain(domain.GroupRead, s.listMetering))

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

// chain composes the middleware stack for an authenticated route.
//
// Order is load-bearing, and v1.1.0 got it wrong:
//
//	recover → request-id → pre-auth limit → body-limit → authenticate
//	        → per-tenant limit → handler
//
// The v1.1.0 stack ran the rate limiter INSIDE authenticate ("it needs the
// authenticated tenant"). That is true for the per-tenant business quota, but
// it meant a request that failed authentication never reached a limiter at
// all — while still costing one audit event (and therefore one fsync) per
// request. 400 unauthenticated requests produced 400 fsyncs and zero 429s
// (poc/probe_live.sh, PoC-5): an I/O amplifier that needs no credentials.
//
// The fix is two limiters on two identities, not one limiter moved:
//   - preAuthLimit runs outside authenticate, keyed by client IP. It is a
//     coarse flood gate (generous by default) and applies to everyone.
//   - rateLimit still runs inside, keyed by tenant, and remains the
//     authoritative per-tenant business quota.
func (s *Server) chain(group string, h http.HandlerFunc) http.Handler {
	return s.accessLog(s.recover(s.requestID(
		s.preAuthLimit(
			s.bodyLimit(
				s.authenticate(
					s.rateLimit(group, h),
				),
			),
		),
	)))
}

// chainAdmin is `chain` plus an admin-scope requirement, for routes that must
// not be reachable with a tenant key (/metrics).
func (s *Server) chainAdmin(h http.HandlerFunc) http.Handler {
	return s.chain(domain.GroupAdmin, s.requireAdmin(h))
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if k := keyFrom(r); k != nil && k.Scope != domain.ScopeAdmin {
			s.writeError(w, r, apperr.ForbiddenErr("this endpoint requires an admin-scoped key"))
			return
		}
		next(w, r)
	}
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
		s.Svc.Reg.Counter("sandboxd_http_requests_total", "HTTP requests", []string{"status", strconv.Itoa(sw.status)}, 1)
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
			s.noteAuthFailure(r)
			s.writeError(w, r, apperr.Auth("authentication required (Authorization: Bearer or X-API-Key)"))
			return
		}
		key, err := s.Svc.Auth.Authenticate(secret)
		if err != nil {
			s.noteAuthFailure(r)
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

// ---- pre-authentication flood control ----------------------------------------

// clientIP is the identity the pre-auth limiter keys on.
//
// It deliberately reads ONLY RemoteAddr. X-Forwarded-For is attacker-supplied
// unless a trusted proxy rewrites it, so honouring it by default would let one
// host rotate a header value into an unlimited number of buckets — turning the
// flood gate into a memory-growth vector instead of a defence. Deployments
// behind a trusted proxy should terminate and re-originate client IPs there.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// preAuthLimit is the gate that v1.1.0 was missing entirely: it sits OUTSIDE
// authenticate, so a caller who never presents a valid key still gets limited.
func (s *Server) preAuthLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rate := s.Cfg.PreAuth
		if ok, retryAfter := s.Svc.Limiter.Allow("ip:"+clientIP(r), "preauth", &rate); !ok {
			w.Header().Set("Retry-After", fmt.Sprint(retryAfter))
			s.Svc.Reg.Counter("sandboxd_preauth_limited_total", "requests rejected before authentication", nil, 1)
			e := apperr.RateLimit(retryAfter)
			e.Details = map[string]any{"scope": "pre-auth"}
			s.writeError(w, r, e)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- auth-failure audit throttling -------------------------------------------

// authFailureRecorder stops an unauthenticated caller from turning each 401
// into a WAL frame plus an fsync. It aggregates per client IP over a window:
// the first failure is recorded in full — so an attack's onset is visible in
// the audit log — and after that only every authFailAuditEvery-th one is, with
// the running count attached. 400 failures become a handful of fsyncs instead
// of 400, and the growth curve stays legible.
//
// The table is bounded for the same reason the limiter's is: the key is
// attacker-chosen.
type authFailureRecorder struct {
	mu      sync.Mutex
	states  map[string]*authFailureState
	now     func() time.Time
	window  time.Duration
	maxKeys int
}

type authFailureState struct {
	windowStart time.Time
	count       int
}

const (
	authFailWindow     = time.Minute
	authFailAuditEvery = 100
	authFailMaxKeys    = 50_000
)

func newAuthFailureRecorder() *authFailureRecorder {
	return &authFailureRecorder{
		states:  map[string]*authFailureState{},
		now:     time.Now,
		window:  authFailWindow,
		maxKeys: authFailMaxKeys,
	}
}

// note records one auth failure and reports whether it should be written to the
// audit log, plus the count of failures seen in the current window.
func (rec *authFailureRecorder) note(ip string) (audit bool, inWindow int) {
	now := rec.now()
	rec.mu.Lock()
	defer rec.mu.Unlock()

	st, ok := rec.states[ip]
	if !ok {
		if len(rec.states) >= rec.maxKeys {
			// Drop the whole window's state rather than grow without limit.
			// The consequence is bounded: a few extra audit writes.
			rec.states = map[string]*authFailureState{}
		}
		st = &authFailureState{windowStart: now}
		rec.states[ip] = st
	} else if now.Sub(st.windowStart) >= rec.window {
		st.windowStart = now
		st.count = 0
	}
	st.count++
	return st.count == 1 || st.count%authFailAuditEvery == 0, st.count
}

// noteAuthFailure records a rejected request. The metric is deliberately
// unlabelled: the client IP is attacker-chosen, and using it as a label value
// would let anyone inflate the metric registry without limit.
func (s *Server) noteAuthFailure(r *http.Request) {
	s.Svc.Reg.Counter("sandboxd_auth_failures_total", "requests rejected for bad or missing credentials", nil, 1)
	if !s.authFails.enabled() {
		return
	}
	ip := clientIP(r)
	audit, inWindow := s.authFails.note(ip)
	if !audit {
		return
	}
	_ = s.Svc.Audit.Record("", "", "unknown", domain.ActKeyAuthFail, map[string]string{
		"path":             r.URL.Path,
		"client_ip":        ip,
		"failed_in_window": strconv.Itoa(inWindow),
	})
}

func (rec *authFailureRecorder) enabled() bool { return rec != nil }

func (s *Server) rateLimit(group string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := keyFrom(r)
		if key == nil {
			// authenticate always runs before this in chain(). Reaching here
			// means the stack was misassembled, so fail closed rather than
			// wave the request through unlimited.
			s.writeError(w, r, apperr.InternalErr("rate limiter reached without an authenticated key"))
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
		identity := limiterIdentity(key)
		if ok, retryAfter := s.Svc.Limiter.Allow(identity, group, rate); !ok {
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

// limiterIdentity namespaces the bucket owner.
//
// Platform-admin keys carry an empty TenantID (see domain.APIKey). v1.1.0 fed
// that empty string straight to the limiter, so every admin key in the fleet
// shared one bucket and the busiest one starved all the others. Namespacing by
// the key itself gives each credential its own budget.
func limiterIdentity(k *domain.APIKey) string {
	if k.TenantID != "" {
		return "tenant:" + k.TenantID
	}
	return "key:" + k.ID
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
