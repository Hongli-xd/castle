package tests

// Containment: the hostile payloads a hijacked agent actually runs, and the
// evidence that the blast radius is the sandbox.
//
// These scenarios replace tests/malicious.sh. The bash version asserted "the
// host is fine" without ever proving the payload did anything, and a probe
// that silently no-ops passes exactly the same assertion a fully contained
// attack does. Every scenario here therefore carries a positive control:
// evidence that the hostile command really executed and really worked on the
// thing it was aimed at. "Nothing bad happened" is only meaningful once
// "something happened" is established.
//
// What is claimed here is containment, not kernel-level isolation. The sandbox
// shares the host kernel by construction; see TestIsolationContract for the
// list of layers an escape would have to defeat simultaneously.

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
)

// hostFileDigest hashes a file on the host, so "the host is unchanged" is a
// measurement rather than an impression.
func hostFileDigest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read host file %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestBlastRadiusIsTheSandbox destroys a sandbox from the inside and proves
// that the wreckage stops at its own boundary: the daemon lives, the host is
// byte-identical, the tenant's other sandboxes are untouched, and the wreck is
// still reclaimable.
func TestBlastRadiusIsTheSandbox(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "blast")

	// Real things on the host that must survive. The canary is a file we
	// create; /etc/hosts is one the operator already had.
	canary := filepath.Join(t.TempDir(), "host-canary.txt")
	canaryBody := []byte("host-canary-do-not-touch\n")
	if err := os.WriteFile(canary, canaryBody, 0o600); err != nil {
		t.Fatalf("write host canary: %v", err)
	}
	etcHostsBefore := hostFileDigest(t, "/etc/hosts")

	t.Run("the_payload_really_works_on_a_path_it_can_reach", func(t *testing.T) {
		// The control. Without this, a `rm -rf /` that silently failed looks
		// identical to one that was contained. This proves the tool works and
		// the filesystem is writable, so the next scenario measures isolation
		// and not a no-op.
		sb := tn.Sandbox(t)
		out := tn.Out(t, sb, "mkdir -p /destroy-me && touch /destroy-me/f && "+
			"rm -rf /destroy-me && test ! -e /destroy-me && echo gone")
		eq(t, out, "gone", "rm -rf inside the sandbox")
	})

	wrecked := tn.Sandbox(t)
	bystander := tn.Sandbox(t)

	t.Run("rm_rf_root_wrecks_only_that_box", func(t *testing.T) {
		res := tn.Exec(t, wrecked, "rm -rf / >/dev/null 2>&1; echo payload-done")
		// The shell reached the statement after `rm`, so the payload ran to
		// completion rather than being refused up front.
		contains(t, string(res.Stdout), "payload-done", "the payload ran to completion")

		// The daemon is unbothered. This is the whole point of running
		// workloads in a container rather than in the control plane.
		r := tn.Client.Get("/healthz")
		eq(t, r.Status, http.StatusOK, "daemon health after rm -rf /")

		// A sandbox in the same tenant, created before the wreck, still does
		// its job. Containment that costs the neighbours is not containment.
		if out := tn.Out(t, bystander, "echo still-serving"); out != "still-serving" {
			t.Errorf("bystander sandbox after a neighbour was wrecked: got %q, want %q", out, "still-serving")
		}

		// A brand new sandbox is fully functional: the damage did not reach
		// the image, the store, or the node.
		fresh := tn.Sandbox(t)
		if out := tn.Out(t, fresh, "echo clean-room"); out != "clean-room" {
			t.Errorf("fresh sandbox after a neighbour was wrecked: got %q, want %q", out, "clean-room")
		}
	})

	t.Run("the_host_is_untouched", func(t *testing.T) {
		got, err := os.ReadFile(canary)
		if err != nil {
			t.Fatalf("host canary is gone after the payload: %v", err)
		}
		eq(t, string(got), string(canaryBody), "host canary contents")
		eq(t, hostFileDigest(t, "/etc/hosts"), etcHostsBefore, "host /etc/hosts digest")
	})

	t.Run("the_wreck_is_still_reclaimable", func(t *testing.T) {
		// A sandbox that cannot be deleted after being wrecked is a leak the
		// operator has no remedy for. Removal must not depend on the box
		// being healthy.
		h.forceDelete(t, tn.ID, wrecked.ID)
		tn.Client.Get("/v1/sandboxes/"+wrecked.ID).
			MustStatus(t, http.StatusNotFound).MustErr(t, apperr.NotFound)
	})
}

