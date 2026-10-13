package service

// Per-tenant usage metering.
//
// Two independent accrual paths feed one durable record per tenant:
//
//   - Resource-seconds (sandbox / CPU / memory) are sampled on a ticker: the
//     sampler integrates the tenant's *concurrent allocation* over the wall
//     time since the last sample. That is the honest definition of a
//     "vCPU-hour" — it is not a request multiplied by a fixed rate, and it is
//     what makes the number survive a sandbox living for hours with no API
//     traffic.
//
//   - Exec counters are event-driven: every real execution increments them.
//     They are held in an in-memory delta and folded into the durable record on
//     the same cadence, so a busy tenant does not pay one WAL fsync per exec.
//
// The two paths merge into `pending`, an in-memory delta table, and `flush`
// persists it with one WAL frame per tenant per interval. A crash therefore
// loses at most one interval of accrual — standard metering semantics, and the
// reason metering is a sampler rather than a synchronous ledger. Live queries
// (service.Metering) always see stored + pending, so the un-flushed tail is
// visible the moment it accrues, not only after the next flush.

import (
	"sort"
	"sync"
	"time"

	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/store"
)

type meter struct {
	st         *store.Store
	mu         sync.Mutex
	pending    map[string]*domain.Usage // tenantID → accruals since last flush
	now        func() time.Time
	lastSample time.Time
}

func newMeter(st *store.Store) *meter {
	return &meter{
		st:         st,
		pending:    map[string]*domain.Usage{},
		now:        time.Now,
		lastSample: time.Now(),
	}
}

// RecordExec accrues one execution. Replays never call it: an idempotency-key
// retry must not be billed as a second command. It is lock-and-map only — no
// store access, no fsync — so it costs the request path nothing measurable.
func (m *meter) RecordExec(tenantID string, d time.Duration) {
	m.mu.Lock()
	u := m.pending[tenantID]
	if u == nil {
		u = &domain.Usage{TenantID: tenantID}
		m.pending[tenantID] = u
	}
	u.ExecCount++
	u.ExecSeconds += d.Seconds()
	m.mu.Unlock()
}

// tick runs one sample-and-flush cycle. It is called by the meter loop every
// MeterEvery; the split exists so tests can drive sample and flush separately.
func (m *meter) tick() {
	m.sample()
	m.flush()
}

// sample integrates resource-seconds since the last sample into pending.
func (m *meter) sample() {
	m.mu.Lock()
	now := m.now()
	dt := now.Sub(m.lastSample)
	m.lastSample = now
	m.mu.Unlock()
	if dt <= 0 {
		return
	}
	secs := dt.Seconds()

	type acc struct{ sandbox, cpu, mem float64 }
	deltas := map[string]acc{}
	m.st.ForEachSandbox(func(sb *domain.Sandbox) bool {
		if sb.State.Terminal() {
			return true
		}
		// CPU accrues only while a sandbox can actually run. Memory accrues in
		// every non-terminal state, because a paused sandbox still holds its
		// working set — this is the "I/O is free, memory is billed" split.
		cpu := 0
		switch sb.State {
		case domain.StateRunning, domain.StateProvisioning, domain.StateResuming:
			cpu = sb.Spec.CPUMillicores
		}
		a := deltas[sb.TenantID]
		a.sandbox += secs
		a.cpu += float64(cpu) * secs
		a.mem += float64(sb.Spec.MemoryMiB) * secs
		deltas[sb.TenantID] = a
		return true
	})

	m.mu.Lock()
	for tenantID, a := range deltas {
		u := m.pending[tenantID]
		if u == nil {
			u = &domain.Usage{TenantID: tenantID}
			m.pending[tenantID] = u
		}
		u.SandboxSeconds += a.sandbox
		u.CPUMillicoreSeconds += a.cpu
		u.MemoryMiBSeconds += a.mem
	}
	m.mu.Unlock()
}

// flush persists the pending delta to the durable record. The pending map is
// swapped to a fresh empty one under the lock, then each tenant's delta is
// written outside the lock — so an exec arriving mid-flush accrues into the
// *next* batch and is never lost, while the request path never blocks on a
// metering fsync.
func (m *meter) flush() {
	m.mu.Lock()
	if len(m.pending) == 0 {
		m.mu.Unlock()
		return
	}
	snapshot := m.pending
	m.pending = map[string]*domain.Usage{}
	m.mu.Unlock()

	ids := make([]string, 0, len(snapshot))
	for id := range snapshot {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, tenantID := range ids {
		delta := snapshot[tenantID]
		_, _ = m.st.UpdateUsage(tenantID, func(u *domain.Usage) error {
			u.SandboxSeconds += delta.SandboxSeconds
			u.CPUMillicoreSeconds += delta.CPUMillicoreSeconds
			u.MemoryMiBSeconds += delta.MemoryMiBSeconds
			u.ExecCount += delta.ExecCount
			u.ExecSeconds += delta.ExecSeconds
			return nil
		})
	}
}

// usage returns a tenant's usage: durable record plus any not-yet-flushed
// delta. It is what the metering API serves, so an exec is visible in a read
// immediately rather than only after the next flush.
func (m *meter) usage(tenantID string) *domain.Usage {
	u, _ := m.st.GetUsage(tenantID)
	if u == nil {
		u = domain.NewUsage(tenantID)
	}
	m.mu.Lock()
	if p := m.pending[tenantID]; p != nil {
		u.SandboxSeconds += p.SandboxSeconds
		u.CPUMillicoreSeconds += p.CPUMillicoreSeconds
		u.MemoryMiBSeconds += p.MemoryMiBSeconds
		u.ExecCount += p.ExecCount
		u.ExecSeconds += p.ExecSeconds
	}
	m.mu.Unlock()
	return u
}

// meterLoop drives the sample-and-flush cadence and guarantees a final flush on
// shutdown so a graceful stop does not drop the in-memory tail.
func (s *Service) meterLoop() {
	t := time.NewTicker(s.Cfg.MeterEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			s.meter.flush()
			return
		case <-t.C:
			s.meter.tick()
		}
	}
}

// Metering returns a tenant's usage, or every tenant's when tenantID is empty
// (admin scope). Results always include the not-yet-flushed tail.
func (s *Service) Metering(tenantID string) []*domain.Usage {
	if tenantID != "" {
		return []*domain.Usage{s.meter.usage(tenantID)}
	}
	out := make([]*domain.Usage, 0, 8)
	for _, id := range s.meter.tenantIDs() {
		out = append(out, s.meter.usage(id))
	}
	return out
}

// tenantIDs is the union of tenants with a durable record and tenants with an
// un-flushed delta, so a freshly-active tenant still shows up in the list.
func (m *meter) tenantIDs() []string {
	set := map[string]bool{}
	for _, u := range m.st.ListUsage() {
		set[u.TenantID] = true
	}
	m.mu.Lock()
	for id := range m.pending {
		set[id] = true
	}
	m.mu.Unlock()
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
