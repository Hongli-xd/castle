package tests

// Reusable fixtures for the end-to-end suites.
//
// Six bash scripts used to each re-implement the same thirty lines: write a
// config, pkill the previous daemon, rm -rf the data dir, start the binary,
// sleep two seconds, grep the admin key out of the log, define two counters.
// Every one of those steps had a failure mode — a sleep that is wrong on a
// slow machine, a grep that picks the wrong key the moment the banner changes,
// a fixed /tmp path that collides between rounds — and none of them was the
// thing under test.
//
// This file is that infrastructure, once.
//
// Everything here lives in a _test.go file so `go build ./...` never acquires
// a dependency on `testing`, while every suite in this package still shares it.

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/runtime"
)

const (
	// testBaseImage is provisioned once per node: alpine plus a shell, awk and
	// tr — everything a probe needs and nothing else.
	testBaseImage = "sandboxd-test-base:alpine322"

	// upstreamImage is what testBaseImage is retagged from.
	upstreamImage = "alpine:3.22"

	// brokenImage is testBaseImage with /bin/sleep removed. The daemon pins
	// PID 1 to `/bin/sh -c "exec sleep infinity"` (see oci/createPayload), so
	// this image passes the HasImage check, creates successfully, and then
	// dies the instant it starts with exit 127. That is the only way to reach
	// the "create finished but failed" path honestly: the image really is on
	// the node, and the container really does refuse to run.
	brokenImage = "sandboxd-test-broken:nosleep"

	// fixedAdminKey replaces the old "scrape sk_… out of the log" dance. The
	// daemon upserts it on every boot when bootstrap_admin_key is set, so the
	// fixture knows the credential before the process has printed a byte.
	fixedAdminKey = "sk_it-admin-000000000000000000000000"

	// labelSandbox is the ownership label the OCI backend stamps on every
	// container it creates; it is what makes leak checks possible.
	labelSandbox = "sandbox.id"
)

// ---- process-level preconditions --------------------------------------------

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// daemonBinary builds the daemon under test once per test binary. Building
// here rather than assuming ./bin/sandboxd keeps `go test ./tests/` self
// contained; SBX_IT_RACE=1 builds it with the race detector so the daemon's
// own concurrency is instrumented, not just the test harness's.
func daemonBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "sandboxd-it-bin-")
		if err != nil {
			buildErr = err
			return
		}
		out := filepath.Join(dir, "sandboxd")
		args := []string{"build", "-o", out}
		if os.Getenv("SBX_IT_RACE") == "1" {
			args = append(args, "-race")
		}
		args = append(args, "./cmd/sandboxd")
		cmd := exec.Command("go", args...)
		cmd.Dir = repoRoot()
		if b, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("%v\n%s", err, b)
			return
		}
		builtBin = out
	})
	if buildErr != nil {
		t.Fatalf("cannot build the daemon under test: %v", buildErr)
	}
	return builtBin
}

func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
		return wd
	}
	return filepath.Dir(wd)
}

func dockerServerVersion() (string, error) {
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// requireDocker skips — never fails — when no container runtime is reachable,
// so a machine without Docker stays green. SBX_REQUIRE_DOCKER=1 turns that
// skip into a failure, because a suite that silently skips everything is
// indistinguishable from a suite that passes.
func requireDocker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration suite skipped in -short mode")
	}
	if _, err := dockerServerVersion(); err != nil {
		msg := fmt.Sprintf("container runtime unreachable (%v): isolation cannot be measured on this host", err)
		if os.Getenv("SBX_REQUIRE_DOCKER") == "1" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
	requireBaseImage(t)
	requireBrokenImage(t)
}

// requireBrokenImage builds brokenImage from the base image once per test
// binary. The lever is removing the /bin/sleep symlink: the image still has a
// working shell, so the container is created, and the pinned PID 1 command
// then fails to exec. Building is cached by the engine after the first run, so
// this costs one trivial layer per machine, not per test.
func requireBrokenImage(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "image", "inspect", brokenImage).Run(); err == nil {
		return
	}
	dir, err := os.MkdirTemp("", "sandboxd-broken-image-")
	if err != nil {
		t.Skipf("cannot stage the broken-image build context: %v", err)
	}
	defer os.RemoveAll(dir)
	df := "FROM " + testBaseImage + "\nRUN rm -f /bin/sleep\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o600); err != nil {
		t.Skipf("cannot write the broken-image Dockerfile: %v", err)
	}
	if out, err := exec.Command("docker", "build", "-t", brokenImage, dir).CombinedOutput(); err != nil {
		t.Skipf("cannot build %s (offline node?): %v\n%s", brokenImage, err, out)
	}
}

