package tests

// Governance and lifecycle: the parts a customer's bill, their audit and
// their retry logic depend on. Quotas must bind per tenant, illegal state
// moves must be refused with a governed error rather than an internal one,
// and deletion must actually free what it claims to free.

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
)

func TestQuotaAndAdmission(t *testing.T) {
	h := New(t, Options{})
	// Room to work, but a small sandbox ceiling and a generous create rate so
	// the gate under test is admission and quota, never the load generator.
	limited := h.TenantWith(t, "limited", quota(2, 2048, fastRates(50, 60)))
	other := h.TenantWith(t, "other", quota(10, 4096, fastRates(50, 60)))

	t.Run("sandbox_ceiling_binds_and_frees_on_delete", func(t *testing.T) {
		first := limited.Sandbox(t)
		limited.Sandbox(t)

		_, r := limited.TrySandbox(t, createBody{BaseImage: h.opts.Image, Spec: specOf()}, "?wait=false")
		r.MustStatus(t, http.StatusPaymentRequired).MustErr(t, apperr.QuotaExceeded)

		limited.Delete(t, first.ID)

		// The slot must actually come back. A counter that only increments is
		// a leak, and leaks turn into support tickets with a bill attached.
		limited.Sandbox(t)
	})

	t.Run("quotas_are_per_tenant", func(t *testing.T) {
		// `other` is untouched by `limited` saturating its own ceiling.
		other.Sandbox(t)
	})

	t.Run("create_rate_limit_is_governed_and_retryable", func(t *testing.T) {
		// 0.01/s, not 1/s. The bucket refills continuously
		// (tokens += elapsed*perSec), so a realistic rate turns this into a
		// timing test: waiting ~1.2s for a container to start refunds a whole
		// token at 1/s and the second create is admitted. At 0.01/s a token
		// costs 100s to earn, so the second call is refused deterministically.
		// Only the create bucket is pinned, so this scenario's own cleanup is
		// not itself throttled.
		throttled := h.TenantWith(t, "throttled", quota(50, 4096,
			ratesExcept(domain.GroupCreate, domain.Rate{PerSecond: 0.01, Burst: 1})))

		// wait=false: consume the single token without waiting for a
		// container. The sandbox itself is not what this scenario measures.
		first := decode[*domain.Sandbox](t, throttled.TryCreateJSON(t,
			createBody{BaseImage: h.opts.Image, Spec: specOf()}, "?wait=false").
			MustStatus(t, http.StatusCreated))

		r := throttled.TryCreateJSON(t, createBody{BaseImage: h.opts.Image, Spec: specOf()}, "?wait=false")
		r.MustStatus(t, http.StatusTooManyRequests).MustErr(t, apperr.RateLimited)
		truthy(t, r.Header.Get("Retry-After") != "", "a 429 must tell the caller when to come back")

		// The refused attempt must not have created a second sandbox. Cleanup
		// goes through the admin plane: the tenant's own DELETE shares the
		// create bucket this scenario just squeezed to 0.01/s.
		eq(t, throttled.Sandboxes(t, "").Count, 1, "a rate-limited create must not admit a sandbox")
		h.forceDelete(t, throttled.ID, first.ID)
	})

	t.Run("specs_outside_platform_limits_are_rejected_with_a_reason", func(t *testing.T) {
		cases := []struct {
			name string
			spec domain.SandboxSpec
		}{
			{"cpu below the floor", specOf(withCPU(10))},
			{"cpu above the ceiling", specOf(withCPU(1_000_000))},
			{"memory below the floor", specOf(withMemory(8))},
			{"memory above the ceiling", specOf(withMemory(1_000_000))},
			{"timeout below the floor", specOf(withTimeout(5))},
			{"timeout above the ceiling", specOf(withTimeout(10_000_000))},
			{"unknown egress mode", specOf(withEgress(domain.EgressPolicy{Mode: "telepathy"}))},
			{"allowlist with no domains", specOf(withEgress(domain.EgressPolicy{Mode: domain.EgressAllowlist}))},
			{"env key that is not an identifier", specOf(withEnv(map[string]string{"not a key": "v"}))},
			{"workdir that is relative", specOf(withWorkDir("relative/path"))},
			{"workdir that escapes", specOf(withWorkDir("/safe/../../etc"))},
			{"user that is not a name", specOf(withUser("../../etc/passwd"))},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, r := limited.TrySandbox(t, createBody{BaseImage: h.opts.Image, Spec: tc.spec}, "?wait=false")
				r.MustErr(t, apperr.InvalidRequest)
			})
		}
	})

	t.Run("env_flood_is_rejected_before_it_reaches_the_backend", func(t *testing.T) {
		env := map[string]string{}
		for i := 0; i < 2000; i++ {
			env["K"+strconv.Itoa(i)] = strings.Repeat("v", 100)
		}
		_, r := limited.TrySandbox(t, createBody{BaseImage: h.opts.Image, Spec: specOf(withEnv(env))}, "?wait=false")
		r.MustErr(t, apperr.InvalidRequest)
	})
}

