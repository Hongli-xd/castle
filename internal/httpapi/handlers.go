package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/runtime"
	"sandcastle.dev/sandcastle/internal/service"
	"sandcastle.dev/sandcastle/internal/store"
)

// ---- context plumbing ---------------------------------------------------------

var _ = context.Background

// lastUsedFlush throttles LastUsedAt persistence: one WAL frame per key per
// minute, not per request.
var lastUsedMu sync.Mutex
var lastUsedSeen = map[string]time.Time{}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.Svc.Backend.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "backend unreachable", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(s.Svc.Reg.Render()))
}

// tenant resolution: tenant keys act in their own tenant; admin keys must
// name the tenant they act on (X-Tenant-Id) — admins never silently touch
// tenant resources.
func (s *Server) resolveTenant(r *http.Request) (*domain.Tenant, *domain.APIKey, error) {
	key := keyFrom(r)
	if key == nil {
		return nil, nil, apperr.Auth("authentication required")
	}
	if key.Scope == domain.ScopeAdmin {
		id := r.Header.Get("X-Tenant-Id")
		if id == "" {
			// admin without tenant context may still query platform-wide reads
			if r.Method == http.MethodGet {
				return &domain.Tenant{ID: "", Quota: s.Cfg.Defaults}, key, nil
			}
			return nil, nil, apperr.ForbiddenErr("admin key must specify X-Tenant-Id for this operation")
		}
		tn, ok := s.Svc.St.GetTenant(id)
		if !ok {
			return nil, nil, apperr.NotFoundErr("tenant not found")
		}
		return tn, key, nil
	}
	tn, ok := s.Svc.St.GetTenant(key.TenantID)
	if !ok {
		return nil, nil, apperr.Auth("tenant no longer exists")
	}
	return tn, key, nil
}

// ---- sandboxes ---------------------------------------------------------------