// TestForkBombIsContained runs a process bomb and proves the pids cap turned it
// into a local, bounded annoyance rather than a wedged node.
func TestForkBombIsContained(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "forkbomb")

	sb := tn.Sandbox(t)

	res, r := tn.TryExec(t, sb, execReq{
		Command:   "i=0; while [ $i -lt 2000 ]; do sleep 30 & i=$((i+1)); done; wait",
		TimeoutMs: 10_000,
		MaxOutput: 1024,
	})
	// A 5xx here would mean the daemon itself came apart, which is the failure
	// this scenario exists to rule out. A timeout or a non-zero exit is the
	// cap doing its job.
	truthy(t, r.Status < 500, "a fork bomb must not produce a server error (got "+strconv.Itoa(r.Status)+")")
	if r.Status == http.StatusOK && res != nil {
		t.Logf("fork bomb exec: exit=%d timed_out=%v", res.ExitCode, res.TimedOut)
	}

	// The control plane must still answer.
	tn.Client.Get("/healthz").MustStatus(t, http.StatusOK)

	// The tenant must still be able to run code: either the box kept serving,
	// or it reports an actionable refusal. What it must not do is hang or
	// return an opaque internal error.
	res2, r2 := tn.TryExec(t, sb, execReq{Command: "echo still-here", TimeoutMs: 30_000})
	switch {
	case r2.Status == http.StatusOK && res2 != nil && res2.ExitCode == 0 &&
		strings.Contains(string(res2.Stdout), "still-here"):
		// The pids cap absorbed the bomb without disturbing this sandbox.
	case r2.Err() != nil && r2.Err().Code == apperr.Conflict:
		// Process table exhausted: an actionable 409, not a hang.
	default:
		t.Errorf("after a fork bomb, a second exec returned %s\nbody: %s", r2.describe(), r2.snippet())
	}

	// And the bombed sandbox is always reclaimable.
	h.forceDelete(t, tn.ID, sb.ID)
}

// TestMemoryBombIsContainedInOneSandbox proves a heap bomb dies against its own
// memory cap and takes nothing else with it. Memory is the resource a customer
// is most likely to under-provision, so "one tenant's runaway agent cannot OOM
// a neighbour" is a commercial claim, not a nicety.
func TestMemoryBombIsContained(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "oom")

	neighbour := tn.Sandbox(t)
	bomb := tn.Sandbox(t, withMemory(64))

	res := tn.Exec(t, bomb, "A=x; i=0; while [ $i -lt 200 ]; do A=$A$A; i=$((i+1)); done; echo survived")
	neq(t, res.ExitCode, 0, "heap bomb exit code against a 64 MiB cap")
	// 137 is SIGKILL, which the kernel's OOM killer sends. Any other non-zero
	// exit is still containment, but the reason matters to whoever reads the
	// deployment's logs, so it is recorded rather than assumed.
	t.Logf("48 GiB of shell string growth against a 64 MiB cap: exit=%d", res.ExitCode)

	// The neighbour, untouched by the OOM.
	eq(t, tn.Out(t, neighbour, "echo neighbour-alive"), "neighbour-alive", "neighbour sandbox after OOM")

	// And the daemon is fine: an OOM kill must not look like a control-plane
	// failure to the next caller.
	tn.Client.Get("/healthz").MustStatus(t, http.StatusOK)
}

