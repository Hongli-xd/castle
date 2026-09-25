package gov

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/store"
)

func newGovStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestMintAndAuthenticate(t *testing.T) {
	st := newGovStore(t)
	a := NewAuthenticator(st)

	plaintext, key, err := a.MintKey("t1", "ci", domain.ScopeTenant, 0)
	if err != nil {
		t.Fatal(err)
	}
	if key.Hash == plaintext || key.Hash == "" {
		t.Fatal("hash must be derived, not the plaintext itself")
	}
	// the stored copy must not contain the plaintext anywhere
	got, _ := st.GetKey(key.ID)
	if got.Hash == plaintext || got.Prefix == plaintext {
		t.Fatal("plaintext leaked into storage")
	}

	authed, err := a.Authenticate(plaintext)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if authed.ID != key.ID || authed.TenantID != "t1" {
		t.Fatalf("wrong key resolved: %+v", authed)
	}

	if _, err := a.Authenticate("sk_totallyboguskeyvalue"); err == nil {
		t.Fatal("bogus key accepted")
	}
	if _, err := a.Authenticate(""); err == nil {
		t.Fatal("empty key accepted")
	}
	// wrong tenant context must not appear — auth returns the key, scoping
	// happens at the service layer; but tampered secrets must fail
	if _, err := a.Authenticate(plaintext + "x"); err == nil {
		t.Fatal("tampered key accepted")
	}
}

