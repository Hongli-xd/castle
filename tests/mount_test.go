package tests

// End-to-end evidence for the read-only host mount feature.
//
// The contract here is deliberately narrow and falsifiable: a host directory
// the operator allowlists MAY be bind-mounted read-only into a sandbox, and
// when it is, the sandbox can read it but every write to it fails without
// touching the host. Everything else — arbitrary host paths, mounts with no
// allowlist entry, mounts by default — must be refused, so the isolation
// contract's "no host directory is mounted" canary holds out of the box.
//
// "Host" here means the Docker VM's filesystem, not the laptop's: the canary
// mechanism (plantHostCanary) already established that convention, because the
// daemon passes mount sources to the engine and the engine resolves them
// inside the VM. A directory staged with os.MkdirTemp on the macOS host is
// invisible there — the first version of this test failed with exactly that
// "bind source path does not exist", which is the whole point: configuration
// is intent, and only the filesystem the engine actually mounts is evidence.

import (
	"fmt"
	"os/exec"
	"path"
	"strings"
	"testing"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
)

// stageHostMountDir plants a host directory inside the Docker VM's /tmp (the
// same convention plantHostCanary uses) so a bind mount made by the daemon
// sees real files rather than an empty VM-local path. It returns the VM path
// and its basename.
func stageHostMountDir(t *testing.T) (vmPath, base string) {
	t.Helper()
	base = fmt.Sprintf("sbx-mount-%d", time.Now().UnixNano())
	vmPath = "/tmp/" + base
	out, err := exec.Command("docker", "run", "--rm", "-v", "/tmp:/hosttmp",
		testBaseImage, "sh", "-c",
		"mkdir -p /hosttmp/"+base+" && echo read-only-host-content > /hosttmp/"+base+"/probe.txt").CombinedOutput()
	if err != nil {
		t.Fatalf("stage host dir in the VM: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "run", "--rm", "-v", "/tmp:/hosttmp",
			testBaseImage, "sh", "-c", "rm -rf /hosttmp/"+base).Run()
	})
	return vmPath, base
}

// vmHostPathExists reports whether a path exists inside the Docker VM's
// filesystem, by way of a throwaway container sharing the VM's /tmp.
func vmHostPathExists(t *testing.T, base string) bool {
	t.Helper()
	err := exec.Command("docker", "run", "--rm", "-v", "/tmp:/hosttmp",
		testBaseImage, "sh", "-c", "test -e /hosttmp/"+base).Run()
	return err == nil
}

func withMounts(m []domain.MountSpec) func(*domain.SandboxSpec) {
	return func(s *domain.SandboxSpec) { s.Mounts = m }
}

// TestReadOnlyMountIsReadableButNotWritable is the positive end-to-end case:
// the allowlisted host directory is visible at the target and readable, and
// every write to it fails without touching the host. Read-only is asserted
// three independent ways — the file reads, the write exits non-zero, and the
// host directory is unchanged — so a single lucky failure mode cannot fake it.
func TestReadOnlyMountIsReadableButNotWritable(t *testing.T) {
	host, base := stageHostMountDir(t)
	h := New(t, Options{Set: map[string]string{"mount.allow": host}})
	tn := h.Tenant(t, "mount")
	sb := tn.Sandbox(t, withMounts([]domain.MountSpec{{Source: host, Target: "/workspace"}}))

	// visible and readable through the mount
	eq(t, tn.Out(t, sb, "cat /workspace/probe.txt"), "read-only-host-content", "mounted file content")

	// relative paths resolve against the mount when the exec runs there — the
	// exact thing pi-sbx relies on so `python test.py` works where the agent
	// thinks it is.
	res := tn.ExecWith(t, sb, execReq{Command: "cat probe.txt", WorkDir: "/workspace"})
	if res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != "read-only-host-content" {
		t.Fatalf("workdir=/workspace + relative path: exit %d stdout %q, want the mounted file", res.ExitCode, res.Stdout)
	}

	// the kernel's own mount table names the target, not just the config
	contains(t, tn.Out(t, sb, "cat /proc/self/mounts"), "/workspace", "mount table lists the target")

	// read-only: writing into the mount must fail
	neq(t, tn.Exit(t, sb, "echo pwn > /workspace/owned.txt"), 0, "write into the read-only mount")

	// and the host is untouched — the whole point of read-only
	if vmHostPathExists(t, path.Join(base, "owned.txt")) {
		t.Fatal("a write inside the sandbox landed on the host directory; the mount is not read-only")
	}
}

// TestMountOutsideAllowlistIsRefused pins the authorization half: the feature
// must never let a tenant read an arbitrary host path, even read-only, because
// reading /etc or /root/.ssh is the whole node.
func TestMountOutsideAllowlistIsRefused(t *testing.T) {
	host, _ := stageHostMountDir(t)
	h := New(t, Options{Set: map[string]string{"mount.allow": host}})
	tn := h.Tenant(t, "mount-refuse")
	_, r := tn.TrySandbox(t, createBody{
		BaseImage: h.opts.Image,
		Spec:      specOf(withMounts([]domain.MountSpec{{Source: "/etc", Target: "/workspace"}})),
	})
	r.MustErr(t, apperr.Forbidden)
}

// TestMountsDisabledByDefault pins the default posture: with no mount.allow
// configured, any mount request is refused, so the "no host directory is
// mounted" canary holds out of the box rather than only after an operator
// opts out.
func TestMountsDisabledByDefault(t *testing.T) {
	host, _ := stageHostMountDir(t)
	h := New(t, Options{}) // no mount.allow
	tn := h.Tenant(t, "mount-off")
	_, r := tn.TrySandbox(t, createBody{
		BaseImage: h.opts.Image,
		Spec:      specOf(withMounts([]domain.MountSpec{{Source: host, Target: "/workspace"}})),
	})
	r.MustErr(t, apperr.Forbidden)
}
