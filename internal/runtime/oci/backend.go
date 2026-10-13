package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/runtime"
)

const (
	labelProduct = "product"
	labelSandbox = "sandbox.id"
	labelTenant  = "sandbox.tenant"
	prodValue    = "sandboxd"

	startProbeDelay = 100 * time.Millisecond
	startProbeMax   = 10 * time.Second
)

// Backend implements runtime.Backend with Docker containers.
type Backend struct {
	c    *Client
	opts Options

	// Filled in by Init from the daemon's own /info. "Not probed" leaves
	// usernsMode empty, which is the identity-map posture — an unverified
	// backend must never claim a user namespace it has not confirmed.
	probed       bool
	usernsMode   string // "" = host/identity map, "private" = remapped
	usernsNote   string
	apparmorHost bool // daemon reports AppArmor, so the profile can be pinned
}

// User-namespace postures accepted by Options.Userns.
const (
	// UsernsAuto enables user-namespace isolation when this daemon can
	// actually provide it, and otherwise discloses the downgrade in the
	// startup log and at /readyz.
	UsernsAuto = "auto"
	// UsernsRequire refuses to start when this daemon cannot provide it.
	UsernsRequire = "require"
	// UsernsOff accepts the shared-identity tier as a conscious choice.
	UsernsOff = "off"
)

// Options configure the container security posture.
type Options struct {
	// Userns selects the user-namespace posture (UsernsAuto/Require/Off).
	//
	// This is a three-way switch rather than a bool because the failure mode
	// it guards against is a silent no-op. HostConfig.UsernsMode="private" is
	// not a request the engine can honour on demand: a daemon with no
	// userns-remap accepts the field, stores it, and returns it verbatim from
	// GET /containers/<id>/json — while the sandbox still runs with the
	// identity uid map. Setting "private" unconditionally therefore buys
	// nothing and costs a great deal: it makes the control plane report a
	// user namespace that does not exist. See Client.info/hasUserns.
	Userns string
}

func New(c *Client) *Backend { return NewWithOptions(c, Options{}) }

func NewWithOptions(c *Client, opts Options) *Backend {
	if opts.Userns == "" {
		opts.Userns = UsernsAuto
	}
	return &Backend{
		c:    c,
		opts: opts,
		// Until Init has established what this daemon can do, ask for the
		// identity map explicitly. The alternative — leaving the field unset
		// and letting the engine decide — is not neutral: an engine with
		// userns-remap configured defaults new containers to a private user
		// namespace, so an unset field silently means "private" on some hosts
		// and "host" on others. Pinning "host" keeps the unverified state
		// honest, and Init replaces it with the verified posture.
		usernsMode: UsernsModeHost,
	}
}

// Values sent in HostConfig.UsernsMode. "host" is written explicitly rather
// than omitted, because omission delegates the decision to daemon.json.
const (
	UsernsModeHost    = "host"
	UsernsModePrivate = "private"
)

func (b *Backend) Name() string { return "oci" }

// IsolationTier is honest about what a container gives you: a shared kernel.
// There is no configuration of the Docker Engine API that changes this.
func (b *Backend) IsolationTier() string { return runtime.TierContainer }

// UsernsPosture reports the decided user-namespace posture for logs and
// /readyz. Before Init it reports the conservative truth: not established.
func (b *Backend) UsernsPosture() (mode, note string) {
	if !b.probed {
		return "unverified", "user-namespace posture not probed yet"
	}
	if b.usernsMode == UsernsModePrivate {
		return "private", b.usernsNote
	}
	return "off", b.usernsNote
}