func TestSandboxLifecycle(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "lifecycle")

	post := func(t *testing.T, path string, body any) *domain.Sandbox {
		t.Helper()
		return decode[*domain.Sandbox](t, tn.Client.Post(path, body).MustStatus(t, http.StatusOK))
	}

	t.Run("pause_freezes_and_an_exec_transparently_resumes", func(t *testing.T) {
		sb := tn.Sandbox(t)
		eq(t, tn.Out(t, sb, "echo before-pause"), "before-pause", "exec while running")

		eq(t, post(t, "/v1/sandboxes/"+sb.ID+"/pause", nil).State, domain.StatePaused, "state after pause")

		// Transparent resume, DSec §6.3, auto_resume on by default: a frozen
		// workload is an implementation detail of pause, not something the
		// caller has to manage. So an exec against a paused sandbox succeeds
		// AND leaves the sandbox running — the assertion is about the state
		// afterwards, not merely about the exec.
		eq(t, tn.Out(t, sb, "echo implicitly-resumed"), "implicitly-resumed", "exec against a paused sandbox")
		eq(t, tn.Get(t, sb.ID).State, domain.StateRunning, "state after an exec implicitly resumed the sandbox")

		// An explicit pause and resume still work.
		eq(t, post(t, "/v1/sandboxes/"+sb.ID+"/pause", nil).State, domain.StatePaused, "state after a second pause")
		eq(t, post(t, "/v1/sandboxes/"+sb.ID+"/resume", nil).State, domain.StateRunning, "state after explicit resume")
	})

	t.Run("with_auto_resume_off_a_paused_sandbox_refuses_work", func(t *testing.T) {
		// The other posture, and the one an operator picks when a frozen
		// sandbox must stay frozen: refuse with a conflict rather than
		// silently resuming something that was deliberately paused.
		frozen := New(t, Options{Set: map[string]string{"auto_resume": "false"}})
		ft := frozen.Tenant(t, "frozen")
		sb := ft.Sandbox(t)
		decode[*domain.Sandbox](t, ft.Client.Post("/v1/sandboxes/"+sb.ID+"/pause", nil).
			MustStatus(t, http.StatusOK))
		_, r := ft.TryExec(t, sb, execReq{Command: "echo while-paused"})
		r.MustStatus(t, http.StatusConflict).MustErr(t, apperr.Conflict)
		eq(t, ft.Get(t, sb.ID).State, domain.StatePaused, "a refused exec must not have resumed the sandbox")
	})

	t.Run("stop_is_terminal_and_exec_is_refused_with_a_conflict", func(t *testing.T) {
		sb := tn.Sandbox(t)
		eq(t, post(t, "/v1/sandboxes/"+sb.ID+"/stop?force=true", nil).State, domain.StateStopped, "state after stop")

		_, r := tn.TryExec(t, sb, execReq{Command: "echo nope"})
		r.MustStatus(t, http.StatusConflict).MustErr(t, apperr.Conflict)
	})

	t.Run("illegal_transitions_are_conflicts_not_internal_errors", func(t *testing.T) {
		sb := tn.Sandbox(t)
		tn.Client.Post("/v1/sandboxes/"+sb.ID+"/stop?force=true", nil).MustStatus(t, http.StatusOK)
		// stopped → paused is not in the state machine. A user who tries it
		// deserves a 409 and a sentence, not a 500 and a support escalation.
		tn.Client.Post("/v1/sandboxes/"+sb.ID+"/pause", nil).
			MustStatus(t, http.StatusConflict).MustErr(t, apperr.Conflict)
	})

	t.Run("delete_is_204_then_404", func(t *testing.T) {
		sb := tn.Sandbox(t)
		tn.Delete(t, sb.ID)
		tn.Client.Get("/v1/sandboxes/"+sb.ID).MustStatus(t, http.StatusNotFound).MustErr(t, apperr.NotFound)
		// Deleting again is the same governed answer, not an internal error:
		// retrying a delete is normal client behaviour after a timeout.
		tn.Client.Delete("/v1/sandboxes/"+sb.ID+"?force=true").
			MustStatus(t, http.StatusNotFound).MustErr(t, apperr.NotFound)
	})

	t.Run("ttl_extension_moves_the_deadline_and_validates", func(t *testing.T) {
		sb := tn.Sandbox(t, withTimeout(120))
		before := tn.Get(t, sb.ID).ExpiresAt

		extended := post(t, "/v1/sandboxes/"+sb.ID+"/timeout", map[string]any{"timeout_seconds": 600})
		truthy(t, extended.ExpiresAt.After(before), "extending the TTL must move the deadline forward")

		tn.Client.Post("/v1/sandboxes/"+sb.ID+"/timeout", map[string]any{"timeout_seconds": 5}).
			MustErr(t, apperr.InvalidRequest)
	})

	t.Run("an_image_that_is_not_on_the_node_is_refused_up_front", func(t *testing.T) {
		// Two distinct failure paths exist and they must not be confused.
		// This one is decided before any sandbox record or container exists:
		// the operator has not made the image available on this node.
		before := tn.Sandboxes(t, "").Count
		r := tn.TryCreateJSON(t, createBody{BaseImage: "sandboxd-definitely-not-here:latest", Spec: specOf()}, "?wait=true")
		r.MustErr(t, apperr.NotFound)

		out := decode[struct {
			Error   apiError        `json:"error"`
			Sandbox *domain.Sandbox `json:"sandbox"`
		}](t, r)
		contains(t, out.Error.Message, "sandboxd-definitely-not-here", "the refusal must name the image the operator must provide")
		eq(t, tn.Sandboxes(t, "").Count, before, "an up-front refusal must not leave a sandbox record behind")
	})

	t.Run("a_create_whose_process_dies_immediately_is_reported_as_failed", func(t *testing.T) {
		// The honest version of "a create that cannot be realized": the image
		// really is present on the node and the container really is created,
		// but PID 1 exits at once (exit 127 — the image has no `sleep`). The
		// caller must not be told the sandbox is running, and the daemon must
		// not leave the corpse behind for the operator to clean up.
		before := tn.Sandboxes(t, "?state=running").Count
		r := tn.TryCreateJSON(t, createBody{BaseImage: brokenImage, Spec: specOf()}, "?wait=true")
		r.MustErr(t, apperr.Conflict)

		out := decode[struct {
			Error   apiError        `json:"error"`
			Sandbox *domain.Sandbox `json:"sandbox"`
		}](t, r)
		truthy(t, out.Sandbox != nil, "a create that failed after admission must still report the sandbox it failed to build")
		if out.Sandbox == nil {
			return
		}
		eq(t, out.Sandbox.State, domain.StateFailed, "state of a sandbox whose process died on start")
		neq(t, out.Sandbox.Reason, "", "a failure must carry a reason an operator can act on")
		contains(t, out.Error.Message, out.Sandbox.Reason,
			"the human-readable message must embed the machine-readable reason, or the two facts can drift apart")
		contains(t, out.Sandbox.Reason, "127",
			"the reason should name the exit code; 127 is the signal that the image is missing a binary")

		// The failed sandbox must not be running, and must not have left a
		// backend container behind: a leak here costs the operator disk.
		eq(t, tn.Sandboxes(t, "?state=running").Count, before, "running count after a failed create")
		eq(t, len(containerIDsMatching(t, "--filter", "label="+labelSandbox+"="+out.Sandbox.ID)), 0,
			"a failed create must reap the backend container it started")
	})
}

