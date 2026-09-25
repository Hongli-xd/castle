package store

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), Options{DisableSync: false})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mkTenant(name string) *domain.Tenant {
	t := domain.NewTenant(name)
	return t
}

func TestPutGetTenant(t *testing.T) {
	s := newStore(t)
	tn := mkTenant("acme")
	if err := s.PutTenant(tn); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok := s.GetTenant(tn.ID)
	if !ok || got.Name != "acme" {
		t.Fatalf("roundtrip failed: %+v ok=%v", got, ok)
	}
}

func TestReopenRecoversState(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tn := mkTenant("acme")
	if err := s.PutTenant(tn); err != nil {
		t.Fatal(err)
	}
	sb := domain.NewSandbox(tn.ID)
	sb.Spec = domain.SandboxSpec{CPUMillicores: 500, MemoryMiB: 256, TimeoutSeconds: 600}
	if err := s.PutSandbox(sb); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSandbox(sb.ID, sb.Version, func(x *domain.Sandbox) error {
		return x.Transition(domain.StateRunning)
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, ok := s2.GetSandbox(sb.ID)
	if !ok {
		t.Fatal("sandbox lost across restart")
	}
	if got.State != domain.StateRunning {
		t.Fatalf("state = %s, want running", got.State)
	}
	if got.Version != 2 {
		t.Fatalf("version = %d, want 2 (put + transition)", got.Version)
	}
	if got.Spec.MemoryMiB != 256 {
		t.Fatalf("spec lost: %+v", got.Spec)
	}
}

func TestCASMechanics(t *testing.T) {
	s := newStore(t)
	sb := domain.NewSandbox("t1")
	if err := s.PutSandbox(sb); err != nil {
		t.Fatal(err)
	}
	// stale base version must be rejected
	if _, err := s.UpdateSandbox(sb.ID, 5, func(x *domain.Sandbox) error { return nil }); err == nil {
		t.Fatal("expected conflict for stale base version")
	}
	cur, _ := s.GetSandbox(sb.ID)
	// -1 disables CAS (internal paths that already hold the truth)
	if _, err := s.UpdateSandbox(sb.ID, -1, func(x *domain.Sandbox) error {
		return x.Transition(domain.StateRunning)
	}); err != nil {
		t.Fatalf("cas disabled update: %v", err)
	}
	_ = cur
	got, _ := s.GetSandbox(sb.ID)
	if got.Version != 2 {
		t.Fatalf("version = %d, want 2", got.Version)
	}
}

func TestBatchMutationsSingleFsync(t *testing.T) {
	s := newStore(t)
	sb := domain.NewSandbox("t1")
	if err := s.PutSandbox(sb); err != nil {
		t.Fatal(err)
	}
	// three logical transitions inside one critical section: version moves
	// by one and one frame lands — this is why CAS compares BaseVersion and
	// not Version-1.
	if _, err := s.UpdateSandbox(sb.ID, sb.Version, func(x *domain.Sandbox) error {
		if err := x.Transition(domain.StateRunning); err != nil {
			return err
		}
		if err := x.Transition(domain.StatePausing); err != nil {
			return err
		}
		return x.Transition(domain.StatePaused)
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSandbox(sb.ID)
	if got.State != domain.StatePaused || got.Version != 2 {
		t.Fatalf("state=%s version=%d, want paused/2", got.State, got.Version)
	}
}

func TestMutatorErrorLeavesStateClean(t *testing.T) {
	s := newStore(t)
	sb := domain.NewSandbox("t1")
	sb.Metadata = map[string]string{"a": "1"}
	if err := s.PutSandbox(sb); err != nil {
		t.Fatal(err)
	}
	want := func() *domain.Sandbox { c, _ := s.GetSandbox(sb.ID); return c }()
	if _, err := s.UpdateSandbox(sb.ID, want.Version, func(x *domain.Sandbox) error {
		x.Metadata["a"] = "mutated-by-failed-tx"
		x.State = domain.StateRunning // also illegal path
		return apperr.ConflictErr("boom")
	}); err == nil {
		t.Fatal("expected mutator error to surface")
	}
	got, _ := s.GetSandbox(sb.ID)
	if got.State != domain.StateProvisioning || got.Metadata["a"] != "1" || got.Version != 1 {
		t.Fatalf("failed tx leaked: state=%s meta=%v v=%d", got.State, got.Metadata, got.Version)
	}
}

func TestTornTailIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{DisableSync: true})
	if err != nil {
		t.Fatal(err)
	}
	tn := mkTenant("acme")
	if err := s.PutTenant(tn); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// simulate a crash mid-write: append garbage after intact frames
	f, _ := openAppend(t, dir)
	f.Write([]byte{0x00, 0x01, 0x02, 0x03, 0xFF, 0xEE, 0xDD, 0xCC, 'j', 'u', 'n', 'k'})
	f.Close()

	s2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, ok := s2.GetTenant(tn.ID); !ok {
		t.Fatal("intact frame lost after torn tail")
	}
	// store remains writable after repair
	if err := s2.PutTenant(mkTenant("second")); err != nil {
		t.Fatalf("store not writable after repair: %v", err)
	}
}

func TestCorruptedFrameTruncatesTail(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, Options{DisableSync: true})
	tn := mkTenant("first")
	s.PutTenant(tn)
	s.Close()

	// flip a byte inside the frame payload region
	blob, _ := osReadFile(walPathIn(dir))
	if len(blob) < 40 {
		t.Fatalf("wal too small: %d", len(blob))
	}
	blob[len(blob)-3] ^= 0xAA
	osWriteFile(walPathIn(dir), blob)

	s2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, ok := s2.GetTenant(tn.ID); ok {
		t.Fatal("corrupted frame must not be applied")
	}
}

func TestCompactionPreservesState(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, Options{})
	tn := mkTenant("acme")
	s.PutTenant(tn)
	for i := 0; i < 3; i++ {
		sb := domain.NewSandbox(tn.ID)
		s.PutSandbox(sb)
		if _, err := s.UpdateSandbox(sb.ID, sb.Version, func(x *domain.Sandbox) error {
			return x.Transition(domain.StateRunning)
		}); err != nil {
			t.Fatalf("transition sandbox %d: %v", i, err)
		}
	}
	sn := domain.NewSnapshot(tn.ID, "sbx_x", "snap1")
	sn.SizeBytes = 1234
	s.PutSnapshot(sn)
	if err := s.Compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if fi, err := osStat(walPathIn(dir)); err == nil && fi.Size() != 0 {
		t.Fatalf("wal not truncated after compaction: %d bytes", fi.Size())
	}
	s.Close()

	s2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	st := s2.Stats()
	if st.Tenants != 1 || st.Sandboxes != 3 || st.Snapshots != 1 {
		t.Fatalf("stats after compact+reopen: %+v", st)
	}
	got, _ := s2.GetSnapshot(sn.ID)
	if got.SizeBytes != 1234 {
		t.Fatal("snapshot fields lost across compaction")
	}
}

