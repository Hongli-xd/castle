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

var version = "1.0.0"

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
	logf("info", "sandboxd %s starting; listen=%s data_dir=%s", version, cfg.Listen, cfg.DataDir)

	if fc.Available() {
		logf("warn", "KVM present but the firecracker backend is a skeleton in this build; using oci backend (docs/DESIGN.md §3.5)")
	}
	backend, err := buildBackend(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()

	st, err := store.Open(cfg.DataDir, store.Options{MaxEvents: cfg.KeepEvents})
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()

	reg := metrics.New()
	svc := service.New(st, backend, cfg, reg)

	if err := bootstrapAdmin(st, svc, cfg); err != nil {
		return err
	}
	bootstrapTemplates(ctx, st, backend, cfg)

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

func buildBackend(cfg *config.Config) (runtime.Backend, error) {
	client, err := oci.NewClient(cfg.DockerHost)
	if err != nil {
		return nil, fmt.Errorf("runtime backend: %w", err)
	}
	b := oci.New(client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Ping(ctx); err != nil {
		return nil, fmt.Errorf("runtime backend unreachable at boot: %w", err)
	}
	logf("info", "backend=oci ready")
	return b, nil
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
	if cfg.BootstrapAdminKey != "" {
		plaintext = cfg.BootstrapAdminKey
		k := domain.NewAPIKey("", "bootstrap", domain.ScopeAdmin)
		k.Prefix = plaintext[:min2(16, len(plaintext))]
		sum := sha256.Sum256([]byte(plaintext))
		k.Hash = base64.RawURLEncoding.EncodeToString(sum[:])
		err = st.PutKey(k)
	} else {
		plaintext, _, err = svc.Auth.MintKey("", "bootstrap-admin", domain.ScopeAdmin, 0)
	}
	if err != nil {
		return err
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
