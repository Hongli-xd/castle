package tests

// Benchmark report: cold start and throughput, measured reproducibly and
// written to a JSON summary. This is the "cold-start / throughput" number the
// vendor survey (§2.1, §7.1) asks a runtime to publish instead of asserting.
//
// The rule from commercial_test.go applies here unchanged: an absolute
// millisecond budget is a statement about the laptop, not the product, so
// nothing below is asserted against a hard ceiling. What this file guarantees
// is *measurement*: the numbers are real, the method is reproducible
// (`make bench`), and the results land in `docs/bench-summary.json` (when
// SBX_BENCH_OUT is set) so a buyer or a future engineer can re-run and diff.
// See docs/ROADMAP.md §P0-2 for the interpretation.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"sandcastle.dev/sandcastle/internal/domain"
)

type benchSummary struct {
	GeneratedAt          time.Time `json:"generated_at"`
	Image                string    `json:"image"`
	ColdStartReady       string    `json:"cold_start_to_ready"`
	ExecP50              string    `json:"exec_p50"`
	ExecP99              string    `json:"exec_p99"`
	CreateP50            string    `json:"create_p50"`
	CreateP99            string    `json:"create_p99"`
	Concurrent80Total    string    `json:"concurrent_80_create_total"`
	Concurrent80Admitted int       `json:"concurrent_80_admitted"`
}

func TestBenchReport(t *testing.T) {
	if testing.Short() {
		t.Skip("benchmark skipped in -short mode")
	}
	requireDocker(t)
	h := New(t, Options{NoStart: true})

	// Cold start to /readyz 200: backend ping + store + reconcile + isolation
	// self-check, the number a buyer's "how fast can I get my first sandbox"
	// question is really about.
	cold := h.StartAndTime(t)

	// The burst needs headroom far beyond the platform's default CPU ceiling
	// (8000 millicores), otherwise the concurrent-create measurement becomes a
	// measurement of quota refusal, not of throughput. CPU and memory quotas
	// are lifted so the only gate in play is the control plane's own speed.
	benchQuota := &domain.TenantQuota{
		MaxSandboxes:     200,
		MaxCPUMillicores: 200_000,
		MaxMemoryMiB:     262144,
		Rates:            fastRates(2000, 20000),
	}
	tn := h.TenantWith(t, "bench", benchQuota)

	// Create throughput: cold creates, one at a time, wait=true.
	createFn := func() *Resp {
		return tn.Client.Post("/v1/sandboxes?wait=true", createBody{BaseImage: h.opts.Image, Spec: specOf()})
	}
	warm(func() { createFn() }, 2)
	createLat := measure(t, 10, "create", http.StatusCreated, createFn)
	createP50, createP99 := percentile(createLat, 0.50), percentile(createLat, 0.99)

	// Exec throughput: the steady-state operation, against one running sandbox.
	sb := tn.Sandbox(t)
	execFn := func() *Resp {
		return tn.Client.Post("/v1/sandboxes/"+sb.ID+"/exec", execReq{Command: "true", TimeoutMs: 30_000})
	}
	warm(func() { execFn() }, 5)
	execLat := measure(t, 40, "exec", http.StatusOK, execFn)
	execP50, execP99 := percentile(execLat, 0.50), percentile(execLat, 0.99)

	// Concurrent create: 80 simultaneous creates against a wide bucket — the
	// admission-under-saturation number. What matters is the wall-clock time
	// for the whole burst to settle and that every create is governed.
	admitted, concurrentTotal := concurrentCreateBurst(t, tn, h.opts.Image, 80)

	summary := benchSummary{
		GeneratedAt:          time.Now().UTC(),
		Image:                h.opts.Image,
		ColdStartReady:       cold.Round(time.Millisecond).String(),
		ExecP50:              execP50.Round(time.Microsecond).String(),
		ExecP99:              execP99.Round(time.Microsecond).String(),
		CreateP50:            createP50.Round(time.Millisecond).String(),
		CreateP99:            createP99.Round(time.Millisecond).String(),
		Concurrent80Total:    concurrentTotal.Round(time.Millisecond).String(),
		Concurrent80Admitted: admitted,
	}

	t.Logf("cold_start_to_ready=%s", summary.ColdStartReady)
	t.Logf("exec p50=%s p99=%s (n=%d)", summary.ExecP50, summary.ExecP99, len(execLat))
	t.Logf("create p50=%s p99=%s (n=%d)", summary.CreateP50, summary.CreateP99, len(createLat))
	t.Logf("concurrent_80_create total=%s admitted=%d", summary.Concurrent80Total, summary.Concurrent80Admitted)

	if out := os.Getenv("SBX_BENCH_OUT"); out != "" {
		// `go test ./tests/` runs with the package dir as cwd, so a relative
		// SBX_BENCH_OUT must be anchored to the repo root, not tests/.
		if !filepath.IsAbs(out) {
			out = filepath.Join(repoRoot(), out)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err == nil {
			blob, _ := json.MarshalIndent(summary, "", "  ")
			if err := os.WriteFile(out, append(blob, '\n'), 0o644); err != nil {
				t.Fatalf("write bench summary: %v", err)
			}
		}
	}
}

// concurrentCreateBurst issues n simultaneous creates and returns how many were
// admitted plus the wall-clock time for the whole burst to settle.
func concurrentCreateBurst(t *testing.T, tn *Tenant, image string, n int) (admitted int, total time.Duration) {
	t.Helper()
	statuses := make([]int, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r := tn.Client.Post("/v1/sandboxes?wait=true", createBody{BaseImage: image, Spec: specOf()})
			statuses[i] = r.Status
		}(i)
	}
	begin := time.Now()
	close(start)
	wg.Wait()
	total = time.Since(begin)
	for _, s := range statuses {
		if s == http.StatusCreated {
			admitted++
		}
	}
	return admitted, total
}