func TestFilesAndSnapshots(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "artifacts")

	t.Run("file_roundtrip_is_byte_exact", func(t *testing.T) {
		sb := tn.Sandbox(t)
		payload := []byte("binary\x00payload\xff\xfe\nsecond line\n")
		tn.PutFile(t, sb, "/root/blob.bin", payload)
		eq(t, string(tn.GetFile(t, sb, "/root/blob.bin")), string(payload), "downloaded bytes")
		eq(t, tn.Out(t, sb, "wc -c < /root/blob.bin"), strconv.Itoa(len(payload)), "size seen inside the sandbox")
	})

	t.Run("paths_must_be_absolute_and_traversal_free", func(t *testing.T) {
		sb := tn.Sandbox(t)
		for _, bad := range []string{"relative.txt", "/safe/../../etc/escape", "/root/../etc/hosts"} {
			tn.TryPutFile(t, sb, bad, []byte("x")).MustErr(t, apperr.InvalidRequest)
		}
	})

	t.Run("a_missing_file_is_a_404", func(t *testing.T) {
		sb := tn.Sandbox(t)
		tn.Client.Get("/v1/sandboxes/"+sb.ID+"/files?path=/root/never-existed").
			MustStatus(t, http.StatusNotFound).MustErr(t, apperr.NotFound)
	})

	t.Run("directories_can_be_listed", func(t *testing.T) {
		sb := tn.Sandbox(t)
		tn.PutFile(t, sb, "/root/listing/one.txt", []byte("1"))
		page := decode[filePage](t, tn.Client.Get("/v1/sandboxes/"+sb.ID+"/files/list?path=/root/listing").
			MustStatus(t, http.StatusOK))
		found := false
		for _, f := range page.Files {
			if f.Name == "one.txt" {
				found = true
			}
		}
		truthy(t, found, "listed directory must contain the file that was just written")
	})

	t.Run("uploads_over_the_limit_are_a_governed_refusal", func(t *testing.T) {
		tight := New(t, Options{Set: map[string]string{"max_file_bytes": "1024"}})
		tt := tight.Tenant(t, "tight")
		sb := tt.Sandbox(t)
		r := tt.TryPutFile(t, sb, "/root/big", bytes.Repeat([]byte("x"), 8192))
		r.MustStatus(t, http.StatusBadRequest, http.StatusRequestEntityTooLarge)
		e := r.Err()
		truthy(t, e != nil && e.Code == apperr.InvalidRequest, "an oversized upload must be a governed refusal")
	})

	t.Run("a_snapshot_captures_state_that_a_new_sandbox_inherits", func(t *testing.T) {
		sb := tn.Sandbox(t)
		tn.Out(t, sb, "echo carried-forward > /root/state.txt && mkdir -p /root/dir && echo nested > /root/dir/n.txt")

		snap := tn.Snapshot(t, sb, "carry")
		eq(t, snap.SandboxID, sb.ID, "snapshot sandbox id")

		listed := decode[snapshotPage](t, tn.Client.Get("/v1/snapshots").MustStatus(t, http.StatusOK))
		found := false
		for _, s := range listed.Snapshots {
			if s.ID == snap.ID {
				found = true
			}
		}
		truthy(t, found, "the new snapshot must appear in the tenant's snapshot list")

		child, r := tn.TrySandbox(t, createBody{SnapshotID: snap.ID, Spec: specOf()})
		if r.Status != http.StatusCreated {
			t.Fatalf("create from snapshot: %s\nbody: %s", r.describe(), r.snippet())
		}
		eq(t, tn.Out(t, child, "cat /root/state.txt"), "carried-forward", "file restored from the snapshot")
		eq(t, tn.Out(t, child, "cat /root/dir/n.txt"), "nested", "nested file restored from the snapshot")

		// Clean up the image this scenario committed. The fixture used to
		// leave it behind on every run, which is the same leak the product
		// had: a snapshot image carries a tag, so it is not dangling, so
		// `docker image prune` never removes it.
		tn.Delete(t, child.ID)
		tn.Client.Delete("/v1/snapshots/"+snap.ID).MustStatus(t, http.StatusNoContent)
	})
}