type createSandboxBody struct {
	TemplateID string             `json:"template_id,omitempty"`
	SnapshotID string             `json:"snapshot_id,omitempty"`
	BaseImage  string             `json:"base_image,omitempty"`
	Spec       domain.SandboxSpec `json:"spec"`
	Metadata   map[string]string  `json:"metadata,omitempty"`
}

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request) {
	tn, key, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body createSandboxBody
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	wait := r.URL.Query().Get("wait") != "false" // agents want ready sandboxes
	sb, err := s.Svc.CreateSandbox(r.Context(), tn, key, service.CreateRequest{
		TemplateID:  body.TemplateID,
		SnapshotID:  body.SnapshotID,
		BaseImage:   body.BaseImage,
		Spec:        body.Spec,
		Metadata:    body.Metadata,
		Wait:        wait,
		WaitTimeout: 120 * time.Second,
	})
	if err != nil {
		var ff *service.SandboxFailedError
		if errors.As(err, &ff) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   map[string]any{"code": "conflict", "message": ff.Error(), "retryable": false, "request_id": w.Header().Get(requestIDHeader)},
				"sandbox": sb,
			})
			return
		}
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sb)
}

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
	tn, _, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var stateFilter *domain.SandboxState
	if v := r.URL.Query().Get("state"); v != "" {
		st := domain.SandboxState(v)
		if !st.Valid() {
			s.writeError(w, r, apperr.Invalid("unknown state filter %q", v))
			return
		}
		stateFilter = &st
	}
	out := s.Svc.ListSandboxes(tn.ID, stateFilter)
	if out == nil {
		out = []*domain.Sandbox{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": out, "count": len(out)})
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	tn, _, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sb, err := s.Svc.GetSandbox(tn.ID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (s *Server) pauseSandbox(w http.ResponseWriter, r *http.Request) {
	tn, key, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sb, err := s.Svc.PauseSandbox(r.Context(), tn.ID, key.ID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (s *Server) resumeSandbox(w http.ResponseWriter, r *http.Request) {
	tn, key, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sb, err := s.Svc.ResumeSandbox(r.Context(), tn.ID, key.ID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (s *Server) stopSandbox(w http.ResponseWriter, r *http.Request) {
	tn, key, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	force := r.URL.Query().Get("force") == "true"
	sb, err := s.Svc.StopSandbox(r.Context(), tn.ID, key.ID, r.PathValue("id"), force)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (s *Server) extendSandbox(w http.ResponseWriter, r *http.Request) {
	tn, key, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	sb, err := s.Svc.ExtendSandbox(r.Context(), tn.ID, key.ID, r.PathValue("id"), body.TimeoutSeconds)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sb)
}

func (s *Server) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	tn, key, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	force := r.URL.Query().Get("force") == "true"
	if err := s.Svc.DeleteSandbox(r.Context(), tn.ID, key.ID, r.PathValue("id"), force); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- exec & files ---------------------------------------------------------------

type execBody struct {
	Argv      []string          `json:"argv,omitempty"`
	Command   string            `json:"command,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	User      string            `json:"user,omitempty"`
	WorkDir   string            `json:"workdir,omitempty"`
	TimeoutMs int               `json:"timeout_ms,omitempty"`
	MaxOutput int               `json:"max_output,omitempty"`
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	tn, key, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body execBody
	if err := decode(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	argv := body.Argv
	if len(argv) == 0 && body.Command != "" {
		argv = []string{"/bin/sh", "-c", body.Command}
	}
	req := runtime.ExecRequest{
		Argv:      argv,
		Env:       body.Env,
		User:      body.User,
		WorkDir:   body.WorkDir,
		Timeout:   time.Duration(body.TimeoutMs) * time.Millisecond,
		MaxOutput: body.MaxOutput,
	}
	res, err := s.Svc.Exec(r.Context(), tn.ID, key.ID, r.PathValue("id"), req)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) putFile(w http.ResponseWriter, r *http.Request) {
	tn, key, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var path string
	var mode uint32
	var content []byte

	if r.Header.Get("Content-Type") == "application/json" {
		var body struct {
			Path       string `json:"path"`
			ContentB64 []byte `json:"content_b64"`
			Mode       uint32 `json:"mode"`
		}
		if err := decode(r, &body); err != nil {
			s.writeError(w, r, err)
			return
		}
		path, mode = body.Path, body.Mode
		content = body.ContentB64
	} else {
		path = r.URL.Query().Get("path")
		if m := r.URL.Query().Get("mode"); m != "" {
			v, err := strconv.ParseUint(m, 8, 32)
			if err != nil {
				s.writeError(w, r, apperr.Invalid("mode must be octal"))
				return
			}
			mode = uint32(v)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			s.writeError(w, r, apperr.Invalid("read upload: %v", err))
			return
		}
		content = data
	}
	if err := s.Svc.PutFile(r.Context(), tn.ID, key.ID, r.PathValue("id"), path, content, mode); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	tn, key, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	data, err := s.Svc.GetFile(r.Context(), tn.ID, key.ID, r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	tn, key, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	files, err := s.Svc.ListFiles(r.Context(), tn.ID, key.ID, r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if files == nil {
		files = []runtime.FileInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	tn, key, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Svc.DeleteFile(r.Context(), tn.ID, key.ID, r.PathValue("id"), r.URL.Query().Get("path")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- snapshots & templates -------------------------------------------------------

func (s *Server) createSnapshot(w http.ResponseWriter, r *http.Request) {
	tn, key, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body struct {
		SandboxID string              `json:"sandbox_id"`
		Name      string              `json:"name"`
		Pack      *service.PackPolicy `json:"pack,omitempty"`
	}
	if err := decode(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	sn, err := s.Svc.CreateSnapshot(r.Context(), tn.ID, key.ID, body.SandboxID, body.Name, body.Pack)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sn)
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	tn, _, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := s.Svc.ListSnapshots(tn.ID)
	if out == nil {
		out = []*domain.Snapshot{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out})
}

func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request) {
	tn, _, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sn, err := s.Svc.GetSnapshot(tn.ID, r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sn)
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	tn, key, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Svc.DeleteSnapshot(r.Context(), tn.ID, key.ID, r.PathValue("id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	tn, _, err := s.resolveTenant(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := s.Svc.ListTemplates(tn.ID)
	if out == nil {
		out = []*domain.Template{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

// ---- events ------------------------------------------------------------------------

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	key := keyFrom(r)
	q := store.EventQuery{}
	if key != nil && key.Scope == domain.ScopeTenant {
		q.TenantID = key.TenantID
	} else if v := r.URL.Query().Get("tenant"); v != "" {
		q.TenantID = v
	}
	q.SandboxID = r.URL.Query().Get("sandbox")
	q.Action = r.URL.Query().Get("action")
	q.Actor = r.URL.Query().Get("actor")
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			q.Limit = n
		}
	}
	if v := r.URL.Query().Get("before"); v != "" {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			q.Before = ts
		}
	}
	out := s.Svc.ListEvents(q.TenantID, q)
	if out == nil {
		out = []*domain.AuditEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}
