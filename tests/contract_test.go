package tests

// The public API contract. Clients are documented to branch on the error
// Code, never on the status or the message, so every refusal asserted here
// checks the code — and MustErr additionally checks that the code travelled
// on the transport its own mapping table says it should.

import (
	"net/http"
	"strings"
	"testing"

	"sandcastle.dev/sandcastle/internal/apperr"
)

func TestAuthenticationAndScope(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "auth-a")
	other := h.Tenant(t, "auth-b")
	sb := tn.Sandbox(t)

	t.Run("credential_rejections_are_all_401", func(t *testing.T) {
		noAuth := newClient(h.Base, "")
		noAuth.Get("/v1/sandboxes").MustStatus(t, http.StatusUnauthorized).MustErr(t, apperr.AuthFailed)

		newClient(h.Base, "sk_forged-value-that-was-never-minted").Get("/v1/sandboxes").
			MustStatus(t, http.StatusUnauthorized).MustErr(t, apperr.AuthFailed)

		// A tampered key that keeps a real key's prefix: if lookup ever
		// matched on the prefix, this would authenticate as `tn`.
		prefix := tn.Key[:16]
		newClient(h.Base, prefix+"AAAAAAAAAAAAAAAAAAAAAAAAAAAA").Get("/v1/sandboxes").
			MustStatus(t, http.StatusUnauthorized).MustErr(t, apperr.AuthFailed)
	})

	t.Run("revocation_is_immediate_and_per_key", func(t *testing.T) {
		burner := h.MintKey(t, tn.ID, "burner")
		keys := decode[keyPage](t, adminCall(t, h.Admin, "GET", "/v1/tenants/"+tn.ID+"/keys", nil).
			MustStatus(t, http.StatusOK))
		var burnerID string
		for _, k := range keys.Keys {
			if k.Name == "burner" {
				burnerID = k.ID
			}
		}
		truthy(t, burnerID != "", "the newly minted key must appear in the key listing")

		adminCall(t, h.Admin, "DELETE", "/v1/tenants/"+tn.ID+"/keys/"+burnerID, nil).
			MustStatus(t, http.StatusNoContent)
		newClient(h.Base, burner).Get("/v1/sandboxes").
			MustStatus(t, http.StatusUnauthorized).MustErr(t, apperr.AuthFailed)

		// Revoking one key must not disturb the tenant's other credential.
		tn.Client.Get("/v1/sandboxes").MustStatus(t, http.StatusOK)
	})

	t.Run("key_plaintext_is_never_readable_after_minting", func(t *testing.T) {
		keys := decode[keyPage](t, adminCall(t, h.Admin, "GET", "/v1/tenants/"+tn.ID+"/keys", nil).
			MustStatus(t, http.StatusOK))
		for _, k := range keys.Keys {
			notContains(t, strings.ToLower(k.Prefix+" "+k.Name+" "+k.Scope), "secret", "key listing field")
		}
		// The listing must not contain the plaintext of any live credential.
		raw := adminCall(t, h.Admin, "GET", "/v1/tenants/"+tn.ID+"/keys", nil)
		notContains(t, string(raw.Raw), tn.Key, "key listing body")
	})

	t.Run("tenant_keys_cannot_reach_admin_surfaces", func(t *testing.T) {
		for _, path := range []string{"/v1/tenants", "/metrics", "/v1/isolation"} {
			tn.Client.Get(path).MustStatus(t, http.StatusForbidden).MustErr(t, apperr.Forbidden)
		}
		tn.Client.Post("/v1/tenants", map[string]any{"name": "escalated"}).
			MustStatus(t, http.StatusForbidden).MustErr(t, apperr.Forbidden)
	})

	t.Run("an_admin_key_must_name_the_tenant_it_acts_on", func(t *testing.T) {
		// An admin acting on tenant resources must say which tenant. Silently
		// picking one is how a support action becomes a cross-tenant write.
		h.Admin.Post("/v1/sandboxes?wait=false", createBody{BaseImage: h.opts.Image, Spec: specOf()}).
			MustStatus(t, http.StatusForbidden).MustErr(t, apperr.Forbidden)

		h.Admin.WithHeader("X-Tenant-Id", tn.ID).
			Post("/v1/sandboxes?wait=false", createBody{BaseImage: h.opts.Image, Spec: specOf()}).
			MustStatus(t, http.StatusCreated)
	})

	t.Run("cross_tenant_access_is_forbidden_on_every_resource", func(t *testing.T) {
		// One sandbox, five ways in. Any of these returning 200 would be a
		// cross-tenant read of a customer's workload.
		eve := other.Client
		eve.Get("/v1/sandboxes/"+sb.ID).MustStatus(t, http.StatusForbidden).MustErr(t, apperr.Forbidden)
		eve.Post("/v1/sandboxes/"+sb.ID+"/exec", execReq{Command: "id"}).
			MustStatus(t, http.StatusForbidden).MustErr(t, apperr.Forbidden)
		eve.Delete("/v1/sandboxes/"+sb.ID+"?force=true").
			MustStatus(t, http.StatusForbidden).MustErr(t, apperr.Forbidden)
		eve.Get("/v1/sandboxes/"+sb.ID+"/files?path=/etc/hostname").
			MustStatus(t, http.StatusForbidden).MustErr(t, apperr.Forbidden)
		eve.Put("/v1/sandboxes/"+sb.ID+"/files?path=/root/pwned", []byte("x")).
			MustStatus(t, http.StatusForbidden).MustErr(t, apperr.Forbidden)
	})

	t.Run("listings_are_scoped_to_the_caller", func(t *testing.T) {
		eq(t, other.Sandboxes(t, "").Count, 0, "another tenant's sandbox list")
		truthy(t, tn.Sandboxes(t, "").Count >= 1, "the owner's sandbox list")
	})
}

