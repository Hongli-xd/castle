// Package service is the orchestration layer: it owns the sandbox state
// machine, tenant quotas, snapshots, the warm pool, and crash recovery.
// It is the ONLY component allowed to change domain state; the HTTP layer
// only authenticates, rate-limits, and parses.
package service

import (
	"context"
	"sync"
	"time"

	"sandcastle.dev/sandcastle/internal/config"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/gov"
	"sandcastle.dev/sandcastle/internal/metrics"
	"sandcastle.dev/sandcastle/internal/runtime"
	"sandcastle.dev/sandcastle/internal/store"
)

type Service struct {
	St      *store.Store
	Backend runtime.Backend
	Auth    *gov.Authenticator
	Quotas  *gov.Quotas
	Limiter *gov.Limiter
	Audit   *gov.Audit
	Cfg     *config.Config
	Reg     *metrics.Registry

	createSem chan struct{}
	poolMu    sync.Mutex
	pool      map[string][]poolItem // imageRef → warm items
	stopCh    chan struct{}
	stopOnce  sync.Once
}

func New(st *store.Store, backend runtime.Backend, cfg *config.Config, reg *metrics.Registry) *Service {
	s := &Service{
		St:        st,
		Backend:   backend,
		Auth:      gov.NewAuthenticator(st),
		Quotas:    gov.NewQuotas(st, cfg.Defaults),
		Limiter:   gov.NewLimiter(),
		Audit:     gov.NewAudit(st),
		Cfg:       cfg,
		Reg:       reg,
		createSem: make(chan struct{}, cfg.MaxConcurrentCreates),
		stopCh:    make(chan struct{}),
	}
	return s
}

// Start launches the background loops: TTL sweeper, reconciler, pool
// maintenance, and WAL compaction.
func (s *Service) StartLoops() {
	go s.sweepLoop()
	go s.reconcileLoop()
	go s.compactLoop()
	if s.Cfg.PoolEnabled && s.Cfg.PoolSize > 0 {
		go s.poolLoop()
	}
}

func (s *Service) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
}

func (s *Service) Limits() domain.Limits { return s.Cfg.Limits }

// ---- create ------------------------------------------------------------------

type CreateRequest struct {
	TemplateID  string             `json:"template_id,omitempty"`
	SnapshotID  string             `json:"snapshot_id,omitempty"`
	BaseImage   string             `json:"base_image,omitempty"`
	Spec        domain.SandboxSpec `json:"spec"`
	Metadata    map[string]string  `json:"metadata,omitempty"`
	Wait        bool               `json:"-"`
	WaitTimeout time.Duration      `json:"-"`
}

// CreateSandbox admits the request against governance, records the intent
// (state=provisioning), then realizes it on the backend. A crash anywhere
// in between leaves a transitional record that the reconciler converges —
// intent-first persistence is what makes crash recovery decidable.
func (s *Service) CreateSandbox(ctx context.Context, tenant *domain.Tenant, key *domain.APIKey, req CreateRequest) (*domain.Sandbox, error) {
	if tenant.Status == domain.TenantSuspended {
		return nil, errSuspended()
	}

	imageRef, defaults, err := s.resolveImage(ctx, tenant, req)
	if err != nil {
		return nil, err
	}
	spec := mergeSpec(defaults, req.Spec, s.Cfg)
	if err := spec.Validate(s.Cfg.Limits, 0); err != nil {
		return nil, err
	}
	if err := validateMetadata(req.Metadata, s.Cfg.Limits); err != nil {
		return nil, err
	}
	if req.Spec.DiskMiB > 0 && s.Cfg.EnforceDiskQuota {
		return nil, diskQuotaRefused()
	}
	if err := runtime.CheckEgress(s.Backend, spec.Egress); err != nil {
		return nil, err
	}
	if err := s.Quotas.CheckCreate(tenant, spec); err != nil {
		return nil, err
	}

	sb := domain.NewSandbox(tenant.ID)
	sb.Spec = spec
	sb.Metadata = req.Metadata
	sb.ExpiresAt = time.Now().UTC().Add(time.Duration(spec.TimeoutSeconds) * time.Second)
	switch {
	case req.TemplateID != "":
		sb.TemplateID = req.TemplateID
	case req.SnapshotID != "":
		sb.SnapshotID = req.SnapshotID
	default:
		sb.BaseImage = imageRef
	}

	// warm-pool fast path: a pristine started container is adopted instead
	// of being provisioned from scratch
	if _, ok := s.poolAdopt(sb, imageRef); ok {
		if err := s.St.PutSandbox(sb); err != nil {
			_ = s.Backend.Remove(ctx, sb.Backend.Ref)
			return nil, err
		}
		s.recordCreateMetrics("pool")
		_ = s.Audit.Record(tenant.ID, sb.ID, key.ID, domain.ActSandboxCreate, map[string]string{"source": "pool", "image": imageRef})
		return sb, nil
	}

	if err := s.St.PutSandbox(sb); err != nil {
		return nil, err
	}
	_ = s.Audit.Record(tenant.ID, sb.ID, key.ID, domain.ActSandboxCreate, map[string]string{"source": "cold", "image": imageRef})
	go s.provision(sb.ID, imageRef)

	if req.Wait {
		waitTimeout := req.WaitTimeout
		if waitTimeout <= 0 {
			waitTimeout = 120 * time.Second
		}
		if err := s.waitForState(ctx, sb.ID, waitTimeout, domain.StateRunning, domain.StateFailed); err != nil {
			return nil, err
		}
		final, _ := s.St.GetSandbox(sb.ID)
		if final.State == domain.StateFailed {
			return final, &SandboxFailedError{reason: final.Reason}
		}
		return final, nil
	}
	return sb, nil
}