// Init probes the daemon once and fixes the user-namespace posture for the
// lifetime of this backend.
//
// Create is deliberately safe to call without it: an unprobed backend runs in
// the identity-map posture rather than optimistically asking for a namespace
// it cannot get. That ordering matters — the one thing this must never do is
// report isolation it has not established.
func (b *Backend) Init(ctx context.Context) error {
	info, err := b.c.info(ctx)
	if err != nil {
		return err
	}
	b.probed = true
	b.apparmorHost = info.hasSecurityOption("apparmor")
	available := info.hasUserns()

	switch b.opts.Userns {
	case UsernsRequire:
		if !available {
			return fmt.Errorf(
				"userns=require was set, but this docker daemon has no user-namespace remap "+
					"configured (GET /info → SecurityOptions %v contains no \"name=userns\"). "+
					"Without it, container uid 0 IS host uid 0, so a single kernel bug reaches host "+
					"root directly with no second privilege escalation — the other isolation layers "+
					"stop mattering the moment one is crossed. Add a userns-remap entry to "+
					"daemon.json and restart the daemon, or set userns=off to accept that tier "+
					"explicitly", info.SecurityOptions)
		}
		b.usernsMode = UsernsModePrivate
		b.usernsNote = "active: container uid 0 maps to an unprivileged host uid"
	case UsernsOff:
		b.usernsMode = UsernsModeHost
		b.usernsNote = "disabled by configuration (userns=off): container uid 0 is host uid 0"
	default: // UsernsAuto
		if available {
			b.usernsMode = UsernsModePrivate
			b.usernsNote = "active: container uid 0 maps to an unprivileged host uid"
		} else {
			b.usernsMode = UsernsModeHost
			b.usernsNote = "unavailable on this daemon (no userns-remap), so container uid 0 is host " +
				"uid 0; set userns=require to turn this disclosed downgrade into a startup failure"
		}
	}
	return nil
}

func (b *Backend) Ping(ctx context.Context) error { return b.c.ping(ctx) }

// ---- container API payload types -------------------------------------------

type createBody struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd"`
	Env        []string          `json:"Env,omitempty"`
	User       string            `json:"User,omitempty"`
	WorkingDir string            `json:"WorkingDir,omitempty"`
	Labels     map[string]string `json:"Labels"`
	HostConfig hostConfig        `json:"HostConfig"`
}

// hostConfig is the security contract. Every field a sandbox depends on is
// written here explicitly, even where the value matches today's engine
// default, because anything omitted is delegated to the host's daemon.json —
// a mutable file that a different engine version or a different operator can
// change without this code changing. The fields that genuinely have a
// daemon-level default capable of weakening a sandbox are IpcMode
// (default-ipc-mode), CgroupnsMode (default-cgroupns-mode), UsernsMode
// (userns-remap), Ulimits (default-ulimits), SecurityOpt's seccomp/apparmor
// entries (seccomp-profile) and Runtime (default-runtime, to be pinned when a
// runtime-selection knob lands). The rest are pinned so that the contract can
// be reviewed in one place instead of reconstructed from the engine's source.
type hostConfig struct {
	Resources    resources `json:"Resources"`
	NetworkMode  string    `json:"NetworkMode"`
	IpcMode      string    `json:"IpcMode"`
	PidMode      string    `json:"PidMode"`
	UsernsMode   string    `json:"UsernsMode"`
	CgroupnsMode string    `json:"CgroupnsMode"`
	Privileged   *bool     `json:"Privileged"`
	// ReadonlyRootfs is pinned false on purpose, not by omission. A read-only
	// root is the usual next hardening step, but it is incompatible with this
	// product's snapshot model: Commit() captures the container's writable
	// layer, so a read-only root would produce empty snapshots. Revisit only
	// together with a writable-workspace overlay design.
	ReadonlyRootfs *bool    `json:"ReadonlyRootfs"`
	CapDrop        []string `json:"CapDrop"`
	CapAdd         []string `json:"CapAdd"`
	SecurityOpt    []string `json:"SecurityOpt"`
	MaskedPaths    []string `json:"MaskedPaths"`
	ReadonlyPaths  []string `json:"ReadonlyPaths"`
	Ulimits        []ulimit `json:"Ulimits"`
	Init           *bool    `json:"Init,omitempty"`
	RestartPolicy  struct {
		Name string `json:"Name"`
	} `json:"RestartPolicy"`
	// Mounts carries the read-only host bind mounts, when the caller asked
	// for any. It is omitempty on purpose: an absent field is the engine's
	// "no mounts" default, and there is no daemon.json knob that can inject
	// a mount on its own, so nothing here needs pinning against a mutable
	// default the way UsernsMode or SecurityOpt do.
	Mounts []mountSpec `json:"Mounts,omitempty"`
}

