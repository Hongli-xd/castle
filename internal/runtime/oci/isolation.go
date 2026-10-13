package oci

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/runtime"
)

// probeScript is the workload the isolation self-check runs inside a real
// sandbox. It reports only what the kernel says about the running process,
// one key=value line each, so the parser interprets nothing and a reviewer can
// re-run it by hand.
//
// Two properties it must have:
//
//   - A test whose own precondition is missing prints an explicit UNKNOWN
//     rather than a failure. "Could not measure" and "measured and failed" are
//     different results and must not collapse into one another.
//   - It only reads; the single write is the obvious mountpoint under /tmp,
//     removed immediately. A self-check must not be able to cause an incident.
//
// Every assertion below is deliberately measurable from inside the sandbox,
// because that is the only place the truth lives: configuration — even
// configuration read back from the engine — describes intent.
// The key on the left of each line is BOTH the wire key and the fact name the
// report uses. Keeping them the same string is deliberate: when they drifted
// apart during development, seven facts silently ended up comparing "" against
// their expected value, and one of them — the uid map, read through the
// non-asserted branch — reported success on an empty observation. See
// probeKeys and the guard in VerifyIsolation.
const probeScript = `set -u
echo "seccomp=$(awk '/^Seccomp:/{print $2}' /proc/self/status)"
echo "no_new_privs=$(awk '/^NoNewPrivs:/{print $2}' /proc/self/status)"
echo "cap_effective=$(awk '/^CapEff:/{print $2}' /proc/self/status)"
echo "cap_bounding=$(awk '/^CapBnd:/{print $2}' /proc/self/status)"
echo "uid=$(id -u 2>/dev/null)"
echo "uid_map=$(tr '\n' ';' < /proc/self/uid_map 2>/dev/null)"
echo "apparmor=$(cat /proc/self/attr/current 2>/dev/null)"
echo "core_dump=$(ulimit -c 2>/dev/null)"
d=/tmp/.sandboxd-probe-$$
if mkdir -p "$d" 2>/dev/null; then
  if mount -t tmpfs none "$d" >/dev/null 2>&1; then echo "can_mount=YES"; else echo "can_mount=no"; fi
  rmdir "$d" 2>/dev/null
else
  echo "can_mount=UNKNOWN"
fi
if head -c 1 /proc/kcore >/dev/null 2>&1; then echo "can_read_kcore=YES"; else echo "can_read_kcore=no"; fi
`

// probeKeys is every key probeScript must emit. VerifyIsolation refuses to
// produce a report when one is missing, because the alternative is a checker
// that compares an empty observation against the expectation and reports a
// result that says nothing about the sandbox — in either direction. A broken
// check must announce itself as broken, not as a pass and not as a finding.
var probeKeys = []string{
	"seccomp", "no_new_privs", "cap_effective", "cap_bounding",
	"uid", "uid_map", "apparmor", "core_dump",
	"can_mount", "can_read_kcore",
}

// identityUIDMap is what the kernel reports when a sandbox has no user
// namespace of its own: uid 0 maps to uid 0 across the whole 32-bit range.
// A sandbox that was *asked* for a private user namespace and still reports
// this is precisely the silent failure this check exists to catch — the
// engine accepted the field, echoed it back, and honoured none of it.
const identityUIDMap = "0 0 4294967295"

