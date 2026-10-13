package httpapi

// Regression tests for the two unauthenticated-path defects and for the
// operational surfaces that made them invisible.
//
//	v1.1.0: the rate limiter ran INSIDE authenticate, so a request that failed
//	authentication never met a limiter at all — while still costing one audit
//	event, and therefore one fsync, per request. poc/probe_live.sh PoC-5 sent
//	400 unauthenticated requests and observed 400 WAL frames, 400 fsyncs, and
//	zero 429s.
//
//	The same build served /metrics with mux.HandleFunc, bypassing the whole
//	middleware chain, so operational data answered 200 to anyone.

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"sandcastle.dev/sandcastle/internal/config"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/metrics"
	"sandcastle.dev/sandcastle/internal/runtime"
	"sandcastle.dev/sandcastle/internal/service"
	"sandcastle.dev/sandcastle/internal/store"
)

type apiHarness struct {
	srv       *Server
	svc       *service.Service
	st        *store.Store
	cfg       *config.Config
	tenantKey string
	adminKey  string
	tenantID  string
}

func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.PoolEnabled = false
	cfg.LogAccessLog = false
	st, err := store.Open(cfg.DataDir, store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	svc := service.New(st, newFakeBackendForAPI(), cfg, metrics.New())
	// The limiter owns a sweeper goroutine; leaving it running would leak one
	// per test and make the goroutine assertions elsewhere unreliable.
	t.Cleanup(svc.Stop)

	tn := domain.NewTenant("api")
	tn.Quota.Rates = map[string]*domain.Rate{
		domain.GroupRead:  {PerSecond: 1000, Burst: 1000},
		domain.GroupAdmin: {PerSecond: 1000, Burst: 1000},
	}
	if err := st.PutTenant(tn); err != nil {
		t.Fatalf("put tenant: %v", err)
	}
	tenantKey, _, err := svc.Auth.MintKey(tn.ID, "tenant", domain.ScopeTenant, 0)
	if err != nil {
		t.Fatalf("mint tenant key: %v", err)
	}
	adminKey, _, err := svc.Auth.MintKey("", "admin", domain.ScopeAdmin, 0)
	if err != nil {
		t.Fatalf("mint admin key: %v", err)
	}
	return &apiHarness{
		srv: New(svc, cfg), svc: svc, st: st, cfg: cfg,
		tenantKey: tenantKey, adminKey: adminKey, tenantID: tn.ID,
	}
}

