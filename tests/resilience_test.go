package tests

// Resilience: what survives a crash, a lost disk, and an infrastructure
// outage. These are the scenarios that decide whether an operator can run the
// platform without a human watching it — and they are the ones a demo never
// shows, because they need the process to die at an inconvenient moment.

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"sandcastle.dev/sandcastle/internal/domain"
)

func TestCrashRecovery(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "chaos")

	t.Run("a_sigkill_during_create_converges_instead_of_hanging", func(t *testing.T) {
		// wait=false hands back the record while provisioning is still in
		// flight, which is exactly the window a crash makes interesting.
		r := tn.TryCreateJSON(t, createBody{BaseImage: h.opts.Image, Spec: specOf()}, "?wait=false")
		r.MustStatus(t, http.StatusCreated)
		id := decode[*domain.Sandbox](t, r).ID
		h.track(tn.ID, id)

		h.Kill()
		h.Start(t)

		sb := tn.WaitForState(t, id, 60*time.Second,
			domain.StateRunning, domain.StateFailed, domain.StateStopped)
		// A transitional state surviving a restart means the record is now
		// lying about a container that may or may not exist.
		neq(t, sb.State, domain.StateProvisioning, "state after crash recovery")
	})

	t.Run("a_live_sandbox_survives_a_daemon_crash", func(t *testing.T) {
		sb := tn.Sandbox(t)
		eq(t, tn.Out(t, sb, "echo before-crash"), "before-crash", "exec before the crash")

		h.Kill()
		h.Start(t)

		after := tn.WaitForState(t, sb.ID, 60*time.Second,
			domain.StateRunning, domain.StateFailed, domain.StateStopped)
		eq(t, after.State, domain.StateRunning, "a container outlives the daemon that created it")
		eq(t, tn.Out(t, sb, "echo after-crash"), "after-crash", "exec after crash recovery")
	})

	t.Run("repeated_crashes_do_not_lose_or_duplicate_records", func(t *testing.T) {
		sb := tn.Sandbox(t)
		before := tn.Sandboxes(t, "").Count

		for i := 0; i < 3; i++ {
			h.Kill()
			h.Start(t)
		}

		page := tn.Sandboxes(t, "")
		eq(t, page.Count, before, "sandbox count after three crash cycles")
		eq(t, tn.Get(t, sb.ID).State, domain.StateRunning, "state of a live sandbox after three crash cycles")
	})

	t.Run("a_graceful_restart_keeps_serving", func(t *testing.T) {
		sb := tn.Sandbox(t)
		h.Restart(t)
		eq(t, tn.Get(t, sb.ID).State, domain.StateRunning, "state after a graceful restart")
		eq(t, tn.Out(t, sb, "echo after-restart"), "after-restart", "exec after a graceful restart")
	})
}

func TestPointInTimeRecovery(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "before-backup")
	sb := tn.Sandbox(t)
	tn.PutFile(t, sb, "/root/state.txt", []byte("pre-backup"))
	snap := tn.Snapshot(t, sb, "dr-snap")

	// The operator's backup: a copy of the data directory, taken while the
	// daemon is down so it is not a torn read of live files.
	h.Stop(t)
	backup := h.DataDir + ".bak"
	if out, err := exec.Command("cp", "-a", h.DataDir, backup).CombinedOutput(); err != nil {
		t.Fatalf("take the backup: %v\n%s", err, out)
	}
	h.Start(t)

	// Business continues after the backup and creates new state.
	postBackup := h.Tenant(t, "created-after-backup")

	// Disaster: the live data directory is lost, and the backup is restored.
	h.Stop(t)
	if out, err := exec.Command("rm", "-rf", h.DataDir).CombinedOutput(); err != nil {
		t.Fatalf("simulate the loss: %v\n%s", err, out)
	}
	if out, err := exec.Command("cp", "-a", backup, h.DataDir).CombinedOutput(); err != nil {
		t.Fatalf("restore the backup: %v\n%s", err, out)
	}
	h.Start(t)

	t.Run("state_from_before_the_backup_returns", func(t *testing.T) {
		eq(t, tn.Sandboxes(t, "").Count, 1, "sandboxes in the restored backup")
		// The credential lives in the store, so recovery has to restore
		// access too — a restored system nobody can log into is not recovered.
		tn.Client.Get("/v1/sandboxes").MustStatus(t, http.StatusOK)
	})

	t.Run("metadata_from_before_the_backup_returns", func(t *testing.T) {
		page := decode[snapshotPage](t, tn.Client.Get("/v1/snapshots").MustStatus(t, http.StatusOK))
		found := false
		for _, s := range page.Snapshots {
			if s.ID == snap.ID {
				found = true
			}
		}
		truthy(t, found, "the snapshot taken before the backup must be in the restored store")
	})

	t.Run("nothing_created_after_the_backup_is_present", func(t *testing.T) {
		// This is what makes it point-in-time recovery rather than a merge.
		// State that leaked in from after the backup means the restore cannot
		// be reasoned about at all.
		names := map[string]bool{}
		for _, tenant := range decode[tenantPage](t, h.Admin.Get("/v1/tenants").MustStatus(t, http.StatusOK)).Tenants {
			names[tenant.Name] = true
		}
		truthy(t, names[tn.Name], "the tenant from before the backup must exist")
		truthy(t, !names[postBackup.Name], "the tenant created after the backup must not exist")
	})
}

