package gov

// Tests for the limiter defects: a sweeper goroutine that could never be
// stopped, and bucket keys that collapsed unrelated callers together.

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"sandcastle.dev/sandcastle/internal/domain"
)

func rate(perSec float64, burst int) *domain.Rate {
	return &domain.Rate{PerSecond: perSec, Burst: burst}
}

// Distinct identities must get distinct budgets. v1.1.0 keyed buckets on the
// tenant ID, which is empty for every platform-admin key, so all of them shared
// one bucket and the busiest credential starved the rest.
func TestDistinctIdentitiesGetDistinctBudgets(t *testing.T) {
	l := NewLimiter()
	defer l.Stop()
	r := rate(0.001, 2) // burst 2, effectively no refill during the test

	// Exhaust identity A.
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("key:A", "admin", r); !ok {
			t.Fatalf("A should have had burst tokens, denied at %d", i)
		}
	}
	if ok, _ := l.Allow("key:A", "admin", r); ok {
		t.Fatal("A should be exhausted")
	}
	// B must be untouched.
	if ok, _ := l.Allow("key:B", "admin", r); !ok {
		t.Fatal("B shares A's bucket — one admin key can starve another")
	}
	// And a tenant identity is separate again.
	if ok, _ := l.Allow("tenant:T1", "admin", r); !ok {
		t.Fatal("tenant identity shares an admin key's bucket")
	}
}

// Different groups are different budgets for the same identity: a create burst
// must not consume the exec budget.
func TestGroupsAreIndependentPerIdentity(t *testing.T) {
	l := NewLimiter()
	defer l.Stop()
	r := rate(0.001, 1)

	if ok, _ := l.Allow("tenant:T", domain.GroupCreate, r); !ok {
		t.Fatal("first create should pass")
	}
	if ok, _ := l.Allow("tenant:T", domain.GroupCreate, r); ok {
		t.Fatal("second create should be denied")
	}
	if ok, _ := l.Allow("tenant:T", domain.GroupExec, r); !ok {
		t.Fatal("exec budget was consumed by the create group")
	}
}

// The bucket key must not let two identities collide by concatenation, whatever
// the caller passes. A delimiter-based key only holds while no caller ever
// includes the delimiter; a length prefix holds unconditionally.
func TestBucketKeyCannotBeForgedByConcatenation(t *testing.T) {
	if bucketKey("a\x00b", "c") == bucketKey("a", "b\x00c") {
		t.Fatal("identities can collide into one bucket")
	}
	if bucketKey("a|b", "c") == bucketKey("a", "b|c") {
		t.Fatal("the delimiter can be shifted out of a value into the group")
	}
	// Normal keys stay distinct too.
	if bucketKey("tenant:x", "exec") == bucketKey("tenant:x", "create") {
		t.Fatal("groups collide")
	}
	if bucketKey("tenant:x", "exec") == bucketKey("tenant:y", "exec") {
		t.Fatal("identities collide")
	}
}

func TestNilOrZeroRateNeverBlocks(t *testing.T) {
	l := NewLimiter()
	defer l.Stop()
	for i := 0; i < 1000; i++ {
		if ok, _ := l.Allow("x", "g", nil); !ok {
			t.Fatal("a nil rate must mean 'no policy', not 'deny'")
		}
		if ok, _ := l.Allow("x", "g", rate(0, 0)); !ok {
			t.Fatal("a zero rate must mean 'no policy', not 'deny'")
		}
	}
}

func TestSweepDropsIdleBuckets(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	clock := base
	l := NewLimiter()
	defer l.Stop()
	l.now = func() time.Time { return clock }

	if ok, _ := l.Allow("old", "g", rate(1, 1)); !ok {
		t.Fatal("setup")
	}
	clock = base.Add(2 * time.Hour)
	if ok, _ := l.Allow("fresh", "g", rate(1, 1)); !ok {
		t.Fatal("setup")
	}

	if n := l.Sweep(30 * time.Minute); n != 1 {
		t.Fatalf("expected exactly the idle bucket to be dropped, dropped %d", n)
	}
	if l.Len() != 1 {
		t.Fatalf("expected 1 bucket left, got %d", l.Len())
	}
}

// The table must stay bounded even if the sweeper has not run yet: identities
// are caller-chosen, so growth cannot depend on a periodic timer alone.
func TestEvictionBoundsTheBucketTable(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	l := NewLimiter()
	defer l.Stop()

	const n = 500
	for i := 0; i < n; i++ {
		l.buckets[fmt.Sprintf("k%d", i)] = &bucket{
			tokens: 1, burst: 1, perSec: 1,
			last:     base,
			lastSeen: base.Add(time.Duration(i) * time.Second), // k0 is oldest
		}
	}
	l.evictOldestLocked(100)
	if l.Len() != n-100 {
		t.Fatalf("expected %d buckets, got %d", n-100, l.Len())
	}
	if _, present := l.buckets["k0"]; present {
		t.Fatal("the least-recently-seen bucket survived eviction")
	}
	if _, present := l.buckets[fmt.Sprintf("k%d", n-1)]; !present {
		t.Fatal("eviction removed the newest buckets instead of the oldest")
	}
}

// Stop must actually terminate the sweeper goroutine. Without it, every Service
// leaks one goroutine that keeps ticking against a discarded bucket table.
func TestStopTerminatesTheSweeper(t *testing.T) {
	const n = 300
	before := runtime.NumGoroutine()

	limiters := make([]*Limiter, 0, n)
	for i := 0; i < n; i++ {
		limiters = append(limiters, NewLimiter())
	}
	// Let them all reach their select and park.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() < before+n && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}

	for _, l := range limiters {
		l.Stop()
	}

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+8 {
			return // the sweeper goroutines are gone
		}
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("sweeper goroutines did not exit after Stop: %d running, started at %d",
		runtime.NumGoroutine(), before)
}

func TestStopIsIdempotent(t *testing.T) {
	l := NewLimiter()
	l.Stop()
	l.Stop() // must not panic on an already-closed channel
	// A stopped limiter still limits; Stop only ends the sweeper.
	if ok, _ := l.Allow("x", "g", rate(1, 1)); !ok {
		t.Fatal("a stopped limiter must still enforce its policy")
	}
}