func (h *apiHarness) do(method, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (h *apiHarness) authFailEvents() int {
	return len(h.st.ListEvents(store.EventQuery{Action: domain.ActKeyAuthFail, Limit: 1000}))
}

// ---- unauthenticated flood control ----------------------------------------

// TestUnauthenticatedFloodIsRateLimited is the core P1-2 assertion: a caller who
// never presents a valid credential must still be limited.
func TestUnauthenticatedFloodIsRateLimited(t *testing.T) {
	h := newAPIHarness(t)
	// A tight gate so the test does not need thousands of requests.
	h.cfg.PreAuth = domain.Rate{PerSecond: 0.001, Burst: 5}

	const requests = 400
	counts := map[int]int{}
	for i := 0; i < requests; i++ {
		counts[h.do("GET", "/v1/sandboxes", "").Code]++
	}

	if counts[429] == 0 {
		t.Fatalf("no request was rate limited: an unauthenticated flood passes freely (%v)", counts)
	}
	if counts[401] == 0 {
		t.Fatal("the gate must still return 401 for the requests that get through")
	}
	// The gate is deliberately generous, so most of the flood must be stopped
	// well before it reaches the store.
	if counts[429] < requests/2 {
		t.Fatalf("the pre-auth gate should have absorbed most of the flood, got %v", counts)
	}
	t.Logf("status distribution over %d unauthenticated requests: %v", requests, counts)
}

// TestUnauthenticatedFloodDoesNotAmplifyIntoWALWrites is the other half: the
// security gate must also stop the I/O amplification. Before the fix every
// rejected request wrote an audit event, i.e. one WAL frame plus one fsync.
func TestUnauthenticatedFloodDoesNotAmplifyIntoWALWrites(t *testing.T) {
	h := newAPIHarness(t)
	h.cfg.PreAuth = domain.Rate{PerSecond: 0.001, Burst: 5}

	const requests = 400
	for i := 0; i < requests; i++ {
		h.do("GET", "/v1/sandboxes", "")
	}

	events := h.authFailEvents()
	if events == 0 {
		t.Fatal("an authentication attack must leave at least one audit event")
	}
	if events > requests/4 {
		t.Fatalf("%d audit events for %d rejected requests — the unauthenticated path still writes "+
			"one WAL frame (and one fsync) per request", events, requests)
	}
	t.Logf("%d rejected requests produced %d audit events", requests, events)
}

// With the pre-auth gate opened wide, every request reaches authenticate. The
// audit recorder must still collapse them, so the amplification cannot be
// reintroduced by tuning the gate.
func TestAuthFailureAuditIsAggregatedPerWindow(t *testing.T) {
	h := newAPIHarness(t)
	h.cfg.PreAuth = domain.Rate{PerSecond: 1e9, Burst: 1 << 20} // effectively off

	const requests = 250
	for i := 0; i < requests; i++ {
		h.do("GET", "/v1/sandboxes", "sk_definitely-not-a-valid-key")
	}

	// The onset is recorded in full, then every 100th failure carries the
	// running count: 250 failures → writes at 1, 100, 200.
	const wantEvents = 1 + requests/authFailAuditEvery // 3
	events := h.authFailEvents()
	if events != wantEvents {
		t.Fatalf("got %d audit events for %d failures, want %d — the unauthenticated "+
			"path is writing one WAL frame (and one fsync) per rejected request again",
			events, requests, wantEvents)
	}
	t.Logf("%d failures collapsed into %d audit events", requests, events)

	// Crucially, the metric is NOT sampled: it counts every failure, so the
	// volume stays visible even though the audit log is aggregated.
	snap := h.svc.Reg.Snapshot()
	got, ok := snap["sandboxd_auth_failures_total{}"]
	if !ok {
		t.Fatalf("expected sandboxd_auth_failures_total to count every failure; got %v", snap)
	}
	if got != requests {
		t.Fatalf("sandboxd_auth_failures_total = %v, want %d: the metric must not be sampled "+
			"along with the audit log", got, requests)
	}
}

// ---- operational surfaces -------------------------------------------------

// TestMetricsRequiresAnAdminKey covers the exposure: /metrics used to be
// registered with mux.HandleFunc, bypassing s.chain() entirely.
func TestMetricsRequiresAnAdminKey(t *testing.T) {
	h := newAPIHarness(t)

	if got := h.do("GET", "/metrics", "").Code; got != 401 {
		t.Fatalf("unauthenticated /metrics returned %d, want 401", got)
	}
	if got := h.do("GET", "/metrics", h.tenantKey).Code; got != 403 {
		t.Fatalf("a tenant key reached /metrics (%d); operational data must be admin-only", got)
	}
	rec := h.do("GET", "/metrics", h.adminKey)
	if rec.Code != 200 {
		t.Fatalf("an admin key was refused /metrics (%d): %s", rec.Code, rec.Body.String())
	}
}

// TestMetricsExposeRealLabelValues is the end-to-end proof of the label fix:
// three production call sites passed a label name with no value, so every
// series rendered unlabelled and the dimension was lost.
func TestMetricsExposeRealLabelValues(t *testing.T) {
	h := newAPIHarness(t)
	h.cfg.LogAccessLog = true // the counter is recorded in the access-log middleware

	h.do("GET", "/v1/sandboxes", h.tenantKey)
	h.do("GET", "/v1/sandboxes", "") // a 401

	body := h.do("GET", "/metrics", h.adminKey).Body.String()
	for _, want := range []string{
		`sandboxd_http_requests_total{status="200"}`,
		`sandboxd_http_requests_total{status="401"}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %s in the exposition; a label value was dropped again:\n%s", want, body)
		}
	}
	// The malformed-call detector must stay quiet.
	if strings.Contains(body, metrics.MalformedMetricName) {
		t.Fatalf("a metric call passed a label name without a value:\n%s", body)
	}
}

// TestReadyzReportsTheIsolationTier is the verifiability guarantee that blocker
// ① needed: the tier in force is published, unauthenticated, on the same
// endpoint a load balancer already probes. A customer no longer has to take a
// README's word for it.
func TestReadyzReportsTheIsolationTier(t *testing.T) {
	h := newAPIHarness(t)

	rec := h.do("GET", "/readyz", "")
	if rec.Code != 200 {
		t.Fatalf("/readyz returned %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"isolation"`) {
		t.Fatalf("/readyz must publish the isolation tier, got: %s", body)
	}
	if !strings.Contains(body, runtime.TierContainer) {
		t.Fatalf("expected the fake backend's tier %q in: %s", runtime.TierContainer, body)
	}
	// And it must say plainly what that tier does not cover, rather than
	// leaving the reader to infer it.
	if !strings.Contains(body, "NOT for running arbitrary untrusted code") {
		t.Fatalf("/readyz should spell out the limitation of a shared-kernel tier: %s", body)
	}
}

// Liveness and readiness must stay open or a load balancer cannot use them.
func TestHealthAndReadyStayUnauthenticated(t *testing.T) {
	h := newAPIHarness(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		if got := h.do("GET", path, "").Code; got != 200 {
			t.Fatalf("%s returned %d without credentials; probes would break", path, got)
		}
	}
}

// ---- admin key isolation ---------------------------------------------------

// Admin keys carry an empty TenantID. v1.1.0 passed that straight to the
// limiter, so every admin key in a fleet shared one bucket.
func TestAdminKeysDoNotShareOneBudget(t *testing.T) {
	h := newAPIHarness(t)
	// Make the admin group effectively non-refilling so exhaustion is visible.
	// Admin keys fall back to the platform defaults, so this is the policy
	// that governs them.
	h.cfg.Defaults.Rates[domain.GroupAdmin] = &domain.Rate{PerSecond: 0.001, Burst: 2}
	h.cfg.PreAuth = domain.Rate{PerSecond: 1e9, Burst: 1 << 20}

	second, _, err := h.svc.Auth.MintKey("", "admin2", domain.ScopeAdmin, 0)
	if err != nil {
		t.Fatalf("mint second admin key: %v", err)
	}

	// Exhaust the first admin key's admin-group budget.
	for i := 0; i < 3; i++ {
		h.do("GET", "/v1/tenants", h.adminKey)
	}
	// The second key must still have its own tokens.
	rec := h.do("GET", "/v1/tenants", second)
	if rec.Code == 429 {
		t.Fatal("the second admin key was throttled by the first key's traffic — " +
			"admin credentials still share a bucket")
	}
}

// ---- concurrency sanity ---------------------------------------------------

// The pre-auth gate and the per-tenant limiter share one Limiter; hammering
// both from many goroutines must stay race-free (run with -race).
func TestPreAuthGateAndTenantLimiterAreRaceFree(t *testing.T) {
	h := newAPIHarness(t)
	h.cfg.PreAuth = domain.Rate{PerSecond: 100, Burst: 100}

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				h.do("GET", "/v1/sandboxes", "")
			} else {
				h.do("GET", "/v1/sandboxes", h.tenantKey)
			}
		}(i)
	}
	wg.Wait()
}