func TestAuditTrail(t *testing.T) {
	h := New(t, Options{})
	alice := h.Tenant(t, "alice")
	bob := h.Tenant(t, "bob")

	sb := alice.Sandbox(t)
	alice.Out(t, sb, "echo audited")
	alice.Delete(t, sb.ID)
	bob.Sandbox(t)

	t.Run("every_mutation_leaves_a_tenant_scoped_trail", func(t *testing.T) {
		events := alice.Events(t, "?limit=500")
		truthy(t, len(events) >= 3, "alice should have create, exec and delete events")
		for _, e := range events {
			eq(t, e.TenantID, alice.ID, "an event leaked into alice's stream")
		}
	})

	t.Run("events_can_be_filtered_by_action", func(t *testing.T) {
		for _, e := range alice.Events(t, "?action=reconcile.fix&limit=5") {
			eq(t, e.Action, "reconcile.fix", "action filter returned a different action")
		}
	})

	t.Run("a_tenants_stream_never_contains_another_tenants_events", func(t *testing.T) {
		for _, e := range bob.Events(t, "?limit=500") {
			eq(t, e.TenantID, bob.ID, "an event leaked into bob's stream")
		}
	})
}

func TestTemplates(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "templates")

	t.Run("a_base_template_is_bootstrapped", func(t *testing.T) {
		page := decode[templatePage](t, tn.Client.Get("/v1/templates").MustStatus(t, http.StatusOK))
		truthy(t, len(page.Templates) >= 1, "no template was seeded from bootstrap.image")
	})

	t.Run("a_malformed_template_id_is_refused_as_client_input", func(t *testing.T) {
		r := tn.TryCreateJSON(t, createBody{TemplateID: "tpl_does-not-exist", Spec: specOf()}, "?wait=true")
		r.MustStatus(t, http.StatusBadRequest).MustErr(t, apperr.InvalidRequest)
		// It used to be a 500 marked retryable. A request that can never
		// succeed must not tell the client to send it again.
		eq(t, r.Err().Retryable, false, "retryable flag on a malformed template id")
	})

	t.Run("an_unknown_but_well_formed_template_id_is_a_404", func(t *testing.T) {
		page := decode[templatePage](t, tn.Client.Get("/v1/templates").MustStatus(t, http.StatusOK))
		truthy(t, len(page.Templates) >= 1, "no template was seeded from bootstrap.image")
		// Same shape, different value. Collapsing this into the 400 above
		// would make "that template was deleted" indistinguishable from
		// "you typed it wrong", and clients act differently on the two.
		unknown := wellFormedUnknownID(page.Templates[0].ID)
		_, r := tn.TrySandbox(t, createBody{TemplateID: unknown, Spec: specOf()})
		r.MustStatus(t, http.StatusNotFound).MustErr(t, apperr.NotFound)
	})
}
