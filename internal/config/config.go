// Package config loads daemon configuration: built-in defaults, then an
// optional key=value file, then SBXD_* environment overrides. The file
// format is deliberately minimal (no third-party parser, no format zoo):
//
//	listen = ":8080"
//	data_dir = "/var/lib/sandboxd"
//	limits.max_cpu_millicores = 8000
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
)

type Config struct {
	Listen       string
	TLSCertFile  string
	TLSKeyFile   string
	DataDir      string
	DockerHost   string
	LogLevel     string // debug|info|warn|error
	LogAccessLog bool

	// Isolation selects the runtime backend tier the operator requires:
	//
	//	"auto"       (default) use the strongest tier this host can actually
	//	             run, and log which one that is
	//	"container"  require the shared-kernel container backend
	//	"microvm"    require the KVM microVM backend; refuse to start if this
	//	             host cannot provide it
	//
	// The point of "microvm" is that it cannot silently degrade. In v1.1.0 the
	// Firecracker backend was unreachable at runtime and the daemon merely
	// logged a warning before falling back to containers, so an operator could
	// believe they were running microVMs when every sandbox shared the host
	// kernel.
	Isolation string

	// Userns selects the user-namespace posture for container sandboxes:
	//
	//	"auto"     (default) enable it when this docker daemon can actually
	//	           provide it, and disclose the outcome in the startup log
	//	           and at /readyz
	//	"require"  refuse to start when it cannot be provided
	//	"off"      accept the shared-identity tier explicitly
	//
	// Without a user namespace, uid 0 inside a sandbox IS uid 0 on the host,
	// so a single kernel bug reaches host root with no second privilege
	// escalation, and every other container-level control (dropped
	// capabilities, seccomp, the LSM profile) stops mattering the moment one
	// is crossed. It is a three-way switch rather than a bool because the
	// failure it guards against is a silent no-op: the Engine API accepts
	// UsernsMode="private", stores it, and echoes it back even when the daemon
	// has no remap configured, while the sandbox keeps the identity uid map.
	Userns string

	// Resource ceilings a single spec may request (platform-wide).
	Limits domain.Limits
	// Defaults for specs the caller leaves unspecified.
	DefaultCPUMillicores      int
	DefaultMemoryMiB          int
	DefaultTimeoutSeconds     int
	DefaultIdleTimeoutSeconds int // 0 = idle reclamation off unless a spec sets it

	// enforce_disk_quota: overlayfs/ext4 cannot enforce per-sandbox disk
	// caps; when true, creates that request disk_mib are REFUSED unless the
	// operator sets enforce_disk_quota=false to accept the documented risk.
	EnforceDiskQuota bool

	// MountAllowlist is the set of host directories a tenant MAY mount
	// read-only into a sandbox. It is the boundary that keeps the read-only
	// mount feature from becoming a whole-node read: without it, any tenant
	// could bind /etc, /root/.ssh or /var/run into a sandbox and read it.
	// Empty (the default) means mounts are disabled entirely — the isolation
	// contract's "no host directory is mounted" canary stays true by default.
	MountAllowlist []string

	// auto_resume: requests to a paused sandbox (exec/files) transparently
	// resume it first (DSec §6.3 semantics). Default true.
	AutoResume bool
	// pause_reclaim: on pause, temporarily lower the container memory limit
	// so the kernel reclaims file pages and swaps anonymous pages while the
	// workload is frozen; restored before unpause. Default true.
	PauseReclaim bool

	Defaults domain.TenantQuota // platform default quotas/rates

	// PreAuth is the coarse flood gate applied to every request before
	// authentication, keyed by client IP. It exists because the per-tenant
	// quota cannot apply to a caller who has not authenticated yet: in v1.1.0
	// the only limiter ran inside authenticate, so rejected requests were
	// unbounded and each one cost an fsync.
	//
	// What it defends against: one origin flooding the control plane. What it
	// does NOT defend against: a distributed flood, which needs an edge layer
	// that this process cannot provide.
	//
	// It is deliberately generous, because a shared NAT egress (a CI pool, an
	// office) puts many legitimate clients behind one IP and they all share
	// this budget. The per-tenant rates are the real business quota and are
	// two orders of magnitude tighter. It is also credential-blind by design:
	// exempting requests that carry a key would let an attacker send a forged
	// one to skip the gate.
	//
	// Because the key is RemoteAddr and nothing else, X-Forwarded-For cannot
	// be used to rotate identities. Deployments behind a proxy should
	// terminate and re-originate client connections there.
	PreAuth domain.Rate

	MaxRequestBody int64
	MaxFileBytes   int64
	MaxExecTimeout time.Duration
	// MaxExecOutput is the hard ceiling, in bytes, for one exec's combined
	// stdout+stderr. A request asking for more is rejected at admission.
	// The request field is a *ceiling*, not a preference: the control plane
	// buffers exec output in its own memory, so an unbounded value let one
	// unauthenticated-adjacent request make the daemon allocate gigabytes
	// (poc/probe_live.sh, PoC-4). A request of 0 means "platform default",
	// which the backend renders as 1 MiB — it does not mean "unlimited".
	MaxExecOutput        int64
	MaxConcurrentCreates int
	CreateQueueTimeout   time.Duration

	PoolEnabled bool
	PoolSize    int // warm sandboxes per template

	SweepEvery     time.Duration
	ReconcileEvery time.Duration
	CompactEvery   time.Duration
	// MeterEvery is the sample-and-flush cadence for per-tenant usage
	// metering (resource-seconds + exec counters). 0 disables the metering
	// loop entirely; the endpoint still serves the last durable record.
	MeterEvery time.Duration
	KeepEvents int
	// WALRepair decides what recovery does with a damaged write-ahead log:
	//
	//	"tail-only" (default) repair a torn tail, but refuse to start when the
	//	            damage is mid-log — those frames were acknowledged and
	//	            fsynced, so dropping them needs a human decision
	//	"salvage"   always truncate at the first damaged frame, accepting the
	//	            loss of everything after it, and report how much
	//	"strict"    refuse to start on any damage, torn tail included
	WALRepair          string
	BootstrapAdminKey  string // pre-seeded admin key (else generated once)
	BootstrapImage     string // seed a default template from this image if present
	BootstrapTemplates []TemplateSeed
}

