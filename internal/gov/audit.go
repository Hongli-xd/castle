package gov

import (
	"sync"

	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/store"
)

// Audit records governance-relevant events. Recording failures are logged by
// the caller via the returned error but never block the operation they
// describe — an audit hiccup must not wedge the control plane. Callers that
// need strict audit-before-mutate semantics (admin/key management) check the
// error themselves.
type Audit struct {
	store *store.Store
	mu    sync.Mutex
	drop  int64 // dropped-event counter for /metrics
}

func NewAudit(st *store.Store) *Audit {
	return &Audit{store: st}
}

// Record persists one event; tenant/sandbox/actor context comes from the
// request scope. Detail values must be sanitized by callers (no file
// contents, no key material) — enforced by convention and code review.
func (a *Audit) Record(tenantID, sandboxID, actor, action string, detail map[string]string) error {
	ev := domain.NewEvent(tenantID, sandboxID, actor, action)
	ev.Detail = detail
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.AppendEvent(ev); err != nil {
		a.drop++
		return err
	}
	return nil
}

// Dropped reports how many events failed to persist since boot.
func (a *Audit) Dropped() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.drop
}
