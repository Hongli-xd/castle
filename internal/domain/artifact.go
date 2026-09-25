package domain

import (
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/idgen"
)

// Snapshot captures a sandbox's writable filesystem layer plus metadata.
// Memory-level snapshots are a MicroVM-backend capability and are not part
// of the OCI backend contract (see docs/DESIGN.md §3.4).
type Snapshot struct {
	ID          string      `json:"id"`
	TenantID    string      `json:"tenant_id"`
	SandboxID   string      `json:"sandbox_id"`
	Name        string      `json:"name"`
	ImageRef    string      `json:"image_ref"` // backend artifact handle
	SizeBytes   int64       `json:"size_bytes"`
	Spec        SandboxSpec `json:"spec"` // origin spec; defaults when restoring
	CreatedAt   time.Time   `json:"created_at"`
	Version     int64       `json:"version"`
	BaseVersion int64       `json:"base_version"`
}

func NewSnapshot(tenantID, sandboxID, name string) *Snapshot {
	return &Snapshot{
		ID:        idgen.New(idgen.PrefixSnapshot),
		TenantID:  tenantID,
		SandboxID: sandboxID,
		Name:      name,
		CreatedAt: time.Now().UTC(),
	}
}

func (s *Snapshot) Validate() error {
	if len(s.Name) == 0 || len(s.Name) > 128 {
		return apperr.Invalid("snapshot name must be 1..128 characters")
	}
	for _, r := range s.Name {
		ok := r == '-' || r == '_' || r == '.' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return apperr.Invalid("snapshot name may contain letters, digits, '-', '_', '.'")
		}
	}
	return nil
}

// Template is a reusable sandbox base: a backend image artifact plus a
// default spec. Platform templates (TenantID=="") are visible to everyone.
type Template struct {
	ID          string      `json:"id"`
	TenantID    string      `json:"tenant_id"` // "" = platform template
	Name        string      `json:"name"`
	ImageRef    string      `json:"image_ref"`
	DefaultSpec SandboxSpec `json:"default_spec"`
	Public      bool        `json:"public"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	Version     int64       `json:"version"`
	BaseVersion int64       `json:"base_version"`
}

func NewTemplate(tenantID, name string) *Template {
	now := time.Now().UTC()
	return &Template{
		ID:        idgen.New(idgen.PrefixTemplate),
		TenantID:  tenantID,
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func (t *Template) Validate(l Limits) error {
	if t.Name == "" || len(t.Name) > 128 {
		return apperr.Invalid("template name must be 1..128 characters")
	}
	if t.ImageRef == "" || len(t.ImageRef) > 512 {
		return apperr.Invalid("template image_ref must be 1..512 characters")
	}
	return t.DefaultSpec.Validate(l, 0)
}
