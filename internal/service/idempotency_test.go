package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/runtime"
)

func execReq(argv ...string) runtime.ExecRequest {
	return runtime.ExecRequest{Argv: argv, Timeout: 30 * time.Second}
}

// TestIdempotencyReplaysCachedResult is the core contract: the same key twice
// returns the remembered result with replayed=true and runs the command once.
func TestIdempotencyReplaysCachedResult(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	ctx := context.Background()

	res1, err := h.svc.Exec(ctx, h.tn.ID, h.key.ID, sb.ID, execReq("echo", "x"), "op-1")
	if err != nil {
		t.Fatalf("first exec: %v", err)
	}
	if res1.Replayed {
		t.Fatal("a first execution must not be flagged as a replay")
	}

	res2, err := h.svc.Exec(ctx, h.tn.ID, h.key.ID, sb.ID, execReq("echo", "x"), "op-1")
	if err != nil {
		t.Fatalf("replayed exec: %v", err)
	}
	if !res2.Replayed {
		t.Fatal("a retry with the same key must be flagged replayed")
	}
	if string(res2.Stdout) != string(res1.Stdout) || res2.ExitCode != res1.ExitCode {
		t.Fatalf("replay returned a different result: %+v vs %+v", res1, res2)
	}
	if h.fb.execCalls != 1 {
		t.Fatalf("backend executed %d times, want exactly 1", h.fb.execCalls)
	}
}

// TestIdempotencyDifferentKeysRunSeparately: keys name logical operations, so
// a different key is a different operation and must execute.
func TestIdempotencyDifferentKeysRunSeparately(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	ctx := context.Background()

	if _, err := h.svc.Exec(ctx, h.tn.ID, h.key.ID, sb.ID, execReq("true"), "op-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Exec(ctx, h.tn.ID, h.key.ID, sb.ID, execReq("true"), "op-b"); err != nil {
		t.Fatal(err)
	}
	if h.fb.execCalls != 2 {
		t.Fatalf("two distinct keys must run twice, got %d", h.fb.execCalls)
	}
}

// TestIdempotencyReusedKeyWithDifferentParamsConflicts: silently returning a
// stale result for a *different* command would be the worst failure mode, so
// the conflict is explicit.
func TestIdempotencyReusedKeyWithDifferentParamsConflicts(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	ctx := context.Background()

	if _, err := h.svc.Exec(ctx, h.tn.ID, h.key.ID, sb.ID, execReq("echo", "a"), "op-x"); err != nil {
		t.Fatal(err)
	}
	_, err := h.svc.Exec(ctx, h.tn.ID, h.key.ID, sb.ID, execReq("echo", "b"), "op-x")
	if apperr.From(err).Code != apperr.Conflict {
		t.Fatalf("reused key with different params must conflict, got %v", err)
	}
	if h.fb.execCalls != 1 {
		t.Fatalf("the conflicting retry must not execute, got %d calls", h.fb.execCalls)
	}
}

// TestIdempotencySingleFlight: concurrent retries of the same key collapse to
// one execution, and every waiter receives the same result with replayed=true
// except the one that actually ran it.
func TestIdempotencySingleFlight(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	ctx := context.Background()

	h.fb.mu.Lock()
	h.fb.execBlock = make(chan struct{})
	h.fb.mu.Unlock()

	const n = 16
	var wg sync.WaitGroup
	replayed := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := h.svc.Exec(ctx, h.tn.ID, h.key.ID, sb.ID, execReq("true"), "op-single")
			errs[i] = err
			if err == nil {
				replayed[i] = res.Replayed
			}
		}(i)
	}

	// Give every caller time to reach the single-flight gate, then release.
	time.Sleep(100 * time.Millisecond)
	h.fb.mu.Lock()
	close(h.fb.execBlock)
	h.fb.mu.Unlock()
	wg.Wait()

	leaders := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if !replayed[i] {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("exactly one caller must execute (non-replayed), got %d", leaders)
	}
	if h.fb.execCalls != 1 {
		t.Fatalf("backend executed %d times under single-flight, want 1", h.fb.execCalls)
	}
}

// TestIdempotencyKeyValidation bounds attacker-chosen input before it becomes a
// map key. An over-long or NUL-bearing key is refused, not stored.
func TestIdempotencyKeyValidation(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	ctx := context.Background()

	long := strings.Repeat("k", idemMaxKeyLen+1)
	if _, err := h.svc.Exec(ctx, h.tn.ID, h.key.ID, sb.ID, execReq("true"), long); apperr.From(err).Code != apperr.InvalidRequest {
		t.Fatalf("over-long key must be rejected as invalid, got %v", err)
	}
	if _, err := h.svc.Exec(ctx, h.tn.ID, h.key.ID, sb.ID, execReq("true"), "bad\x00key"); apperr.From(err).Code != apperr.InvalidRequest {
		t.Fatalf("NUL-bearing key must be rejected as invalid, got %v", err)
	}
	if h.fb.execCalls != 0 {
		t.Fatalf("invalid keys must not execute, got %d calls", h.fb.execCalls)
	}
}

// TestIdemRegistryBoundsAndTTL proves the cache cannot grow without limit from
// an endless stream of invented keys.
func TestIdemRegistryBoundsAndTTL(t *testing.T) {
	now := time.Now()
	r := newIdemRegistry(4, time.Minute)
	r.now = func() time.Time { return now }

	run := func(key string) *runtime.ExecResult {
		res, _, err := r.run(context.Background(), key, "d", func() (*runtime.ExecResult, error) {
			return &runtime.ExecResult{ExitCode: 0, Stdout: []byte(key)}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	for i := 0; i < 100; i++ {
		run("key-" + string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}
	if got := len(r.entries); got > r.maxEntries {
		t.Fatalf("registry grew to %d entries, cap is %d", got, r.maxEntries)
	}

	// Expiry evicts: advancing the clock past the TTL drops everything.
	now = now.Add(2 * time.Minute)
	r.evictLocked()
	if len(r.entries) != 0 {
		t.Fatalf("expired entries not evicted: %d remain", len(r.entries))
	}
}