// TestCPUBurnDoesNotStarveTheControlPlane pins one sandbox's core to 100% and
// checks the control plane keeps answering. This is the difference between
// "CPU limits are configured" and "CPU limits are enforced".
func TestCPUBurnDoesNotStarveTheControlPlane(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "cpuburn")

	burner := tn.Sandbox(t, withCPU(500))
	other := tn.Sandbox(t)

	// The burner must be started without waiting: the exec cannot return until
	// its own timeout, and we need the control plane probed while it runs.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Raw client, no testing.T: this goroutine outlives the assertion.
		tn.Client.Post("/v1/sandboxes/"+burner.ID+"/exec",
			execReq{Command: "while :; do :; done", TimeoutMs: 5_000, MaxOutput: 256})
	}()

	// Give the burner time to actually saturate its quota before measuring;
	// otherwise the probes time an idle machine and prove nothing.
	time.Sleep(1 * time.Second)

	// The control plane must stay responsive.
	start := time.Now()
	for i := 0; i < 5; i++ {
		tn.Client.Get("/healthz").MustStatus(t, http.StatusOK)
	}
	healthElapsed := time.Since(start)

	// Another tenant's-or-this-tenant's unrelated sandbox must still get work
	// done. The health endpoint is cheap; a real exec is not.
	execStart := time.Now()
	eq(t, tn.Out(t, other, "echo not-starved"), "not-starved", "exec in a second sandbox during the burn")
	execElapsed := time.Since(execStart)

	wg.Wait()

	// Generous bounds: a loaded CI box is slow, and a tight bound here would
	// be a flake generator rather than a test. The failure this catches — the
	// daemon starved by one tenant's burner — costs seconds, not milliseconds.
	if healthElapsed > 3*time.Second {
		t.Errorf("5 health checks took %s while a core-burner ran; the control plane is being starved", healthElapsed)
	}
	if execElapsed > 10*time.Second {
		t.Errorf("an exec in an unrelated sandbox took %s while a core-burner ran; CPU is not isolated", execElapsed)
	}
	t.Logf("during the burn: 5 health checks in %s, unrelated exec in %s", healthElapsed.Round(time.Millisecond), execElapsed.Round(time.Millisecond))
}

// TestInSandboxHostPathWritesDoNotReachTheHost writes to /etc/hosts and
// /etc/passwd from inside a sandbox. The same path string means a different
// file on each side of the boundary, and this is where that stops being an
// assumption.
//
// The positive control matters more here than anywhere else: if the in-sandbox
// write had been refused by a read-only mount, "the host is unchanged" would
// pass while proving nothing at all.
func TestInSandboxHostPathWritesDoNotReachTheHost(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "hostpaths")

	etcHostsBefore := hostFileDigest(t, "/etc/hosts")
	etcPasswdBefore := hostFileDigest(t, "/etc/passwd")

	sb := tn.Sandbox(t)

	// The payload writes a recognisable marker to both files. Whether the
	// write succeeds or fails inside the sandbox, the host must not change —
	// but we insist on knowing which it was.
	out := tn.Out(t, sb, "echo pwned >> /etc/hosts 2>&1; rc1=$?; "+
		"echo pwned >> /etc/passwd 2>&1; rc2=$?; echo rc1=$rc1 rc2=$rc2")

	control := tn.Out(t, sb, "grep -c pwned /etc/hosts; grep -c pwned /etc/passwd")
	t.Logf("in-sandbox write result: %s; in-sandbox read-back: %s", out, strings.ReplaceAll(control, "\n", ","))

	// One of the two writes must have landed inside the sandbox, or the probe
	// never tested anything. Docker binds /etc/hosts and /etc/passwd into the
	// container, so they are writable there.
	truthy(t, strings.Contains(control, "1") || strings.Contains(control, "2"),
		"the payload must have modified the in-sandbox copy, or this proves nothing about the boundary")

	eq(t, hostFileDigest(t, "/etc/hosts"), etcHostsBefore, "host /etc/hosts digest")
	eq(t, hostFileDigest(t, "/etc/passwd"), etcPasswdBefore, "host /etc/passwd digest")
}
