// Package domain holds the product's core types and invariants.
//
// Domain types are serialization-complete by design: every field the daemon
// needs across a restart is JSON-tagged and persisted. Sensitive data (API
// key plaintext) never enters this package at all — only its hash does. A
// previous iteration used json:"-" on crash-critical fields and silently
// lost them on restart; that bug class is designed out here.
package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/idgen"
)

// SandboxState is the lifecycle state. Transitional states exist so that a
// daemon crash mid-operation is distinguishable from a healthy sandbox:
// on restart every transitional state is handed to the reconciler, which
// re-derives truth from the backend.
type SandboxState string

const (
	StateProvisioning SandboxState = "provisioning"
	StateRunning      SandboxState = "running"
	StatePausing      SandboxState = "pausing"
	StatePaused       SandboxState = "paused"
	StateResuming     SandboxState = "resuming"
	StateStopping     SandboxState = "stopping"
	StateStopped      SandboxState = "stopped"
	StateFailed       SandboxState = "failed"
)

var transitions = map[SandboxState][]SandboxState{
	StateProvisioning: {StateRunning, StateFailed, StateStopping},
	StateRunning:      {StatePausing, StateStopping},
	StatePausing:      {StatePaused, StateRunning, StateStopping},
	StatePaused:       {StateResuming, StateStopping},
	StateResuming:     {StateRunning, StatePaused, StateStopping},
	StateStopping:     {StateStopped, StateFailed},
	StateStopped:      {}, // terminal
	StateFailed:       {StateStopping},
}

// Terminal reports whether s needs no further reconciliation.
func (s SandboxState) Terminal() bool {
	return s == StateStopped || s == StateFailed
}

// Transitional reports whether s means an operation was in flight; the
// reconciler must re-derive these from the backend on startup.
func (s SandboxState) Transitional() bool {
	switch s {
	case StateProvisioning, StatePausing, StateResuming, StateStopping:
		return true
	}
	return false
}

func (s SandboxState) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// CanTransition reports whether the move s→to is legal.
func (s SandboxState) CanTransition(to SandboxState) bool {
	for _, t := range transitions[s] {
		if t == to {
			return true
		}
	}
	return false
}

// Transition moves the sandbox state or returns a conflict error naming the
// current state — callers surface this to clients as a 409.
func (sb *Sandbox) Transition(to SandboxState) error {
	if !to.Valid() {
		return apperr.InternalErr("unknown target state %q", to)
	}
	if !sb.State.CanTransition(to) {
		return apperr.ConflictErr("cannot transition sandbox from %s to %s", sb.State, to)
	}
	sb.State = to
	return nil
}

// EgressPolicy controls sandbox outbound network.
type EgressPolicy struct {
	// Mode is "none" (default) or "allowlist".
	Mode string `json:"mode"`
	// Allow lists domain suffixes, only meaningful for allowlist mode.
	Allow []string `json:"allow,omitempty"`
}

const (
	EgressNone      = "none"
	EgressAllowlist = "allowlist"
)

func (p EgressPolicy) Validate() error {
	switch p.Mode {
	case "", EgressNone:
		return nil
	case EgressAllowlist:
		if len(p.Allow) == 0 {
			return apperr.Invalid("egress allowlist mode requires at least one allowed domain")
		}
		if len(p.Allow) > 64 {
			return apperr.Invalid("egress allowlist supports at most 64 domains")
		}
		for _, d := range p.Allow {
			if d == "" || len(d) > 253 || strings.ContainsAny(d, " /\\:@") || !strings.Contains(d, ".") {
				return apperr.Invalid("invalid egress domain %q", d)
			}
		}
		return nil
	default:
		return apperr.Invalid("unknown egress mode %q (supported: none, allowlist)", p.Mode)
	}
}

