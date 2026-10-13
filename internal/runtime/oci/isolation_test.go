package oci

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"sandcastle.dev/sandcastle/internal/runtime"
)

// ---- payload pinning (no daemon required) ----------------------------------

// TestCreatePayloadPinsEveryIsolationField guards the security contract at the
// level where it is actually written. A deleted field here does not fail any
// other test — the sandbox still starts, it is simply weaker — so the payload
// itself needs a test rather than only an end-to-end one.
func TestCreatePayloadPinsEveryIsolationField(t *testing.T) {
	b := NewWithOptions(nil, Options{})
	hc := b.createPayload(runtime.CreateRequest{
		Name: "sbx", Image: "img", CPUMillicores: 500, MemoryMiB: 256,
	}).HostConfig

	eq := func(name, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	eq("NetworkMode", hc.NetworkMode, "none")
	eq("IpcMode", hc.IpcMode, "private")
	eq("PidMode", hc.PidMode, "")
	eq("CgroupnsMode", hc.CgroupnsMode, "private")
	eq("CapDrop", strings.Join(hc.CapDrop, ","), "ALL")

	if hc.CapAdd == nil {
		t.Error("CapAdd must be an explicit empty list, not nil: an unset field is one the engine may fill in")
	}
	if len(hc.CapAdd) != 0 {
		t.Errorf("CapAdd = %v, want empty", hc.CapAdd)
	}
	if hc.Privileged == nil || *hc.Privileged {
		t.Error("Privileged must be explicitly false")
	}
	if hc.ReadonlyRootfs == nil || *hc.ReadonlyRootfs {
		t.Error("ReadonlyRootfs must be explicitly false: Commit() captures the writable layer, so a read-only root would produce empty snapshots")
	}
	if !strings.Contains(strings.Join(hc.SecurityOpt, ","), "no-new-privileges") {
		t.Errorf("SecurityOpt = %v, want no-new-privileges", hc.SecurityOpt)
	}
	if !containsStr(hc.MaskedPaths, "/proc/kcore") {
		t.Errorf("MaskedPaths = %v, want /proc/kcore masked", hc.MaskedPaths)
	}
	if !containsStr(hc.ReadonlyPaths, "/proc/sysrq-trigger") {
		t.Errorf("ReadonlyPaths = %v, want /proc/sysrq-trigger", hc.ReadonlyPaths)
	}

	// core=0 is the one ulimit that is a security control rather than a
	// resource control: a core dump writes process memory into a filesystem
	// this product snapshots.
	var sawCore bool
	for _, u := range hc.Ulimits {
		if u.Name == "core" {
			sawCore = true
			if u.Soft != 0 || u.Hard != 0 {
				t.Errorf("core ulimit = %d/%d, want 0/0", u.Soft, u.Hard)
			}
		}
	}
	if !sawCore {
		t.Error("no core ulimit pinned: a sandbox can dump its memory into a snapshotted filesystem")
	}

	// swap off, so an OOM hits one sandbox visibly instead of the host quietly
	if hc.Resources.MemorySwap != hc.Resources.Memory {
		t.Errorf("MemorySwap = %d, Memory = %d; swap must be disabled", hc.Resources.MemorySwap, hc.Resources.Memory)
	}
	if hc.Resources.PidsLimit == nil {
		t.Error("PidsLimit must be set: a fork bomb in one sandbox would otherwise exhaust host PIDs")
	}

	// No mounts by default: the "no host directory is mounted" posture is the
	// contract's default, and an empty request must not inject one.
	if hc.Mounts != nil {
		t.Errorf("Mounts = %v on an empty create; want none so the no-host-mount canary stays true by default", hc.Mounts)
	}
}

// TestMountsAreAlwaysReadOnly pins the one property a mount must never lose.
// The runtime Mount type has no writable mode, and the backend translates it
// to a bind mount with ReadOnly=true and rprivate propagation. A regression
// that dropped either would silently turn "read the project" into "write the
// host", which is the whole reason this path exists as read-only only.
func TestMountsAreAlwaysReadOnly(t *testing.T) {
	b := NewWithOptions(nil, Options{})
	p := b.createPayload(runtime.CreateRequest{
		Name:   "sbx",
		Image:  "img",
		Mounts: []runtime.Mount{{Source: "/srv/proj", Target: "/workspace"}},
	})
	if len(p.HostConfig.Mounts) != 1 {
		t.Fatalf("Mounts = %v, want exactly one", p.HostConfig.Mounts)
	}
	m := p.HostConfig.Mounts[0]
	if m.Type != "bind" {
		t.Errorf("Type = %q, want bind", m.Type)
	}
	if m.Source != "/srv/proj" || m.Target != "/workspace" {
		t.Errorf("Source/Target = %q/%q, want /srv/proj//workspace", m.Source, m.Target)
	}
	if !m.ReadOnly {
		t.Error("ReadOnly = false; the only mount this product offers is read-only and this is never allowed to flip")
	}
	if m.BindOptions == nil || m.BindOptions.Propagation != "rprivate" {
		t.Errorf("BindOptions = %+v, want rprivate so a sandbox-side mount cannot propagate to the host", m.BindOptions)
	}
}

// TestMountsSurviveJSONEncoding applies the wire-level check to the mount:
// the Go value can be correct and the encoded payload still drop ReadOnly.
func TestMountsSurviveJSONEncoding(t *testing.T) {
	b := NewWithOptions(nil, Options{})
	raw, err := json.Marshal(b.createPayload(runtime.CreateRequest{
		Name:   "sbx",
		Image:  "img",
		Mounts: []runtime.Mount{{Source: "/srv/proj", Target: "/workspace"}},
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"Type":"bind"`,
		`"ReadOnly":true`,
		`"Propagation":"rprivate"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("mount payload missing %s\npayload: %s", want, raw)
		}
	}
	// The empty case must leave Mounts absent from the wire, not "null": an
	// explicit empty array is fine, but "null" is what an older engine might
	// read as "please choose", which is exactly what this contract avoids.
	empty, _ := json.Marshal(b.createPayload(runtime.CreateRequest{Name: "sbx", Image: "img"}))
	if strings.Contains(string(empty), `"Mounts"`) {
		t.Errorf("empty create emitted a Mounts field; it should be omitted\npayload: %s", empty)
	}
}

// TestPinnedFieldsSurviveJSONEncoding is the test that would have caught the
// original defect class: a struct tag that drops a field at the boundary. The
// Go value can be correct and the wire payload still weaker, so the assertion
// has to run on the encoded bytes.
func TestPinnedFieldsSurviveJSONEncoding(t *testing.T) {
	b := NewWithOptions(nil, Options{})
	raw, err := json.Marshal(b.createPayload(runtime.CreateRequest{
		Name: "sbx", Image: "img", CPUMillicores: 500, MemoryMiB: 256,
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"NetworkMode":"none"`,
		`"IpcMode":"private"`,
		`"PidMode":""`,
		`"CgroupnsMode":"private"`,
		`"Privileged":false`,
		`"ReadonlyRootfs":false`,
		`"CapDrop":["ALL"]`,
		`"CapAdd":[]`,
		`"UsernsMode":"host"`,
		`{"Name":"core","Soft":0,"Hard":0}`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("create payload is missing %s\nthe Go value may be right while the wire payload is weaker\npayload: %s",
				want, raw)
		}
	}
}

// TestUsernsModeIsNeverLeftToTheDaemon pins a decision that is easy to undo by
// accident. Omitting UsernsMode is not neutral: an engine with userns-remap
// configured defaults new containers to a private user namespace, so an unset
// field silently means "private" on some hosts and "host" on others — and
// userns=off could not actually turn it off.
func TestUsernsModeIsNeverLeftToTheDaemon(t *testing.T) {
	for _, opts := range []Options{{}, {Userns: UsernsAuto}, {Userns: UsernsOff}, {Userns: UsernsRequire}} {
		mode := NewWithOptions(nil, opts).createPayload(runtime.CreateRequest{}).HostConfig.UsernsMode
		if mode != UsernsModeHost && mode != UsernsModePrivate {
			t.Errorf("Options{Userns: %q} produced UsernsMode %q; it must be %q or %q, never empty",
				opts.Userns, mode, UsernsModeHost, UsernsModePrivate)
		}
	}
	// A backend that has not been probed must not claim a user namespace.
	if got := NewWithOptions(nil, Options{Userns: UsernsAuto}).createPayload(runtime.CreateRequest{}).HostConfig.UsernsMode; got != UsernsModeHost {
		t.Errorf("unprobed backend asked for UsernsMode %q; it must not claim a namespace before Init establishes one", got)
	}
}

// ---- the silent-failure trap -----------------------------------------------

// TestIdentityUIDMapIsNotMistakenForRemapping pins the measurement this whole
// mechanism was built around. On Docker 29.5.2 with no userns-remap, a
// container created with HostConfig.UsernsMode="private" reports:
//
//	GET /containers/<id>/json  → HostConfig.UsernsMode = "private"
//	/proc/self/uid_map         → 0 0 4294967295
//
// The engine accepted the request and echoed it back as applied. Nothing was
// applied. Any check that reads configuration — even configuration read back
// from the engine — passes; only the uid map tells the truth, so the uid map
// is what gets asserted.
func TestIdentityUIDMapIsNotMistakenForRemapping(t *testing.T) {
	cases := []struct {
		in   string
		want bool
		why  string
	}{
		{"0 0 4294967295;", false, "identity map: the sandbox has no user namespace"},
		{"0 0 4294967295", false, "identity map without the probe's trailing separator"},
		// The literal bytes the kernel writes: columns are space-padded for
		// alignment. A comparison that does not collapse whitespace reads this
		// as "remapped" and reports a user namespace that does not exist.
		{"         0          0 4294967295;", false, "identity map as /proc actually pads it"},
		{"0 100000 65536;", true, "uid 0 inside maps to unprivileged 100000 outside"},
		{"0 100000 65536;1 200000 1;", true, "multi-line remapped map"},
		{"", false, "unmeasurable must not read as remapped"},
		{";;", false, "unmeasurable must not read as remapped"},
	}
	for _, c := range cases {
		if got := uidMapIsRemapped(c.in); got != c.want {
			t.Errorf("uidMapIsRemapped(%q) = %v, want %v (%s)", c.in, got, c.want, c.why)
		}
	}
}

// TestMissingProbeKeyIsDetected covers the failure this check itself had: the
// parser looked up seven keys the probe never emitted, so those facts compared
// "" against their expectation. One of them passed. A checker that cannot
// measure has to say so, in both directions.
func TestMissingProbeKeyIsDetected(t *testing.T) {
	full := map[string]string{}
	for _, k := range probeKeys {
		full[k] = "x"
	}
	if missing := missingProbeKeys(full); len(missing) != 0 {
		t.Fatalf("complete observation reported missing keys: %v", missing)
	}
	delete(full, "no_new_privs")
	delete(full, "uid_map")
	missing := missingProbeKeys(full)
	if len(missing) != 2 {
		t.Fatalf("missing = %v, want exactly [no_new_privs uid_map]", missing)
	}
	for _, k := range []string{"no_new_privs", "uid_map"} {
		if !containsStr(missing, k) {
			t.Errorf("%q should have been reported missing; got %v", k, missing)
		}
	}
}

func TestAllZeroHexRejectsUnmeasurableAndNonZero(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"0000000000000000", true},
		{"000001ffffffffff", false},
		{"", false},
		{"00", true},
	}
	for _, c := range cases {
		if got := allZeroHex(c.in); got != c.want {
			t.Errorf("allZeroHex(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestProbeContainerIsNotRegisteredAsASandbox checks the one property that
// keeps the self-check from colliding with crash recovery: the reconciler
// treats a product-labeled object with no sandbox.id as unattributable and
// leaves it alone, whereas one *with* sandbox.id and no store record gets
// reaped as crash residue.
func TestProbeContainerIsNotRegisteredAsASandbox(t *testing.T) {
	b := NewWithOptions(nil, Options{})
	p := b.createPayload(runtime.CreateRequest{
		Name: "sandboxd-isolation-check-1", Image: "img",
		Labels: map[string]string{"sandboxd.check": "isolation"},
	})
	if _, ok := p.Labels["sandbox.id"]; ok {
		t.Error("the self-check container carries sandbox.id; the reconciler would reap it as crash residue")
	}
	if p.Labels["product"] != prodValue {
		t.Errorf("product label = %q, want %q: the container must still be attributable as ours",
			p.Labels["product"], prodValue)
	}
}

// ---- daemon-backed ---------------------------------------------------------

// TestIsolationSelfCheckPassesOnThisHost is the automated form of the manual
// /proc inspection that started this work. It asserts what the kernel reports
// from inside a sandbox built by the production payload path.
func TestIsolationSelfCheckPassesOnThisHost(t *testing.T) {
	b := needDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := b.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rep, err := b.VerifyIsolation(ctx, testBaseImage)
	if err != nil {
		t.Fatalf("VerifyIsolation: %v", err)
	}
	for _, f := range rep.Facts {
		t.Logf("%-14s observed=%-20q want=%-24s ok=%v", f.Name, f.Observed, f.Want, f.OK)
	}
	if !rep.OK() {
		t.Fatalf("isolation self-check failed: %s", strings.Join(rep.Failures, "; "))
	}
	if rep.Tier != runtime.TierContainer {
		t.Errorf("tier = %q, want %q", rep.Tier, runtime.TierContainer)
	}

	// The facts that motivated this work, asserted individually so a failure
	// names the control rather than reporting a generic mismatch.
	want := map[string]string{
		"seccomp":        "2",
		"no_new_privs":   "1",
		"core_dump":      "0",
		"can_mount":      "no",
		"can_read_kcore": "no",
	}
	for name, wantVal := range want {
		f, ok := fact(rep, name)
		if !ok {
			t.Errorf("report has no %q fact", name)
			continue
		}
		if f.Observed != wantVal {
			t.Errorf("%s observed %q, want %q", name, f.Observed, wantVal)
		}
	}
	if f, ok := fact(rep, "cap_effective"); !ok || !allZeroHex(f.Observed) {
		t.Errorf("cap_effective = %q, want an all-zero capability set", f.Observed)
	}
}

// TestSelfCheckLeavesNothingBehind: a check that leaks a container per startup
// would fill the node with junk over a few restarts.
func TestSelfCheckLeavesNothingBehind(t *testing.T) {
	b := needDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	before, err := b.Orphans(ctx)
	if err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	if _, err := b.VerifyIsolation(ctx, testBaseImage); err != nil {
		t.Fatalf("VerifyIsolation: %v", err)
	}
	// Orphans() filters by the product label, which the probe carries, so a
	// leaked probe shows up here.
	after, err := b.Orphans(ctx)
	if err != nil {
		t.Fatalf("Orphans after: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("self-check changed the orphan count from %d to %d; it must clean up after itself",
			len(before), len(after))
	}
	for _, o := range after {
		if strings.HasPrefix(o.Ref, "sandboxd-isolation-check-") {
			t.Errorf("self-check container %q survived the check", o.Ref)
		}
	}
}

// TestUnrunnableProbeIsAnErrorNotAPass: "could not measure" must never be
// reported as "measured and clean". A missing image is the most likely cause
// of an unrunnable probe in production.
func TestUnrunnableProbeIsAnErrorNotAPass(t *testing.T) {
	b := needDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	rep, err := b.VerifyIsolation(ctx, "sandboxd-does-not-exist:never")
	if err == nil {
		t.Fatalf("VerifyIsolation returned a report for an image that cannot be run: %+v", rep)
	}
	if rep != nil {
		t.Errorf("a failed check must not also return a report; got %+v", rep)
	}
}

// TestUsernsRequireFailsClosedWhenTheDaemonCannotProvideIt is the end-to-end
// form of the gate. On a host that does have userns-remap the assertion
// inverts, so the test is meaningful in both environments.
func TestUsernsRequireFailsClosedWhenTheDaemonCannotProvideIt(t *testing.T) {
	b := needDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	info, err := b.c.info(ctx)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	strict := NewWithOptions(b.c, Options{Userns: UsernsRequire})
	err = strict.Init(ctx)

	if info.hasUserns() {
		if err != nil {
			t.Fatalf("this daemon reports userns support, so userns=require should be satisfiable: %v", err)
		}
		if mode, _ := strict.UsernsPosture(); mode != "private" {
			t.Errorf("posture = %q, want private", mode)
		}
		return
	}
	if err == nil {
		t.Fatal("userns=require was accepted on a daemon without userns-remap; the setting is meant to be impossible to satisfy silently")
	}
	// The message has to be actionable, not merely a refusal.
	for _, want := range []string{"userns-remap", "daemon.json", "userns=off"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message should mention %q so an operator can act on it; got: %v", want, err)
		}
	}
}

// TestAutoDisclosesTheDowngradeInsteadOfClaimingIt: with userns unavailable,
// the auto posture must run in host mode AND say so in its note, because a
// disclosed downgrade is the whole point of the three-way switch.
func TestAutoDisclosesTheDowngradeInsteadOfClaimingIt(t *testing.T) {
	b := needDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	info, err := b.c.info(ctx)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	auto := NewWithOptions(b.c, Options{Userns: UsernsAuto})
	if err := auto.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	mode, note := auto.UsernsPosture()
	if info.hasUserns() {
		if mode != "private" {
			t.Errorf("posture = %q, want private when the daemon supports it", mode)
		}
		return
	}
	if mode != "off" {
		t.Errorf("posture = %q, want off when the daemon has no remap", mode)
	}
	if !strings.Contains(note, "host") || !strings.Contains(note, "userns=require") {
		t.Errorf("the auto downgrade must be disclosed and name the way to refuse it; got %q", note)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func fact(r *runtime.IsolationReport, name string) (runtime.IsolationFact, bool) {
	for _, f := range r.Facts {
		if f.Name == name {
			return f, true
		}
	}
	return runtime.IsolationFact{}, false
}
