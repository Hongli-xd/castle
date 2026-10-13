package tests

// The container tier's security contract, as executable evidence.
//
// The tier was chosen deliberately, so the claim that goes on a quote is
// "an escape requires defeating every one of these layers at the same time",
// never "escape is impossible" — the sandbox shares this host's kernel, and
// nothing below pretends otherwise.
//
// Every scenario measures one fact from *inside* a real sandbox and compares
// it against the value the contract requires. That direction matters: a
// container engine will accept security settings it cannot honour and report
// them back as applied, so configuration is intent and only the kernel
// talking from inside the sandbox is evidence.

import (
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/runtime"
)

func TestIsolationContract(t *testing.T) {
	h := New(t, Options{})
	tn := h.Tenant(t, "isolation")
	sb := tn.Sandbox(t)

	// statusField reads a field of /proc/self/status — what the kernel says
	// about this process, not what the daemon asked for.
	statusField := func(field string) string {
		return tn.Out(t, sb, "awk '/^"+field+":/{print $2}' /proc/self/status")
	}

	t.Run("seccomp_filter_is_in_force", func(t *testing.T) {
		// 2 = SECCOMP_MODE_FILTER. Any other value means no syscall filter,
		// and the syscall surface is the main escape surface.
		eq(t, statusField("Seccomp"), "2", "seccomp mode")
	})

	t.Run("privilege_cannot_be_regained", func(t *testing.T) {
		// NoNewPrivs=1 blocks setuid binaries, file capabilities and
		// ptrace-based privilege gain — the whole "become more privileged
		// later" family.
		eq(t, statusField("NoNewPrivs"), "1", "NoNewPrivs")
	})

	t.Run("capability_sets_are_empty", func(t *testing.T) {
		for _, field := range []string{"CapEff", "CapPrm", "CapInh", "CapBnd", "CapAmb"} {
			mustBeAllZeroHex(t, statusField(field), field)
		}
	})

	t.Run("uid_is_root_but_powerless", func(t *testing.T) {
		// The contract is not "non-root". It is root-by-name with an empty
		// effective capability set: no CAP_DAC_OVERRIDE, no CAP_SYS_ADMIN,
		// no CAP_NET_ADMIN. Stated plainly because the difference is exactly
		// what a customer's security reviewer will ask about.
		eq(t, tn.Out(t, sb, "id -u"), "0", "uid inside the sandbox")
		mustBeAllZeroHex(t, statusField("CapEff"), "effective capabilities of uid 0")
	})

	t.Run("core_dumps_are_disabled", func(t *testing.T) {
		// A core dump writes process memory to disk; for a sandbox handling
		// credentials that is a disclosure path with no upside.
		eq(t, tn.Out(t, sb, "ulimit -c"), "0", "core dump rlimit")
	})

	// ---- filesystem and mount surface -------------------------------------

	t.Run("mounting_is_denied", func(t *testing.T) {
		// CAP_SYS_ADMIN absent means no new mounts, which is what stops the
		// classic "bind-mount the host root" step.
		neq(t, tn.Exit(t, sb, "mkdir -p /tmp/mnt && mount -t tmpfs none /tmp/mnt 2>/dev/null"), 0,
			"exit code of an attempted tmpfs mount")
	})

	t.Run("kernel_memory_is_unreadable", func(t *testing.T) {
		neq(t, tn.Exit(t, sb, "head -c 1 /proc/kcore 2>/dev/null"), 0, "exit code of reading /proc/kcore")
	})

	t.Run("kernel_tunables_are_unwritable", func(t *testing.T) {
		neq(t, tn.Exit(t, sb, "echo 1 > /proc/sys/kernel/sysrq 2>/dev/null"), 0, "exit code of writing /proc/sysrq-trigger")
		neq(t, tn.Exit(t, sb, "echo 1 > /proc/sysrq-trigger 2>/dev/null"), 0, "exit code of writing sysrq-trigger")
	})

	t.Run("device_nodes_cannot_be_created", func(t *testing.T) {
		// mknod needs CAP_MKNOD. With it, /dev/mem is a host memory reader.
		neq(t, tn.Exit(t, sb, "mknod /tmp/null2 c 1 3 2>/dev/null"), 0, "exit code of mknod")
	})

	t.Run("raw_devices_are_absent", func(t *testing.T) {
		for _, dev := range []string{"/dev/sda", "/dev/vda", "/dev/mem", "/dev/kmem", "/dev/kmsg"} {
			eq(t, tn.Exit(t, sb, "test -e "+dev), 1, dev+" must not exist in the sandbox")
		}
	})

	// ---- the two escapes that matter most in practice ---------------------

	t.Run("the_container_runtime_socket_is_not_reachable", func(t *testing.T) {
		// A reachable Docker socket is not a sandbox escape, it is a host
		// takeover: whoever reaches it can start a privileged container.
		for _, sock := range []string{"/var/run/docker.sock", "/run/docker.sock", "/run/containerd/containerd.sock"} {
			eq(t, tn.Exit(t, sb, "test -e "+sock), 1, sock+" must not be present")
		}
	})

	t.Run("no_host_directory_is_mounted", func(t *testing.T) {
		// Plant a canary on the daemon's own filesystem, then prove the
		// sandbox cannot see it. This is the assertion that a bind mount
		// added to backend.go would break immediately.
		canary := "sbx-canary-" + strings.ReplaceAll(tn.ID, "_", "")
		plantHostCanary(t, canary)
		defer removeHostCanary(t, canary)

		eq(t, tn.Exit(t, sb, "test -e /tmp/"+canary), 1, "canary visible at /tmp inside the sandbox")
		eq(t, tn.Exit(t, sb, "test -e /hosttmp/"+canary), 1, "canary visible at /hosttmp inside the sandbox")
		mounts := tn.Out(t, sb, "cat /proc/self/mounts")
		notContains(t, mounts, canary, "sandbox mount table")
	})

	t.Run("proc_cannot_be_used_to_step_outside", func(t *testing.T) {
		// /proc/<pid>/root is the classic mount-namespace hop when the target
		// PID belongs to the host. Write a marker into the sandbox root and
		// read it back through /proc/1/root: if PID 1 were a host process,
		// that path would resolve to the host's root and the marker would be
		// absent — which is exactly the failure this catches.
		tn.Out(t, sb, "echo sandbox-root > /sbx-root-marker")
		eq(t, tn.Out(t, sb, "cat /proc/1/root/sbx-root-marker 2>/dev/null || echo unreadable"),
			"sandbox-root", "content read through /proc/1/root")
	})

	// ---- process namespace -------------------------------------------------

	t.Run("pid_namespace_hides_the_control_plane", func(t *testing.T) {
		// If the sandbox shared the host PID namespace it would also see the
		// daemon and every other tenant's workload.
		eq(t, tn.Out(t, sb, "grep -l sandboxd /proc/[0-9]*/comm 2>/dev/null | wc -l"), "0",
			"control-plane processes visible inside the sandbox")
		neq(t, tn.Out(t, sb, "cat /proc/1/comm"), "sandboxd", "PID 1 inside the sandbox")
		// A shared namespace on a live host shows hundreds of PIDs. The bound
		// is deliberately loose: the exec that runs this probe is itself a
		// process in the sandbox, so an exact count would be a flake.
		lessThan(t, tn.Out(t, sb, "ls -d /proc/[0-9]* 2>/dev/null | wc -l"), 10,
			"pids visible inside an otherwise idle sandbox")
	})

	t.Run("uts_namespace_differs_from_the_host", func(t *testing.T) {
		host := hostHostname(t)
		if host == "" {
			t.Skip("cannot read the daemon host's hostname; not measurable here")
		}
		neq(t, tn.Out(t, sb, "hostname"), host, "sandbox hostname vs daemon host hostname")
	})

	// ---- network surface ---------------------------------------------------

	t.Run("there_is_no_route_off_the_box", func(t *testing.T) {
		// Egress none is enforced by having no path out, not by a firewall
		// rule inside a namespace the workload controls.
		eq(t, tn.Out(t, sb, "ip route 2>/dev/null | wc -l"), "0", "routing table entries")
		for _, iface := range []string{"eth0", "ens3", "enp0s1"} {
			eq(t, tn.Exit(t, sb, "test -e /sys/class/net/"+iface), 1, iface+" must not exist")
		}
	})

	t.Run("exfiltration_dies_at_the_network_boundary", func(t *testing.T) {
		neq(t, tn.Exit(t, sb, "wget -T 3 -q -O- http://1.1.1.1/ 2>/dev/null"), 0, "exit code of an outbound HTTP request")
		neq(t, tn.Exit(t, sb, "nslookup example.com 2>/dev/null"), 0, "exit code of a DNS lookup")
	})

	t.Run("loopback_is_the_sandboxes_own", func(t *testing.T) {
		// The daemon listens on the host's loopback. Inside the sandbox the
		// same address must be the sandbox's own empty loopback, not a route
		// back to the control plane.
		neq(t, tn.Exit(t, sb, "wget -T 2 -q -O- http://127.0.0.1:1/ 2>/dev/null"), 0,
			"exit code of a request to loopback")
	})

	// ---- what the control plane claims vs what the kernel says -------------

	t.Run("user_namespace_posture_matches_the_kernel", func(t *testing.T) {
		// The measured trap: an engine with no userns-remap configured
		// accepts HostConfig.UsernsMode="private", stores it, and echoes it
		// back — while the container keeps the identity uid map. So the
		// configuration is compared against /proc/self/uid_map, never trusted.
		info := decode[readyInfo](t, h.Ready().MustStatus(t, http.StatusOK))
		uidMap := collapseSpaces(tn.Out(t, sb, "tr '\\n' ' ' < /proc/self/uid_map"))
		switch info.IsolationUserns {
		case "off":
			eq(t, uidMap, "0 0 4294967295", "uid map with userns off")
		case "private":
			neq(t, uidMap, "0 0 4294967295", "uid map with userns claimed private")
		default:
			t.Fatalf("unexpected userns posture %q reported by /readyz", info.IsolationUserns)
		}
	})

	t.Run("readiness_reports_the_tier_and_its_limits", func(t *testing.T) {
		// "Not verified" must be its own state. A tier string is a claim; the
		// verified flag is the evidence, and the note is the boundary.
		info := decode[readyInfo](t, h.Ready().MustStatus(t, http.StatusOK))
		eq(t, info.Isolation, runtime.TierContainer, "isolation tier")
		truthy(t, info.IsolationVerified, "isolation_verified on /readyz")
		contains(t, info.IsolationNote, "shares this host's kernel", "isolation note")
	})

	t.Run("the_self_check_inventory_agrees_with_this_suite", func(t *testing.T) {
		// Two independent measurements of the same facts: the daemon's own
		// startup self-check, and the probes above. Agreement is the evidence;
		// a disagreement means one of them is wrong and both are worthless.
		rep := decode[*runtime.IsolationReport](t, h.Admin.Get("/v1/isolation").MustStatus(t, http.StatusOK))
		truthy(t, rep.OK(), "daemon self-check failures: "+strings.Join(rep.Failures, "; "))
		eq(t, rep.Tier, runtime.TierContainer, "self-check tier")
		seen := map[string]bool{}
		for _, f := range rep.Facts {
			seen[f.Name] = true
		}
		for _, want := range []string{"seccomp", "no_new_privs", "cap_effective", "cap_bounding", "core_dump", "uid_map"} {
			truthy(t, seen[want], "self-check covers "+want)
		}
	})

	// ---- honest refusals ---------------------------------------------------

	t.Run("unenforceable_limits_are_refused_not_silently_ignored", func(t *testing.T) {
		// The failure mode this guards against is the worst kind: a sandbox
		// the caller believes is restricted and is not.
		_, r := tn.TrySandbox(t, createBody{
			BaseImage: h.opts.Image,
			Spec:      specOf(withEgress(domain.EgressPolicy{Mode: domain.EgressAllowlist, Allow: []string{"api.github.com"}})),
		})
		// Unenforceable limits must be refused with the code that says so —
		// MustErr also checks that 501 is the transport the code maps to.
		r.MustErr(t, apperr.Unimplemented)

		tn.TryCreateJSON(t, map[string]any{
			"base_image": h.opts.Image,
			"spec":       specOf(withDisk(1024)),
		}, "?wait=false").MustErr(t, apperr.Unimplemented)
	})
}

