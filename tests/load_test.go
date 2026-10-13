package tests

// Load: mixed create and exec traffic under concurrency. The gate is not a
// latency number — those are machine-specific and make flaky tests — but the
// absence of 5xx and of ungoverned responses, which is a statement about the
// product rather than about the laptop it ran on. Latency is reported, not
// asserted.

import (
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"
)

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

// recorder collects status codes and latencies without a data race.
type recorder struct {
	mu     sync.Mutex
	counts map[int]int
	times  map[int][]time.Duration
}

func newRecorder() *recorder {
	return &recorder{counts: map[int]int{}, times: map[int][]time.Duration{}}
}

func (r *recorder) add(status int, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[status]++
	r.times[status] = append(r.times[status], d)
}

// report logs the observed distribution and fails on any ungoverned outcome.
func (r *recorder) report(t *testing.T, what string, okStatus int) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()

	statuses := make([]int, 0, len(r.counts))
	for s := range r.counts {
		statuses = append(statuses, s)
	}
	sort.Ints(statuses)
	for _, s := range statuses {
		lat := append([]time.Duration(nil), r.times[s]...)
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		t.Logf("%s: status %d × %d  p50=%s p99=%s max=%s",
			what, s, r.counts[s], percentile(lat, 0.50), percentile(lat, 0.99), lat[len(lat)-1])
		if s >= 500 {
			t.Errorf("%s: %d responses with status %d", what, r.counts[s], s)
		}
	}
	eq(t, r.counts[okStatus], r.totalExcept429(), what+": every non-throttled request must have succeeded")
}

func (r *recorder) totalExcept429() int {
	n := 0
	for status, c := range r.counts {
		if status != http.StatusTooManyRequests {
			n += c
		}
	}
	return n
}

func TestLoadMixedTraffic(t *testing.T) {
	if testing.Short() {
		t.Skip("load suite skipped in -short mode")
	}
	h := New(t, Options{
		Pool: true,
		Set:  map[string]string{"max_concurrent_creates": "16", "quota.max_sandboxes": "200"},
	})
	tn := h.TenantWith(t, "load", quota(200, 262144, fastRates(500, 5000)))

	const creates, execs, workers = 24, 200, 10

	t.Run("concurrent_creates", func(t *testing.T) {
		rec := newRecorder()
		var wg sync.WaitGroup
		for i := 0; i < creates; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				status, _ := concurrentCreate(tn, h.opts.Image)
				rec.add(status, time.Since(start))
			}()
		}
		wg.Wait()
		rec.report(t, "create", http.StatusCreated)

		page := tn.Sandboxes(t, "")
		truthy(t, page.Count >= creates, "creates admitted under load must exist afterwards")
	})

	t.Run("concurrent_execs_across_many_sandboxes", func(t *testing.T) {
		ids := []string{}
		for _, sb := range tn.Sandboxes(t, "").Sandboxes {
			ids = append(ids, sb.ID)
		}
		if len(ids) == 0 {
			t.Fatal("no sandboxes to exec against")
		}

		rec := newRecorder()
		tasks := make(chan int, execs)
		for i := 0; i < execs; i++ {
			tasks <- i
		}
		close(tasks)

		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range tasks {
					id := ids[i%len(ids)]
					start := time.Now()
					r := tn.Client.Post("/v1/sandboxes/"+id+"/exec",
						execReq{Command: "true", TimeoutMs: 30_000})
					rec.add(r.Status, time.Since(start))
				}
			}()
		}
		wg.Wait()
		rec.report(t, "exec", http.StatusOK)
	})

	t.Run("the_daemon_is_still_healthy_after_load", func(t *testing.T) {
		h.Ready().MustStatus(t, http.StatusOK)
		eq(t, decode[readyInfo](t, h.Ready()).Status, "ready", "readiness after load")
	})

	t.Run("load_leaves_no_backend_leak", func(t *testing.T) {
		before := managedContainers(t)
		tn.DeleteAll(t)
		eventually(t, 120*time.Second, func() bool {
			return managedContainers(t) < before
		}, "containers reclaimed after deleting everything under load")
	})
}

func TestPoolReuseDoesNotBleed(t *testing.T) {
	// The warm pool exists to hand a ready container to whoever asks next. The
	// risk that comes with it is specific: a container that is reused without
	// being cleared serves one tenant another tenant's files.
	//
	// Black-box limits, stated rather than glossed: a passing result here
	// cannot distinguish "reuse is safe" from "this run happened not to reuse
	// anything". It only ever catches a bleed, which is the direction that
	// matters.
	h := New(t, Options{Pool: true, Set: map[string]string{"pool.size": "2"}})
	alice := h.Tenant(t, "pool-alice")
	bob := h.Tenant(t, "pool-bob")

	first := alice.Sandbox(t)
	alice.PutFile(t, first, "/root/secret.txt", []byte("alice-only"))
	alice.Delete(t, first.ID)

	for i := 0; i < 6; i++ {
		sb := bob.Sandbox(t)
		// exit 1 = the path does not exist, which is the required answer.
		eq(t, bob.Exit(t, sb, "test -e /root/secret.txt"), 1,
			"a previous tenant's file present in a newly acquired sandbox")
		eq(t, bob.Out(t, sb, "ls -a /root 2>/dev/null | grep -c secret || true"), "0",
			"entries matching the previous tenant's file in a newly acquired sandbox")
		bob.Delete(t, sb.ID)
	}
}