type TemplateSeed struct {
	Name     string
	ImageRef string
}

// Isolation requirements accepted by Config.Isolation.
const (
	IsolationAuto      = "auto"
	IsolationContainer = "container"
	IsolationMicroVM   = "microvm"
)

// WAL recovery policies accepted by Config.WALRepair. These mirror
// store.RepairPolicy; config stays free of a store import so the two layers
// remain independently replaceable.
const (
	WALRepairTailOnly = "tail-only"
	WALRepairSalvage  = "salvage"
	WALRepairStrict   = "strict"
)

// User-namespace postures accepted by Config.Userns. These mirror
// oci.Options.Userns; config stays free of a runtime import for the same
// reason it stays free of a store import.
const (
	UsernsAuto    = "auto"
	UsernsRequire = "require"
	UsernsOff     = "off"
)

func Default() *Config {
	return &Config{
		Listen:       "127.0.0.1:8080",
		DataDir:      "./data",
		LogLevel:     "info",
		LogAccessLog: true,
		Isolation:    IsolationAuto,
		Userns:       UsernsAuto,
		Limits: domain.Limits{
			MaxCPUMillicores:  8000,
			MaxMemoryMiB:      16384,
			MaxDiskMiB:        51200,
			MaxTimeoutSeconds: 86400,
			MaxEnvEntries:     50,
			MaxEnvValueBytes:  8192,
			MaxMetadataKeys:   20,
			MaxMetadataBytes:  1024,
		},
		DefaultCPUMillicores:      1000,
		DefaultMemoryMiB:          512,
		DefaultTimeoutSeconds:     3600,
		DefaultIdleTimeoutSeconds: 0,
		EnforceDiskQuota:          true,
		AutoResume:                true,
		PauseReclaim:              true,
		Defaults: domain.TenantQuota{
			MaxSandboxes:     10,
			MaxCPUMillicores: 8000,
			MaxMemoryMiB:     8192,
			MaxSnapshots:     20,
			MaxSnapshotBytes: 20 << 30,
			Rates: map[string]*domain.Rate{
				domain.GroupCreate:   {PerSecond: 1, Burst: 5},
				domain.GroupExec:     {PerSecond: 10, Burst: 20},
				domain.GroupFiles:    {PerSecond: 20, Burst: 40},
				domain.GroupSnapshot: {PerSecond: 0.5, Burst: 2},
				domain.GroupAdmin:    {PerSecond: 1, Burst: 5},
				domain.GroupRead:     {PerSecond: 20, Burst: 40},
			},
		},
		MaxRequestBody:       1 << 20,
		MaxFileBytes:         100 << 20,
		MaxExecTimeout:       30 * time.Minute,
		MaxExecOutput:        8 << 20, // 8 MiB; the backend default is 1 MiB
		MaxConcurrentCreates: 8,
		CreateQueueTimeout:   15 * time.Second,
		WALRepair:            WALRepairTailOnly,
		// 200 rps with a burst of 500 per client IP. High enough that a shared
		// NAT egress is not throttled, low enough that one host cannot flood
		// the control plane. The tenant rates below are 10-200x tighter and
		// remain the authoritative business quota.
		PreAuth:        domain.Rate{PerSecond: 200, Burst: 500},
		PoolEnabled:    true,
		PoolSize:       2,
		SweepEvery:     30 * time.Second,
		ReconcileEvery: 60 * time.Second,
		CompactEvery:   10 * time.Minute,
		MeterEvery:     30 * time.Second,
		KeepEvents:     100_000,
	}
}

