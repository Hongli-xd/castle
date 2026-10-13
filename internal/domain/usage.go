package domain

import "time"

// Usage is a tenant's cumulative metering record. Every field is monotonic and
// never reset, so a billing system can query it and diff successive snapshots
// into a per-period invoice.
//
// Resource-second counters (sandbox/cpu/memory) are the integral of the
// tenant's concurrent allocation over wall time, computed by the service's
// sampling loop — not by multiplying a request by a fixed rate. Exec counters
// accrue on the request path and are folded into this record on the same
// cadence, so a busy tenant never costs one WAL fsync per exec.
//
// The report's billing units follow directly:
//
//	vCPU-hours   = CPUMillicoreSeconds / (1000 * 3600)
//	GiB-hours    = MemoryMiBSeconds / (1024 * 3600)
//	session-hours = SandboxSeconds / 3600
type Usage struct {
	TenantID string `json:"tenant_id"`

	// SandboxSeconds is the sum, over every non-terminal sandbox, of the
	// seconds it existed. One sandbox held for one hour is one session-hour.
	SandboxSeconds float64 `json:"sandbox_seconds"`
	// CPUMillicoreSeconds is the sum of allocated CPU over time (1000
	// millicores = one vCPU). CPU accrues only while the sandbox is actually
	// scheduled (running/provisioning/resuming), not while paused.
	CPUMillicoreSeconds float64 `json:"cpu_millicore_seconds"`
	// MemoryMiBSeconds is the sum of allocated memory over time. Unlike CPU,
	// memory accrues for every non-terminal state, because a paused sandbox
	// still holds its working set (the "I/O is free but memory is billed"
	// distinction from the vendor survey).
	MemoryMiBSeconds float64 `json:"memory_mib_seconds"`

	// ExecCount is the number of commands actually executed (idempotency-key
	// replays do not increment it).
	ExecCount int64 `json:"exec_count"`
	// ExecSeconds is wall-clock time spent inside exec calls.
	ExecSeconds float64 `json:"exec_seconds"`

	UpdatedAt time.Time `json:"updated_at"`
	Version   int64     `json:"version"`
}

// NewUsage returns a zeroed record for a tenant. UpdatedAt is the only field
// that seeds to a real value, because it anchors the sampler's dt on restart.
func NewUsage(tenantID string) *Usage {
	return &Usage{TenantID: tenantID, UpdatedAt: time.Now().UTC()}
}
