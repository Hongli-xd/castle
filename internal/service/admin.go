package service

import (
	"context"
	"time"

	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/store"
)

// ---- tenants (admin only) ---------------------------------------------------

func (s *Service) CreateTenant(ctx context.Context, actorKeyID, name string, quota domain.TenantQuota) (*domain.Tenant, error) {
	tn := domain.NewTenant(name)
	tn.Quota = quota
	if err := tn.Validate(s.Cfg.Limits); err != nil {
		return nil, err
	}
	if err := s.St.PutTenant(tn); err != nil {
		return nil, err
	}
	_ = s.Audit.Record(tn.ID, "", actorKeyID, domain.ActTenantCreate, map[string]string{"name": name})
	return tn, nil
}

func (s *Service) GetTenant(id string) (*domain.Tenant, error) {
	tn, ok := s.St.GetTenant(id)
	if !ok {
		return nil, errMissing("tenant not found")
	}
	return tn, nil
}

func (s *Service) ListTenants() []*domain.Tenant {
	return s.St.ListTenants()
}

// UpdateTenant applies a patch function under CAS; both quota and status
// changes flow through here so concurrent admin edits are safe.
func (s *Service) UpdateTenant(ctx context.Context, actorKeyID string, id string, baseVersion int64, fn func(t *domain.Tenant) error) (*domain.Tenant, error) {
	out, err := s.St.UpdateTenant(id, baseVersion, fn)
	if err != nil {
		return nil, err
	}
	_ = s.Audit.Record(id, "", actorKeyID, domain.ActTenantUpdate, nil)
	return out, nil
}

// ---- API keys ---------------------------------------------------------------

// MintTenantKey creates a tenant-scoped key. Plaintext is returned exactly
// once; the audit trail records only the key ID.
func (s *Service) MintTenantKey(ctx context.Context, actorKeyID, tenantID, name string, ttl time.Duration) (string, *domain.APIKey, error) {
	if _, ok := s.St.GetTenant(tenantID); !ok {
		return "", nil, errMissing("tenant not found")
	}
	plaintext, key, err := s.Auth.MintKey(tenantID, name, domain.ScopeTenant, ttl)
	if err != nil {
		return "", nil, err
	}
	_ = s.Audit.Record(tenantID, "", actorKeyID, domain.ActKeyCreate, map[string]string{"key": key.ID, "name": name})
	return plaintext, key, nil
}

func (s *Service) ListKeys(tenantID string) []*domain.APIKey {
	return s.St.ListKeys(tenantID)
}

func (s *Service) RevokeKey(ctx context.Context, actorKeyID, tenantID, keyID string) error {
	if err := idgenCheck("ak", keyID); err != nil {
		return err
	}
	k, ok := s.St.GetKey(keyID)
	if !ok {
		return errMissing("api key not found")
	}
	if k.TenantID != tenantID {
		return errForbidden()
	}
	_, err := s.St.UpdateKey(keyID, k.Version, func(x *domain.APIKey) error {
		if x.RevokedAt != nil {
			return nil // idempotent
		}
		now := time.Now().UTC()
		x.RevokedAt = &now
		return nil
	})
	if err != nil {
		return err
	}
	_ = s.Audit.Record(tenantID, "", actorKeyID, domain.ActKeyRevoke, map[string]string{"key": keyID})
	return nil
}

// ---- events -------------------------------------------------------------------

func (s *Service) ListEvents(tenantID string, q store.EventQuery) []*domain.AuditEvent {
	q.TenantID = tenantID
	return s.St.ListEvents(q)
}
