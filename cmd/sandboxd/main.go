// sandboxd is a single-binary, multi-tenant agent sandbox runtime.
//
// Boot order matters: backend ping, then store (crash-safe), then reconcile
// (converge whatever the last run left mid-flight), then the HTTP listener.
// The admin bootstrap key is generated once and printed exactly once.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sandcastle.dev/sandcastle/internal/config"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/httpapi"
	"sandcastle.dev/sandcastle/internal/metrics"
	"sandcastle.dev/sandcastle/internal/runtime"
	"sandcastle.dev/sandcastle/internal/runtime/fc"
	"sandcastle.dev/sandcastle/internal/runtime/oci"
	"sandcastle.dev/sandcastle/internal/service"
	"sandcastle.dev/sandcastle/internal/store"
)

var version = "1.1.6"

func logf(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "%s level=%s msg=%q\n", time.Now().UTC().Format(time.RFC3339Nano), level, msg)
}

func main() {
	configPath := flag.String("config", "", "path to config file (key = value)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("sandboxd", version)
		return
	}

	if err := run(*configPath); err != nil {
		logf("fatal", "sandboxd exited: %v", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	logf("info", "sandboxd %s starting; listen=%s data_dir=%s isolation=%s",
		version, cfg.Listen, cfg.DataDir, cfg.Isolation)

	backend, err := buildBackend(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()

	st, err := store.Open(cfg.DataDir, store.Options{
		MaxEvents: cfg.KeepEvents,
		Repair:    repairPolicy(cfg.WALRepair),
	})
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()

	// Recovery is never silent. A torn tail is normal crash debris and is
	// reported as such; anything more serious refuses to start inside
	// store.Open, so if we get here the data is accounted for.
	if rep := st.Repair(); rep.Damaged {
		logf("warn", "wal recovery: %s", rep.String())
	} else if rep.Action == "none" {
		logf("info", "wal recovery: intact (%d frames replayed)", rep.FramesKept)
	}

	reg := metrics.New()
	svc := service.New(st, backend, cfg, reg)

	if err := bootstrapAdmin(st, svc, cfg); err != nil {
		return err
	}
	bootstrapTemplates(ctx, st, backend, cfg)

	// Prove the isolation before serving the first request: a claim that only
	// appears in the log once traffic is flowing is a claim nobody checked.
	if err := isolationSelfCheck(cfg, svc); err != nil {
		return err
	}

	svc.StartLoops()
	// converge whatever the previous process left mid-flight, before the
	// first request can observe a stale state; at boot nothing is in flight
	svc.ReconcileAtBoot(ctx)
	logf("info", "reconciliation complete: %s", st.Stats().String())

	api := httpapi.New(svc, cfg)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// exec streams are long-lived; per-request contexts govern them
		WriteTimeout:   0,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		logf("info", "http listening on %s (tls=%v)", cfg.Listen, cfg.TLSCertFile != "")
		if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
			srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			errCh <- srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
			return
		}
		errCh <- srv.ListenAndServe()
	}()

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sig:
		logf("info", "signal %s: draining", s)
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	svc.Stop()
	logf("info", "sandboxd stopped cleanly")
	return nil
}

// buildBackend resolves the isolation tier the operator asked for against what
// this host can actually deliver, and refuses the mismatch.
//
// v1.1.0 had no selection at all: it logged a warning when /dev/kvm existed and
// then unconditionally returned the container backend, so the microVM backend
// was unreachable at runtime and nothing outside the process could tell. An
// operator who chose this product *because* it promised microVM isolation got
// shared-kernel containers with a log line they never read.
func buildBackend(cfg *config.Config) (runtime.Backend, error) {
	switch cfg.Isolation {
	case config.IsolationMicroVM:
		if !fc.Available() {
			return nil, fmt.Errorf(
				"isolation=microvm was required but this host exposes no /dev/kvm; refusing to start " +
					"rather than silently serving weaker isolation. Deploy on Linux with KVM, or set " +
					"isolation=container to accept the shared-kernel tier explicitly")
		}
		// KVM is present but the driver is not implemented in this build.
		// The honest answer is to refuse, not to start anyway and hope nobody
		// checks: docs/DESIGN.md §3.5 calls this the honesty contract.
		return nil, fmt.Errorf(
			"isolation=microvm was required and /dev/kvm is present, but the %s driver is still a "+
				"compile-checked skeleton in this build (it returns unimplemented for every operation, "+
				"and this daemon has no backend-selection path to it). No microVM isolation exists yet. "+
				"Set isolation=container to make that trade-off explicit",
			fc.New().Name())

	case config.IsolationContainer:
		return buildOCIBackend(cfg)

	default: // IsolationAuto
		if fc.Available() {
			logf("warn", "this host has /dev/kvm, but the firecracker driver is a skeleton in this "+
				"build, so auto-selection stays on containers (shared kernel). Set isolation=microvm "+
				"to turn this silent-looking fallback into a startup failure")
		}
		return buildOCIBackend(cfg)
	}
}

// repairPolicy maps the config string onto the store's recovery policy. config
// deliberately does not import store, so the translation lives here.
func repairPolicy(s string) store.RepairPolicy {
	switch s {
	case config.WALRepairSalvage:
		return store.RepairSalvage
	case config.WALRepairStrict:
		return store.RepairStrict
	default:
		return store.RepairTailOnly
	}
}

func buildOCIBackend(cfg *config.Config) (runtime.Backend, error) {
	client, err := oci.NewClient(cfg.DockerHost)
	if err != nil {
		return nil, fmt.Errorf("runtime backend: %w", err)
	}
	b := oci.NewWithOptions(client, oci.Options{Userns: cfg.Userns})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.Ping(ctx); err != nil {
		return nil, fmt.Errorf("runtime backend unreachable at boot: %w", err)
	}
	// Init reads the daemon's own /info and fixes the user-namespace posture
	// for the process lifetime. This is what makes userns=require meaningful:
	// a container engine accepts and echoes back security settings it cannot
	// honour, so the capability must be established from the daemon rather
	// than requested from it and assumed.
	if err := b.Init(ctx); err != nil {
		return nil, err
	}
	mode, note := b.UsernsPosture()
	logf("info", "backend=%s isolation=%s userns=%s ready", b.Name(), b.IsolationTier(), mode)
	logf("info", "userns posture: %s", note)
	if mode != "private" {
		logf("warn", "USER NAMESPACE OFF: uid 0 inside a sandbox is uid 0 on this host, so one kernel "+
			"bug reaches host root with no second privilege escalation and the other container controls "+
			"(dropped capabilities, seccomp, LSM) stop mattering the moment it is crossed. Set "+
			"userns=require to make this a startup failure, or configure userns-remap in daemon.json")
	}
	if b.IsolationTier() != runtime.TierMicroVM {
		logf("warn", "ISOLATION TIER: sandboxes share this host's kernel. This tier is appropriate "+
			"for trusted or semi-trusted tenants; it is NOT adequate for running arbitrary untrusted "+
			"code. Verify at any time with GET /readyz → .isolation")
	}
	return b, nil
}

// isolationSelfCheck asks the backend to prove its isolation on this host and
// records the evidence where /readyz and the admin API can serve it.
//
// The whole point is that configuration is not evidence. Every container
// setting this daemon writes is a request; only what the kernel reports from
// inside a sandbox is a fact. So the check is run against a sandbox built by
// the identical payload path production uses, and its findings are logged
// individually — including the ones that pass, because a report that only
// speaks up when something is wrong cannot be distinguished from a report
// that never ran.
func isolationSelfCheck(cfg *config.Config, svc *service.Service) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	rep, err := svc.Backend.VerifyIsolation(ctx, cfg.BootstrapImage)
	if err != nil {
		if cfg.Userns == config.UsernsRequire {
			return fmt.Errorf("userns=require demands proof of isolation, but the self-check could "+
				"not run: %w", err)
		}
		logf("warn", "isolation self-check could not run (%v) — the isolation reported at /readyz is "+
			"configuration, not verified evidence", err)
		return nil
	}
	svc.SetIsolationReport(rep)

	logf("info", "isolation self-check in %s: tier=%s userns=%s verified=%v",
		rep.Image, rep.Tier, rep.Userns, rep.OK())
	for _, f := range rep.Facts {
		state := "ok  "
		if !f.OK {
			state = "FAIL"
		}
		logf("info", "  isolation %s %-13s = %-20q want %s", state, f.Name, f.Observed, f.Want)
	}
	if !rep.OK() {
		if cfg.Userns == config.UsernsRequire {
			return fmt.Errorf("userns=require was set and the isolation self-check failed: %s",
				strings.Join(rep.Failures, "; "))
		}
		logf("warn", "isolation self-check failed: %s", strings.Join(rep.Failures, "; "))
	}
	return nil
}

