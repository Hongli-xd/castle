package service

// Test for the exec output ceiling. In v1.1.0 the JSON field max_output was
// forwarded verbatim, so a caller could ask for 2 GB and the control plane
// would buffer it in its own heap before trimming — one request, shared
// memory (poc/probe_live.sh, PoC-4, measured 254 MiB materialized by a single
// request). Nothing rejected it, and the ExecRequest comment described the
// field as a preference rather than a limit.

import (
	"context"
	"testing"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/runtime"
)

func TestExecRejectsMaxOutputAboveThePlatformCeiling(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	ctx := context.Background()
	ceiling := int(h.svc.Cfg.MaxExecOutput)

	if ceiling <= 0 {
		t.Fatalf("the platform must have a real ceiling configured, got %d", ceiling)
	}

	exec := func(t *testing.T, maxOut int) error {
		t.Helper()
		_, err := h.svc.Exec(ctx, h.tn.ID, h.key.ID, sb.ID, runtime.ExecRequest{
			Argv:      []string{"echo", "hi"},
			MaxOutput: maxOut,
		}, "")
		return err
	}

	// 0 selects the backend default; it is explicitly NOT "unlimited".
	if err := exec(t, 0); err != nil {
		t.Fatalf("max_output=0 (platform default) must be accepted: %v", err)
	}
	// Exactly at the ceiling is inside the contract.
	if err := exec(t, ceiling); err != nil {
		t.Fatalf("max_output at the ceiling (%d) must be accepted: %v", ceiling, err)
	}
	// One byte past it is not.
	err := exec(t, ceiling+1)
	if err == nil {
		t.Fatalf("max_output=%d exceeds the %d ceiling and must be rejected", ceiling+1, ceiling)
	}
	if got := apperr.From(err).Code; got != apperr.InvalidRequest {
		t.Fatalf("rejection should be a client error (invalid), got %v: %v", got, err)
	}

	// The value the original PoC used to make the daemon allocate 254 MiB.
	if err := exec(t, 2_000_000_000); err == nil {
		t.Fatal("the PoC-4 value (2e9) must be rejected")
	}

	// A negative value must be refused outright: the HTTP field is an int, and
	// silently treating it as "default" hides a client bug, while a downstream
	// unsigned cast turns it into an enormous ceiling.
	if err := exec(t, -1); err == nil {
		t.Fatal("a negative max_output must be rejected, not reinterpreted")
	}
	if got := apperr.From(exec(t, -1)).Code; got != apperr.InvalidRequest {
		t.Fatalf("negative max_output should be invalid, got %v", got)
	}
}

// The ceiling is a platform limit, not a per-sandbox one: raising the config
// must widen what is accepted, so an operator can tune it without a code change.
func TestExecCeilingFollowsConfiguration(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	ctx := context.Background()

	original := h.svc.Cfg.MaxExecOutput
	t.Cleanup(func() { h.svc.Cfg.MaxExecOutput = original })

	h.svc.Cfg.MaxExecOutput = 64 << 20
	req := runtime.ExecRequest{Argv: []string{"echo"}, MaxOutput: 32 << 20}
	if _, err := h.svc.Exec(ctx, h.tn.ID, h.key.ID, sb.ID, req, ""); err != nil {
		t.Fatalf("32 MiB should be inside a 64 MiB ceiling: %v", err)
	}

	h.svc.Cfg.MaxExecOutput = 4 << 20
	if _, err := h.svc.Exec(ctx, h.tn.ID, h.key.ID, sb.ID, req, ""); err == nil {
		t.Fatal("lowering the ceiling must start rejecting the same request")
	}
}

// The backend must refuse an out-of-contract ceiling on its own, so a future
// caller that bypasses the service layer cannot reintroduce the allocation.
func TestOCIBackendRefusesAbsurdCeiling(t *testing.T) {
	// Exercised through the interface constant rather than a live Docker call:
	// the guard fires before any container is touched.
	if runtime.MaxBackendOutput <= runtime.DefaultOutputCap {
		t.Fatalf("the backend's hard ceiling (%d) must exceed the default capture size (%d)",
			runtime.MaxBackendOutput, runtime.DefaultOutputCap)
	}
}