// mountSpec is a read-only bind mount. ReadOnly is written explicitly true on
// every code path that can construct one; there is no branch that sets it
// false. Propagation rprivate keeps a mount the workload makes inside the
// sandbox from propagating back to the host's namespace.
type mountSpec struct {
	Type        string       `json:"Type"`        // "bind"
	Source      string       `json:"Source"`      // absolute host path
	Target      string       `json:"Target"`      // absolute in-sandbox path
	ReadOnly    bool         `json:"ReadOnly"`    // always true
	BindOptions *bindOptions `json:"BindOptions"` // always rprivate
}

type bindOptions struct {
	Propagation string `json:"Propagation"` // "rprivate"
}

type ulimit struct {
	Name string `json:"Name"`
	Soft int64  `json:"Soft"`
	Hard int64  `json:"Hard"`
}

type resources struct {
	NanoCPUs   int64  `json:"NanoCpus"`   // 1e9 = one core
	Memory     int64  `json:"Memory"`     // bytes
	MemorySwap int64  `json:"MemorySwap"` // == Memory → swap disabled
	PidsLimit  *int64 `json:"PidsLimit"`
}

// Paths the engine masks and makes read-only by default, pinned here. Nothing
// stops a daemon or a future engine version from changing these defaults, and
// a sandbox that can read /proc/kcore or write /proc/sys is a materially
// different sandbox.
var (
	pinnedMaskedPaths = []string{
		"/proc/asound", "/proc/acpi", "/proc/kcore", "/proc/keys",
		"/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats",
		"/proc/sched_debug", "/proc/scsi", "/sys/firmware",
		"/sys/devices/virtual/powercap",
	}
	pinnedReadonlyPaths = []string{
		"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
	}
)

// pinnedUlimits bound the two resource classes the pids and memory cgroups do
// not cover.
//
// core=0 is a data-exfiltration control rather than housekeeping: this product
// commits a sandbox's filesystem into snapshots, so a core dump writes the
// process's memory — including any credential it was holding — into an
// artifact that is then stored and handed to other sandboxes.
//
// nofile is a cross-tenant availability control: file descriptors are a
// host-global resource, so with no per-sandbox ceiling one tenant can exhaust
// the host table and starve every other tenant.
var pinnedUlimits = []ulimit{
	{Name: "core", Soft: 0, Hard: 0},
	{Name: "nofile", Soft: 1024, Hard: 65536},
}

func (b *Backend) createPayload(req runtime.CreateRequest) createBody {
	pids := int64(512)
	no := false
	secOpt := []string{"no-new-privileges"}
	if b.apparmorHost {
		// Pin the profile instead of relying on the engine to attach its
		// default one; only done when /info says AppArmor is present, because
		// naming a profile on a host without AppArmor is an error.
		secOpt = append(secOpt, "apparmor=docker-default")
	}
	mounts := bindMounts(req.Mounts)
	return createBody{
		Image:      req.Image,
		Cmd:        []string{"/bin/sh", "-c", "exec sleep infinity"}, // stable PID 1; workloads run via exec
		Env:        envSlice(req.Env),
		User:       req.User,
		WorkingDir: req.WorkDir,
		Labels:     withProduct(req.Labels),
		HostConfig: hostConfig{
			Resources: resources{
				NanoCPUs:   int64(req.CPUMillicores) * 1_000_000,
				Memory:     int64(req.MemoryMiB) << 20,
				MemorySwap: int64(req.MemoryMiB) << 20, // swap off: an OOM must hit this sandbox only, and visibly
				PidsLimit:  &pids,
			},
			NetworkMode:    "none",
			IpcMode:        "private",
			PidMode:        "",
			UsernsMode:     b.usernsMode,
			CgroupnsMode:   "private",
			Privileged:     &no,
			ReadonlyRootfs: &no,
			CapDrop:        []string{"ALL"},
			CapAdd:         []string{},
			SecurityOpt:    secOpt,
			MaskedPaths:    pinnedMaskedPaths,
			ReadonlyPaths:  pinnedReadonlyPaths,
			Ulimits:        pinnedUlimits,
			Init:           boolPtr(true), // reap zombies from workload execs
			RestartPolicy: struct {
				Name string `json:"Name"`
			}{Name: "no"},
			Mounts: mounts,
		},
	}
}