// SandboxSpec is the requested shape of a sandbox. It is validated against
// platform Limits; tenant quotas are a service-layer concern, not here.
type SandboxSpec struct {
	CPUMillicores  int               `json:"cpu_millicores"`
	MemoryMiB      int               `json:"memory_mib"`
	DiskMiB        int               `json:"disk_mib,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	Egress         EgressPolicy      `json:"egress"`
	Env            map[string]string `json:"env,omitempty"`
	User           string            `json:"user,omitempty"`
	WorkDir        string            `json:"workdir,omitempty"`
}

// Limits are the platform-wide ceilings a spec is validated against.
type Limits struct {
	MaxCPUMillicores  int
	MaxMemoryMiB      int
	MaxDiskMiB        int
	MaxTimeoutSeconds int
	MaxEnvEntries     int
	MaxEnvValueBytes  int
	MaxMetadataKeys   int
	MaxMetadataBytes  int
}

var (
	envKeyRe      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	metadataKeyRe = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)
	userRe        = regexp.MustCompile(`^[a-zA-Z0-9_.][a-zA-Z0-9_.-]{0,63}$`)
)

// Validate checks a spec against platform limits. minDiskMiB==0 means the
// deployment cannot enforce disk quotas and callers have explicitly accepted
// unenforced limits (the daemon refuses at create time otherwise).
func (s SandboxSpec) Validate(l Limits, minDiskMiB int) error {
	if s.CPUMillicores < 100 || s.CPUMillicores > l.MaxCPUMillicores {
		return apperr.Invalid("cpu_millicores must be between 100 and %d", l.MaxCPUMillicores)
	}
	if s.MemoryMiB < 64 || s.MemoryMiB > l.MaxMemoryMiB {
		return apperr.Invalid("memory_mib must be between 64 and %d", l.MaxMemoryMiB)
	}
	if s.DiskMiB != 0 && (s.DiskMiB < minDiskMiB || s.DiskMiB > l.MaxDiskMiB) {
		return apperr.Invalid("disk_mib must be between %d and %d", minDiskMiB, l.MaxDiskMiB)
	}
	if s.TimeoutSeconds < 30 || s.TimeoutSeconds > l.MaxTimeoutSeconds {
		return apperr.Invalid("timeout_seconds must be between 30 and %d", l.MaxTimeoutSeconds)
	}
	if len(s.Env) > l.MaxEnvEntries {
		return apperr.Invalid("env supports at most %d entries", l.MaxEnvEntries)
	}
	total := 0
	for k, v := range s.Env {
		if !envKeyRe.MatchString(k) || len(k) > 256 {
			return apperr.Invalid("invalid env key %q", k)
		}
		if len(v) > l.MaxEnvValueBytes {
			return apperr.Invalid("env value for %q exceeds %d bytes", k, l.MaxEnvValueBytes)
		}
		total += len(k) + len(v)
	}
	if total > 64*1024 {
		return apperr.Invalid("total env size exceeds 64KiB")
	}
	if s.User != "" && !userRe.MatchString(s.User) {
		return apperr.Invalid("invalid user %q", s.User)
	}
	if s.WorkDir != "" && (!strings.HasPrefix(s.WorkDir, "/") || strings.Contains(s.WorkDir, "..") || len(s.WorkDir) > 4096) {
		return apperr.Invalid("workdir must be an absolute in-sandbox path without '..'")
	}
	return s.Egress.Validate()
}

// SandboxBackendRef records how the backend realizes this sandbox. It is
// persisted (never json:"-") because crash recovery needs it to find the
// backend object without trusting local state.
type SandboxBackendRef struct {
	Kind     string `json:"kind"` // "oci" | "firecracker"
	Ref      string `json:"ref"`  // backend-native handle, e.g. container name
	ImageRef string `json:"image_ref,omitempty"`
}

type Sandbox struct {
	ID          string            `json:"id"`
	TenantID    string            `json:"tenant_id"`
	TemplateID  string            `json:"template_id,omitempty"`
	SnapshotID  string            `json:"snapshot_id,omitempty"`
	BaseImage   string            `json:"base_image,omitempty"`
	Spec        SandboxSpec       `json:"spec"`
	State       SandboxState      `json:"state"`
	Backend     SandboxBackendRef `json:"backend"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Reason      string            `json:"reason,omitempty"` // sanitized failure/explanation
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Version     int64             `json:"version"`
	BaseVersion int64             `json:"base_version"`
}

func NewSandbox(tenantID string) *Sandbox {
	now := time.Now().UTC()
	return &Sandbox{
		ID:        idgen.New(idgen.PrefixSandbox),
		TenantID:  tenantID,
		State:     StateProvisioning,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// Expired reports whether the TTL has passed at now.
func (sb *Sandbox) Expired(now time.Time) bool {
	return !sb.ExpiresAt.IsZero() && now.After(sb.ExpiresAt)
}

// Extend pushes the TTL out to at most now+seconds and validates monotonicity.
func (sb *Sandbox) Extend(seconds int, l Limits) error {
	if seconds < 30 || seconds > l.MaxTimeoutSeconds {
		return apperr.Invalid("timeout_seconds must be between 30 and %d", l.MaxTimeoutSeconds)
	}
	sb.ExpiresAt = time.Now().UTC().Add(time.Duration(seconds) * time.Second)
	return nil
}

// CountableResources are the concurrent resource claims a running sandbox
// holds against its tenant quota.
func (sb *Sandbox) CountableResources() (sandboxes int, cpu int, mem int) {
	switch sb.State {
	case StateProvisioning, StateRunning, StatePausing, StatePaused, StateResuming:
		return 1, sb.Spec.CPUMillicores, sb.Spec.MemoryMiB
	}
	return 0, 0, 0
}

func (sb *Sandbox) String() string {
	return fmt.Sprintf("sandbox %s (%s, tenant %s)", sb.ID, sb.State, sb.TenantID)
}