// Guards the assumption the whole design rests on: the limiter must be reachable
// from the unauthenticated path. If someone reorders `chain`, the pre-auth gate
// silently stops protecting anything, and only this assertion notices.
func TestPreAuthGateRunsBeforeAuthentication(t *testing.T) {
	h := newAPIHarness(t)
	h.cfg.PreAuth = domain.Rate{PerSecond: 0.001, Burst: 1}

	// First request consumes the only token and fails authentication.
	if got := h.do("GET", "/v1/sandboxes", "").Code; got != 401 {
		t.Fatalf("first unauthenticated request should reach authenticate (401), got %d", got)
	}
	// The second must be rejected by the gate, which can only happen if the
	// gate sits outside authenticate.
	if got := h.do("GET", "/v1/sandboxes", "").Code; got != 429 {
		t.Fatalf("expected 429 from the pre-auth gate, got %d — the limiter is inside "+
			"authenticate again, so failed logins are unlimited", got)
	}
	// A valid key is not exempt: the gate is intentionally credential-blind,
	// because an attacker can always send a forged credential to skip an
	// exemption check.
	if got := h.do("GET", "/v1/sandboxes", h.tenantKey).Code; got != 429 {
		t.Fatalf("expected the flood gate to apply to authenticated traffic too, got %d", got)
	}
}
