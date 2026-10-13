package service

import (
	"math"
	"testing"
	"time"

	"sandcastle.dev/sandcastle/internal/domain"
)

func approx(got, want, eps float64) bool { return math.Abs(got-want) <= eps }

// advanceClock pins the meter's clock so the next sample integrates an exact,
// known dt of `by` (now = lastSample + by).
func advanceClock(h *harness, by time.Duration) {
	base := time.Now()
	h.svc.meter.lastSample = base
	h.svc.meter.now = func() time.Time { return base.Add(by) }
}

// TestMeterSamplesResourceSeconds: a running sandbox integrates its allocation
// over wall time — the definition of a vCPU/GiB/session hour, not a rate guess.
func TestMeterSamplesResourceSeconds(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true) // template default spec: cpu 500, mem 256
	_ = sb

	advanceClock(h, 30*time.Second)
	h.svc.meter.sample()
	h.svc.meter.flush()

	u, ok := h.st.GetUsage(h.tn.ID)
	if !ok {
		t.Fatal("usage not persisted after a sample+flush")
	}
	if !approx(u.SandboxSeconds, 30, 0.01) {
		t.Fatalf("sandbox_seconds = %f, want 30", u.SandboxSeconds)
	}
	if !approx(u.CPUMillicoreSeconds, 500*30, 0.01) {
		t.Fatalf("cpu_millicore_seconds = %f, want %d", u.CPUMillicoreSeconds, 500*30)
	}
	if !approx(u.MemoryMiBSeconds, 256*30, 0.01) {
		t.Fatalf("memory_mib_seconds = %f, want %d", u.MemoryMiBSeconds, 256*30)
	}
}

// TestMeterPausedSandboxAccruesMemoryNotCPU: the "I/O is free, memory is
// billed" split — a paused sandbox holds its working set but is not scheduled.
func TestMeterPausedSandboxAccruesMemoryNotCPU(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	if _, err := h.svc.PauseSandbox(t.Context(), h.tn.ID, h.key.ID, sb.ID); err != nil {
		t.Fatal(err)
	}

	advanceClock(h, 30*time.Second)
	h.svc.meter.sample()
	h.svc.meter.flush()

	u, _ := h.st.GetUsage(h.tn.ID)
	if !approx(u.CPUMillicoreSeconds, 0, 0.01) {
		t.Fatalf("paused sandbox must accrue no CPU, got %f", u.CPUMillicoreSeconds)
	}
	if !approx(u.MemoryMiBSeconds, 256*30, 0.01) {
		t.Fatalf("paused sandbox must still accrue memory, got %f", u.MemoryMiBSeconds)
	}
}

// TestMeterRecordsExecsAndIsVisibleBeforeFlush: exec counters accrue on the
// request path and are served by Metering immediately, without waiting for the
// periodic flush.
func TestMeterRecordsExecsAndIsVisibleBeforeFlush(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)

	for i := 0; i < 3; i++ {
		if _, err := h.svc.Exec(t.Context(), h.tn.ID, h.key.ID, sb.ID, execReq("true"), ""); err != nil {
			t.Fatal(err)
		}
	}

	u := h.svc.Metering(h.tn.ID)[0]
	if u.ExecCount != 3 {
		t.Fatalf("exec_count = %d, want 3 (before any flush)", u.ExecCount)
	}

	// Flush makes it durable, and a fresh read still agrees.
	h.svc.meter.flush()
	durable, _ := h.st.GetUsage(h.tn.ID)
	if durable.ExecCount != 3 {
		t.Fatalf("durable exec_count = %d, want 3", durable.ExecCount)
	}
}

// TestMeteringMergesStoredAndPending: the un-flushed tail plus the durable
// record must sum, not overwrite.
func TestMeteringMergesStoredAndPending(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)

	// First batch: one exec, flushed.
	if _, err := h.svc.Exec(t.Context(), h.tn.ID, h.key.ID, sb.ID, execReq("true"), ""); err != nil {
		t.Fatal(err)
	}
	h.svc.meter.flush()

	// Second batch: two more execs, NOT flushed.
	for i := 0; i < 2; i++ {
		if _, err := h.svc.Exec(t.Context(), h.tn.ID, h.key.ID, sb.ID, execReq("true"), ""); err != nil {
			t.Fatal(err)
		}
	}

	u := h.svc.Metering(h.tn.ID)[0]
	if u.ExecCount != 3 {
		t.Fatalf("merged exec_count = %d, want 3 (1 durable + 2 pending)", u.ExecCount)
	}
}

// TestMeteringScopesByTenant: the meter attributes accruals to the owning
// tenant, never a neighbour.
func TestMeteringScopesByTenant(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	if _, err := h.svc.Exec(t.Context(), h.tn.ID, h.key.ID, sb.ID, execReq("true"), ""); err != nil {
		t.Fatal(err)
	}
	h.svc.meter.flush()

	other := domain.NewTenant("neighbour")
	if err := h.st.PutTenant(other); err != nil {
		t.Fatal(err)
	}

	got := h.svc.Metering(h.tn.ID)
	if len(got) != 1 || got[0].TenantID != h.tn.ID || got[0].ExecCount != 1 {
		t.Fatalf("tenant metering wrong: %+v", got)
	}
	otherUsage := h.svc.Metering(other.ID)[0]
	if otherUsage.ExecCount != 0 {
		t.Fatalf("neighbour leaked usage: %+v", otherUsage)
	}
}
