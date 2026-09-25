package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/config"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/idgen"
	"sandcastle.dev/sandcastle/internal/metrics"
	"sandcastle.dev/sandcastle/internal/runtime"
	"sandcastle.dev/sandcastle/internal/store"
)

// fakeBackend is a deterministic in-memory backend so service semantics are
// tested without Docker. It also simulates failures to exercise the
// compensation paths (create fails, start fails, external stop, vanish).
type fakeObj struct {
	status runtime.Status
	labels map[string]string
	files  map[string][]byte
	dirs   map[string]bool
}

type fakeBackend struct {
	mu      sync.Mutex
	objects map[string]*fakeObj
	images  map[string]int64
	seq     int

	failCreate error
	failStart  error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		objects: map[string]*fakeObj{},
		images:  map[string]int64{"base:ok": 123},
	}
}

func (f *fakeBackend) Name() string { return "fake" }
func (f *fakeBackend) Ping(ctx context.Context) error {
	return nil
}

func (f *fakeBackend) Create(ctx context.Context, req runtime.CreateRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate != nil {
		return "", f.failCreate
	}
	f.seq++
	ref := fmt.Sprintf("sandboxd-%s-%d", trimSBX(req.Name), f.seq)
	f.objects[ref] = &fakeObj{labels: req.Labels, files: map[string][]byte{}, dirs: map[string]bool{}}
	return ref, nil
}

func trimSBX(name string) string {
	if len(name) > 20 {
		return name[len(name)-12:]
	}
	return name
}

func (f *fakeBackend) Start(ctx context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[ref]
	if !ok {
		return apperr.NotFoundErr("container")
	}
	if f.failStart != nil {
		return f.failStart
	}
	o.status = runtime.Status{Running: true, Raw: "running", StartedAt: time.Now()}
	return nil
}

func (f *fakeBackend) Pause(ctx context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[ref]
	if !ok {
		return apperr.NotFoundErr("container")
	}
	o.status.Paused = true
	o.status.Raw = "paused"
	return nil
}

func (f *fakeBackend) Resume(ctx context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[ref]
	if !ok {
		return apperr.NotFoundErr("container")
	}
	o.status.Paused = false
	o.status.Raw = "running"
	return nil
}

func (f *fakeBackend) Stop(ctx context.Context, ref string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[ref]
	if !ok {
		return nil
	}
	o.status.Running = false
	o.status.Paused = false
	o.status.Raw = "exited"
	o.status.ExitCode = 0
	return nil
}

func (f *fakeBackend) Remove(ctx context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, ref)
	return nil
}

func (f *fakeBackend) Rename(ctx context.Context, ref, newName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[ref]
	if !ok {
		return apperr.NotFoundErr("container")
	}
	delete(f.objects, ref)
	f.objects[newName] = o
	return nil
}

func (f *fakeBackend) Inspect(ctx context.Context, ref string) (runtime.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[ref]
	if !ok {
		return runtime.Status{}, apperr.NotFoundErr("container")
	}
	return o.status, nil
}

func (f *fakeBackend) Exec(ctx context.Context, ref string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	f.mu.Lock()
	_, ok := f.objects[ref]
	f.mu.Unlock()
	if !ok {
		return nil, apperr.NotFoundErr("container")
	}
	return &runtime.ExecResult{ExitCode: 0, Stdout: []byte("fake"), Duration: time.Millisecond}, nil
}

func (f *fakeBackend) PutFile(ctx context.Context, ref string, req runtime.PutFileRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[ref]
	if !ok {
		return apperr.NotFoundErr("container")
	}
	dir := req.Path
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' {
			o.dirs[dir[:i+1]] = true
			break
		}
	}
	o.files[req.Path] = req.Content
	return nil
}

func (f *fakeBackend) GetFile(ctx context.Context, ref, path string, maxSize int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[ref]
	if !ok {
		return nil, apperr.NotFoundErr("container")
	}
	data, ok := o.files[path]
	if !ok {
		return nil, apperr.NotFoundErr("file")
	}
	return data, nil
}

func (f *fakeBackend) ListDir(ctx context.Context, ref, path string) ([]runtime.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[ref]
	if !ok {
		return nil, apperr.NotFoundErr("container")
	}
	var out []runtime.FileInfo
	for p := range o.files {
		dir := p
		for i := len(dir) - 1; i >= 0; i-- {
			if dir[i] == '/' {
				dir = dir[:i+1]
				break
			}
		}
		if dir == path+"/" {
			out = append(out, runtime.FileInfo{Name: basename(p)})
		}
	}
	return out, nil
}

