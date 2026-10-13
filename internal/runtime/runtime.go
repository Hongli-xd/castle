// Package runtime defines the backend abstraction between the control plane
// (which owns state) and the machinery that actually isolates workloads.
//
// Two implementations ship:
//   - oci: Docker Engine API containers. Runs anywhere Docker runs, including
//     colima/Docker Desktop on macOS for development. Kernel is shared with
//     the host — this is the "strong process isolation" tier.
//   - firecracker: microVM-per-sandbox with kernel-level isolation and
//     memory snapshots. Requires Linux + KVM; ships as a compile-checked
//     skeleton and registers itself only when /dev/kvm is present.
//
// The interface refuses to express what a backend cannot enforce: egress
// allowlists, for example, return apperr.Unimplemented rather than creating
// a sandbox the caller believes is restricted when it is not.
package runtime

import (
	"context"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
)

// CreateRequest carries everything a backend needs to realize a sandbox.
type CreateRequest struct {
	Name          string // backend-native object name, e.g. sandboxd-<id>
	Image         string // backend-native image reference
	CPUMillicores int    // 1000 = one full core
	MemoryMiB     int    // hard cap
	Env           map[string]string
	User          string            // "" = image default
	WorkDir       string            // "" = image default
	Labels        map[string]string // ownership tags; backends MUST set these — orphan recovery depends on them
	// Mounts are read-only host directories. Empty means none. A backend
	// that cannot enforce read-only must refuse the create rather than mount
	// writable — there is no writable field to get wrong on purpose.
	Mounts []Mount
}

// Mount attaches one host directory into a sandbox. Read-only is the only
// mode that exists: a writable host mount is a host-write escape, so it is
// unrepresentable at the type level instead of "validated away".
type Mount struct {
	Source string // absolute host path
	Target string // absolute in-sandbox path
}

// Status is the backend's view of a live object. The reconciler trusts this
// over local state.
type Status struct {
	Running   bool
	Paused    bool
	Raw       string // backend-native status text for logs/events
	ExitCode  int
	OOMKilled bool
	StartedAt time.Time
}

// ExecRequest is one command execution inside a sandbox.
type ExecRequest struct {
	Argv    []string          // argv[0] must be resolved inside the sandbox
	Env     map[string]string // additional env for this exec only
	User    string            // "" = sandbox default
	WorkDir string            // "" = sandbox workdir
	Timeout time.Duration     // must be > 0; hard deadline
	// MaxOutput caps the bytes captured per stream. 0 means DefaultOutputCap,
	// NOT "unlimited" — the control plane buffers this in its own memory, so
	// the service layer rejects anything above config.MaxExecOutput before it
	// reaches a backend. Receiving a value above that ceiling here is a
	// programming error, and backends may refuse it.
	MaxOutput int
}

// DefaultOutputCap is the per-stream capture size a backend uses when the
// caller leaves ExecRequest.MaxOutput at zero. It lives here, in the interface
// contract, so the service layer and every backend quote the same number
// instead of each hard-coding their own "1 MiB".
const DefaultOutputCap = 1 << 20 // 1 MiB

// MaxBackendOutput bounds what a backend will accept as an explicit
// MaxOutput. It exists to catch a service layer that forgot to enforce
// config.MaxExecOutput: a value past this is never a legitimate request.
const MaxBackendOutput = 1 << 30 // 1 GiB

// ExecResult is the outcome of one execution.
type ExecResult struct {
	ExitCode  int           `json:"exit_code"`
	Stdout    []byte        `json:"stdout"`
	Stderr    []byte        `json:"stderr"`
	Duration  time.Duration `json:"duration_ns"`
	Truncated bool          `json:"truncated"`
	TimedOut  bool          `json:"timed_out"`
	// Replayed reports that this result was served from the control plane's
	// idempotency cache — a retry of a command whose response the caller lost
	// — rather than a fresh execution. Backends never set it; only the
	// service layer does, on a cache hit or a single-flight dedupe.
	Replayed bool `json:"replayed,omitempty"`
}

// PauseOptions carries what the backend needs for reclaim-on-pause.
// MemoryBytes is the sandbox's configured memory cap in bytes; 0 skips
// reclamation (plain freeze).
type PauseOptions struct {
	MemoryBytes int64
}

// FileInfo describes one directory entry.
type FileInfo struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
	Mode  string `json:"mode,omitempty"`
}

type PutFileRequest struct {
	Path    string // absolute in-sandbox path (file, not dir)
	Content []byte
	Mode    uint32 // octal, e.g. 0o644; 0 = 0o644
}

// Orphan describes a backend object carrying our ownership labels, used by
// crash recovery to adopt or reap objects after a daemon death.
type Orphan struct {
	Ref    string
	Labels map[string]string
	Status Status
}

// Isolation tiers, from weakest to strongest.
const (
	// TierContainer is process isolation: namespaces, dropped capabilities,
	// no network, pids and memory ceilings — but the sandbox shares the host
	// kernel. A kernel LPE is a sandbox escape.
	TierContainer = "container-shared-kernel"
	// TierMicroVM is a dedicated guest kernel per sandbox via KVM.
	TierMicroVM = "microvm"
)

// IsolationFact is one independently checkable claim about a running sandbox,
// as observed from inside it rather than from the configuration that was meant
// to produce it.
type IsolationFact struct {
	Name     string `json:"name"`     // e.g. "seccomp", "uid_map", "core_dump"
	Observed string `json:"observed"` // what the kernel actually reported
	Want     string `json:"want"`     // what the isolation contract requires
	OK       bool   `json:"ok"`
	Note     string `json:"note,omitempty"`
}