// requireBaseImage provisions the tiny base image the same way the unit-level
// integration tests do, so there is one provisioning story rather than two.
func requireBaseImage(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "image", "inspect", testBaseImage).Run(); err == nil {
		return
	}
	for _, step := range [][]string{
		{"pull", "docker.io/library/" + upstreamImage},
		{"tag", upstreamImage, testBaseImage},
	} {
		if out, err := exec.Command("docker", step...).CombinedOutput(); err != nil {
			t.Skipf("cannot provision %s (offline node?): %v\n%s", testBaseImage, err, out)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate a listen port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// containerIDsMatching returns backend container ids for a docker filter.
func containerIDsMatching(t *testing.T, filter ...string) []string {
	t.Helper()
	args := append([]string{"ps", "-aq"}, filter...)
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	return strings.Fields(string(out))
}

// managedContainers counts every container the daemon owns, by ownership
// label. A number that climbs across runs is a leak, not a slow test.
func managedContainers(t *testing.T) int {
	t.Helper()
	return len(containerIDsMatching(t, "--filter", "label="+labelSandbox))
}

// snapshotImages counts the images produced by committing sandboxes. Deleting
// a snapshot must remove its image: in a multi-tenant deployment a leak here
// grows without bound and nobody notices until the disk is full.
func snapshotImages(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("docker", "images", "--format", "{{.Repository}}").Output()
	if err != nil {
		t.Fatalf("docker images: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "sandboxd-snap") {
			n++
		}
	}
	return n
}

// ---- harness ----------------------------------------------------------------

// Options configures one daemon under test.
type Options struct {
	// Image is the base image sandboxes are created from.
	Image string
	// Set holds extra config keys, e.g. {"userns": "require"}.
	Set map[string]string
	// Env holds extra environment entries for the daemon process.
	Env []string
	// Pool enables the warm pool.
	Pool bool
	// Frozen disables the background sweeper and reconciler so a scenario can
	// observe a state that would otherwise be repaired underneath it.
	Frozen bool
	// NoStart builds the fixture without launching the daemon, so a scenario
	// can measure the launch itself. Without it every startup measurement is
	// impossible: New() waits for readiness before it returns.
	NoStart bool
}

// Harness owns one daemon, its data directory, and the bookkeeping needed to
// clean up after it.
type Harness struct {
	t        *testing.T
	opts     Options
	Base     string
	AdminKey string
	DataDir  string
	ConfPath string
	LogPath  string
	Admin    *Client
	bin      string
	port     int

	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan struct{}
	tracked []*trackedSandbox
	started int
}

// New starts a daemon on a free port with a private data directory and waits
// until it is genuinely ready — store healthy, backend reachable, isolation
// self-check completed. A daemon that cannot reach ready is not a usable
// fixture, so this fails rather than returning a half-built harness.
func New(t *testing.T, opts Options) *Harness {
	t.Helper()
	requireDocker(t)
	if opts.Image == "" {
		opts.Image = testBaseImage
	}
	h := &Harness{
		t: t, opts: opts, bin: daemonBinary(t),
		AdminKey: fixedAdminKey, port: freePort(t),
	}
	h.Base = fmt.Sprintf("http://127.0.0.1:%d", h.port)
	root := t.TempDir()
	h.DataDir = filepath.Join(root, "data")
	h.ConfPath = filepath.Join(root, "sandboxd.conf")
	h.LogPath = filepath.Join(root, "sandboxd.log")
	h.Admin = newClient(h.Base, h.AdminKey)
	h.WriteConfig(t)
	if !opts.NoStart {
		h.Start(t)
	}
	t.Cleanup(func() {
		h.Drain()
		h.Kill()
		h.Reap()
	})
	return h
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func quoteConf(v string) string {
	if v == "true" || v == "false" {
		return v
	}
	if _, err := strconv.Atoi(v); err == nil {
		return v
	}
	return strconv.Quote(v)
}

// WriteConfig renders the daemon's config file from a map so that a scenario
// states only the keys it cares about.
func (h *Harness) WriteConfig(t *testing.T) {
	t.Helper()
	kv := map[string]string{
		"listen":              fmt.Sprintf("127.0.0.1:%d", h.port),
		"data_dir":            h.DataDir,
		"bootstrap.image":     h.opts.Image,
		"bootstrap_admin_key": fixedAdminKey,
		"pool.enabled":        boolStr(h.opts.Pool),
		"sweep_every":         "3s",
		"reconcile_every":     "3s",
	}
	for k, v := range h.opts.Set {
		kv[k] = v
	}
	if h.opts.Frozen {
		kv["sweep_every"] = "1h"
		kv["reconcile_every"] = "1h"
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %s\n", k, quoteConf(kv[k]))
	}
	if err := os.WriteFile(h.ConfPath, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write daemon config: %v", err)
	}
}

// Config renders what was written, for diagnostics.
func (h *Harness) Config() string {
	b, err := os.ReadFile(h.ConfPath)
	if err != nil {
		return ""
	}
	return string(b)
}

// Start launches the daemon and blocks until /readyz is green.
func (h *Harness) Start(t *testing.T) {
	t.Helper()
	h.launch(t)
	h.WaitReady(t, 90*time.Second)
}

// StartAndTime is Start with the startup duration returned instead of thrown
// away. Startup used to be nothing but a 90-second timeout: the suite could
// tell that a daemon had started, never how long it took, so a regression that
// turned a two-second boot into a forty-second one was invisible until it
// crossed the timeout.
func (h *Harness) StartAndTime(t *testing.T) time.Duration {
	t.Helper()
	start := time.Now()
	h.launch(t)
	h.WaitReady(t, 90*time.Second)
	return time.Since(start)
}

// launch starts the process and returns; readiness is the caller's problem.
func (h *Harness) launch(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	if h.cmd != nil {
		h.mu.Unlock()
		t.Fatal("daemon is already running")
	}
	logFile, err := os.OpenFile(h.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		h.mu.Unlock()
		t.Fatalf("open daemon log: %v", err)
	}
	cmd := exec.Command(h.bin, "-config", h.ConfPath)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = append(os.Environ(), h.opts.Env...)
	if err := cmd.Start(); err != nil {
		h.mu.Unlock()
		t.Fatalf("start daemon: %v", err)
	}
	done := make(chan struct{})
	h.cmd, h.done, h.started = cmd, done, h.started+1
	h.mu.Unlock()

	// Reap the child asynchronously: without this it stays a zombie and
	// Exited() below can never be true.
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
}

// Kill sends SIGKILL and waits for the process to be gone. It deliberately
// skips Drain: crash scenarios need the daemon dead mid-flight.
func (h *Harness) Kill() {
	h.mu.Lock()
	cmd, done := h.cmd, h.done
	h.cmd = nil
	h.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}
}

// Stop asks the daemon to exit and waits, for graceful-restart scenarios.
func (h *Harness) Stop(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	cmd, done := h.cmd, h.done
	h.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
	}
	h.mu.Lock()
	h.cmd, h.done = nil, nil
	h.mu.Unlock()
}

// Restart is a graceful stop followed by a start against the same data dir.
func (h *Harness) Restart(t *testing.T) {
	t.Helper()
	h.Stop(t)
	h.Start(t)
}

// Running reports whether the daemon process is alive.
func (h *Harness) Running() bool {
	h.mu.Lock()
	cmd, done := h.cmd, h.done
	h.mu.Unlock()
	if cmd == nil {
		return false
	}
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// EnsureRunning restarts a daemon that a scenario deliberately killed, so
// cleanup can still reach the API. It is a no-op when already up.
func (h *Harness) EnsureRunning(t *testing.T) {
	t.Helper()
	if !h.Running() {
		h.Start(t)
	}
}

// Log returns the tail of the daemon's combined stdout/stderr.
func (h *Harness) Log() string {
	b, err := os.ReadFile(h.LogPath)
	if err != nil {
		return "(no log)"
	}
	s := string(b)
	if len(s) > 12000 {
		s = "…(truncated)…\n" + s[len(s)-12000:]
	}
	return s
}

// WaitReady polls /readyz until the daemon reports ready. Skipping
// /healthz on the way is deliberate: readiness is the only state in which the
// fixture is meaningful, since it also covers backend reachability.
func (h *Harness) WaitReady(t *testing.T, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if !h.Running() {
			t.Fatalf("daemon exited during startup\nexit: %s\nlog:\n%s", h.exitStatus(), h.Log())
		}
		if r := h.Admin.Get("/readyz"); r.TransportErr == nil && r.Status == http.StatusOK {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatalf("daemon not ready within %s\nconfig:\n%s\nlog:\n%s", limit, h.Config(), h.Log())
}

func (h *Harness) exitStatus() string {
	if st, err := os.Stat(h.LogPath); err == nil && st.Size() == 0 {
		return "(no output)"
	}
	lines := strings.Split(strings.TrimSpace(h.Log()), "\n")
	if len(lines) == 0 {
		return "(unknown)"
	}
	return lines[len(lines)-1]
}

// Ready fetches /readyz, asserting only that the call succeeded.
func (h *Harness) Ready() *Resp {
	return h.Admin.Get("/readyz")
}

// ---- bookkeeping ------------------------------------------------------------

// trackedSandbox records who owns a sandbox, so cleanup can address it through
// the admin plane, which requires the tenant to be named.
type trackedSandbox struct {
	tenantID string
	id       string
}

func (h *Harness) track(tenantID, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tracked = append(h.tracked, &trackedSandbox{tenantID: tenantID, id: id})
}

// Drain force-deletes every sandbox the harness created, best effort. It runs
// with the daemon alive where possible; failures are silent because cleanup
// must never turn a passing scenario red.
//
// The tenant is named explicitly: an admin key acting on a tenant resource
// must identify the tenant (see handlers.go), so a bare admin DELETE is
// refused with 403. That refusal used to be swallowed here, which made Drain a
// silent no-op and left Reap — which only removes containers, not records —
// as the sole real cleanup. Sandbox tracking therefore carries the owner.
func (h *Harness) Drain() {
	if !h.Running() {
		return
	}
	h.mu.Lock()
	refs := append([]*trackedSandbox(nil), h.tracked...)
	h.mu.Unlock()
	for _, ref := range refs {
		_ = h.Admin.WithHeader("X-Tenant-Id", ref.tenantID).Delete("/v1/sandboxes/" + ref.id + "?force=true")
	}
}

// Reap removes backend containers this harness created, by ownership label,
// without needing the daemon to be alive. Left-behind containers cost the
// operator disk and, worse, make the next run's orphan assertions ambiguous.
func (h *Harness) Reap() {
	h.mu.Lock()
	refs := append([]*trackedSandbox(nil), h.tracked...)
	h.mu.Unlock()
	for _, ref := range refs {
		// No testing.T here on purpose: this runs from t.Cleanup, where a
		// Fatalf would panic rather than report.
		out, err := exec.Command("docker", "ps", "-aq", "--filter", "label="+labelSandbox+"="+ref.id).Output()
		if err != nil {
			continue
		}
		for _, cid := range strings.Fields(string(out)) {
			_ = exec.Command("docker", "rm", "-f", cid).Run()
		}
	}
}

// ---- tenants ----------------------------------------------------------------

// Tenant is a tenant plus a working credential for it.
type Tenant struct {
	h      *Harness
	ID     string
	Name   string
	Key    string
	Client *Client
}

// adminCall issues an admin-plane request, honouring 429 with the server's
// Retry-After.
//
// Platform administration runs on its own token bucket — 1/s with a burst of
// 5 by default, per admin key — and that ceiling is a deliberate protection
// for the control plane, not a defect. A suite that onboards tenants in a
// loop must therefore behave like any well-mannered client. The ceiling
// itself is asserted where it belongs, in the quota scenarios.
func adminCall(t *testing.T, c *Client, method, path string, body any) *Resp {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var r *Resp
		switch method {
		case "GET":
			r = c.Get(path)
		case "POST":
			r = c.Post(path, body)
		case "PATCH":
			r = c.Patch(path, body)
		case "DELETE":
			r = c.Delete(path)
		default:
			t.Fatalf("adminCall: unsupported method %q", method)
		}
		if r.Status != http.StatusTooManyRequests || time.Now().After(deadline) {
			return r
		}
		delay := time.Second
		if v := strings.TrimSpace(r.Header.Get("Retry-After")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				delay = time.Duration(n)*time.Second + 50*time.Millisecond
			}
		}
		time.Sleep(delay)
	}
}

// forceDelete removes a sandbox through the platform-admin plane rather than
// the tenant's, and registers it so cleanup can reach it a second way.
//
// DELETE /v1/sandboxes/{id} is routed to the tenant's `create` bucket
// (internal/httpapi/server.go), so a scenario that deliberately squeezes the
// create rate also throttles its own teardown and strands the sandbox it made.
// Cleanup must never be subject to the exact gate the scenario is measuring.
//
// This is a deliberate deletion, not best-effort cleanup, so it asserts: a
// teardown that silently failed would leave the scenario asserting against
// state it believes it removed.
func (h *Harness) forceDelete(t *testing.T, tenantID, id string) {
	t.Helper()
	h.track(tenantID, id)
	adminCall(t, h.Admin.WithHeader("X-Tenant-Id", tenantID), "DELETE",
		"/v1/sandboxes/"+id+"?force=true", nil).MustStatus(t, http.StatusNoContent)
}

// Tenant creates a tenant with a rate budget wide enough not to interfere.
func (h *Harness) Tenant(t *testing.T, name string) *Tenant {
	t.Helper()
	return h.TenantWith(t, name, nil)
}

// TenantWith creates a tenant with an explicit quota. When the caller does
// not name rates, generous ones are filled in.
//
// The fixture must never be the reason a scenario sees a 429. The first
// version of this harness inherited the platform's default tenant rates, so
// the isolation suite — which runs about twenty-five execs back to back —
// failed half its scenarios on rate limiting instead of on isolation, and a
// scenario that is genuinely about rate limits passes its own.
func (h *Harness) TenantWith(t *testing.T, name string, q *domain.TenantQuota) *Tenant {
	t.Helper()
	if q == nil {
		q = quota(100, 65536, fastRates(500, 2000))
	} else if q.Rates == nil {
		q.Rates = fastRates(500, 2000)
	}
	tn := decode[*domain.Tenant](t, adminCall(t, h.Admin, "POST", "/v1/tenants", map[string]any{
		"name": name, "quota": *q,
	}).MustStatus(t, http.StatusCreated))
	secret := h.MintKey(t, tn.ID, name)
	return &Tenant{h: h, ID: tn.ID, Name: name, Key: secret, Client: h.Admin.As(secret)}
}

// MintKey issues a tenant-scoped key and returns its plaintext.
func (h *Harness) MintKey(t *testing.T, tenantID, name string) string {
	t.Helper()
	out := decode[keyMint](t, adminCall(t, h.Admin, "POST",
		"/v1/tenants/"+tenantID+"/keys", map[string]any{"name": name}).
		MustStatus(t, http.StatusCreated))
	if out.Secret == "" {
		t.Fatal("minting a key returned no secret")
	}
	return out.Secret
}

// quota is a convenience for building a tenant quota inline.
func quota(maxSandboxes, maxMemoryMiB int, rates map[string]*domain.Rate) *domain.TenantQuota {
	return &domain.TenantQuota{
		MaxSandboxes: maxSandboxes, MaxMemoryMiB: maxMemoryMiB, Rates: rates,
	}
}

// fastRates removes the default token-bucket guardrail so a scenario measures
// the gate it is actually about.
func fastRates(perSecond float64, burst int) map[string]*domain.Rate {
	out := map[string]*domain.Rate{}
	for _, g := range domain.AllGroups {
		out[g] = &domain.Rate{PerSecond: perSecond, Burst: burst}
	}
	return out
}

// ratesExcept leaves every group generous but pins one to a specific policy.
// A rate-limit scenario must throttle exactly the bucket under test: throttling
// every group makes the scenario's own DELETE fail with 429 and strands the
// sandbox it created, which then shows up as somebody else's leak.
func ratesExcept(group string, r domain.Rate) map[string]*domain.Rate {
	out := fastRates(500, 2000)
	out[group] = &r
	return out
}

// ---- sandbox specs ----------------------------------------------------------

func specOf(mutate ...func(*domain.SandboxSpec)) domain.SandboxSpec {
	s := domain.SandboxSpec{
		CPUMillicores:  500,
		MemoryMiB:      128,
		TimeoutSeconds: 900,
		Egress:         domain.EgressPolicy{Mode: domain.EgressNone},
	}
	for _, m := range mutate {
		m(&s)
	}
	return s
}

func withMemory(mib int) func(*domain.SandboxSpec) {
	return func(s *domain.SandboxSpec) { s.MemoryMiB = mib }
}
func withCPU(millicores int) func(*domain.SandboxSpec) {
	return func(s *domain.SandboxSpec) { s.CPUMillicores = millicores }
}
func withTimeout(seconds int) func(*domain.SandboxSpec) {
	return func(s *domain.SandboxSpec) { s.TimeoutSeconds = seconds }
}
func withDisk(mib int) func(*domain.SandboxSpec) {
	return func(s *domain.SandboxSpec) { s.DiskMiB = mib }
}
func withEgress(p domain.EgressPolicy) func(*domain.SandboxSpec) {
	return func(s *domain.SandboxSpec) { s.Egress = p }
}
func withEnv(kv map[string]string) func(*domain.SandboxSpec) {
	return func(s *domain.SandboxSpec) { s.Env = kv }
}
func withUser(u string) func(*domain.SandboxSpec) {
	return func(s *domain.SandboxSpec) { s.User = u }
}
func withWorkDir(dir string) func(*domain.SandboxSpec) {
	return func(s *domain.SandboxSpec) { s.WorkDir = dir }
}

// withIdleTimeout sets the idle-reclaim budget independently of the hard TTL,
// which is the only way to show that expiry is activity-driven rather than a
// second name for the same clock. The minimum accepted value is 60s
// (domain.SandboxSpec.Validate), so any scenario using it is necessarily slow.
func withIdleTimeout(seconds int) func(*domain.SandboxSpec) {
	return func(s *domain.SandboxSpec) { s.IdleTimeoutSeconds = seconds }
}

// ---- sandbox flows ----------------------------------------------------------

// Sandbox creates a sandbox and waits for it to be running.
func (tn *Tenant) Sandbox(t *testing.T, mutate ...func(*domain.SandboxSpec)) *domain.Sandbox {
	t.Helper()
	sb, r := tn.TrySandbox(t, createBody{BaseImage: tn.h.opts.Image, Spec: specOf(mutate...)})
	if r.Status != http.StatusCreated {
		t.Fatalf("create sandbox: %s\nbody: %s", r.describe(), r.snippet())
	}
	return sb
}

// TrySandbox submits an arbitrary create body, returning the raw response so
// negative scenarios can assert on the refusal.
func (tn *Tenant) TrySandbox(t *testing.T, body createBody, query ...string) (*domain.Sandbox, *Resp) {
	t.Helper()
	q := "?wait=true"
	if len(query) > 0 {
		q = query[0]
	}
	r := tn.Client.Post("/v1/sandboxes"+q, body)
	if r.Status != http.StatusCreated {
		return nil, r
	}
	sb := decode[*domain.Sandbox](t, r)
	tn.h.track(tn.ID, sb.ID)
	t.Cleanup(func() { tn.deleteQuietly(sb.ID) })
	return sb, r
}

// TryCreateJSON posts a raw create body, for wire-level abuse.
func (tn *Tenant) TryCreateJSON(t *testing.T, body any, query string) *Resp {
	t.Helper()
	return tn.Client.Post("/v1/sandboxes"+query, body)
}

// Get fetches a sandbox by id.
func (tn *Tenant) Get(t *testing.T, id string) *domain.Sandbox {
	t.Helper()
	return decode[*domain.Sandbox](t, tn.Client.Get("/v1/sandboxes/"+id).MustStatus(t, http.StatusOK))
}

// Sandboxes lists this tenant's sandboxes.
func (tn *Tenant) Sandboxes(t *testing.T, query string) sandboxPage {
	t.Helper()
	return decode[sandboxPage](t, tn.Client.Get("/v1/sandboxes"+query).MustStatus(t, http.StatusOK))
}

// Delete force-deletes a sandbox.
func (tn *Tenant) Delete(t *testing.T, id string) {
	t.Helper()
	tn.Client.Delete("/v1/sandboxes/"+id+"?force=true").MustStatus(t, http.StatusNoContent)
}

// DeleteAll force-deletes every sandbox this tenant owns, and registers them
// with the harness so cleanup can still reach any that survive. Scenarios that
// create through the raw client from goroutines use this instead of per-sandbox
// cleanups, because t.Cleanup is not safe to call off the test goroutine.
func (tn *Tenant) DeleteAll(t *testing.T) {
	t.Helper()
	for _, sb := range tn.Sandboxes(t, "").Sandboxes {
		tn.h.track(tn.ID, sb.ID)
		tn.deleteQuietly(sb.ID)
	}
}

func (tn *Tenant) deleteQuietly(id string) {
	_ = tn.Client.Delete("/v1/sandboxes/" + id + "?force=true")
}

// ---- exec -------------------------------------------------------------------

// Exec runs a shell command, failing on anything but a 200.
func (tn *Tenant) Exec(t *testing.T, sb *domain.Sandbox, cmd string) *runtime.ExecResult {
	t.Helper()
	return tn.ExecWith(t, sb, execReq{Command: cmd})
}

// ExecWith runs a fully specified exec request.
func (tn *Tenant) ExecWith(t *testing.T, sb *domain.Sandbox, req execReq) *runtime.ExecResult {
	t.Helper()
	res, r := tn.TryExec(t, sb, req)
	if r.Status != http.StatusOK {
		t.Fatalf("exec %s: %s\nbody: %s", req.describe(), r.describe(), r.snippet())
	}
	return res
}

// TryExec runs a command and returns the raw response for negative cases.
func (tn *Tenant) TryExec(t *testing.T, sb *domain.Sandbox, req execReq) (*runtime.ExecResult, *Resp) {
	t.Helper()
	if req.TimeoutMs == 0 {
		req.TimeoutMs = 30_000
	}
	r := tn.Client.Post("/v1/sandboxes/"+sb.ID+"/exec", req)
	if r.Status != http.StatusOK {
		return nil, r
	}
	return decode[*runtime.ExecResult](t, r), r
}

// Out runs a command and returns trimmed stdout, failing on a non-zero exit.
// This is the workhorse: most isolation claims are one `Out` plus an `eq`.
func (tn *Tenant) Out(t *testing.T, sb *domain.Sandbox, cmd string) string {
	t.Helper()
	res := tn.Exec(t, sb, cmd)
	if res.ExitCode != 0 {
		t.Fatalf("exec %q: exit %d\nerr: %s", cmd, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return strings.TrimSpace(string(res.Stdout))
}

// Exit runs a command and returns only its exit code, for the many probes
// whose answer is "did it succeed at all".
func (tn *Tenant) Exit(t *testing.T, sb *domain.Sandbox, cmd string) int {
	t.Helper()
	return tn.Exec(t, sb, cmd).ExitCode
}

// ---- files ------------------------------------------------------------------

// PutFile uploads raw bytes to an absolute in-sandbox path.
func (tn *Tenant) PutFile(t *testing.T, sb *domain.Sandbox, path string, content []byte) {
	t.Helper()
	tn.Client.Put("/v1/sandboxes/"+sb.ID+"/files?path="+url.QueryEscape(path), content).
		MustStatus(t, http.StatusNoContent)
}

// TryPutFile is PutFile without the status assertion.
func (tn *Tenant) TryPutFile(t *testing.T, sb *domain.Sandbox, path string, content []byte) *Resp {
	t.Helper()
	return tn.Client.Put("/v1/sandboxes/"+sb.ID+"/files?path="+url.QueryEscape(path), content)
}

// GetFile downloads a file.
func (tn *Tenant) GetFile(t *testing.T, sb *domain.Sandbox, path string) []byte {
	t.Helper()
	return tn.Client.Get("/v1/sandboxes/"+sb.ID+"/files?path="+url.QueryEscape(path)).
		MustStatus(t, http.StatusOK).Raw
}

// ---- snapshots, events, templates -------------------------------------------

func (tn *Tenant) Snapshot(t *testing.T, sb *domain.Sandbox, name string) *domain.Snapshot {
	t.Helper()
	return decode[*domain.Snapshot](t, tn.Client.Post("/v1/snapshots",
		map[string]any{"sandbox_id": sb.ID, "name": name}).MustStatus(t, http.StatusCreated))
}

func (tn *Tenant) Events(t *testing.T, query string) []*domain.AuditEvent {
	t.Helper()
	return decode[eventPage](t, tn.Client.Get("/v1/events"+query).MustStatus(t, http.StatusOK)).Events
}

// ---- assertions -------------------------------------------------------------

// eventually polls cond until it holds or the deadline passes. It exists so
// that scenarios about convergence assert on the converged state without
// hard-coding a sleep that is either flaky or slow.
func eventually(t *testing.T, limit time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s: still not true after %s", what, limit)
}

// WaitForState polls until the sandbox reports one of the wanted states, and
// fails with the state it actually settled on.
func (tn *Tenant) WaitForState(t *testing.T, id string, limit time.Duration, want ...domain.SandboxState) *domain.Sandbox {
	t.Helper()
	// A non-positive limit is almost always an untyped constant that was meant
	// to be seconds and became nanoseconds, and the loop below then never runs.
	// Without this guard the failure surfaces as a nil dereference inside the
	// failure message itself — the fixture exploding instead of reporting, which
	// is the one thing a fixture must never do.
	if limit <= 0 {
		t.Fatalf("WaitForState(%s): limit is %s, so the condition could never be observed", id, limit)
	}
	deadline := time.Now().Add(limit)
	var last *domain.Sandbox
	for time.Now().Before(deadline) {
		last = tn.Get(t, id)
		for _, w := range want {
			if last.State == w {
				return last
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if last == nil {
		t.Fatalf("sandbox %s was never observed at all within %s (wanted one of %v)", id, limit, want)
	}
	t.Fatalf("sandbox %s settled on %s; wanted one of %v within %s", id, last.State, want, limit)
	return last
}

func eq[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func neq[T comparable](t *testing.T, got, unwanted T, what string) {
	t.Helper()
	if got == unwanted {
		t.Fatalf("%s: got %v, want anything else", what, got)
	}
}

func contains(t *testing.T, hay, needle, what string) {
	t.Helper()
	if !strings.Contains(hay, needle) {
		t.Fatalf("%s: %q does not contain %q", what, hay, needle)
	}
}

func notContains(t *testing.T, hay, needle, what string) {
	t.Helper()
	if strings.Contains(hay, needle) {
		t.Fatalf("%s: %q unexpectedly contains %q", what, hay, needle)
	}
}

func truthy(t *testing.T, ok bool, what string) {
	t.Helper()
	if !ok {
		t.Fatalf("%s: not true", what)
	}
}

// lessThan parses a numeric probe result and bounds it. A result that is not
// a number is reported as unmeasured rather than as a pass.
func lessThan(t *testing.T, got string, limit int, what string) {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(got))
	if err != nil {
		t.Fatalf("%s: %q is not a number, so this was not measured", what, got)
	}
	if n >= limit {
		t.Fatalf("%s: got %d, want fewer than %d", what, n, limit)
	}
}

// wellFormedUnknownID returns an id of exactly the right shape whose value
// cannot exist: same length, same Crockford-base32 alphabet, one character
// different. It exists so a suite can assert the difference between "you sent
// nonsense" (400) and "that does not exist here" (404) — two answers a client
// treats completely differently, and which a length check alone collapses.
func wellFormedUnknownID(realID string) string {
	if realID == "" {
		return realID
	}
	last := realID[len(realID)-1]
	replacement := byte('A')
	if last == 'A' || last == 'a' {
		replacement = 'B'
	}
	return realID[:len(realID)-1] + string(replacement)
}