// bindMounts renders runtime mounts as read-only Docker bind mounts. ReadOnly
// is set true unconditionally — the runtime type has no writable mode, so
// there is no caller value that can flip it.
func bindMounts(in []runtime.Mount) []mountSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]mountSpec, 0, len(in))
	for _, m := range in {
		out = append(out, mountSpec{
			Type:        "bind",
			Source:      m.Source,
			Target:      m.Target,
			ReadOnly:    true,
			BindOptions: &bindOptions{Propagation: "rprivate"},
		})
	}
	return out
}

type inspectBody struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		Memory     int64 `json:"Memory"`
		MemorySwap int64 `json:"MemorySwap"`
	} `json:"HostConfig"`
	State struct {
		Status     string `json:"Status"` // created|running|paused|restarting|removing|exited|dead
		Running    bool   `json:"Running"`
		Paused     bool   `json:"Paused"`
		ExitCode   int    `json:"ExitCode"`
		OOMKilled  bool   `json:"OOMKilled"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Error      string `json:"Error"`
	} `json:"State"`
}

func (i *inspectBody) toStatus() runtime.Status {
	st := runtime.Status{
		Running:   i.State.Running,
		Paused:    i.State.Paused,
		Raw:       i.State.Status,
		ExitCode:  i.State.ExitCode,
		OOMKilled: i.State.OOMKilled,
	}
	if t, err := time.Parse(time.RFC3339Nano, i.State.StartedAt); err == nil && !t.IsZero() {
		st.StartedAt = t
	}
	return st
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// ---- lifecycle ---------------------------------------------------------------

func (b *Backend) Create(ctx context.Context, req runtime.CreateRequest) (string, error) {
	payload, _ := json.Marshal(b.createPayload(req))
	respBody, err := b.c.do(ctx, "POST", "/containers/create?name="+urlQueryEscape(req.Name), nil, bytes.NewReader(payload))
	if err != nil {
		if ae, ok := asAppErr(err); ok && ae.Code == apperr.NotFound {
			return "", apperr.Invalid("base image %q not present on this node; import a rootfs or reference an available image", req.Image)
		}
		return "", err
	}
	var cr struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(respBody, &cr); err != nil || cr.ID == "" {
		return "", apperr.InternalErr("docker create returned unusable body: %v", err)
	}
	return req.Name, nil
}

func withProduct(labels map[string]string) map[string]string {
	out := map[string]string{labelProduct: prodValue}
	for k, v := range labels {
		out[k] = v
	}
	return out
}

func boolPtr(v bool) *bool { return &v }

// Start flips the container to running and then verifies the transition.
// Docker's 204 means "request accepted": PID 1 can still die instantly
// (broken image, instant OOM) and callers must not be told "running".
func (b *Backend) Start(ctx context.Context, ref string) error {
	if _, err := b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/start", nil, nil); err != nil {
		if ae, ok := asAppErr(err); ok && ae.Code == apperr.Conflict {
			return nil // already running — idempotent
		}
		return err
	}
	deadline := time.Now().Add(startProbeMax)
	for {
		ins, err := b.inspect(ctx, ref)
		if err != nil {
			return err
		}
		switch {
		case ins.State.Running:
			return nil
		case ins.State.OOMKilled:
			return apperr.ConflictErr("sandbox process was killed by the memory limiter (OOM) on start; raise memory_mib")
		case ins.State.Status == "exited" || ins.State.Status == "dead":
			msg := fmt.Sprintf("sandbox process exited immediately with code %d", ins.State.ExitCode)
			switch {
			case ins.State.ExitCode == 137:
				msg += " (exit 137 = killed by the memory limiter; raise memory_mib)"
			case ins.State.ExitCode == 126 || ins.State.ExitCode == 127:
				msg += " (command not executable — image is missing its interpreter or linker)"
			}
			return apperr.ConflictErr("%s", msg)
		}
		if time.Now().After(deadline) {
			return apperr.Unavailable("container %s did not reach running within %s", ref, startProbeMax)
		}
		select {
		case <-ctx.Done():
			return apperr.From(ctx.Err())
		case <-time.After(startProbeDelay):
		}
	}
}

// reclaimTempLimit caps the temporary memory limit used during pause:
// frozen workloads cannot allocate, so the kernel safely evicts file pages
// and swaps anonymous pages down toward this ceiling.
const reclaimTempLimit = 64 << 20

func (b *Backend) Pause(ctx context.Context, ref string, opts runtime.PauseOptions) error {
	ins, err := b.inspect(ctx, ref)
	if err != nil {
		return err
	}
	if ins.State.Paused {
		return nil // idempotent by real phase, not by error-code guessing
	}
	if !ins.State.Running {
		return apperr.ConflictErr("cannot pause sandbox in state %q", ins.State.Status)
	}
	if _, err := b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/pause", nil, nil); err != nil {
		return err
	}
	// Best-effort reclamation while frozen (DSec §6.3): lower the memory
	// limit so the kernel evicts file pages and swaps anonymous pages.
	// Failures are non-fatal — the freeze itself is the containment; deep
	// reclaim resumes on the next host memory pressure either way.
	if opts.MemoryBytes > 0 {
		tmp := opts.MemoryBytes
		if tmp > reclaimTempLimit {
			tmp = reclaimTempLimit
		}
		payload, _ := json.Marshal(resources{
			Memory:     tmp,
			MemorySwap: tmp, // swap allowed up to the same ceiling for anon pages
		})
		if _, err := b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/update", nil, bytes.NewReader(payload)); err != nil {
			_ = err // reclaim is opportunistic; the original limit is restored on Resume
		}
	}
	return nil
}

func (b *Backend) Resume(ctx context.Context, ref string, opts runtime.PauseOptions) error {
	ins, err := b.inspect(ctx, ref)
	if err != nil {
		return err
	}
	if !ins.State.Paused {
		return nil
	}
	// The original limit MUST be restored before unfreezing, or the first
	// allocations after resume hit the temporary ceiling and OOM. Restore
	// failures are retried; if they persist, the sandbox is left frozen
	// and the error surfaces — an unpaused OOM loop is worse.
	if opts.MemoryBytes > 0 {
		payload, _ := json.Marshal(resources{
			Memory:     opts.MemoryBytes,
			MemorySwap: opts.MemoryBytes,
		})
		var lastErr error
		for attempt := 0; attempt < 5; attempt++ {
			if _, lastErr = b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/update", nil, bytes.NewReader(payload)); lastErr == nil {
				break
			}
			select {
			case <-ctx.Done():
				return apperr.From(ctx.Err())
			case <-time.After(200 * time.Millisecond):
			}
		}
		if lastErr != nil {
			return apperr.Unavailable("resume: could not restore the original memory limit (sandbox left paused): %v", lastErr)
		}
	}
	_, err = b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/unpause", nil, nil)
	return err
}

// Stop terminates the container. grace gives clean shutdown a window before
// the signal escalates; force kills immediately.
func (b *Backend) Stop(ctx context.Context, ref string, force bool) error {
	if force {
		_, err := b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/kill", nil, nil)
		if ae, ok := asAppErr(err); ok && ae.Code == apperr.NotFound {
			return nil
		}
		return err
	}
	if _, err := b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/stop?t=10", nil, nil); err != nil {
		ae, ok := asAppErr(err)
		if ok && (ae.Code == apperr.NotFound || ae.Code == apperr.Conflict) {
			return nil // gone already, or stopped already
		}
		return err
	}
	return nil
}

func (b *Backend) Remove(ctx context.Context, ref string) error {
	// A wedged container (e.g. process table exhausted) can answer removal
	// with 409 "removal already in progress" while it tears down — retry
	// briefly before giving up; force-delete must always eventually win.
	var lastErr error
	for attempt := 0; attempt < 16; attempt++ {
		_, err := b.c.do(ctx, "DELETE", "/containers/"+urlQueryEscape(ref)+"?force=true&v=true", nil, nil)
		if err == nil {
			return nil
		}
		if ae, ok := asAppErr(err); ok && ae.Code == apperr.NotFound {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return apperr.From(ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	return lastErr
}

func (b *Backend) inspect(ctx context.Context, ref string) (*inspectBody, error) {
	body, err := b.c.do(ctx, "GET", "/containers/"+urlQueryEscape(ref)+"/json", nil, nil)
	if err != nil {
		return nil, err
	}
	var ins inspectBody
	if err := json.Unmarshal(body, &ins); err != nil {
		return nil, apperr.InternalErr("docker inspect unparseable: %v", err)
	}
	return &ins, nil
}

func (b *Backend) Inspect(ctx context.Context, ref string) (runtime.Status, error) {
	ins, err := b.inspect(ctx, ref)
	if err != nil {
		return runtime.Status{}, err
	}
	return ins.toStatus(), nil
}

func (b *Backend) Rename(ctx context.Context, ref, newName string) error {
	_, err := b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/rename?name="+urlQueryEscape(newName), nil, nil)
	return err
}

// ---- exec ----------------------------------------------------------------

type execCreateBody struct {
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env,omitempty"`
	User         string   `json:"User,omitempty"`
	WorkingDir   string   `json:"WorkingDir,omitempty"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
}

func (b *Backend) Exec(ctx context.Context, ref string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	if len(req.Argv) == 0 {
		return nil, apperr.Invalid("exec requires a non-empty argv")
	}
	// Reject an out-of-contract ceiling before creating anything in Docker.
	if req.MaxOutput > runtime.MaxBackendOutput {
		return nil, apperr.Invalid("exec max_output %d exceeds the backend's hard ceiling of %d bytes",
			req.MaxOutput, runtime.MaxBackendOutput)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultExecTO
	}
	// Outer ctx covers create+inspect calls; the exec stream itself runs on
	// execCtx so its read loop unblocks exactly at the deadline instead of
	// hanging on a silent stream until the outer context expires.
	execCtx, cancelExec := context.WithTimeout(ctx, timeout)
	defer cancelExec()
	ctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()

	payload, _ := json.Marshal(execCreateBody{
		Cmd:          req.Argv,
		Env:          envSlice(req.Env),
		User:         req.User,
		WorkingDir:   req.WorkDir,
		AttachStdout: true,
		AttachStderr: true,
	})
	body, err := b.c.do(ctx, "POST", "/containers/"+urlQueryEscape(ref)+"/exec", nil, bytes.NewReader(payload))
	if err != nil {
		// A workload that exhausted the pids limit wedges the container:
		// even docker exec cannot setns to spawn. Surface an actionable
		// conflict instead of leaking OCI internals.
		if ae, ok := asAppErr(err); ok && ae.Code == apperr.Conflict &&
			(strings.Contains(ae.Message, "setns") || strings.Contains(ae.Message, "unable to start container process")) {
			return nil, apperr.ConflictErr(
				"sandbox process table is exhausted (fork bomb or process leak); " +
					"it recovers when its processes exit, or stop/delete and recreate the sandbox",
			)
		}
		return nil, err
	}
	var ec struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(body, &ec); err != nil || ec.ID == "" {
		return nil, apperr.InternalErr("docker exec create returned unusable body")
	}
	started := time.Now()
	res, execErr := b.execStart(execCtx, ec.ID, started, timeout, req.MaxOutput)
	if execErr == nil {
		// final state read gives the authoritative exit code — must happen
		// BEFORE OCI-start-failure detection, which keys on exit code 128
		if ins, err := b.execInspect(ctx, ec.ID); err == nil && ins.ExitCode != nil {
			res.ExitCode = *ins.ExitCode
		}
		// Docker reports exec-START failures (OCI runtime errors) as a
		// normal stream with exit code 128 and the error in the output —
		// recognize them and translate into actionable conflicts.
		combined := string(res.Stdout) + string(res.Stderr)
		if res.ExitCode == 128 && strings.Contains(combined, "OCI runtime exec failed") {
			if strings.Contains(combined, "setns") || strings.Contains(combined, "unable to start container process") {
				execErr = apperr.ConflictErr(
					"sandbox process table is exhausted (fork bomb or process leak); " +
						"it recovers when its processes exit, or stop/delete and recreate the sandbox",
				)
			} else {
				execErr = apperr.ConflictErr("process could not be started in the sandbox: %s",
					strings.TrimSpace(strings.SplitN(combined, "\n", 2)[0]))
			}
		}
	}
	if execErr != nil {
		// best-effort kill so a timed-out process does not keep burning quota
		_, _ = b.c.do(context.Background(), "DELETE", "/exec/"+urlQueryEscape(ec.ID), nil, nil)
		return nil, execErr
	}
	res.Duration = time.Since(started)
	return res, nil
}

type execStartBody struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}

func (b *Backend) execStart(execCtx context.Context, execID string, started time.Time, timeout time.Duration, maxOut int) (*runtime.ExecResult, error) {
	payload, _ := json.Marshal(execStartBody{})
	resp, closer, err := b.c.raw(execCtx, "POST", "/exec/"+urlQueryEscape(execID)+"/start", nil, bytes.NewReader(payload),
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return nil, err
	}
	defer closer()

	cap := int64(maxOut)
	if cap <= 0 {
		cap = runtime.DefaultOutputCap
	}
	// Defence in depth. The service layer is the authority on the ceiling, but
	// this backend is also driven directly by tests and future callers, and a
	// forgotten admission check must not turn into a multi-gigabyte heap
	// allocation here (that is exactly how PoC-4 worked).
	if cap > runtime.MaxBackendOutput {
		return nil, apperr.Invalid("exec max_output %d exceeds the backend's hard ceiling of %d bytes",
			cap, runtime.MaxBackendOutput)
	}
	deadline := started.Add(timeout)
	res := &runtime.ExecResult{}
	var stdout, stderr bytes.Buffer

	buf := make([]byte, 64*1024)
	for {
		// The deadline is enforced by execCtx cancelling the underlying
		// read; check it here too for the no-data fast path.
		if execCtx.Err() != nil || time.Now().After(deadline) {
			res.TimedOut = true
			res.Stdout = stdout.Bytes()
			res.Stderr = stderr.Bytes()
			res.Truncated = stdout.Len() >= int(cap) || stderr.Len() >= int(cap)
			return res, nil
		}
		header := make([]byte, 8)
		if _, err := io.ReadFull(resp.Body, header); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break // stream finished
			}
			if execCtx.Err() != nil {
				res.TimedOut = true
				res.Stdout = stdout.Bytes()
				res.Stderr = stderr.Bytes()
				return res, nil
			}
			return nil, apperr.Unavailable("docker exec stream: %v", err)
		}
		streamType := header[0]
		size := int64(header[4])<<24 | int64(header[5])<<16 | int64(header[6])<<8 | int64(header[7])
		if size <= 0 {
			continue
		}
		dst := &stdout
		if streamType == 2 {
			dst = &stderr
		}
		for remaining := size; remaining > 0; {
			n := int64(len(buf))
			if n > remaining {
				n = remaining
			}
			nr, err := io.ReadFull(resp.Body, buf[:n])
			if nr > 0 {
				remaining -= int64(nr)
				if int64(dst.Len()) < cap {
					room := cap - int64(dst.Len())
					if room > int64(nr) {
						room = int64(nr)
					}
					dst.Write(buf[:room])
				}
				if int64(dst.Len()) >= cap {
					res.Truncated = true
				}
			}
			if err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					return nil, apperr.Unavailable("docker exec stream cut off")
				}
				return nil, apperr.Unavailable("docker exec stream: %v", err)
			}
		}
	}
	res.Stdout = stdout.Bytes()
	res.Stderr = stderr.Bytes()
	return res, nil
}

func (b *Backend) execInspect(ctx context.Context, execID string) (*struct {
	Running  *bool `json:"Running"`
	ExitCode *int  `json:"ExitCode"`
}, error) {
	body, err := b.c.do(ctx, "GET", "/exec/"+urlQueryEscape(execID)+"/json", nil, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Running  *bool `json:"Running"`
		ExitCode *int  `json:"ExitCode"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, apperr.InternalErr("docker exec inspect unparseable: %v", err)
	}
	return &out, nil
}