// ---- helpers ----------------------------------------------------------------

func collapseSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

func mustBeAllZeroHex(t *testing.T, value, field string) {
	t.Helper()
	v := strings.TrimSpace(value)
	if v == "" {
		t.Fatalf("%s: the kernel reported nothing; this was not measured rather than passing", field)
	}
	if strings.Trim(v, "0") != "" {
		t.Fatalf("%s: got %s, want an all-zero capability set", field, v)
	}
}

// sbHasOwnInit is deliberately gone: the version of this suite that shipped
// first compared a value against itself through this helper, which passes on
// every host including a catastrophically broken one.

// plantHostCanary writes a marker onto the daemon host's filesystem by way of
// a throwaway container, so "the sandbox cannot see host files" is tested
// against the machine the daemon actually runs on rather than the laptop.
func plantHostCanary(t *testing.T, name string) {
	t.Helper()
	out, err := exec.Command("docker", "run", "--rm", "-v", "/tmp:/hosttmp",
		testBaseImage, "sh", "-c", "echo host-canary > /hosttmp/"+name).CombinedOutput()
	if err != nil {
		t.Fatalf("plant host canary: %v\n%s", err, out)
	}
}

func removeHostCanary(t *testing.T, name string) {
	t.Helper()
	_ = exec.Command("docker", "run", "--rm", "-v", "/tmp:/hosttmp",
		testBaseImage, "sh", "-c", "rm -f /hosttmp/"+name).Run()
}

// hostHostname reads the hostname of the machine the daemon runs on, using a
// container that shares its UTS namespace.
func hostHostname(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "run", "--rm", "--uts=host", testBaseImage, "hostname").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
