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

	// auto_resume: requests to a paused sandbox (exec/files) transparently
	// resume it first (DSec §6.3 semantics). Default true.
	AutoResume bool
	// pause_reclaim: on pause, temporarily lower the container memory limit
	// so the kernel reclaims file pages and swaps anonymous pages while the
	// workload is frozen; restored before unpause. Default true.
	PauseReclaim bool

	Defaults domain.TenantQuota // platform default quotas/rates

	MaxRequestBody       int64
	MaxFileBytes         int64
	MaxExecTimeout       time.Duration
	MaxConcurrentCreates int
	CreateQueueTimeout   time.Duration

	PoolEnabled bool
	PoolSize    int // warm sandboxes per template

	SweepEvery         time.Duration
	ReconcileEvery     time.Duration
	CompactEvery       time.Duration
	KeepEvents         int
	BootstrapAdminKey  string // pre-seeded admin key (else generated once)
	BootstrapImage     string // seed a default template from this image if present
	BootstrapTemplates []TemplateSeed
}

type TemplateSeed struct {
	Name     string
	ImageRef string
}

func Default() *Config {
	return &Config{
		Listen:       "127.0.0.1:8080",
		DataDir:      "./data",
		LogLevel:     "info",
		LogAccessLog: true,
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
		MaxConcurrentCreates: 8,
		CreateQueueTimeout:   15 * time.Second,
		PoolEnabled:          true,
		PoolSize:             2,
		SweepEvery:           30 * time.Second,
		ReconcileEvery:       60 * time.Second,
		CompactEvery:         10 * time.Minute,
		KeepEvents:           100_000,
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
	cfg.applyEnv()
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
	case "quota.max_sandboxes":
		return setInt(&c.Defaults.MaxSandboxes)
	case "quota.max_cpu_millicores":
		return setInt(&c.Defaults.MaxCPUMillicores)
	case "quota.max_memory_mib":
		return setInt(&c.Defaults.MaxMemoryMiB)
	case "quota.max_snapshots":
		return setInt(&c.Defaults.MaxSnapshots)
	case "max_request_body_bytes":
		return setInt64(&c.MaxRequestBody)
	case "max_file_bytes":
		return setInt64(&c.MaxFileBytes)
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

func (c *Config) applyEnv() {
	env := func(k string) (string, bool) {
		v, ok := os.LookupEnv("SBXD_" + k)
		return v, ok && v != ""
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
	if v, ok := env("BOOTSTRAP_ADMIN_KEY"); ok {
		c.BootstrapAdminKey = v
	}
	if v, ok := env("ENFORCE_DISK_QUOTA"); ok {
		c.EnforceDiskQuota = v == "true" || v == "1"
	}
	if v, ok := env("AUTO_RESUME"); ok {
		c.AutoResume = v == "true" || v == "1"
	}
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
	if !c.EnforceDiskQuota {
		// accepted-risk configurations must be a conscious choice; the
		// message lands in the startup log via Validate warnings.
		fmt.Fprintln(os.Stderr, "WARNING: enforce_disk_quota=false — per-sandbox disk usage is NOT enforced; one tenant can fill the shared filesystem")
	}
	return nil
}