func TestHTTPContract(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "contract")

	t.Run("unknown_routes_are_404", func(t *testing.T) {
		tn.Client.Get("/v1/nonexistent").MustStatus(t, http.StatusNotFound)
		tn.Client.Get("/v1/sandboxes/sbx_nope/teleport").MustStatus(t, http.StatusNotFound)
	})

	t.Run("wrong_methods_are_405", func(t *testing.T) {
		// A method mismatch must not fall through to "not found", and must
		// certainly not be handled by the wrong verb's logic.
		tn.Client.Patch("/v1/sandboxes", map[string]any{}).MustStatus(t, http.StatusMethodNotAllowed)
		tn.Client.Post("/v1/sandboxes/sbx_any", map[string]any{}).MustStatus(t, http.StatusMethodNotAllowed)
	})

	t.Run("unknown_json_fields_are_refused", func(t *testing.T) {
		// Accepting unknown fields silently is how a client ships a typo'd
		// field for a year and never notices the isolation it asked for was
		// never applied.
		tn.TryCreateJSON(t, map[string]any{
			"base_image": h.opts.Image,
			"spec":       specOf(),
			"priviliged": true,
		}, "?wait=false").MustStatus(t, http.StatusBadRequest).MustErr(t, apperr.InvalidRequest)
	})

	t.Run("malformed_json_is_a_governed_400", func(t *testing.T) {
		tn.TryCreateJSON(t, "not json at all", "?wait=false").
			MustStatus(t, http.StatusBadRequest).MustErr(t, apperr.InvalidRequest)
	})

	t.Run("an_empty_exec_body_is_refused", func(t *testing.T) {
		sb := tn.Sandbox(t)
		_, r := tn.TryExec(t, sb, execReq{})
		r.MustStatus(t, http.StatusBadRequest).MustErr(t, apperr.InvalidRequest)
	})

	t.Run("an_empty_argv_is_refused", func(t *testing.T) {
		sb := tn.Sandbox(t)
		_, r := tn.TryExec(t, sb, execReq{Argv: []string{}})
		r.MustStatus(t, http.StatusBadRequest).MustErr(t, apperr.InvalidRequest)
	})

	t.Run("request_ids_are_echoed_or_generated", func(t *testing.T) {
		r := tn.Client.WithHeader("X-Request-Id", "caller-supplied-id").Get("/v1/sandboxes")
		r.MustStatus(t, http.StatusOK)
		eq(t, r.Header.Get("X-Request-Id"), "caller-supplied-id", "echoed request id")

		// An oversized or attacker-shaped id is replaced rather than trusted:
		// it is echoed into logs and error bodies.
		long := strings.Repeat("A", 200)
		r2 := tn.Client.WithHeader("X-Request-Id", long).Get("/v1/sandboxes")
		r2.MustStatus(t, http.StatusOK)
		neq(t, r2.Header.Get("X-Request-Id"), long, "request id echo of an oversized value")
		eq(t, len(r2.Header.Get("X-Request-Id")), 16, "generated request id length")
	})

	t.Run("a_malformed_id_is_refused_as_client_input", func(t *testing.T) {
		// Found by this suite. A malformed id used to surface as a 500 marked
		// *retryable*, so a client with a typo was told to try again —
		// forever, against a request that can never succeed. Obscure
		// characters here are deliberate: O is not in Crockford base32.
		for _, bad := range []string{
			"sbx_missing",
			"sbx_" + strings.Repeat("O", 26),
			"sbx_" + strings.Repeat("0", 25),
			"not_the_right_prefix",
		} {
			r := tn.Client.Get("/v1/sandboxes/" + bad)
			r.MustStatus(t, http.StatusBadRequest).MustErr(t, apperr.InvalidRequest)
			eq(t, r.Err().Retryable, false, "retryable flag on malformed id "+bad)
		}
	})

	t.Run("an_unknown_but_well_formed_id_is_a_404", func(t *testing.T) {
		sb := tn.Sandbox(t)
		tn.Client.Get("/v1/sandboxes/"+wellFormedUnknownID(sb.ID)).
			MustStatus(t, http.StatusNotFound).MustErr(t, apperr.NotFound)
	})

	t.Run("errors_carry_the_code_clients_branch_on", func(t *testing.T) {
		sb := tn.Sandbox(t)
		tn.Delete(t, sb.ID)
		r := tn.Client.Get("/v1/sandboxes/" + sb.ID)
		r.MustStatus(t, http.StatusNotFound).MustErr(t, apperr.NotFound)
		e := r.Err()
		eq(t, e.Code, apperr.NotFound, "error code")
		truthy(t, e.Message != "", "an error must explain itself")
		truthy(t, e.RequestID != "", "an error must carry the request id so support can find the log line")
	})

	t.Run("health_and_readiness_are_unauthenticated_but_honest", func(t *testing.T) {
		anon := newClient(h.Base, "")
		anon.Get("/healthz").MustStatus(t, http.StatusOK)
		info := decode[readyInfo](t, anon.Get("/readyz").MustStatus(t, http.StatusOK))
		eq(t, info.Status, "ready", "readiness status")
		neq(t, info.Isolation, "", "readiness must state the isolation tier")
		// The tier is a claim; verified is the evidence. Both are exposed so a
		// customer can tell them apart without holding a key.
		truthy(t, info.IsolationVerified, "isolation_verified on the unauthenticated readiness probe")
	})

	t.Run("the_isolation_inventory_stays_admin_only", func(t *testing.T) {
		// It enumerates which controls are actually in force, which is
		// reconnaissance for anyone planning an escape.
		newClient(h.Base, "").Get("/v1/isolation").
			MustStatus(t, http.StatusUnauthorized).MustErr(t, apperr.AuthFailed)
		h.Admin.Get("/v1/isolation").MustStatus(t, http.StatusOK)
	})
}