// bootstrapAdmin ensures exactly one live admin key exists. The plaintext is
// either operator-provided via config or generated once and printed — once.
func bootstrapAdmin(st *store.Store, svc *service.Service, cfg *config.Config) error {
	for _, k := range st.ListKeys("") {
		if k.Scope == domain.ScopeAdmin && k.Valid(time.Now()) {
			return nil
		}
	}
	var plaintext string
	var err error
	// Only a key this process GENERATED is echoed. A key the operator
	// supplied in configuration is already in the config file, and stdout
	// goes to the journal — printing it again would leave a live
	// platform-admin credential in a log with a retention policy nobody
	// chose. Found by the end-to-end suite, which asserts that no credential
	// reaches the daemon's output.
	generated := false
	if cfg.BootstrapAdminKey != "" {
		plaintext = cfg.BootstrapAdminKey
		k := domain.NewAPIKey("", "bootstrap", domain.ScopeAdmin)
		k.Prefix = plaintext[:min2(16, len(plaintext))]
		sum := sha256.Sum256([]byte(plaintext))
		k.Hash = base64.RawURLEncoding.EncodeToString(sum[:])
		err = st.PutKey(k)
	} else {
		plaintext, _, err = svc.Auth.MintKey("", "bootstrap-admin", domain.ScopeAdmin, 0)
		generated = true
	}
	if err != nil {
		return err
	}
	if !generated {
		logf("info", "bootstrap admin key taken from configuration; not printed")
		return nil
	}
	fmt.Println("===============================================")
	fmt.Println("sandboxd ADMIN API KEY (printed once; store it):")
	fmt.Println()
	fmt.Println("  " + plaintext)
	fmt.Println()
	fmt.Println("===============================================")
	return nil
}

// bootstrapTemplates registers a default public template when the node has
// the configured image. Missing image = no template, the correct air-gapped
// default; operators then import rootfs via POST /v1/templates/import.
func bootstrapTemplates(ctx context.Context, st *store.Store, backend runtime.Backend, cfg *config.Config) {
	if cfg.BootstrapImage == "" {
		return
	}
	for _, tpl := range st.ListTemplates("") {
		if tpl.ImageRef == cfg.BootstrapImage {
			return // already seeded
		}
	}
	ok, err := backend.HasImage(ctx, cfg.BootstrapImage)
	if err != nil || !ok {
		return
	}
	tpl := domain.NewTemplate("", "default")
	tpl.ImageRef = cfg.BootstrapImage
	tpl.Public = true
	tpl.DefaultSpec = domain.SandboxSpec{
		CPUMillicores:  cfg.DefaultCPUMillicores,
		MemoryMiB:      cfg.DefaultMemoryMiB,
		TimeoutSeconds: cfg.DefaultTimeoutSeconds,
	}
	if err := st.PutTemplate(tpl); err == nil {
		logf("info", "seeded default template %s from image %s", tpl.ID, cfg.BootstrapImage)
	}
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