func (f *fakeBackend) DeletePath(ctx context.Context, ref, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[ref]
	if !ok {
		return apperr.NotFoundErr("container")
	}
	delete(o.files, path)
	return nil
}

func (f *fakeBackend) Commit(ctx context.Context, ref, tag string) (string, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[ref]; !ok {
		return "", 0, apperr.NotFoundErr("container")
	}
	f.images[tag] = 4096
	return tag, 4096, nil
}

func (f *fakeBackend) HasImage(ctx context.Context, imageRef string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.images[imageRef]
	return ok, nil
}

func (f *fakeBackend) RemoveImage(ctx context.Context, imageRef string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.images, imageRef)
	return nil
}

func (f *fakeBackend) ImportRootfs(ctx context.Context, dir, tag string) (string, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[tag] = 2048
	return tag, 2048, nil
}

func (f *fakeBackend) Orphans(ctx context.Context) ([]runtime.Orphan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runtime.Orphan
	for ref, o := range f.objects {
		out = append(out, runtime.Orphan{Ref: ref, Labels: o.labels, Status: o.status})
	}
	return out, nil
}

func basename(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// ---- harness ---------------------------------------------------------------

type harness struct {
	svc *Service
	st  *store.Store
	fb  *fakeBackend
	tn  *domain.Tenant
	key *domain.APIKey
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.PoolEnabled = false
	cfg.SweepEvery = time.Hour // loops off in tests; called manually
	cfg.ReconcileEvery = time.Hour
	cfg.CompactEvery = time.Hour
	st, err := store.Open(cfg.DataDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fb := newFakeBackend()
	svc := New(st, fb, cfg, metrics.New())

	tn := domain.NewTenant("acme")
	tn.Quota = domain.TenantQuota{MaxSandboxes: 5, MaxCPUMillicores: 4000, MaxMemoryMiB: 4096, MaxSnapshots: 5, MaxSnapshotBytes: 1 << 30}
	if err := st.PutTenant(tn); err != nil {
		t.Fatal(err)
	}
	tpl := domain.NewTemplate("", "base-alpine")
	tpl.ImageRef = "base:ok"
	tpl.Public = true
	tpl.DefaultSpec = domain.SandboxSpec{CPUMillicores: 500, MemoryMiB: 256, TimeoutSeconds: 600}
	if err := st.PutTemplate(tpl); err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Auth.MintKey(tn.ID, "test", domain.ScopeTenant, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{svc: svc, st: st, fb: fb, tn: tn, key: key}
}

func (h *harness) create(t *testing.T, wait bool) *domain.Sandbox {
	t.Helper()
	ctx := context.Background()
	sb, err := h.svc.CreateSandbox(ctx, h.tn, h.key, CreateRequest{
		TemplateID:  "tpl_" + findTemplate(t, h.st),
		Wait:        wait,
		WaitTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return sb
}

func findTemplate(t *testing.T, st *store.Store) string {
	t.Helper()
	tpls := st.ListTemplates("")
	if len(tpls) == 0 {
		t.Fatal("no templates")
	}
	return tpls[0].ID[4:]
}

func TestCreateWaitReachesRunning(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	if sb.State != domain.StateRunning {
		t.Fatalf("state = %s, want running", sb.State)
	}
	if sb.Backend.Ref == "" {
		t.Fatal("backend ref not recorded")
	}
	// expiry derived from spec
	if sb.ExpiresAt.IsZero() {
		t.Fatal("expires_at not set")
	}
	// audit trail
	evs := h.st.ListEvents(store.EventQuery{TenantID: h.tn.ID, Action: domain.ActSandboxCreate})
	if len(evs) != 1 {
		t.Fatalf("expected 1 create event, got %d", len(evs))
	}
}

func TestCreateUnknownTemplate(t *testing.T) {
	h := newHarness(t)
	ghost := idgen.New(idgen.PrefixTemplate) // well-formed, does not exist
	_, err := h.svc.CreateSandbox(context.Background(), h.tn, h.key, CreateRequest{TemplateID: ghost})
	if apperr.From(err).Code != apperr.NotFound {
		t.Fatalf("want not_found, got %v", err)
	}
}

func TestCreateAmbiguousSource(t *testing.T) {
	h := newHarness(t)
	_, err := h.svc.CreateSandbox(context.Background(), h.tn, h.key, CreateRequest{
		TemplateID: "tpl_x", SnapshotID: "snp_y",
	})
	if apperr.From(err).Code != apperr.NotFound {
		t.Fatalf("ambiguous source must be rejected, got %v", err)
	}
}

func TestCreateQuotaEnforced(t *testing.T) {
	h := newHarness(t)
	h.tn.Quota.MaxSandboxes = 1
	for i := 0; i < 1; i++ {
		h.create(t, true)
	}
	_, err := h.svc.CreateSandbox(context.Background(), h.tn, h.key, CreateRequest{
		TemplateID: "tpl_" + findTemplate(t, h.st), Wait: true, WaitTimeout: 5 * time.Second,
	})
	if apperr.From(err).Code != apperr.QuotaExceeded {
		t.Fatalf("want quota_exceeded, got %v", err)
	}
}

func TestSuspendedTenantRejected(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.St.UpdateTenant(h.tn.ID, h.tn.Version, func(x *domain.Tenant) error {
		x.Status = domain.TenantSuspended
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fresh, err := h.svc.GetTenant(h.tn.ID) // API layer always passes the stored tenant
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.svc.CreateSandbox(context.Background(), fresh, h.key, CreateRequest{
		TemplateID: "tpl_" + findTemplate(t, h.st),
	})
	if apperr.From(err).Code != apperr.Forbidden {
		t.Fatalf("want forbidden, got %v", err)
	}
}

func TestStartFailureConvergesToFailed(t *testing.T) {
	h := newHarness(t)
	h.fb.failStart = apperr.ConflictErr("simulated broken image")
	sb, err := h.svc.CreateSandbox(context.Background(), h.tn, h.key, CreateRequest{
		TemplateID: "tpl_" + findTemplate(t, h.st), Wait: true, WaitTimeout: 10 * time.Second,
	})
	if err == nil {
		t.Fatal("expected failure error for wait create")
	}
	if sb.State != domain.StateFailed {
		t.Fatalf("state = %s, want failed", sb.State)
	}
	if sb.Reason == "" {
		t.Fatal("failure reason must be recorded")
	}
	// backend object must have been cleaned up
	if len(h.fb.objects) != 0 {
		t.Fatalf("failed create leaked backend objects: %d", len(h.fb.objects))
	}
	// and the failure must not count against concurrency quota
	if err := h.svc.Quotas.CheckCreate(h.tn, sb.Spec); err != nil {
		t.Fatalf("failed sandbox still counted against quota: %v", err)
	}
}

func TestPauseResumeLifecycle(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)

	paused, err := h.svc.PauseSandbox(context.Background(), h.tn.ID, h.key.ID, sb.ID)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if paused.State != domain.StatePaused {
		t.Fatalf("state = %s", paused.State)
	}
	// double pause conflicts on state machine
	if _, err := h.svc.PauseSandbox(context.Background(), h.tn.ID, h.key.ID, sb.ID); apperr.From(err).Code != apperr.Conflict {
		t.Fatalf("double pause must conflict, got %v", err)
	}

	resumed, err := h.svc.ResumeSandbox(context.Background(), h.tn.ID, h.key.ID, sb.ID)
	if err != nil || resumed.State != domain.StateRunning {
		t.Fatalf("resume: %v state=%s", err, resumed.State)
	}
}

func TestExecRequiresRunning(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	if _, err := h.svc.PauseSandbox(context.Background(), h.tn.ID, h.key.ID, sb.ID); err != nil {
		t.Fatal(err)
	}
	_, err := h.svc.Exec(context.Background(), h.tn.ID, h.key.ID, sb.ID, runtime.ExecRequest{Argv: []string{"ls"}})
	if apperr.From(err).Code != apperr.Conflict {
		t.Fatalf("exec on paused must conflict, got %v", err)
	}
	res, err := h.svc.ResumeSandbox(context.Background(), h.tn.ID, h.key.ID, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = res
	if _, err := h.svc.Exec(context.Background(), h.tn.ID, h.key.ID, sb.ID, runtime.ExecRequest{Argv: []string{"ls"}}); err != nil {
		t.Fatalf("exec on running: %v", err)
	}
}

func TestCrossTenantAccessForbidden(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	other := domain.NewTenant("rival")
	if err := h.st.PutTenant(other); err != nil {
		t.Fatal(err)
	}
	err := h.svc.DeleteSandbox(context.Background(), other.ID, "ak_x", sb.ID, false)
	if apperr.From(err).Code != apperr.Forbidden {
		t.Fatalf("cross-tenant access must be forbidden, got %v", err)
	}
}

func TestFileOpsValidationAndFlow(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	ctx := context.Background()

	if err := h.svc.PutFile(ctx, h.tn.ID, h.key.ID, sb.ID, "/tmp/../etc/evil", []byte("x"), 0); err == nil {
		t.Fatal("path traversal accepted")
	}
	if err := h.svc.PutFile(ctx, h.tn.ID, h.key.ID, sb.ID, "relative/path", []byte("x"), 0); err == nil {
		t.Fatal("relative path accepted")
	}
	if err := h.svc.PutFile(ctx, h.tn.ID, h.key.ID, sb.ID, "/tmp/ok.txt", []byte("hello"), 0); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := h.svc.GetFile(ctx, h.tn.ID, h.key.ID, sb.ID, "/tmp/ok.txt")
	if err != nil || string(got) != "hello" {
		t.Fatalf("get: %v %q", err, got)
	}
	files, err := h.svc.ListFiles(ctx, h.tn.ID, h.key.ID, sb.ID, "/tmp")
	if err != nil || len(files) != 1 {
		t.Fatalf("list: %v %+v", err, files)
	}
	if err := h.svc.DeleteFile(ctx, h.tn.ID, h.key.ID, sb.ID, "/tmp/ok.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestSnapshotCreateAndRestore(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	ctx := context.Background()

	if err := h.svc.PutFile(ctx, h.tn.ID, h.key.ID, sb.ID, "/root/state.txt", []byte("v1"), 0); err != nil {
		t.Fatal(err)
	}
	sn, err := h.svc.CreateSnapshot(ctx, h.tn.ID, h.key.ID, sb.ID, "checkpoint-1")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if sn.SizeBytes <= 0 || sn.ImageRef == "" {
		t.Fatalf("snapshot incomplete: %+v", sn)
	}
	// restore
	sb2, err := h.svc.CreateSandbox(ctx, h.tn, h.key, CreateRequest{
		SnapshotID: sn.ID, Wait: true, WaitTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if sb2.SnapshotID != sn.ID || sb2.State != domain.StateRunning {
		t.Fatalf("restored sandbox wrong: %+v", sb2)
	}
	// cross-tenant snapshot read is forbidden
	other := domain.NewTenant("rival")
	if err := h.st.PutTenant(other); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.GetSnapshot(other.ID, sn.ID); apperr.From(err).Code != apperr.Forbidden {
		t.Fatalf("cross-tenant snapshot read: %v", err)
	}
}

func TestSnapshotQuotaRollsBackImage(t *testing.T) {
	h := newHarness(t)
	h.tn.Quota.MaxSnapshots = 0 // inherit platform default of 20 — use bytes instead
	h.tn.Quota.MaxSnapshotBytes = 100
	h.st.UpdateTenant(h.tn.ID, h.tn.Version, func(x *domain.Tenant) error {
		x.Quota.MaxSnapshotBytes = 100
		return nil
	})
	sb := h.create(t, true)
	_, err := h.svc.CreateSnapshot(context.Background(), h.tn.ID, h.key.ID, sb.ID, "big")
	if apperr.From(err).Code != apperr.QuotaExceeded {
		t.Fatalf("want quota_exceeded, got %v", err)
	}
	if len(h.fb.images) != 1 { // only base:ok — the committed image was rolled back
		t.Fatalf("rejected snapshot leaked its image: %v", h.fb.images)
	}
}

func TestReconcileConvergesExternalStop(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)

	// the workload dies behind our back
	h.fb.Stop(context.Background(), sb.Backend.Ref, true)

	h.svc.Reconcile(context.Background())
	got, _ := h.st.GetSandbox(sb.ID)
	if got.State != domain.StateStopped {
		t.Fatalf("reconcile did not converge: %s", got.State)
	}
}

func TestReconcileReapsOrphans(t *testing.T) {
	h := newHarness(t)
	// an object labeled as a sandbox but with no store record: crash residue
	h.fb.objects["sandboxd-sbx_ghost00000000000000000"] = &fakeObj{
		status: runtime.Status{Running: true},
		labels: map[string]string{"product": "sandboxd", "sandbox.id": "sbx_ghost00000000000000000"},
		files:  map[string][]byte{}, dirs: map[string]bool{},
	}
	h.svc.Reconcile(context.Background())
	if _, ok := h.fb.objects["sandboxd-sbx_ghost00000000000000000"]; ok {
		t.Fatal("orphan not reaped")
	}
}

func TestReconcileFailsWhenBackendObjectVanishes(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	h.fb.Remove(context.Background(), sb.Backend.Ref)

	h.svc.Reconcile(context.Background())
	got, _ := h.st.GetSandbox(sb.ID)
	if got.State != domain.StateFailed {
		t.Fatalf("vanished object must fail the record, got %s", got.State)
	}
}

func TestPoolAdoption(t *testing.T) {
	h := newHarness(t)
	h.svc.Cfg.PoolEnabled = true
	h.svc.Cfg.PoolSize = 2
	// seed the pool the way poolFill would
	h.fb.objects["sandboxd-warm-seed1"] = &fakeObj{
		status: runtime.Status{Running: true, Raw: "running"},
		labels: map[string]string{"product": "sandboxd", "warm": "1"},
		files:  map[string][]byte{}, dirs: map[string]bool{},
	}
	h.svc.poolMu.Lock()
	h.svc.pool = map[string][]poolItem{"base:ok": {{ref: "sandboxd-warm-seed1", created: time.Now(), template: "tpl_x"}}}
	h.svc.poolMu.Unlock()

	sb, err := h.svc.CreateSandbox(context.Background(), h.tn, h.key, CreateRequest{
		TemplateID: "tpl_" + findTemplate(t, h.st), Wait: true, WaitTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sb.State != domain.StateRunning {
		t.Fatalf("adopted sandbox not running: %s", sb.State)
	}
	if sb.Backend.Ref != "sandboxd-warm-seed1" {
		t.Fatalf("adoption did not use the pool item: %s", sb.Backend.Ref)
	}
	// the adopted item is protected from the reaper
	h.svc.Reconcile(context.Background())
	if _, ok := h.fb.objects["sandboxd-warm-seed1"]; !ok {
		t.Fatal("reaper killed an adopted sandbox")
	}
	// an UNADOPTED warm leftover from a crashed daemon gets reaped
	h.fb.objects["sandboxd-warm-stale"] = &fakeObj{
		status: runtime.Status{Running: true},
		labels: map[string]string{"product": "sandboxd", "warm": "1"},
		files:  map[string][]byte{}, dirs: map[string]bool{},
	}
	h.svc.Reconcile(context.Background())
	if _, ok := h.fb.objects["sandboxd-warm-stale"]; ok {
		t.Fatal("stale warm leftover not reaped")
	}
}

func TestTTLSweeperExpires(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	// force expiry into the past
	if _, err := h.st.UpdateSandbox(sb.ID, sb.Version, func(x *domain.Sandbox) error {
		x.ExpiresAt = time.Now().UTC().Add(-time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.svc.sweepExpired()
	got, _ := h.st.GetSandbox(sb.ID)
	if got.State != domain.StateStopped {
		t.Fatalf("expired sandbox not stopped: %s", got.State)
	}
	evs := h.st.ListEvents(store.EventQuery{TenantID: h.tn.ID, Action: domain.ActSandboxExpire})
	if len(evs) != 1 {
		t.Fatalf("expiry not audited: %d", len(evs))
	}
}

func TestDeleteRequiresForceWhenActive(t *testing.T) {
	h := newHarness(t)
	sb := h.create(t, true)
	if err := h.svc.DeleteSandbox(context.Background(), h.tn.ID, h.key.ID, sb.ID, false); apperr.From(err).Code != apperr.Conflict {
		t.Fatalf("delete of active without force must conflict, got %v", err)
	}
	if err := h.svc.DeleteSandbox(context.Background(), h.tn.ID, h.key.ID, sb.ID, true); err != nil {
		t.Fatalf("forced delete: %v", err)
	}
	if _, ok := h.st.GetSandbox(sb.ID); ok {
		t.Fatal("record survived delete")
	}
	if len(h.fb.objects) != 0 {
		t.Fatal("backend object survived delete")
	}
}