// VerifyIsolation builds a sandbox through the same payload path production
// uses, runs probeScript inside it, and compares what the kernel reports
// against the isolation contract.
func (b *Backend) VerifyIsolation(ctx context.Context, image string) (*runtime.IsolationReport, error) {
	img := strings.TrimSpace(image)
	if img == "" {
		picked, err := b.c.smallestUsableImage(ctx)
		if err != nil {
			return nil, err
		}
		img = picked
	}

	ref := fmt.Sprintf("sandboxd-isolation-check-%d", time.Now().UnixNano())

	// The labels deliberately omit "sandbox.id". The reconciler treats a
	// product-labeled object with no sandbox.id as unattributable and leaves
	// it for the operator, so a self-check cannot be reaped out from under
	// itself, and cannot be mistaken for a registered sandbox.
	if _, err := b.Create(ctx, runtime.CreateRequest{
		Name:          ref,
		Image:         img,
		CPUMillicores: 100,
		MemoryMiB:     64,
		Labels:        map[string]string{"sandboxd.check": "isolation"},
	}); err != nil {
		return nil, err
	}
	defer b.reapProbe(ref)

	if err := b.Start(ctx, ref); err != nil {
		return nil, err
	}

	res, err := b.Exec(ctx, ref, runtime.ExecRequest{
		Argv:      []string{"/bin/sh", "-c", probeScript},
		Timeout:   30 * time.Second,
		MaxOutput: 64 << 10,
	})
	if err != nil {
		return nil, err
	}
	obs := parseProbe(res.Stdout)

	// A probe that produced nothing is an error, never a pass. The most
	// likely cause is an image with no shell, and reporting that as "isolation
	// verified" would be the same class of mistake as the one being guarded
	// against.
	if len(obs) < 6 {
		return nil, apperr.Invalid(
			"isolation probe produced no usable output in image %q (exit=%d, stderr=%q): the image "+
				"probably has no /bin/sh. Set bootstrap.image to a small image that does — a check "+
				"that did not run must not be reported as a check that passed",
			img, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	// And a probe that ran but left keys unaccounted for is a bug in the
	// checker, which is a third outcome: neither verified nor violated.
	if missing := missingProbeKeys(obs); len(missing) > 0 {
		return nil, apperr.InternalErr(
			"isolation probe and its parser disagree: no output for %v (probe emitted %d keys). "+
				"Fixing this requires a code change, so the check reports neither pass nor failure "+
				"rather than comparing an empty observation against the contract",
			missing, len(obs))
	}
	return b.assertIsolation(img, obs), nil
}

// missingProbeKeys lists the keys the report needs that the probe did not emit.
func missingProbeKeys(obs map[string]string) []string {
	var missing []string
	for _, k := range probeKeys {
		if _, ok := obs[k]; !ok {
			missing = append(missing, k)
		}
	}
	return missing
}

// reapProbe tears the self-check sandbox down on a fresh context: the caller's
// context is usually the startup one, which may already be near its deadline
// by the time the probe finishes.
func (b *Backend) reapProbe(ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = b.Stop(ctx, ref, true)
	_ = b.Remove(ctx, ref)
}

// parseProbe reads key=value lines, ignoring anything else the workload or the
// shell may have emitted.
func parseProbe(out []byte) map[string]string {
	obs := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		obs[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return obs
}

func (b *Backend) assertIsolation(image string, obs map[string]string) *runtime.IsolationReport {
	rep := &runtime.IsolationReport{
		Tier:      b.IsolationTier(),
		Image:     image,
		CheckedAt: time.Now(),
	}
	if b.usernsMode == UsernsModePrivate {
		rep.Userns = "private"
	} else {
		rep.Userns = "off"
	}

	add := func(key, observed, want string, ok bool, note string) {
		rep.Facts = append(rep.Facts, runtime.IsolationFact{
			Name: key, Observed: observed, Want: want, OK: ok, Note: note,
		})
		if !ok {
			rep.Failures = append(rep.Failures,
				fmt.Sprintf("%s: observed %q, want %s", key, observed, want))
		}
	}
	// Each helper takes the observation key once and derives both the reported
	// value and the verdict from it. That is not stylistic. When the key had to
	// be written twice — once to look up the value and once to compare it — the
	// two drifted apart, and four facts displayed the correct observation while
	// judging themselves against an empty lookup. Writing it once makes that
	// class of mistake unrepresentable.
	addEq := func(key, want, note string) {
		got := obs[key]
		add(key, got, want, got == want, note)
	}
	addPred := func(key, want, note string, pred func(string) bool) {
		got := obs[key]
		add(key, got, want, pred(got), note)
	}
	// Facts that are not asserted still get recorded: the report is meant to be
	// readable evidence, including about the things this host cannot check.
	record := func(key, want, note string) {
		rep.Facts = append(rep.Facts, runtime.IsolationFact{
			Name: key, Observed: obs[key], Want: want, OK: true, Note: note,
		})
	}

	addEq("seccomp", "2",
		"a syscall filter is the surface a kernel exploit must get through first")
	addEq("no_new_privs", "1",
		"stops privilege gain through setuid/setgid binaries")
	addPred("cap_effective", "all zero",
		"capabilities are what turn uid 0 into actual power", allZeroHex)
	addPred("cap_bounding", "all zero",
		"an empty bounding set means no capability can be regained across exec", allZeroHex)
	addEq("core_dump", "0",
		"a core dump writes process memory — including credentials — into a filesystem that gets snapshotted")
	addEq("can_mount", "no",
		"mounting is the conventional first step of a container escape")
	addEq("can_read_kcore", "no",
		"/proc/kcore is host kernel memory")

	if b.apparmorHost {
		addPred("apparmor", "an enforcing profile",
			"a second, independent gate that still applies to root inside the sandbox",
			func(s string) bool { return strings.Contains(s, "(enforce)") })
	} else {
		record("apparmor", "n/a",
			"this daemon reports no AppArmor, so no profile is expected; an SELinux host would differ")
	}

	if b.usernsMode == UsernsModePrivate {
		addPred("uid_map", "a remapped range, not "+identityUIDMap,
			"the identity map means uid 0 inside the sandbox IS uid 0 on the host, so none of the "+
				"assertions above would survive a single kernel bug",
			uidMapIsRemapped)
	} else {
		record("uid_map", "identity map (disclosed)",
			"user namespaces are off for this deployment: "+b.usernsNote)
	}

	// Not asserted, only reported: the uid the workload actually ran as. A
	// sandbox that runs as uid 0 with no user namespace is a weaker position
	// than one that does not, and an operator reading the report should see it
	// without the report pretending to judge it.
	record("uid", "informational", "uid the probe workload ran as")
	return rep
}

// allZeroHex reports whether s is a non-empty run of zeros — how the kernel
// renders an empty capability set (e.g. "0000000000000000", or
// "000001ffffffffff" on narrower kernels' bounding sets when not dropped).
func allZeroHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// uidMapIsRemapped reports whether the uid map the sandbox sees is anything
// other than the identity. The probe reports lines separated by ";" because
// /proc/self/uid_map is multi-line for complex mappings.
//
// Comparison happens on whitespace-collapsed text, because the kernel pads
// uid_map columns for alignment: the real bytes are
//
//	"         0          0 4294967295"
//
// Comparing the raw line against a single-spaced constant therefore fails to
// recognise the identity map and reports a user namespace that does not exist
// — the exact false negative this function is supposed to prevent.
func uidMapIsRemapped(v string) bool {
	saw := false
	for _, line := range strings.Split(v, ";") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		saw = true
		if line == identityUIDMap {
			return false
		}
	}
	return saw
}