// ---- images & snapshots ------------------------------------------------------

func (b *Backend) Commit(ctx context.Context, ref, tag string) (string, int64, error) {
	if _, err := b.c.do(ctx, "POST",
		"/commit?container="+urlQueryEscape(ref)+"&repo="+urlQueryEscape(tag), nil, nil); err != nil {
		return "", 0, err
	}
	size, err := b.imageSize(ctx, tag)
	if err != nil {
		return tag, 0, err
	}
	return tag, size, nil
}

func (b *Backend) HasImage(ctx context.Context, imageRef string) (bool, error) {
	_, err := b.c.do(ctx, "GET", "/images/"+urlQueryEscape(imageRef)+"/json", nil, nil)
	if err != nil {
		if ae, ok := asAppErr(err); ok && ae.Code == apperr.NotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (b *Backend) RemoveImage(ctx context.Context, imageRef string) error {
	_, err := b.c.do(ctx, "DELETE", "/images/"+urlQueryEscape(imageRef)+"?force=true", nil, nil)
	if ae, ok := asAppErr(err); ok && ae.Code == apperr.NotFound {
		return nil
	}
	return err
}

func (b *Backend) imageSize(ctx context.Context, tag string) (int64, error) {
	body, err := b.c.do(ctx, "GET", "/images/"+urlQueryEscape(tag)+"/json", nil, nil)
	if err != nil {
		return 0, err
	}
	var im struct {
		Size int64 `json:"Size"`
	}
	if err := json.Unmarshal(body, &im); err != nil {
		return 0, apperr.InternalErr("image inspect unparseable: %v", err)
	}
	return im.Size, nil
}

// Orphans returns every container labeled as ours, regardless of state.
func (b *Backend) Orphans(ctx context.Context) ([]runtime.Orphan, error) {
	filters := fmt.Sprintf(`{"label":["%s=%s"]}`, labelProduct, prodValue)
	body, err := b.c.do(ctx, "GET", "/containers/json", map[string]string{"all": "1", "filters": filters}, nil)
	if err != nil {
		return nil, err
	}
	var list []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		State  string            `json:"State"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, apperr.InternalErr("docker container list unparseable: %v", err)
	}
	out := make([]runtime.Orphan, 0, len(list))
	for _, it := range list {
		ref := it.ID
		if len(it.Names) > 0 && len(it.Names[0]) > 1 {
			ref = strings.TrimPrefix(it.Names[0], "/")
		}
		ins, err := b.inspect(ctx, it.ID)
		if err != nil {
			continue
		}
		out = append(out, runtime.Orphan{
			Ref:    ref,
			Labels: it.Labels,
			Status: ins.toStatus(),
		})
	}
	return out, nil
}

// managedImagePrefixes are the repository names this product commits to. The
// reference filter below is what keeps the call cheap on a shared daemon: a
// node may hold thousands of unrelated images, and enumerating them all on
// every reconcile tick would be a self-inflicted load spike.
var managedImagePrefixes = []string{"sandboxd-snap-", "sandboxd-tpl-"}

func (b *Backend) ManagedImages(ctx context.Context) ([]string, error) {
	patterns := make([]string, 0, len(managedImagePrefixes))
	for _, p := range managedImagePrefixes {
		patterns = append(patterns, p+"*")
	}
	raw, err := json.Marshal(map[string][]string{"reference": patterns})
	if err != nil {
		return nil, apperr.InternalErr("encode image filters: %v", err)
	}
	body, err := b.c.do(ctx, "GET", "/images/json", map[string]string{"filters": string(raw)}, nil)
	if err != nil {
		return nil, err
	}
	var list []struct {
		RepoTags []string `json:"RepoTags"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, apperr.InternalErr("docker image list unparseable: %v", err)
	}
	out := make([]string, 0, len(list))
	for _, im := range list {
		for _, tag := range im.RepoTags {
			if tag == "<none>:<none>" {
				continue
			}
			out = append(out, tag)
		}
	}
	return out, nil
}
