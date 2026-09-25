package domain

import (
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/idgen"
)

type TenantStatus string

const (
	TenantActive    TenantStatus = "active"
	TenantSuspended TenantStatus = "suspended"
)

// Rate is a token-bucket policy for one operation group.
type Rate struct {
	PerSecond float64 `json:"per_second"`
	Burst     int     `json:"burst"`
}

// Operation groups. Rate limits are per tenant × group: a create burst must
// not starve exec traffic, and admin operations get their own small budget.
const (
	GroupCreate   = "create"
	GroupExec     = "exec"
	GroupFiles    = "files"
	GroupSnapshot = "snapshot"
	GroupAdmin    = "admin"
	GroupRead     = "read"
)

var AllGroups = []string{GroupCreate, GroupExec, GroupFiles, GroupSnapshot, GroupAdmin, GroupRead}

// TenantQuota caps concurrent resources and operation rates for a tenant.
// Zero fields inherit the platform defaults at service layer.
type TenantQuota struct {
	MaxSandboxes     int              `json:"max_sandboxes"`
	MaxCPUMillicores int              `json:"max_cpu_millicores"`
	MaxMemoryMiB     int              `json:"max_memory_mib"`
	MaxSnapshots     int              `json:"max_snapshots"`
	MaxSnapshotBytes int64            `json:"max_snapshot_bytes"`
	Rates            map[string]*Rate `json:"rates,omitempty"` // group → rate
}

func (q TenantQuota) Validate(l Limits) error {
	if q.MaxSandboxes < 0 || q.MaxSandboxes > 10_000 {
		return apperr.Invalid("max_sandboxes out of range")
	}
	if q.MaxCPUMillicores < 0 || q.MaxCPUMillicores > 100*l.MaxCPUMillicores {
		return apperr.Invalid("max_cpu_millicores out of range")
	}
	if q.MaxMemoryMiB < 0 || q.MaxMemoryMiB > 100*l.MaxMemoryMiB {
		return apperr.Invalid("max_memory_mib out of range")
	}
	for g, r := range q.Rates {
		if !validGroup(g) {
			return apperr.Invalid("unknown rate group %q", g)
		}
		if r == nil || r.PerSecond <= 0 || r.PerSecond > 10_000 || r.Burst < 1 || r.Burst > 100_000 {
			return apperr.Invalid("invalid rate for group %q", g)
		}
	}
	return nil
}

func validGroup(g string) bool {
	for _, a := range AllGroups {
		if a == g {
			return true
		}
	}
	return false
}

type Tenant struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Status      TenantStatus `json:"status"`
	Quota       TenantQuota  `json:"quota"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	Version     int64        `json:"version"`
	BaseVersion int64        `json:"base_version"`
}

func NewTenant(name string) *Tenant {
	now := time.Now().UTC()
	return &Tenant{
		ID:        idgen.New(idgen.PrefixTenant),
		Name:      name,
		Status:    TenantActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func (t *Tenant) Validate(l Limits) error {
	if t.Name == "" || len(t.Name) > 128 {
		return apperr.Invalid("tenant name must be 1..128 characters")
	}
	switch t.Status {
	case TenantActive, TenantSuspended:
	case "":
		t.Status = TenantActive
	default:
		return apperr.Invalid("unknown tenant status %q", t.Status)
	}
	return t.Quota.Validate(l)
}

type KeyScope string

const (
	ScopeTenant KeyScope = "tenant"
	ScopeAdmin  KeyScope = "admin"
)

// APIKey stores only the SHA-256 hash of the secret. The plaintext is shown
// exactly once at creation and never persisted, logged, or echoed.
type APIKey struct {
	ID          string     `json:"id"`
	TenantID    string     `json:"tenant_id"` // "" only for platform admin keys
	Name        string     `json:"name"`
	Scope       KeyScope   `json:"scope"`
	Prefix      string     `json:"prefix"` // lookup handle: first 12 chars of secret
	Hash        string     `json:"hash"`   // hex sha256 of full secret
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	Version     int64      `json:"version"`
	BaseVersion int64      `json:"base_version"`
}

func NewAPIKey(tenantID, name string, scope KeyScope) *APIKey {
	return &APIKey{
		ID:        idgen.New(idgen.PrefixKey),
		TenantID:  tenantID,
		Name:      name,
		Scope:     scope,
		CreatedAt: time.Now().UTC(),
	}
}

// Valid reports whether the key may authenticate a request at time now.
func (k *APIKey) Valid(now time.Time) bool {
	if k.RevokedAt != nil {
		return false
	}
	if k.ExpiresAt != nil && now.After(*k.ExpiresAt) {
		return false
	}
	switch k.Scope {
	case ScopeTenant:
		return k.TenantID != ""
	case ScopeAdmin:
		return true
	}
	return false
}