// IsolationReport is the outcome of Backend.VerifyIsolation: evidence, not
// intent. It is deliberately a list of raw observations with their expected
// values, so a reviewer can disagree with the assertions without having to
// re-run anything.
type IsolationReport struct {
	Tier      string          `json:"tier"`
	Userns    string          `json:"userns"` // "private" or "off"
	Image     string          `json:"image"`
	Facts     []IsolationFact `json:"facts"`
	Failures  []string        `json:"failures,omitempty"`
	CheckedAt time.Time       `json:"checked_at"`
}

// OK reports whether every asserted fact held.
func (r *IsolationReport) OK() bool { return len(r.Failures) == 0 }

// Backend is the pluggable isolation driver. All methods must be safe for
// concurrent use and must return *apperr.E errors.
type Backend interface {
	Name() string

	// IsolationTier names the strength of the isolation this backend
	// actually delivers, from the constants above. It is part of the
	// interface, not a comment, so that:
	//   - a backend cannot be added without stating its tier;
	//   - the daemon can refuse to start when the operator asked for a tier
	//     the host cannot provide, instead of silently downgrading;
	//   - the running tier is machine-readable at /readyz, so a customer can
	//     verify what they are paying for rather than trusting a README.
	IsolationTier() string

	// VerifyIsolation makes the backend PROVE its isolation on this host, by
	// running a probe workload inside a real sandbox and reading back
	// kernel-reported facts: the uid map, seccomp mode, the capability sets,
	// the LSM label, the core-dump rlimit, and canary attempts to escape
	// (mounting, reading kernel memory).
	//
	// It belongs in the interface for the same reason IsolationTier does, but
	// the failure it guards against is sharper: a container engine will accept
	// security settings it cannot honour and report them back as applied. A
	// daemon with no user-namespace remap stores and echoes
	// HostConfig.UsernsMode="private" while running the sandbox with the
	// identity uid map, so reading configuration — even read back from the
	// engine — is not evidence. Only what the kernel reports from inside the
	// sandbox is.
	//
	// image names the workload to probe in; a backend may substitute a local
	// image when it is empty. Returning an error means the check could not be
	// performed, which must never be conflated with a passing check.
	VerifyIsolation(ctx context.Context, image string) (*IsolationReport, error)

	// Ping verifies the backend is reachable; /readyz fails without it.
	Ping(ctx context.Context) error

	// Lifecycle
	Create(ctx context.Context, req CreateRequest) (ref string, err error)
	Start(ctx context.Context, ref string) error
	// Pause freezes the workload. When opts.MemoryBytes > 0 the backend
	// additionally performs best-effort proactive reclamation while the
	// workload is frozen (DSec §6.3): the memory limit is temporarily
	// lowered so the kernel evicts file pages and swaps anonymous pages;
	// Resume restores the original limit BEFORE unfreezing.
	Pause(ctx context.Context, ref string, opts PauseOptions) error
	Resume(ctx context.Context, ref string, opts PauseOptions) error
	Stop(ctx context.Context, ref string, force bool) error
	Remove(ctx context.Context, ref string) error
	Inspect(ctx context.Context, ref string) (Status, error)
	// Rename rebrands a backend object without recreating it — the warm
	// pool hands a pristine object to a tenant this way.
	Rename(ctx context.Context, ref, newName string) error

	// Execution and files
	Exec(ctx context.Context, ref string, req ExecRequest) (*ExecResult, error)
	PutFile(ctx context.Context, ref string, req PutFileRequest) error
	GetFile(ctx context.Context, ref, path string, maxSize int64) ([]byte, error)
	ListDir(ctx context.Context, ref, path string) ([]FileInfo, error)
	DeletePath(ctx context.Context, ref, path string) error
	// CopyDir grafts the directory tree at srcPath of srcRef onto dstPath
	// of dstRef (same-path grafting: the tree keeps its basename). This is
	// the primitive behind template layer composition (DSec §5.1) — the
	// base stays untouched, each toolkit layer is grafted on top.
	CopyDir(ctx context.Context, srcRef, srcPath, dstRef, dstPath string) error

	// Snapshots and images
	Commit(ctx context.Context, ref, tag string) (imageRef string, sizeBytes int64, err error)
	HasImage(ctx context.Context, imageRef string) (bool, error)
	RemoveImage(ctx context.Context, imageRef string) error
	ImportRootfs(ctx context.Context, rootfsDir, tag string) (imageRef string, sizeBytes int64, err error)

	// Recovery
	// Orphans lists backend objects labeled as ours, whatever their state,
	// so the reconciler can adopt or reap them after a daemon crash.
	Orphans(ctx context.Context) ([]Orphan, error)
	// ManagedImages lists container images this product created (snapshot
	// and template commits). They carry a tag, so they are not *dangling*
	// and no engine's own garbage collection will ever remove them: if the
	// data directory is lost or restored from before a commit, nothing else
	// on the node knows the image exists. This is what lets the reconciler
	// find them and delete the ones no record references.
	ManagedImages(ctx context.Context) ([]string, error)
}

// SpecLimits mirrors domain limits that backends need for admission.
type SpecLimits = domain.Limits

// CheckEgress enforces the honest-refusal contract: a backend that cannot
// truly enforce a policy must refuse the create.
func CheckEgress(b Backend, policy domain.EgressPolicy) error {
	switch policy.Mode {
	case "", domain.EgressNone:
		return nil
	case domain.EgressAllowlist:
		return apperr.Unimpl("backend %q cannot enforce egress allowlists; use egress=none or deploy the firecracker backend (roadmap)", b.Name())
	default:
		return apperr.Invalid("unknown egress mode %q", policy.Mode)
	}
}
