package gov

import (
	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/store"
)

// Quotas checks tenant-level resource ceilings before admission. Defaults
// fill in whatever the tenant does not override (zero = inherit).
type Quotas struct {
	store    *store.Store
	defaults domain.TenantQuota
}

// NewQuotas copies the platform defaults. The copy must be deep for Rates: a
// plain struct copy shares the map, so a later mutation of config.Defaults
// would silently rewrite the rates already handed to a running Quotas, and
// RateFor would return a policy nobody configured.
func NewQuotas(st *store.Store, defaults domain.TenantQuota) *Quotas {
	return &Quotas{store: st, defaults: cloneQuota(defaults)}
}

func cloneQuota(q domain.TenantQuota) domain.TenantQuota {
	c := q
	if q.Rates != nil {
		c.Rates = make(map[string]*domain.Rate, len(q.Rates))
		for g, r := range q.Rates {
			if r == nil {
				c.Rates[g] = nil
				continue
			}
			rc := *r
			c.Rates[g] = &rc
		}
	}
	return c
}

// Effective merges tenant quota over platform defaults.
func (q *Quotas) Effective(t *domain.Tenant) domain.TenantQuota {
	eff := q.defaults
	if t.Quota.MaxSandboxes > 0 {
		eff.MaxSandboxes = t.Quota.MaxSandboxes
	}
	if t.Quota.MaxCPUMillicores > 0 {
		eff.MaxCPUMillicores = t.Quota.MaxCPUMillicores
	}
	if t.Quota.MaxMemoryMiB > 0 {
		eff.MaxMemoryMiB = t.Quota.MaxMemoryMiB
	}
	if t.Quota.MaxSnapshots > 0 {
		eff.MaxSnapshots = t.Quota.MaxSnapshots
	}
	if t.Quota.MaxSnapshotBytes > 0 {
		eff.MaxSnapshotBytes = t.Quota.MaxSnapshotBytes
	}
	return eff
}

// RateFor returns the effective rate policy for a tenant+group, or nil when
// the group is unconfigured (unlimited).
func (q *Quotas) RateFor(t *domain.Tenant, group string) *domain.Rate {
	if r, ok := t.Quota.Rates[group]; ok && r != nil {
		return r
	}
	return q.defaults.Rates[group]
}

// CheckCreate enforces concurrent-sandbox, CPU, and memory ceilings. It is
// best-effort under races (two concurrent creates can both pass the check);
// the ceiling then trips on the next create instead — standard admission
// semantics, documented in the threat model.
func (q *Quotas) CheckCreate(t *domain.Tenant, spec domain.SandboxSpec) error {
	eff := q.Effective(t)
	var n, cpu, mem int
	q.store.ForEachSandbox(func(sb *domain.Sandbox) bool {
		if sb.TenantID != t.ID {
			return true
		}
		sn, c, m := sb.CountableResources()
		n += sn
		cpu += c
		mem += m
		return true
	})
	if eff.MaxSandboxes > 0 && n+1 > eff.MaxSandboxes {
		return apperr.Quota("tenant holds %d sandboxes, quota is %d", n, eff.MaxSandboxes)
	}
	if eff.MaxCPUMillicores > 0 && cpu+spec.CPUMillicores > eff.MaxCPUMillicores {
		return apperr.Quota("tenant would use %d millicores, quota is %d", cpu+spec.CPUMillicores, eff.MaxCPUMillicores)
	}
	if eff.MaxMemoryMiB > 0 && mem+spec.MemoryMiB > eff.MaxMemoryMiB {
		return apperr.Quota("tenant would use %d MiB memory, quota is %d", mem+spec.MemoryMiB, eff.MaxMemoryMiB)
	}
	return nil
}

// CheckSnapshot enforces snapshot count and byte ceilings.
func (q *Quotas) CheckSnapshot(t *domain.Tenant, incomingBytes int64) error {
	eff := q.Effective(t)
	if eff.MaxSnapshots > 0 && q.store.SnapshotCount(t.ID)+1 > eff.MaxSnapshots {
		return apperr.Quota("tenant holds %d snapshots, quota is %d", q.store.SnapshotCount(t.ID), eff.MaxSnapshots)
	}
	if eff.MaxSnapshotBytes > 0 {
		used := q.store.SnapshotBytesUsed(t.ID)
		if used+incomingBytes > eff.MaxSnapshotBytes {
			return apperr.Quota("snapshot storage would reach %d bytes, quota is %d", used+incomingBytes, eff.MaxSnapshotBytes)
		}
	}
	return nil
}