func TestEventsAppendQueryCap(t *testing.T) {
	s, err := Open(t.TempDir(), Options{MaxEvents: 50})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 80; i++ {
		ev := domain.NewEvent("t1", "", "ak_x", domain.ActSandboxCreate)
		if err := s.AppendEvent(ev); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond) // distinct timestamps
	}
	got := s.ListEvents(EventQuery{TenantID: "t1", Limit: 1000})
	if len(got) != 50 {
		t.Fatalf("kept %d events, want capped 50", len(got))
	}
	if got[0].TS.Before(got[len(got)-1].TS) {
		t.Fatal("events not newest-first")
	}
	n := s.CountEventsSince(time.Now().UTC().Add(-time.Minute), "t1", domain.ActSandboxCreate)
	if n != 50 {
		t.Fatalf("CountEventsSince = %d, want 50", n)
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	s := newStore(t)
	tn := mkTenant("acme")
	s.PutTenant(tn)
	sb := domain.NewSandbox(tn.ID)
	s.PutSandbox(sb)

	backupDir := filepath.Join(t.TempDir(), "backup")
	if err := s.Backup(backupDir); err != nil {
		t.Fatalf("backup: %v", err)
	}
	s2, err := Open(backupDir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, ok := s2.GetTenant(tn.ID); !ok {
		t.Fatal("tenant missing in backup")
	}
	if _, ok := s2.GetSandbox(sb.ID); !ok {
		t.Fatal("sandbox missing in backup")
	}
}

func TestConcurrentCASExactlyOneWinner(t *testing.T) {
	s := newStore(t)
	sb := domain.NewSandbox("t1")
	s.PutSandbox(sb)
	v := sb.Version

	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.UpdateSandbox(sb.ID, v, func(x *domain.Sandbox) error {
				return x.Transition(domain.StateRunning)
			})
			if err == nil {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners.Load())
	}
}

func TestDirectoryLockPreventsSecondStore(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Open(dir, Options{}); err == nil {
		t.Fatal("second Open on locked dir must fail")
	}
}

func TestKeyPrefixIndexRebuildsOnReplay(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir, Options{})
	k := domain.NewAPIKey("t1", "ci", domain.ScopeTenant)
	k.Prefix = "ak_abcdef1234"
	k.Hash = "deadbeef"
	if err := s.PutKey(k); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, ok := s2.KeyByPrefix("ak_abcdef1234"); !ok {
		t.Fatal("prefix index lost across restart")
	}
}