// Load applies a config file (optional) and environment overrides (optional)
// on top of defaults. Both are additive; missing sources are not errors.
func Load(file string) (*Config, error) {
	cfg := Default()
	if file != "" {
		if err := cfg.applyFile(file); err != nil {
			return nil, err
		}
	}
	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return apperr.Invalid("open config %s: %v", path, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		eq := strings.IndexByte(text, '=')
		if eq <= 0 {
			return apperr.Invalid("config %s:%d: expected key = value", path, line)
		}
		key := strings.TrimSpace(text[:eq])
		val := strings.Trim(strings.TrimSpace(text[eq+1:]), `"'`)
		if err := c.set(key, val); err != nil {
			return apperr.Invalid("config %s:%d: %v", path, line, err)
		}
	}
	return sc.Err()
}

func (c *Config) set(key, val string) error {
	setInt := func(dst *int) error {
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("key %q: not a number: %q", key, val)
		}
		*dst = n
		return nil
	}
	setInt64 := func(dst *int64) error {
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return fmt.Errorf("key %q: not a number: %q", key, val)
		}
		*dst = n
		return nil
	}
	setFloat := func(dst *float64) error {
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return fmt.Errorf("key %q: not a number: %q", key, val)
		}
		*dst = f
		return nil
	}
	setDur := func(dst *time.Duration) error {
		d, err := time.ParseDuration(val)
		if err != nil {
			return fmt.Errorf("key %q: not a duration: %q", key, val)
		}
		*dst = d
		return nil
	}
	switch key {
	case "listen":
		c.Listen = val
	case "tls_cert":
		c.TLSCertFile = val
	case "tls_key":
		c.TLSKeyFile = val
	case "data_dir":
		c.DataDir = val
	case "docker_host":
		c.DockerHost = val
	case "log_level":
		c.LogLevel = val
	case "isolation":
		c.Isolation = strings.ToLower(strings.TrimSpace(val))
	case "userns":
		c.Userns = strings.ToLower(strings.TrimSpace(val))
	case "wal_repair":
		c.WALRepair = strings.ToLower(strings.TrimSpace(val))
	case "access_log":
		c.LogAccessLog = val == "true" || val == "1"
	case "limits.max_cpu_millicores":
		return setInt(&c.Limits.MaxCPUMillicores)
	case "limits.max_memory_mib":
		return setInt(&c.Limits.MaxMemoryMiB)
	case "limits.max_disk_mib":
		return setInt(&c.Limits.MaxDiskMiB)
	case "limits.max_timeout_seconds":
		return setInt(&c.Limits.MaxTimeoutSeconds)
	case "defaults.cpu_millicores":
		return setInt(&c.DefaultCPUMillicores)
	case "defaults.memory_mib":
		return setInt(&c.DefaultMemoryMiB)
	case "defaults.timeout_seconds":
		return setInt(&c.DefaultTimeoutSeconds)
	case "defaults.idle_timeout_seconds":
		return setInt(&c.DefaultIdleTimeoutSeconds)
	case "auto_resume":
		c.AutoResume = val == "true" || val == "1"
	case "pause_reclaim":
		c.PauseReclaim = val == "true" || val == "1"
	case "enforce_disk_quota":
		c.EnforceDiskQuota = val == "true" || val == "1"
	case "mount.allow":
		// Comma-separated host directories tenants may mount read-only.
		// Repeatable; a host path containing a comma cannot be expressed,
		// which is an accepted limitation of the minimal config format.
		for _, p := range strings.Split(val, ",") {
			if p = strings.TrimSpace(p); p != "" {
				c.MountAllowlist = append(c.MountAllowlist, p)
			}
		}
	case "quota.max_sandboxes":
		return setInt(&c.Defaults.MaxSandboxes)
	case "quota.max_cpu_millicores":
		return setInt(&c.Defaults.MaxCPUMillicores)
	case "quota.max_memory_mib":
		return setInt(&c.Defaults.MaxMemoryMiB)
	case "quota.max_snapshots":
		return setInt(&c.Defaults.MaxSnapshots)
	case "preauth.per_second":
		return setFloat(&c.PreAuth.PerSecond)
	case "preauth.burst":
		return setInt(&c.PreAuth.Burst)
	case "max_request_body_bytes":
		return setInt64(&c.MaxRequestBody)
	case "max_file_bytes":
		return setInt64(&c.MaxFileBytes)
	case "max_exec_output_bytes":
		return setInt64(&c.MaxExecOutput)
	case "max_concurrent_creates":
		return setInt(&c.MaxConcurrentCreates)
	case "pool.enabled":
		c.PoolEnabled = val == "true" || val == "1"
	case "pool.size":
		return setInt(&c.PoolSize)
	case "sweep_every":
		return setDur(&c.SweepEvery)
	case "reconcile_every":
		return setDur(&c.ReconcileEvery)
	case "compact_every":
		return setDur(&c.CompactEvery)
	case "meter.every":
		return setDur(&c.MeterEvery)
	case "keep_events":
		return setInt(&c.KeepEvents)
	case "bootstrap.image":
		c.BootstrapImage = val
	case "bootstrap_admin_key":
		c.BootstrapAdminKey = val
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return nil
}

