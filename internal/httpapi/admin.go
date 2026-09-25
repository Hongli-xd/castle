package httpapi

import (
	"net/http"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
)

// Admin endpoints require ScopeAdmin keys. Tenant scoping on key responses:
// hashes never leave the store, plaintexts are shown exactly once.

func adminOnly(key *domain.APIKey) error {
	if key == nil || key.Scope != domain.ScopeAdmin {
		return apperr.ForbiddenErr("admin key required")
	}
	return nil
}

type tenantResponse struct {
	*domain.Tenant
	Keys int `json:"keys"`
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	key := keyFrom(r)
	if err := adminOnly(key); err != nil {
		s.writeError(w, r, err)
		return
	}
	var body struct {
		Name  string             `json:"name"`
		Quota domain.TenantQuota `json:"quota"`
	}
	if err := decode(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	tn, err := s.Svc.CreateTenant(r.Context(), key.ID, body.Name, body.Quota)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, tn)
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	key := keyFrom(r)
	if err := adminOnly(key); err != nil {
		s.writeError(w, r, err)
		return
	}
	tenants := s.Svc.ListTenants()
	if tenants == nil {
		tenants = []*domain.Tenant{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": tenants})
}

func (s *Server) getTenant(w http.ResponseWriter, r *http.Request) {
	key := keyFrom(r)
	if err := adminOnly(key); err != nil {
		s.writeError(w, r, err)
		return
	}
	tn, err := s.Svc.GetTenant(r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tn)
}

func (s *Server) updateTenant(w http.ResponseWriter, r *http.Request) {
	key := keyFrom(r)
	if err := adminOnly(key); err != nil {
		s.writeError(w, r, err)
		return
	}
	var body struct {
		Status      *string             `json:"status,omitempty"`
		Quota       *domain.TenantQuota `json:"quota,omitempty"`
		BaseVersion int64               `json:"base_version"`
	}
	if err := decode(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	tn, err := s.Svc.UpdateTenant(r.Context(), key.ID, r.PathValue("id"), body.BaseVersion, func(t *domain.Tenant) error {
		if body.Status != nil {
			switch domain.TenantStatus(*body.Status) {
			case domain.TenantActive, domain.TenantSuspended:
				t.Status = domain.TenantStatus(*body.Status)
			default:
				return apperr.Invalid("unknown status %q", *body.Status)
			}
		}
		if body.Quota != nil {
			t.Quota = *body.Quota
		}
		return nil
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tn)
}

type keyResponse struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenant_id"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func toKeyResponse(k *domain.APIKey) keyResponse {
	return keyResponse{
		ID: k.ID, TenantID: k.TenantID, Name: k.Name, Scope: string(k.Scope),
		Prefix: k.Prefix, CreatedAt: k.CreatedAt,
		ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt, LastUsedAt: k.LastUsedAt,
	}
}

func (s *Server) mintKey(w http.ResponseWriter, r *http.Request) {
	key := keyFrom(r)
	if err := adminOnly(key); err != nil {
		s.writeError(w, r, err)
		return
	}
	var body struct {
		Name       string `json:"name"`
		TTLSeconds int    `json:"ttl_seconds,omitempty"`
	}
	if err := decode(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	ttl := time.Duration(body.TTLSeconds) * time.Second
	plaintext, k, err := s.Svc.MintTenantKey(r.Context(), key.ID, r.PathValue("id"), body.Name, ttl)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// the ONLY response in the product that ever contains a plaintext key
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":    toKeyResponse(k),
		"secret": plaintext,
		"note":   "store this secret now; it is not retrievable later",
	})
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	key := keyFrom(r)
	if err := adminOnly(key); err != nil {
		s.writeError(w, r, err)
		return
	}
	keys := s.Svc.ListKeys(r.PathValue("id"))
	out := make([]keyResponse, 0, len(keys))
	for _, k := range keys {
		out = append(out, toKeyResponse(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	key := keyFrom(r)
	if err := adminOnly(key); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Svc.RevokeKey(r.Context(), key.ID, r.PathValue("id"), r.PathValue("key_id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// importTemplate registers a template from a local rootfs directory — the
// air-gapped deployment path: rootfs tarballs are released as artifacts,
// imported on the node, and never require a registry.
func (s *Server) importTemplate(w http.ResponseWriter, r *http.Request) {
	key := keyFrom(r)
	if err := adminOnly(key); err != nil {
		s.writeError(w, r, err)
		return
	}
	var body struct {
		Name        string             `json:"name"`
		RootfsDir   string             `json:"rootfs_dir"`
		Public      bool               `json:"public"`
		DefaultSpec domain.SandboxSpec `json:"default_spec"`
	}
	if err := decode(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	tpl, err := s.Svc.ImportRootfsTemplate(r.Context(), key.ID, body.Name, body.RootfsDir, body.Public, body.DefaultSpec)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, tpl)
}