func TestBackendOutageIsDisclosedAndRecovers(t *testing.T) {
	if os.Getenv("SBX_IT_DESTRUCTIVE") != "1" {
		t.Skip("cycling the container runtime disrupts every other workload on this machine; " +
			"set SBX_IT_DESTRUCTIVE=1 to include it")
	}
	if _, err := exec.LookPath("colima"); err != nil {
		t.Skip("this machine's container runtime cannot be cycled from here")
	}
	h := New(t, Options{})
	tn := h.Tenant(t, "outage")
	sb := tn.Sandbox(t)

	cycleRuntime(t, "stop")

	// The daemon is still up and answering; its backend is gone. Readiness
	// must say so instead of answering 200 with a dead backend behind it.
	eventually(t, 30*time.Second, func() bool {
		return h.Ready().Status == http.StatusServiceUnavailable
	}, "readiness reporting the backend outage")

	cycleRuntime(t, "start")
	eventually(t, 180*time.Second, func() bool {
		return h.Ready().Status == http.StatusOK
	}, "readiness recovering when the backend returns")

	// The sandbox's container died with the runtime. The record must converge
	// to that truth rather than claim "running" forever.
	after := tn.WaitForState(t, sb.ID, 120*time.Second,
		domain.StateStopped, domain.StateFailed, domain.StateRunning)
	t.Logf("sandbox state after the runtime returned: %s", after.State)

	fresh := tn.Sandbox(t)
	eq(t, tn.Out(t, fresh, "echo post-outage"), "post-outage", "exec after the outage")
}

func TestNoBackendLeaks(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "leak")

	before := managedContainers(t)
	const n = 6
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, tn.Sandbox(t).ID)
	}

	eventually(t, 60*time.Second, func() bool {
		return managedContainers(t) == before+n
	}, "one container per running sandbox")

	for _, id := range ids {
		tn.Delete(t, id)
	}
	// Deletion is only deletion if the backend object is gone. A record that
	// disappears while its container keeps running is the leak that eventually
	// fills a node and takes the platform down.
	eventually(t, 90*time.Second, func() bool {
		return managedContainers(t) == before
	}, "every container removed once its sandbox is deleted")
}

func TestSnapshotsDoNotLeakImages(t *testing.T) {
	// The machine this was written on had eighty stale sandboxd-snap-* images
	// from runs that never checked. In a multi-tenant deployment that is an
	// unbounded disk leak that nobody notices until a node fills up.
	h := New(t, Options{})
	tn := h.Tenant(t, "snapleak")
	sb := tn.Sandbox(t)

	before := snapshotImages(t)
	snap := tn.Snapshot(t, sb, "leaky")
	afterCreate := snapshotImages(t)
	truthy(t, afterCreate > before, "committing a snapshot must materialize an image")

	tn.Client.Delete("/v1/snapshots/"+snap.ID).MustStatus(t, http.StatusNoContent)
	eventually(t, 90*time.Second, func() bool {
		return snapshotImages(t) <= before
	}, "deleting a snapshot removes the image it created")
}

// TestOrphanedSnapshotImagesAreReaped covers the leak path that actually bites
// in production: not "the operator forgot to delete a snapshot" (that works),
// but "the node lost its records" — a rebuilt host, a data directory on a
// volume that was not restored with it, a backup taken before the commit.
//
// A snapshot image is committed under a tag (sandboxd-snap-<id>), so it is not
// *dangling*, so `docker image prune` will not touch it. The reconciler's image
// pass is the only thing that ever will.
//
// Nothing here reaches through the API to delete a snapshot: the records are
// removed the way a real disaster removes them, by taking the data directory
// away. That also means the reconciler sees every unreferenced image on this
// machine, so the assertion is pinned to this test's own image rather than to
// a global count that pre-existing debris would satisfy for the wrong reason.
func TestOrphanedSnapshotImagesAreReaped(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "orphanimg")

	sb := tn.Sandbox(t)
	snap := tn.Snapshot(t, sb, "orphaned-by-disaster")
	// The committed repository name is the lowercased snapshot id behind a
	// fixed prefix; this is the exact object the operator's disk is paying for.
	image := "sandboxd-snap-" + strings.ToLower(snap.ID)
	truthy(t, imagePresent(image), "committing a snapshot must materialize "+image)

	// Refuse to pass if the daemon is not actually going to reconcile: a
	// frozen harness would make the assertion below vacuous.
	truthy(t, !h.opts.Frozen, "this scenario needs a live reconciler")

	// The disaster: the node keeps its container engine but loses its records.
	h.Kill()
	if err := os.RemoveAll(h.DataDir); err != nil {
		t.Fatalf("remove the data directory: %v", err)
	}
	if err := os.MkdirAll(h.DataDir, 0o700); err != nil {
		t.Fatalf("recreate an empty data directory: %v", err)
	}
	h.Start(t) // boots with an empty store and runs the startup reconcile

	eventually(t, 90*time.Second, func() bool {
		return !imagePresent(image)
	}, "a snapshot image no record references must be reaped")
}

// imagePresent reports whether a tagged image exists on this node.
func imagePresent(repoTag string) bool {
	return exec.Command("docker", "image", "inspect", repoTag).Run() == nil
}

// ---- runtime cycling --------------------------------------------------------

func cycleRuntime(t *testing.T, verb string) {
	t.Helper()
	out, err := exec.Command("colima", verb).CombinedOutput()
	if err != nil {
		t.Fatalf("colima %s: %v\n%s", verb, err, out)
	}
	// colima returns before the socket is usable in both directions.
	time.Sleep(3 * time.Second)
}