// applyEnv applies SBXD_* overrides. A malformed value is an error, never a
// silent fallback to the default: an operator who typo'd a limit must find out
// at startup, not discover months later that the limit was never in effect.
func (c *Config) applyEnv() error {
	env := func(k string) (string, bool) {
		v, ok := os.LookupEnv("SBXD_" + k)
		return v, ok && v != ""
	}
	setInt64Env := func(k string, dst *int64) error {
		v, ok := env(k)
		if !ok {
			return nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return apperr.Invalid("SBXD_%s=%q is not a number", k, v)
		}
		*dst = n
		return nil
	}
	if v, ok := env("LISTEN"); ok {
		c.Listen = v
	}
	if v, ok := env("DATA_DIR"); ok {
		c.DataDir = v
	}
	if v, ok := env("DOCKER_HOST"); ok {
		c.DockerHost = v
	}
	if v, ok := env("LOG_LEVEL"); ok {
		c.LogLevel = v
	}
	if v, ok := env("ISOLATION"); ok {
		c.Isolation = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := env("USERNS"); ok {
		c.Userns = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := env("WAL_REPAIR"); ok {
		c.WALRepair = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := env("BOOTSTRAP_ADMIN_KEY"); ok {
		c.BootstrapAdminKey = v
	}
	if v, ok := env("ENFORCE_DISK_QUOTA"); ok {
		c.EnforceDiskQuota = v == "true" || v == "1"
	}
	if v, ok := env("MOUNT_ALLOW"); ok {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				c.MountAllowlist = append(c.MountAllowlist, p)
			}
		}
	}
	if v, ok := env("AUTO_RESUME"); ok {
		c.AutoResume = v == "true" || v == "1"
	}
	return setInt64Env("MAX_EXEC_OUTPUT_BYTES", &c.MaxExecOutput)
}

func (c *Config) validate() error {
	if c.Listen == "" {
		return apperr.Invalid("listen must not be empty")
	}
	if c.DataDir == "" {
		return apperr.Invalid("data_dir must not be empty")
	}
	if c.DefaultCPUMillicores <= 0 || c.DefaultMemoryMiB <= 0 || c.DefaultTimeoutSeconds < 30 {
		return apperr.Invalid("default spec values are out of range")
	}
	if c.Limits.MaxCPUMillicores < c.DefaultCPUMillicores {
		return apperr.Invalid("limits.max_cpu_millicores is below the default spec")
	}
	if c.MaxConcurrentCreates < 1 {
		return apperr.Invalid("max_concurrent_creates must be >= 1")
	}
	if c.MaxExecOutput < 4*1024 || c.MaxExecOutput > 1<<30 {
		return apperr.Invalid("max_exec_output_bytes must be between 4096 and %d", 1<<30)
	}
	if c.PreAuth.PerSecond <= 0 || c.PreAuth.Burst < 1 {
		return apperr.Invalid("preauth.per_second must be > 0 and preauth.burst must be >= 1")
	}
	switch c.Isolation {
	case IsolationAuto, IsolationContainer, IsolationMicroVM:
	default:
		return apperr.Invalid("isolation must be one of %q, %q, %q (got %q)",
			IsolationAuto, IsolationContainer, IsolationMicroVM, c.Isolation)
	}
	switch c.WALRepair {
	case WALRepairTailOnly, WALRepairSalvage, WALRepairStrict:
	default:
		return apperr.Invalid("wal_repair must be one of %q, %q, %q (got %q)",
			WALRepairTailOnly, WALRepairSalvage, WALRepairStrict, c.WALRepair)
	}
	if c.WALRepair == WALRepairSalvage {
		fmt.Fprintln(os.Stderr, "WARNING: wal_repair=salvage — mid-log WAL damage will be truncated "+
			"automatically, discarding acknowledged transactions. Use only to recover a node you "+
			"cannot otherwise bring back.")
	}
	switch c.Userns {
	case UsernsAuto, UsernsRequire, UsernsOff:
	default:
		return apperr.Invalid("userns must be one of %q, %q, %q (got %q)",
			UsernsAuto, UsernsRequire, UsernsOff, c.Userns)
	}
	if c.Userns == UsernsOff {
		fmt.Fprintln(os.Stderr, "WARNING: userns=off — sandboxes share this host's uid space, so uid 0 "+
			"inside a sandbox is uid 0 on the host and a single kernel bug reaches host root directly. "+
			"Acceptable only when every tenant is inside one trust domain.")
	}
	if c.Userns == UsernsRequire && c.Isolation == IsolationMicroVM {
		fmt.Fprintln(os.Stderr, "NOTE: userns=require has no effect on the microvm tier, which already "+
			"gives each sandbox its own kernel; the setting is reported as unverified there.")
	}
	if !c.EnforceDiskQuota {
		// accepted-risk configurations must be a conscious choice; the
		// message lands in the startup log via Validate warnings.
		fmt.Fprintln(os.Stderr, "WARNING: enforce_disk_quota=false — per-sandbox disk usage is NOT enforced; one tenant can fill the shared filesystem")
	}
	for _, p := range c.MountAllowlist {
		if !strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			return apperr.Invalid("mount.allow entries must be absolute host paths without '..': %q", p)
		}
	}
	return nil
}