// resolveImage maps the request onto a concrete backend image plus spec
// defaults. Exactly one of template/snapshot/base_image must be given.
func (s *Service) resolveImage(ctx context.Context, tenant *domain.Tenant, req CreateRequest) (string, *domain.SandboxSpec, error) {
	n := 0
	for _, v := range []bool{req.TemplateID != "", req.SnapshotID != "", req.BaseImage != ""} {
		if v {
			n++
		}
	}
	if n == 0 {
		return "", nil, errMissing("one of template_id, snapshot_id, base_image is required")
	}
	if n > 1 {
		return "", nil, errMissing("template_id, snapshot_id and base_image are mutually exclusive")
	}

	switch {
	case req.TemplateID != "":
		if err := idgenCheck("tpl", req.TemplateID); err != nil {
			return "", nil, err
		}
		tpl, ok := s.St.GetTemplate(req.TemplateID)
		if !ok {
			return "", nil, errMissing("template not found")
		}
		if !tpl.Public && tpl.TenantID != tenant.ID {
			return "", nil, errForbidden()
		}
		return tpl.ImageRef, &tpl.DefaultSpec, nil
	case req.SnapshotID != "":
		if err := idgenCheck("snp", req.SnapshotID); err != nil {
			return "", nil, err
		}
		snap, ok := s.St.GetSnapshot(req.SnapshotID)
		if !ok {
			return "", nil, errMissing("snapshot not found")
		}
		if snap.TenantID != tenant.ID {
			return "", nil, errForbidden()
		}
		return snap.ImageRef, &snap.Spec, nil
	default:
		ok, err := s.Backend.HasImage(ctx, req.BaseImage)
		if err != nil {
			return "", nil, err
		}
		if !ok {
			return "", nil, errMissing("base image %q is not present on this node (import a rootfs or ask the operator to pull it)", req.BaseImage)
		}
		return req.BaseImage, &domain.SandboxSpec{}, nil
	}
}

// mergeSpec layers explicit request fields over defaults: zero fields inherit.
func mergeSpec(defaults *domain.SandboxSpec, req domain.SandboxSpec, cfg *config.Config) domain.SandboxSpec {
	out := *defaults
	if out.CPUMillicores == 0 {
		out.CPUMillicores = cfg.DefaultCPUMillicores
	}
	if out.MemoryMiB == 0 {
		out.MemoryMiB = cfg.DefaultMemoryMiB
	}
	if out.TimeoutSeconds == 0 {
		out.TimeoutSeconds = cfg.DefaultTimeoutSeconds
	}
	if req.CPUMillicores != 0 {
		out.CPUMillicores = req.CPUMillicores
	}
	if req.MemoryMiB != 0 {
		out.MemoryMiB = req.MemoryMiB
	}
	if req.DiskMiB != 0 {
		out.DiskMiB = req.DiskMiB
	}
	if req.TimeoutSeconds != 0 {
		out.TimeoutSeconds = req.TimeoutSeconds
	}
	if req.User != "" {
		out.User = req.User
	}
	if req.WorkDir != "" {
		out.WorkDir = req.WorkDir
	}
	if len(req.Env) > 0 {
		out.Env = req.Env
	}
	if req.Egress.Mode != "" {
		out.Egress = req.Egress
	}
	return out
}

