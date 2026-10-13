package tests

// End-to-end metering: the endpoint exists, is scoped to the tenant, and its
// exec counter is request-driven — an exec is visible to a billing read the
// moment it completes, without waiting for the 30s sampler flush. Resource
// -second integration lives at the unit level (internal/service/meter_test.go)
// where the sampler's clock is deterministic.

import (
	"net/http"
	"testing"

	"sandcastle.dev/sandcastle/internal/domain"
)

func (tn *Tenant) metering(t *testing.T) []*domain.Usage {
	t.Helper()
	return decode[usagePage](t, tn.Client.Get("/v1/metering").MustStatus(t, http.StatusOK)).Usage
}

func usageFor(t *testing.T, list []*domain.Usage, tenantID string) *domain.Usage {
	t.Helper()
	for _, u := range list {
		if u.TenantID == tenantID {
			return u
		}
	}
	t.Fatalf("usage list does not contain tenant %s: %+v", tenantID, list)
	return nil
}

func TestMeteringReflectsExecsImmediately(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "meter")
	sb := tn.Sandbox(t)

	const execs = 3
	for i := 0; i < execs; i++ {
		tn.Exec(t, sb, "true")
	}

	u := usageFor(t, tn.metering(t), tn.ID)
	if u.ExecCount < execs {
		t.Fatalf("exec_count = %d, want >= %d immediately after execs", u.ExecCount, execs)
	}
}

func TestMeteringIsScopedPerTenant(t *testing.T) {
	h := New(t, Options{})
	alice := h.Tenant(t, "meter-alice")
	bob := h.Tenant(t, "meter-bob")

	sb := alice.Sandbox(t)
	alice.Exec(t, sb, "true")

	// alice sees her own usage.
	aliceUsage := usageFor(t, alice.metering(t), alice.ID)
	if aliceUsage.ExecCount < 1 {
		t.Fatalf("alice should have at least 1 exec, got %d", aliceUsage.ExecCount)
	}

	// bob's scoped read must not contain alice's tenant or her exec count.
	for _, u := range bob.metering(t) {
		if u.TenantID == alice.ID {
			t.Fatalf("bob's metering leaked alice's tenant: %+v", u)
		}
	}
}

func TestMeteringAdminSeesFleet(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "meter-fleet")
	sb := tn.Sandbox(t)
	// One exec gives the tenant an in-memory delta, so it appears in the fleet
	// list immediately — without waiting for the 30s resource sampler, which
	// has not produced a durable record for a brand-new tenant yet.
	tn.Exec(t, sb, "true")

	// The admin key, without naming a tenant, receives every tenant's usage.
	page := decode[usagePage](t, h.Admin.Get("/v1/metering").MustStatus(t, http.StatusOK))
	usageFor(t, page.Usage, tn.ID)

	// Naming a tenant pins the response to it.
	pinned := decode[usagePage](t, h.Admin.Get("/v1/metering?tenant="+tn.ID).MustStatus(t, http.StatusOK))
	if len(pinned.Usage) != 1 || pinned.Usage[0].TenantID != tn.ID {
		t.Fatalf("?tenant= must pin to one record, got %+v", pinned.Usage)
	}
}
