package domain

import (
	"time"

	"sandcastle.dev/sandcastle/internal/idgen"
)

// Audit actions. Every mutation and lifecycle change emits exactly one event.
const (
	ActSandboxCreate     = "sandbox.create"
	ActSandboxPause      = "sandbox.pause"
	ActSandboxResume     = "sandbox.resume"
	ActSandboxStop       = "sandbox.stop"
	ActSandboxDelete     = "sandbox.delete"
	ActSandboxExpire     = "sandbox.expire"
	ActSandboxIdleExpire = "sandbox.idle_expire"
	ActSandboxExtend     = "sandbox.extend"
	ActSnapshotPack      = "snapshot.pack"
	ActSandboxExec       = "sandbox.exec"
	ActFilePut           = "file.put"
	ActFileGet           = "file.get"
	ActFileList          = "file.list"
	ActFileDelete        = "file.delete"
	ActSnapshotCreate    = "snapshot.create"
	ActSnapshotDelete    = "snapshot.delete"
	ActTenantCreate      = "tenant.create"
	ActTenantUpdate      = "tenant.update"
	ActKeyCreate         = "key.create"
	ActKeyRevoke         = "key.revoke"
	ActKeyAuthFail       = "key.auth_failed"
	ActReconcileFix      = "reconcile.fix"
	ActTemplateCreate    = "template.create"
	ActTemplateDelete    = "template.delete"
)

// AuditEvent is append-only. Detail values must be non-sensitive: IDs,
// counts, durations — never file contents, env values, or key material.
type AuditEvent struct {
	ID        string            `json:"id"`
	TS        time.Time         `json:"ts"`
	TenantID  string            `json:"tenant_id,omitempty"`
	SandboxID string            `json:"sandbox_id,omitempty"`
	Actor     string            `json:"actor,omitempty"` // key ID; admin keys surface as themselves
	Action    string            `json:"action"`
	Detail    map[string]string `json:"detail,omitempty"`
}

func NewEvent(tenantID, sandboxID, actor, action string) *AuditEvent {
	return &AuditEvent{
		ID:        idgen.New(idgen.PrefixEvent),
		TS:        time.Now().UTC(),
		TenantID:  tenantID,
		SandboxID: sandboxID,
		Actor:     actor,
		Action:    action,
	}
}
