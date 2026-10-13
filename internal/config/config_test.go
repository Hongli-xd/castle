package config

// Tests for the two knobs whose absence made a documented promise untrue:
//   - MaxExecOutput: the exec output ceiling. Without it, max_output was an
//     unbounded request field, so one request could make the daemon allocate
//     gigabytes (poc/probe_live.sh, PoC-4).
//   - Isolation: the tier requirement. Without it, the daemon could not be told
//     that microVM-grade isolation was mandatory, so it always silently ran
//     shared-kernel containers.
//
// Both are configuration-boundary tests: a limit that the loader will not
// enforce or will silently ignore is not a limit.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAreValid(t *testing.T) {
	if _, err := Load(""); err != nil {
		t.Fatalf("the built-in defaults must pass their own validation: %v", err)
	}
}

func TestMaxExecOutputHasUsableBounds(t *testing.T) {
	cfg := Default()
	if cfg.MaxExecOutput <= 0 {
		t.Fatal("the default exec output ceiling must be a real ceiling")
	}

	for _, tc := range []struct {
		name string
		v    int64
		ok   bool
	}{
		{"zero would mean unlimited", 0, false},
		{"negative", -1, false},
		{"absurd", 1 << 40, false},
		{"tiny", 16, false},
		{"reasonable", 4 << 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.MaxExecOutput = tc.v
			err := c.validate()
			if tc.ok && err != nil {
				t.Fatalf("value %d should be accepted: %v", tc.v, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("value %d should be rejected", tc.v)
			}
		})
	}
}

func TestExecOutputCeilingIsLoadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sandboxd.conf")
	if err := os.WriteFile(path, []byte("max_exec_output_bytes = 2097152\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MaxExecOutput != 2097152 {
		t.Fatalf("config file value ignored: got %d", cfg.MaxExecOutput)
	}
}

func TestIsolationRequirementIsValidated(t *testing.T) {
	for _, tc := range []struct {
		v  string
		ok bool
	}{
		{IsolationAuto, true},
		{IsolationContainer, true},
		{IsolationMicroVM, true},
		{"", false},
		{"vm", false},
		{"MICROVM", false}, // must go through the case-folding loader path
	} {
		c := Default()
		c.Isolation = tc.v
		if err := c.validate(); (err == nil) != tc.ok {
			t.Fatalf("isolation=%q: got err=%v, want accepted=%v", tc.v, err, tc.ok)
		}
	}
}

func TestUsernsPostureIsValidated(t *testing.T) {
	for _, tc := range []struct {
		v  string
		ok bool
	}{
		{UsernsAuto, true},
		{UsernsRequire, true},
		{UsernsOff, true},
		{"", false},
		{"true", false},  // tempting, and wrong: the posture is not a boolean
		{"false", false}, // because "off" is not the same as "not requested"
		{"REQUIRE", false},
	} {
		c := Default()
		c.Userns = tc.v
		if err := c.validate(); (err == nil) != tc.ok {
			t.Fatalf("userns=%q: got err=%v, want accepted=%v", tc.v, err, tc.ok)
		}
	}
}

// userns=require must be expressible in a config file and in the environment,
// or an operator cannot demand that the daemon refuse to run without it.
func TestUsernsCanBeRequiredFromFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sandboxd.conf")
	if err := os.WriteFile(path, []byte("userns = require\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Userns != UsernsRequire {
		t.Fatalf("got %q, want require", cfg.Userns)
	}

	t.Setenv("SBXD_USERNS", "OFF")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("load with env override: %v", err)
	}
	if cfg.Userns != UsernsOff {
		t.Fatalf("env should normalise and win over the file: got %q, want off", cfg.Userns)
	}
}

// The default must be the posture that discloses rather than the one that
// assumes: auto enables userns when the daemon can provide it and says so when
// it cannot, which is the only setting that cannot mislead an operator who
// never edited the file.
func TestDefaultUsernsPostureIsAuto(t *testing.T) {
	if got := Default().Userns; got != UsernsAuto {
		t.Fatalf("default userns = %q, want %q", got, UsernsAuto)
	}
}

// "microvm is required" has to be expressible in a config file, or the daemon
// can never refuse to downgrade.
func TestIsolationCanBeRequiredFromFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sandboxd.conf")
	if err := os.WriteFile(path, []byte("isolation = microvm\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Isolation != IsolationMicroVM {
		t.Fatalf("got %q, want microvm", cfg.Isolation)
	}

	// Case and whitespace are normalised, not rejected.
	if err := os.WriteFile(path, []byte("isolation = MICROVM \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Isolation != IsolationMicroVM {
		t.Fatalf("got %q, want microvm", cfg.Isolation)
	}

	t.Setenv("SBXD_ISOLATION", "container")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Isolation != IsolationContainer {
		t.Fatalf("env override ignored: got %q", cfg.Isolation)
	}
}

func TestWALRepairPolicyIsValidated(t *testing.T) {
	for _, tc := range []struct {
		v  string
		ok bool
	}{
		{WALRepairTailOnly, true},
		{WALRepairSalvage, true},
		{WALRepairStrict, true},
		{"", false},
		{"truncate", false},
	} {
		c := Default()
		c.WALRepair = tc.v
		if err := c.validate(); (err == nil) != tc.ok {
			t.Fatalf("wal_repair=%q: got err=%v, want accepted=%v", tc.v, err, tc.ok)
		}
	}
}

// A typo in an environment limit must be an error. Silently keeping the default
// is how an operator ends up believing a limit is in force for months when it
// never was.
func TestMalformedNumericEnvIsAnError(t *testing.T) {
	t.Setenv("SBXD_MAX_EXEC_OUTPUT_BYTES", "8MiB")
	_, err := Load("")
	if err == nil {
		t.Fatal("a non-numeric SBXD_MAX_EXEC_OUTPUT_BYTES must fail startup, not be ignored")
	}
	if !strings.Contains(err.Error(), "SBXD_MAX_EXEC_OUTPUT_BYTES") {
		t.Fatalf("error should name the offending variable: %v", err)
	}
}

func TestPreAuthDefaultsAreUsable(t *testing.T) {
	c := Default()
	if c.PreAuth.PerSecond <= 0 || c.PreAuth.Burst < 1 {
		t.Fatalf("the pre-auth gate must be enabled by default, got %+v", c.PreAuth)
	}
	// It is a flood gate, not a business quota: it must sit well above the
	// per-tenant rates or legitimate single-origin traffic would trip it.
	for group, r := range c.Defaults.Rates {
		if c.PreAuth.PerSecond <= r.PerSecond {
			t.Fatalf("pre-auth rate %v is not above the %s tenant rate %v — legitimate clients would be throttled",
				c.PreAuth.PerSecond, group, r.PerSecond)
		}
	}
}