// provision realizes a recorded intent on the backend. All failures converge
// the record to failed with a sanitized reason — never a dangling "running".
func (s *Service) provision(sbID, imageRef string) {
	select {
	case s.createSem <- struct{}{}:
	case <-time.After(s.Cfg.CreateQueueTimeout):
		s.failSandbox(sbID, "create queue timeout — node is saturated")
		return
	}
	defer func() { <-s.createSem }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cur, ok := s.St.GetSandbox(sbID)
	if !ok || cur.State != domain.StateProvisioning {
		return // deleted or already converged meanwhile
	}
	name := "sandboxd-" + sbID
	ref, err := s.Backend.Create(ctx, runtime.CreateRequest{
		Name:          name,
		Image:         imageRef,
		CPUMillicores: cur.Spec.CPUMillicores,
		MemoryMiB:     cur.Spec.MemoryMiB,
		Env:           cur.Spec.Env,
		User:          cur.Spec.User,
		WorkDir:       cur.Spec.WorkDir,
		Labels: map[string]string{
			"sandbox.id":     sbID,
			"sandbox.tenant": cur.TenantID,
		},
	})
	if err != nil {
		s.failSandbox(sbID, "backend create: "+err.Error())
		return
	}
	if _, err := s.St.UpdateSandbox(sbID, cur.Version, func(x *domain.Sandbox) error {
		x.Backend = domain.SandboxBackendRef{Kind: s.Backend.Name(), Ref: ref, ImageRef: imageRef}
		return nil
	}); err != nil {
		_ = s.Backend.Remove(context.Background(), ref)
		return
	}
	if err := s.Backend.Start(ctx, ref); err != nil {
		_ = s.Backend.Remove(context.Background(), ref)
		s.failSandbox(sbID, "backend start: "+err.Error())
		return
	}
	if _, err := s.St.UpdateSandbox(sbID, -1, func(x *domain.Sandbox) error {
		return x.Transition(domain.StateRunning)
	}); err != nil {
		return
	}
	s.recordCreateMetrics("cold")
}

func (s *Service) failSandbox(sbID, reason string) {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	_, err := s.St.UpdateSandbox(sbID, -1, func(x *domain.Sandbox) error {
		if x.State.Terminal() {
			return nil // someone else converged it; keep their truth
		}
		// the reconciler and the provision path both converge to failed from
		// any live state; the state machine's legal-transition table governs
		// client paths, while this is internal truth repair
		x.State = domain.StateFailed
		x.Reason = sanitize(reason)
		return nil
	})
	if err == nil {
		_ = s.Audit.Record("", sbID, "system", domain.ActReconcileFix, map[string]string{"note": "provision failed: " + sanitize(reason)})
	}
}

// waitForState polls the store until the sandbox reaches one of the target
// states or the deadline passes. Polling (vs. pub/sub) is deliberate: the
// store is the single source of truth and this cannot miss a transition.
func (s *Service) waitForState(ctx context.Context, id string, maxWait time.Duration, targets ...domain.SandboxState) error {
	deadline := maxWait
	if ctxDeadline, ok := ctx.Deadline(); ok && time.Until(ctxDeadline) < deadline {
		deadline = time.Until(ctxDeadline)
	}
	if deadline > 180*time.Second {
		deadline = 180 * time.Second
	}
	targetSet := map[domain.SandboxState]bool{}
	for _, t := range targets {
		targetSet[t] = true
	}
	after := time.After(deadline)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-after:
			return errCreateTimeout(deadline)
		case <-ctx.Done():
			return errFromCtx(ctx)
		case <-tick.C:
			sb, ok := s.St.GetSandbox(id)
			if ok && targetSet[sb.State] {
				return nil
			}
		}
	}
}

func (s *Service) recordCreateMetrics(source string) {
	s.Reg.Counter("sandboxd_creates_total", "sandbox creates by source", []string{"source"}, 1)
	_ = source
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			out = append(out, '?')
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}