func TestKeyExpiryAndRevocation(t *testing.T) {
	st := newGovStore(t)
	a := NewAuthenticator(st)
	plaintext, _, _ := a.MintKey("t1", "ttl", domain.ScopeTenant, 50*time.Millisecond)
	if _, err := a.Authenticate(plaintext); err != nil {
		t.Fatalf("fresh key rejected: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := a.Authenticate(plaintext); err == nil {
		t.Fatal("expired key accepted")
	}

	plaintext2, key2, _ := a.MintKey("t1", "revoke", domain.ScopeTenant, 0)
	if _, err := a.Authenticate(plaintext2); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := st.UpdateKey(key2.ID, key2.Version, func(k *domain.APIKey) error {
		k.RevokedAt = &now
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(plaintext2); err == nil {
		t.Fatal("revoked key accepted")
	}
}

func TestLimiterBurstAndRefill(t *testing.T) {
	l := NewLimiter()
	rate := &domain.Rate{PerSecond: 10, Burst: 3}

	allowed := 0
	for i := 0; i < 5; i++ {
		ok, _ := l.Allow("t1", domain.GroupExec, rate)
		if ok {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("burst allowed %d, want 3", allowed)
	}
	// refill ~10/s: after 300ms one more token
	time.Sleep(300 * time.Millisecond)
	if ok, _ := l.Allow("t1", domain.GroupExec, rate); !ok {
		t.Fatal("no token refilled after 300ms at 10/s")
	}
}

func TestLimiterPerTenantPerGroupIsolation(t *testing.T) {
	l := NewLimiter()
	rate := &domain.Rate{PerSecond: 1, Burst: 1}
	if ok, _ := l.Allow("t1", domain.GroupCreate, rate); !ok {
		t.Fatal("first allow failed")
	}
	if ok, _ := l.Allow("t1", domain.GroupCreate, rate); ok {
		t.Fatal("burst exceeded within tenant+group")
	}
	if ok, _ := l.Allow("t1", domain.GroupExec, rate); !ok {
		t.Fatal("groups must not share budget")
	}
	if ok, _ := l.Allow("t2", domain.GroupCreate, rate); !ok {
		t.Fatal("tenants must not share budget")
	}
}

func TestLimiterConcurrentSafety(t *testing.T) {
	l := NewLimiter()
	rate := &domain.Rate{PerSecond: 1000, Burst: 50}
	var wg sync.WaitGroup
	var allowed int
	var mu sync.Mutex
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _ := l.Allow(fmt.Sprintf("t%d", i%7), domain.GroupExec, rate)
			if ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed == 0 {
		t.Fatal("no request allowed under concurrency")
	}
}

func TestQuotaCheckCreate(t *testing.T) {
	st := newGovStore(t)
	tn := domain.NewTenant("acme")
	tn.Quota = domain.TenantQuota{MaxSandboxes: 2, MaxCPUMillicores: 1000, MaxMemoryMiB: 1024}
	if err := st.PutTenant(tn); err != nil {
		t.Fatal(err)
	}
	q := NewQuotas(st, domain.TenantQuota{})

	sb := domain.NewSandbox(tn.ID)
	sb.Spec = domain.SandboxSpec{CPUMillicores: 500, MemoryMiB: 256, TimeoutSeconds: 600}
	sb.State = domain.StateRunning
	if err := st.PutSandbox(sb); err != nil {
		t.Fatal(err)
	}
	if err := q.CheckCreate(tn, domain.SandboxSpec{CPUMillicores: 400, MemoryMiB: 256}); err != nil {
		t.Fatalf("within quota rejected: %v", err)
	}
	err := q.CheckCreate(tn, domain.SandboxSpec{CPUMillicores: 600, MemoryMiB: 256})
	if err == nil {
		t.Fatal("cpu over-quota accepted")
	}
	if apperr.From(err).Code != apperr.QuotaExceeded {
		t.Fatalf("wrong code: %v", err)
	}
	// stopped sandboxes must not count against concurrency
	sb2 := domain.NewSandbox(tn.ID)
	sb2.Spec = sb.Spec
	sb2.State = domain.StateStopped
	if err := st.PutSandbox(sb2); err != nil {
		t.Fatal(err)
	}
	if err := q.CheckCreate(tn, domain.SandboxSpec{CPUMillicores: 500, MemoryMiB: 700}); err != nil {
		t.Fatalf("stopped sandbox counted: %v", err)
	}
}

func TestQuotaDefaultsInherit(t *testing.T) {
	st := newGovStore(t)
	tn := domain.NewTenant("lean")
	tn.Quota = domain.TenantQuota{} // inherits everything
	if err := st.PutTenant(tn); err != nil {
		t.Fatal(err)
	}
	q := NewQuotas(st, domain.TenantQuota{MaxSandboxes: 5})
	eff := q.Effective(tn)
	if eff.MaxSandboxes != 5 {
		t.Fatalf("default not inherited: %+v", eff)
	}
}

func TestAuditRecordAndQuery(t *testing.T) {
	st := newGovStore(t)
	au := NewAudit(st)
	if err := au.Record("t1", "sbx_1", "ak_1", domain.ActSandboxCreate, map[string]string{"image": "tpl_x"}); err != nil {
		t.Fatal(err)
	}
	if err := au.Record("t2", "sbx_2", "ak_2", domain.ActSandboxExec, nil); err != nil {
		t.Fatal(err)
	}
	got := st.ListEvents(store.EventQuery{TenantID: "t1"})
	if len(got) != 1 || got[0].Action != domain.ActSandboxCreate {
		t.Fatalf("tenant filter broken: %+v", got)
	}
	if got[0].Detail["image"] != "tpl_x" {
		t.Fatal("detail lost")
	}
}

func TestExtractSecretHeaders(t *testing.T) {
	h := map[string][]string{}
	if ExtractSecret(h) != "" {
		t.Fatal("empty headers must yield empty secret")
	}
	h["X-Api-Key"] = []string{"sk_a"}
	if ExtractSecret(h) != "sk_a" {
		t.Fatal("X-API-Key not read")
	}
	h["Authorization"] = []string{"Bearer sk_b"}
	if ExtractSecret(h) != "sk_a" {
		t.Fatal("X-API-Key must take precedence for e2b/daytona client compatibility")
	}
	h["X-Api-Key"] = nil
	if ExtractSecret(h) != "sk_b" {
		t.Fatal("Bearer not read")
	}
}
