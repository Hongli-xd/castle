package domain

import (
	"testing"
	"time"
)

var testLimits = Limits{
	MaxCPUMillicores:  8000,
	MaxMemoryMiB:      16384,
	MaxDiskMiB:        51200,
	MaxTimeoutSeconds: 86400,
	MaxEnvEntries:     50,
	MaxEnvValueBytes:  8192,
	MaxMetadataKeys:   20,
	MaxMetadataBytes:  1024,
}

func TestStateMachineLegalTransitions(t *testing.T) {
	legal := [][2]SandboxState{
		{StateProvisioning, StateRunning},
		{StateProvisioning, StateFailed},
		{StateProvisioning, StateStopping},
		{StateRunning, StatePausing},
		{StateRunning, StateStopping},
		{StatePausing, StatePaused},
		{StatePausing, StateRunning}, // pause aborted
		{StatePaused, StateResuming},
		{StateResuming, StateRunning},
		{StateResuming, StatePaused}, // resume aborted
		{StateStopping, StateStopped},
		{StateStopping, StateFailed},
		{StateFailed, StateStopping}, // cleanup path
	}
	for _, tc := range legal {
		if !tc[0].CanTransition(tc[1]) {
			t.Errorf("%s → %s should be legal", tc[0], tc[1])
		}
	}
}

func TestStateMachineIllegalTransitions(t *testing.T) {
	illegal := [][2]SandboxState{
		{StateStopped, StateRunning}, // resurrection forbidden
		{StateStopped, StateProvisioning},
		{StateRunning, StatePaused}, // must pass through pausing
		{StatePaused, StateRunning}, // must pass through resuming
		{StatePaused, StatePausing},
		{StateFailed, StateRunning},
		{StateProvisioning, StatePaused},
	}
	for _, tc := range illegal {
		if tc[0].CanTransition(tc[1]) {
			t.Errorf("%s → %s should be illegal", tc[0], tc[1])
		}
	}
}

func TestTransitionalStatesCoverAllInFlightOps(t *testing.T) {
	// every non-terminal state must be classified — a state that is neither
	// terminal nor transitional would be silently skipped by the reconciler.
	// running/paused are the healthy live states; all others must be exactly
	// one of terminal or transitional.
	for s := StateProvisioning; ; s = next(s) {
		live := s == StateRunning || s == StatePaused
		if !live && s.Terminal() == s.Transitional() {
			t.Errorf("state %s must be exactly one of terminal/transitional/live", s)
		}
		if s == StateFailed {
			break
		}
	}
}

func next(s SandboxState) SandboxState {
	order := []SandboxState{StateProvisioning, StateRunning, StatePausing, StatePaused, StateResuming, StateStopping, StateStopped, StateFailed}
	for i, v := range order {
		if v == s && i+1 < len(order) {
			return order[i+1]
		}
	}
	return StateFailed
}

func TestSpecValidation(t *testing.T) {
	cases := []struct {
		name string
		spec SandboxSpec
		ok   bool
	}{
		{"ok", SandboxSpec{CPUMillicores: 500, MemoryMiB: 256, TimeoutSeconds: 3600}, true},
		{"cpu too low", SandboxSpec{CPUMillicores: 50, MemoryMiB: 256, TimeoutSeconds: 3600}, false},
		{"cpu too high", SandboxSpec{CPUMillicores: 64000, MemoryMiB: 256, TimeoutSeconds: 3600}, false},
		{"mem too low", SandboxSpec{CPUMillicores: 500, MemoryMiB: 16, TimeoutSeconds: 3600}, false},
		{"timeout too short", SandboxSpec{CPUMillicores: 500, MemoryMiB: 256, TimeoutSeconds: 10}, false},
		{"env bad key", SandboxSpec{CPUMillicores: 500, MemoryMiB: 256, TimeoutSeconds: 3600,
			Env: map[string]string{"1BAD": "x"}}, false},
		{"env ok", SandboxSpec{CPUMillicores: 500, MemoryMiB: 256, TimeoutSeconds: 3600,
			Env: map[string]string{"FOO": "bar"}}, true},
		{"workdir traversal", SandboxSpec{CPUMillicores: 500, MemoryMiB: 256, TimeoutSeconds: 3600,
			WorkDir: "/safe/../escape"}, false},
		{"relative workdir", SandboxSpec{CPUMillicores: 500, MemoryMiB: 256, TimeoutSeconds: 3600,
			WorkDir: "relative"}, false},
		{"bad user", SandboxSpec{CPUMillicores: 500, MemoryMiB: 256, TimeoutSeconds: 3600,
			User: "-evil"}, false},
	}
	for _, tc := range cases {
		err := tc.spec.Validate(testLimits, 0)
		if tc.ok != (err == nil) {
			t.Errorf("%s: err=%v want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestEgressValidation(t *testing.T) {
	if err := (EgressPolicy{Mode: EgressNone}).Validate(); err != nil {
		t.Errorf("none should validate: %v", err)
	}
	if err := (EgressPolicy{Mode: EgressAllowlist}).Validate(); err == nil {
		t.Error("allowlist without domains must be rejected")
	}
	if err := (EgressPolicy{Mode: "surprise"}).Validate(); err == nil {
		t.Error("unknown egress mode must be rejected, never silently accepted")
	}
	if err := (EgressPolicy{Mode: EgressAllowlist, Allow: []string{"api.github.com", "proxy.golang.org"}}).Validate(); err != nil {
		t.Errorf("valid allowlist rejected: %v", err)
	}
}

func TestSandboxExpiry(t *testing.T) {
	sb := NewSandbox("t1")
	sb.ExpiresAt = time.Now().UTC().Add(-time.Second)
	if !sb.Expired(time.Now()) {
		t.Fatal("expired sandbox not detected")
	}
	if err := sb.Extend(600, testLimits); err != nil {
		t.Fatalf("extend: %v", err)
	}
	if sb.Expired(time.Now()) {
		t.Fatal("extension did not move expiry")
	}
	if err := sb.Extend(1, testLimits); err == nil {
		t.Fatal("extend below minimum must be rejected")
	}
}

func TestAPIKeyValidity(t *testing.T) {
	k := NewAPIKey("t1", "ci", ScopeTenant)
	k.Prefix = "ak_123456789012"
	k.Hash = "x"
	if !k.Valid(time.Now()) {
		t.Fatal("fresh key should be valid")
	}
	past := time.Now().Add(-time.Hour)
	k.ExpiresAt = &past
	if k.Valid(time.Now()) {
		t.Fatal("expired key must not validate")
	}
	k.ExpiresAt = nil
	now := time.Now()
	k.RevokedAt = &now
	if k.Valid(time.Now()) {
		t.Fatal("revoked key must not validate")
	}
	admin := NewAPIKey("", "root", ScopeAdmin)
	if !admin.Valid(time.Now()) {
		t.Fatal("admin key without tenant should validate")
	}
	orphan := NewAPIKey("", "broken", ScopeTenant)
	if orphan.Valid(time.Now()) {
		t.Fatal("tenant key without tenant must not validate")
	}
}

func TestTenantQuotaValidation(t *testing.T) {
	q := TenantQuota{MaxSandboxes: 10, Rates: map[string]*Rate{
		GroupExec: {PerSecond: 5, Burst: 10},
	}}
	if err := q.Validate(testLimits); err != nil {
		t.Fatalf("valid quota rejected: %v", err)
	}
	q.Rates["nope"] = &Rate{PerSecond: 1, Burst: 1}
	if err := q.Validate(testLimits); err == nil {
		t.Fatal("unknown rate group must be rejected")
	}
}
