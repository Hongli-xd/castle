package httpapi

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"

	"sandcastle.dev/sandcastle/internal/config"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/metrics"
	"sandcastle.dev/sandcastle/internal/runtime"
	"sandcastle.dev/sandcastle/internal/service"
	"sandcastle.dev/sandcastle/internal/store"
)

// TestRateLimitOverHTTP drives the full middleware stack to prove 429s fire
// through the real route path — the earlier integration gap was that unit
// tests covered the Limiter but not its wiring.
func TestRateLimitOverHTTP(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.PoolEnabled = false
	st, err := store.Open(cfg.DataDir, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fb := newFakeBackendForAPI()
	svc := service.New(st, fb, cfg, metrics.New())
	srv := New(svc, cfg)

	tn := domain.NewTenant("rl")
	tn.Quota.Rates = map[string]*domain.Rate{
		domain.GroupRead: {PerSecond: 1000, Burst: 5}, // tiny burst, high refill
	}
	if err := st.PutTenant(tn); err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := svc.Auth.MintKey(tn.ID, "rl", domain.ScopeTenant, 0)
	if err != nil {
		t.Fatal(err)
	}

	statuses := make(chan int, 60)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/v1/sandboxes", nil)
			req.Header.Set("Authorization", "Bearer "+plaintext)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			statuses <- rec.Code
		}()
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for c := range statuses {
		counts[c]++
	}
	if counts[429] == 0 {
		t.Fatalf("expected 429s with burst 5 over 50 requests, got %v", counts)
	}
	t.Logf("status distribution: %v", counts)
}

// minimal fake backend (service package's fake is test-scoped there)
type fakeBackendForAPI struct {
	mu sync.Mutex
	n  int
}

func newFakeBackendForAPI() *fakeBackendForAPI { return &fakeBackendForAPI{} }

func (f *fakeBackendForAPI) Name() string                   { return "fake" }
func (f *fakeBackendForAPI) Ping(ctx context.Context) error { return nil }
func (f *fakeBackendForAPI) Create(ctx context.Context, req runtime.CreateRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return fmt.Sprintf("fake-%d", f.n), nil
}
func (f *fakeBackendForAPI) Start(ctx context.Context, ref string) error            { return nil }
func (f *fakeBackendForAPI) Pause(ctx context.Context, ref string) error            { return nil }
func (f *fakeBackendForAPI) Resume(ctx context.Context, ref string) error           { return nil }
func (f *fakeBackendForAPI) Stop(ctx context.Context, ref string, force bool) error { return nil }
func (f *fakeBackendForAPI) Remove(ctx context.Context, ref string) error           { return nil }
func (f *fakeBackendForAPI) Rename(ctx context.Context, ref, newName string) error  { return nil }
func (f *fakeBackendForAPI) Inspect(ctx context.Context, ref string) (runtime.Status, error) {
	return runtime.Status{Running: true}, nil
}
func (f *fakeBackendForAPI) Exec(ctx context.Context, ref string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	return &runtime.ExecResult{}, nil
}
func (f *fakeBackendForAPI) PutFile(ctx context.Context, ref string, req runtime.PutFileRequest) error {
	return nil
}
func (f *fakeBackendForAPI) GetFile(ctx context.Context, ref, path string, maxSize int64) ([]byte, error) {
	return nil, nil
}
func (f *fakeBackendForAPI) ListDir(ctx context.Context, ref, path string) ([]runtime.FileInfo, error) {
	return nil, nil
}
func (f *fakeBackendForAPI) DeletePath(ctx context.Context, ref, path string) error { return nil }
func (f *fakeBackendForAPI) Commit(ctx context.Context, ref, tag string) (string, int64, error) {
	return tag, 1, nil
}
func (f *fakeBackendForAPI) HasImage(ctx context.Context, imageRef string) (bool, error) {
	return true, nil
}
func (f *fakeBackendForAPI) RemoveImage(ctx context.Context, imageRef string) error { return nil }
func (f *fakeBackendForAPI) ImportRootfs(ctx context.Context, dir, tag string) (string, int64, error) {
	return tag, 1, nil
}
func (f *fakeBackendForAPI) Orphans(ctx context.Context) ([]runtime.Orphan, error) {
	return nil, nil
}
