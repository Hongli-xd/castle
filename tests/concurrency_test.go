package tests

// Concurrency: the properties that exist only when two things happen at once.
// A sandbox platform is multi-tenant by construction, so "it works when run
// one at a time" is not a statement about the product.
//
// Build the daemon under test with SBX_IT_RACE=1 to instrument its own
// goroutines with the race detector, rather than only the harness's.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/runtime"
)

// concurrentCreate issues a create from a worker goroutine and returns only
// values, never a fatal: FailNow is illegal off the test goroutine, so the
// assertion happens later, on the main one.
func concurrentCreate(tn *Tenant, image string) (status int, transportErr error) {
	r := tn.Client.Post("/v1/sandboxes?wait=true", createBody{BaseImage: image, Spec: specOf()})
	return r.Status, r.TransportErr
}

func TestConcurrentCreateAttribution(t *testing.T) {
	h := New(t, Options{})
	const tenants, perTenant = 5, 2

	tenantList := make([]*Tenant, tenants)
	for i := range tenantList {
		tenantList[i] = h.Tenant(t, "conc-"+strconv.Itoa(i))
	}

	type outcome struct {
		tenant int
		status int
		err    error
	}
	results := make([]outcome, 0, tenants*perTenant)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, tn := range tenantList {
		for j := 0; j < perTenant; j++ {
			wg.Add(1)
			go func(i int, tn *Tenant) {
				defer wg.Done()
				status, err := concurrentCreate(tn, h.opts.Image)
				mu.Lock()
				results = append(results, outcome{tenant: i, status: status, err: err})
				mu.Unlock()
			}(i, tn)
		}
	}
	wg.Wait()

	for _, res := range results {
		if res.err != nil {
			t.Fatalf("tenant %d: transport error during concurrent create: %v", res.tenant, res.err)
		}
		eq(t, res.status, http.StatusCreated, "concurrent create status")
	}

	for i, tn := range tenantList {
		page := tn.Sandboxes(t, "")
		eq(t, page.Count, perTenant, "sandbox count for tenant "+strconv.Itoa(i))
		for _, sb := range page.Sandboxes {
			eq(t, sb.TenantID, tn.ID, "sandbox owner")
		}
	}
	for _, tn := range tenantList {
		tn.DeleteAll(t)
	}
}

func TestConcurrentExec(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "execstorm")

	t.Run("parallel_execs_inside_one_sandbox_keep_their_own_output", func(t *testing.T) {
		sb := tn.Sandbox(t)
		const n = 20
		outputs := make([]string, n)
		statuses := make([]int, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r := tn.Client.Post("/v1/sandboxes/"+sb.ID+"/exec",
					execReq{Command: "echo marker-" + strconv.Itoa(i), TimeoutMs: 30_000})
				statuses[i] = r.Status
				if r.Status == http.StatusOK {
					var res runtime.ExecResult
					if json.Unmarshal(r.Raw, &res) == nil {
						outputs[i] = strings.TrimSpace(string(res.Stdout))
					}
				}
			}(i)
		}
		wg.Wait()

		for i := 0; i < n; i++ {
			eq(t, statuses[i], http.StatusOK, "status of concurrent exec "+strconv.Itoa(i))
			// Cross-talk here would mean one caller reading another's output,
			// which for an agent platform means acting on someone else's
			// command output.
			eq(t, outputs[i], "marker-"+strconv.Itoa(i), "output of concurrent exec "+strconv.Itoa(i))
		}
	})

	t.Run("concurrent_workflows_do_not_share_a_filesystem", func(t *testing.T) {
		const n = 4
		workers := make([]*Tenant, n)
		sandboxes := make([]*domain.Sandbox, n)
		for i := range workers {
			workers[i] = h.Tenant(t, "fs-"+strconv.Itoa(i))
			sandboxes[i] = workers[i].Sandbox(t)
		}

		contents := make([]string, n)
		putStatuses := make([]int, n)
		getStatuses := make([]int, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				marker := "owner-" + strconv.Itoa(i)
				put := workers[i].Client.Put("/v1/sandboxes/"+sandboxes[i].ID+"/files?path=/root/who.txt",
					[]byte(marker))
				putStatuses[i] = put.Status
				if put.Status != http.StatusNoContent {
					return
				}
				get := workers[i].Client.Get("/v1/sandboxes/" + sandboxes[i].ID + "/files?path=/root/who.txt")
				getStatuses[i] = get.Status
				contents[i] = strings.TrimSpace(string(get.Raw))
			}(i)
		}
		wg.Wait()

		for i := 0; i < n; i++ {
			eq(t, putStatuses[i], http.StatusNoContent, "write status in worker "+strconv.Itoa(i))
			eq(t, getStatuses[i], http.StatusOK, "read status in worker "+strconv.Itoa(i))
			eq(t, contents[i], "owner-"+strconv.Itoa(i), "file content in worker "+strconv.Itoa(i))
		}
	})
}