func TestUnauthenticatedFloodGate(t *testing.T) {
	// The v1.1.0 defect this guards: the rate limiter lived inside
	// authenticate, so a request that failed authentication was never limited
	// while still costing one audit write and one fsync. Four hundred bad
	// requests produced four hundred fsyncs and zero 429s — an I/O amplifier
	// that needed no credentials at all.
	h := New(t, Options{Set: map[string]string{"preauth.per_second": "5", "preauth.burst": "5"}})
	anon := newClient(h.Base, "")

	counts := map[int]int{}
	for i := 0; i < 40; i++ {
		counts[anon.Get("/v1/sandboxes").Status]++
	}

	truthy(t, counts[http.StatusUnauthorized] > 0, "unauthenticated requests must still be rejected")
	truthy(t, counts[http.StatusTooManyRequests] > 0,
		"an unauthenticated flood must be limited before authentication, not only after it")
	for status, n := range counts {
		if status >= 500 {
			t.Fatalf("flood produced %d responses with status %d", n, status)
		}
	}
}

func TestSecretsNeverReachTheLog(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "secrets")

	// Drive traffic that legitimately logs, including a rejection, then read
	// the daemon's own output. A credential that lands in a log file is a
	// credential in every log shipper downstream.
	tn.Client.Get("/v1/sandboxes").MustStatus(t, http.StatusOK)
	newClient(h.Base, "").Get("/v1/sandboxes").MustStatus(t, http.StatusUnauthorized)

	notContains(t, h.Log(), tn.Key, "the daemon log")
	notContains(t, h.Log(), h.AdminKey, "the daemon log")
}
