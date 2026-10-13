package tests

// End-to-end idempotency: the wire contract an agent (via MCP, now stateless)
// relies on to retry an exec whose response was lost without running the
// command twice. Unit tests cover the registry; this file proves the whole
// path — handler, service, cache, and the `replayed` field on the wire.

import (
	"net/http"
	"testing"

	"sandcastle.dev/sandcastle/internal/apperr"
)

func TestExecIdempotencyOverHTTP(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "idem")
	sb := tn.Sandbox(t)

	const key = "e2e-op-1"
	first, r1 := tn.TryExec(t, sb, execReq{Command: "echo once", TimeoutMs: 30_000, IdempotencyKey: key})
	r1.MustStatus(t, http.StatusOK)
	if first.Replayed {
		t.Fatal("first execution must not be flagged replayed")
	}

	second, r2 := tn.TryExec(t, sb, execReq{Command: "echo once", TimeoutMs: 30_000, IdempotencyKey: key})
	r2.MustStatus(t, http.StatusOK)
	if !second.Replayed {
		t.Fatal("a retry with the same key must be flagged replayed on the wire")
	}
	if string(second.Stdout) != string(first.Stdout) || second.ExitCode != first.ExitCode {
		t.Fatalf("replay changed the result: %+v vs %+v", first, second)
	}

	// A different key is a different logical operation and must run fresh.
	other, r3 := tn.TryExec(t, sb, execReq{Command: "echo once", TimeoutMs: 30_000, IdempotencyKey: "e2e-op-2"})
	r3.MustStatus(t, http.StatusOK)
	if other.Replayed {
		t.Fatal("a distinct key must not be a replay")
	}

	// Reusing a key for a different command is a client bug and is refused.
	_, rc := tn.TryExec(t, sb, execReq{Command: "echo different", TimeoutMs: 30_000, IdempotencyKey: key})
	rc.MustErr(t, apperr.Conflict)
}

func TestExecIdempotencyIsScopedPerSandbox(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "idem-scope")
	a := tn.Sandbox(t)
	b := tn.Sandbox(t)

	const key = "shared-key-name"
	if _, r := tn.TryExec(t, a, execReq{Command: "echo A", TimeoutMs: 30_000, IdempotencyKey: key}); r.Status != http.StatusOK {
		t.Fatalf("first sandbox exec: %s", r.describe())
	}
	// The same key against a *different* sandbox is a different operation, not
	// a replay of the first sandbox's result.
	res, r := tn.TryExec(t, b, execReq{Command: "echo B", TimeoutMs: 30_000, IdempotencyKey: key})
	r.MustStatus(t, http.StatusOK)
	if res.Replayed {
		t.Fatal("an idempotency key must not leak results across sandboxes")
	}
}