// TestQuotaAdmissionIsBestEffortButConverges records what the ceiling really
// promises, because the honest answer is narrower than "a quota is a quota".
//
// gov.Quotas.CheckCreate reads the store, decides, and only then is the record
// written, with no lock across the two steps. Its own doc comment says so: "It
// is best-effort under races (two concurrent creates can both pass the check);
// the ceiling then trips on the next create instead — standard admission
// semantics, documented in the threat model."
//
// So the property a customer can rely on is convergence, not instantaneous
// enforcement: a burst may overshoot, and from then on every create is
// refused. That is what this test asserts, and it prints the overshoot so the
// number is on the record instead of being smoothed away.
func TestQuotaAdmissionIsBestEffortButConverges(t *testing.T) {
	h := New(t, Options{})
	const ceiling, attempts = 5, 20
	tn := h.TenantWith(t, "ceiling", quota(ceiling, 4096, fastRates(500, 2000)))

	counts := map[int]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// wait=false: this scenario is about admission, not about how
			// long a container takes to start.
			r := tn.Client.Post("/v1/sandboxes?wait=false", createBody{BaseImage: h.opts.Image, Spec: specOf()})
			mu.Lock()
			counts[r.Status]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	for status, n := range counts {
		if status >= 500 {
			t.Fatalf("%d creates failed with status %d; a saturated quota is a governed refusal, never a server error", n, status)
		}
	}
	admitted := counts[http.StatusCreated]
	refused := counts[http.StatusPaymentRequired] + counts[http.StatusTooManyRequests]
	t.Logf("burst of %d concurrent creates against a ceiling of %d: %d admitted, %d refused (overshoot %d)",
		attempts, ceiling, admitted, refused, admitted-ceiling)

	// The ceiling must never under-admit: refusing a request that fits would
	// be a worse defect than overshooting by one.
	truthy(t, admitted >= ceiling, "the ceiling admitted "+strconv.Itoa(admitted)+" of a permitted "+strconv.Itoa(ceiling))
	// The overshoot is bounded by the number of creates actually in flight,
	// which the create rate limit and max_concurrent_creates both cap.
	truthy(t, admitted <= attempts, "the ceiling admitted more creates than were issued")

	// No accounting drift: every admitted create produced a real sandbox
	// owned by this tenant, and nothing else appeared.
	page := tn.Sandboxes(t, "")
	eq(t, page.Count, admitted, "sandboxes after the storm must equal the creates that were admitted")
	for _, sb := range page.Sandboxes {
		eq(t, sb.TenantID, tn.ID, "sandbox owner after the storm")
	}

	// The property that matters: the storm is over, and the ceiling now holds.
	// The next create is refused, not admitted.
	//
	// The refusal is 402 Payment Required, not 429: apperr deliberately types a
	// quota as `quota_exceeded` — "billing-shaped rejection" — and keeps it
	// distinct from `rate_limited`, because "come back in a second" and "buy
	// more capacity" are different instructions to the caller. MustErr checks
	// the code's own status mapping, so the two facts cannot drift.
	tn.TryCreateJSON(t, createBody{BaseImage: h.opts.Image, Spec: specOf()}, "?wait=false").
		MustErr(t, apperr.QuotaExceeded)

	tn.DeleteAll(t)
}

func TestConcurrentDeleteAndRead(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "races")

	sb := tn.Sandbox(t)
	const readers = 8

	var wg sync.WaitGroup
	statuses := make([]int, readers)
	errCodes := make([]apperr.Code, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := tn.Client.Get("/v1/sandboxes/" + sb.ID)
			statuses[i] = r.Status
			if e := r.Err(); e != nil {
				errCodes[i] = e.Code
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = tn.Client.Delete("/v1/sandboxes/" + sb.ID + "?force=true")
	}()
	wg.Wait()

	// Whatever the interleaving, a reader either saw the sandbox or was told
	// it is gone — never a 500, and never a half-deleted record.
	for i := 0; i < readers; i++ {
		switch statuses[i] {
		case http.StatusOK:
		case http.StatusNotFound:
			eq(t, errCodes[i], apperr.NotFound, "error code for a concurrently deleted sandbox")
		default:
			t.Fatalf("reader %d got status %d, want 200 or 404", i, statuses[i])
		}
	}

	tn.Client.Get("/v1/sandboxes/"+sb.ID).
		MustStatus(t, http.StatusNotFound).MustErr(t, apperr.NotFound)
}
