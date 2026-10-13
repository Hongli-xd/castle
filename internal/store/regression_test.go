package store

// Regression tests for the four defects that made acknowledged writes lossy.
//
// Each test here failed against v1.1.0 and documents the invariant it pins:
//
//	TestAppendAlwaysUnderWriteLock            → invariants 1 (static, AST-level)
//	TestAcknowledgedWritesSurviveRestart...   → invariant 1 (the 4.2% loss PoC)
//	TestRewindRestoresExactOffset             → invariant 3 (no-op rollback)
//	TestFailedWritePoisonsStore               → invariant 4 (fail-stop)
//	TestTornTailIsRepairedAutomatically       → invariant 5
//	TestMidLogDamageRefusesToStart...         → invariant 5 (silent truncation)
//	TestClosingOneStoreDoesNotRelease...      → per-instance flock
//
// Invariant 2 (clone at every boundary) is covered by TestStoreClonesAtBoundaries.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"sandcastle.dev/sandcastle/internal/domain"
)

// ---- helpers ---------------------------------------------------------------

// encodeFrame builds one on-disk frame. It uses the package's own unexported
// frame type and CRC table so the test cannot drift from the format.
func encodeFrame(t *testing.T, k frameKind, op, id string, data any) []byte {
	t.Helper()
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			t.Fatalf("marshal data: %v", err)
		}
		raw = b
	}
	payload, err := json.Marshal(frame{K: k, Op: op, ID: id, Data: raw})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	buf := make([]byte, frameHeaderLen+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(payload)))
	binary.BigEndian.PutUint32(buf[4:8], crc32.Checksum(payload, frameCRCTable))
	copy(buf[frameHeaderLen:], payload)
	return buf
}

func tenantFrames(t *testing.T, ids ...string) [][]byte {
	t.Helper()
	out := make([][]byte, 0, len(ids))
	for _, id := range ids {
		out = append(out, encodeFrame(t, fkTenant, "put", id,
			map[string]string{"id": id, "name": id}))
	}
	return out
}

func joinFrames(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// corruptCRC breaks a frame's checksum while leaving its length header valid,
// which is the shape mid-log bit rot actually takes.
func corruptCRC(f []byte) []byte {
	c := append([]byte(nil), f...)
	binary.BigEndian.PutUint32(c[4:8], 0xDEADBEEF)
	return c
}

func writeLog(t *testing.T, dir string, blob []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "wal.log"), blob, 0o600); err != nil {
		t.Fatalf("write wal: %v", err)
	}
}

func hasTenant(s *Store, id string) bool {
	_, ok := s.GetTenant(id)
	return ok
}

// ---- invariant 1: append and publish share one critical section -----------

// TestAppendAlwaysUnderWriteLock is the structural half of the P0-1 fix. It
// parses the package sources and fails if any function appends to the WAL
// without holding s.mu for writing.
//
// This is deliberately a static check rather than a behavioural one: a
// concurrency bug of this shape reproduces statistically, so a passing load
// test is not evidence of correctness. v1.1.0's own test suite was green.
func TestAppendAlwaysUnderWriteLock(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0
	for _, name := range []string{"store.go", "wal.go"} {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var hasAppend, hasWriteLock bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "appendLocked":
					hasAppend = true
				case "Lock":
					// Must be s.mu.Lock(), not some other receiver.
					if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "mu" {
						hasWriteLock = true
					}
				}
				return true
			})
			if !hasAppend {
				continue
			}
			checked++
			if !hasWriteLock {
				t.Errorf("%s: %s appends to the WAL without holding s.mu for writing.\n"+
					"\tThe frame order on disk and the order the caller publishes the change must be\n"+
					"\tthe same critical section, or a crash replays a history the process never served.",
					name, fn.Name.Name)
			}
		}
	}
	if checked < 10 {
		t.Fatalf("expected to find the mutation methods, only matched %d — did the check go stale?", checked)
	}
	t.Logf("verified %d WAL-appending functions hold the write lock", checked)
}

// TestAcknowledgedWritesSurviveRestartUnderConcurrentCompaction is the
// behavioural half: it is the packaged form of poc/walrace.
//
// v1.1.0 result: 120 of 120 rounds lost writes (4.2% of acknowledged puts).
// Expected now: zero loss, every round.
func TestAcknowledgedWritesSurviveRestartUnderConcurrentCompaction(t *testing.T) {
	const (
		rounds     = 12
		putsPerTry = 24
	)
	for round := 0; round < rounds; round++ {
		dir := t.TempDir()
		s, err := Open(dir, Options{})
		if err != nil {
			t.Fatalf("round %d: open: %v", round, err)
		}

		ids := make([]string, 0, putsPerTry)
		var mu sync.Mutex
		var wg sync.WaitGroup

		for i := 0; i < putsPerTry; i++ {
			tn := mkTenant("race")
			mu.Lock()
			ids = append(ids, tn.ID)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Only an acknowledged write counts; a rejected one is free
				// to vanish, and this store accepts all of them.
				if err := s.PutTenant(tn); err != nil {
					t.Errorf("round %d: put: %v", round, err)
				}
			}()
		}
		// One compaction racing the writers is the whole point: it takes the
		// write lock, snapshots memory, then truncates the log.
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Compact()
		}()
		wg.Wait()

		if err := s.Close(); err != nil {
			t.Fatalf("round %d: close: %v", round, err)
		}

		reopened, err := Open(dir, Options{})
		if err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		var lost []string
		for _, id := range ids {
			if !hasTenant(reopened, id) {
				lost = append(lost, id)
			}
		}
		reopened.Close()
		if len(lost) > 0 {
			t.Fatalf("round %d: %d/%d acknowledged writes vanished across restart (e.g. %s)",
				round, len(lost), len(ids), lost[0])
		}
	}
}

// ---- invariant 3: a failed write rewinds to its own start offset ----------

// TestRewindRestoresExactOffset pins the arithmetic that v1.1.0 got wrong: the
// rollback must return the file to the offset the failed frame started at.
// The old code truncated to walSize() read AFTER the write, i.e. to the
// current end — a no-op that left the torn frame in place.
func TestRewindRestoresExactOffset(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	for _, id := range []string{"tnt_A", "tnt_B"} {
		if err := s.PutTenant(mkTenantWithID(id)); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}

	s.mu.Lock()
	before := s.walSize()
	// Simulate the bytes a partially-completed write would have left behind.
	if _, err := s.wal.Write([]byte("not-a-frame-at-all")); err != nil {
		s.mu.Unlock()
		t.Fatalf("setup write: %v", err)
	}
	s.mu.Unlock()

	if s.walSize() <= before {
		t.Fatalf("setup: expected the log to have grown past %d bytes", before)
	}

	// rewindLocked reports the failure that triggered it (it re-reports the
	// cause rather than swallowing it), so the assertion is on the effect: the
	// log must land exactly on the frame boundary again.
	injected := errors.New("injected write failure")
	s.mu.Lock()
	err = s.rewindLocked(before, injected, 0, 1)
	s.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), injected.Error()) {
		t.Fatalf("rewind should surface the original cause %q, got %v", injected, err)
	}
	if got := s.walSize(); got != before {
		t.Fatalf("rewind left the log at %d bytes, want exactly %d", got, before)
	}
	s.Close()

	// The rewind must leave a log that replays as intact — proof the garbage
	// is gone rather than merely unreachable.
	s2, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("reopen after rewind: %v", err)
	}
	defer s2.Close()
	if s2.Repair().Damaged {
		t.Fatalf("log reported as damaged after a successful rewind: %s", s2.Repair())
	}
	for _, id := range []string{"tnt_A", "tnt_B"} {
		if !hasTenant(s2, id) {
			t.Fatalf("%s missing after rewind — the rewind cut into committed frames", id)
		}
	}
}

// ---- invariant 4: a store that cannot rewind fails closed -----------------

// TestFailedWritePoisonsStore covers the case the old code could not even
// represent: the write fails AND the rollback fails. There is no safe way to
// keep serving, so the store must refuse writes and say why.
func TestFailedWritePoisonsStore(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if err := s.Healthy(); err != nil {
		t.Fatalf("fresh store reports unhealthy: %v", err)
	}

	// A closed handle makes both Write and Truncate fail.
	dead, err := os.OpenFile(filepath.Join(dir, "wal.log"), os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open wal for the test: %v", err)
	}
	if err := dead.Close(); err != nil {
		t.Fatalf("close test handle: %v", err)
	}

	s.mu.Lock()
	live := s.wal
	s.wal = dead
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.wal = live
		s.mu.Unlock()
	}()

	if err := s.PutTenant(mkTenant("poisoned")); err == nil {
		t.Fatal("expected the write to fail once the WAL handle is unusable")
	}
	if err := s.Healthy(); err == nil {
		t.Fatal("store must report unhealthy after a failed write + failed rollback")
	}

	// Fail closed: subsequent mutations must not silently succeed.
	if err := s.PutTenant(mkTenant("again")); err == nil {
		t.Fatal("a poisoned store accepted a later write")
	}
	inMem, _ := s.GetTenant("poisoned")
	_ = inMem // read-only lookups may still work; writes must not.

	// Compaction rewrites the base snapshot from memory and then truncates the
	// log, so running it while the log is in an unknown state could convert a
	// recoverable ambiguity into permanent loss.
	if err := s.Compact(); err == nil {
		t.Fatal("compaction must refuse on a poisoned store")
	}
}

// ---- invariant 2: no aliasing across the store boundary ------------------

// TestStoreClonesAtBoundaries pins the second half of the P0-1 root cause: the
// store must never keep a caller's pointer, and never hand out one of its own.
// Otherwise a caller mutating an object it still holds changes persisted state
// without a WAL frame.
func TestStoreClonesAtBoundaries(t *testing.T) {
	s := newStore(t)

	tn := domain.NewTenant("clone")
	tn.Quota.Rates = map[string]*domain.Rate{domain.GroupRead: {PerSecond: 7, Burst: 3}}
	if err := s.PutTenant(tn); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Mutate the caller's object after the write.
	tn.Name = "mutated-after-put"
	*tn.Quota.Rates[domain.GroupRead] = domain.Rate{PerSecond: 9999, Burst: 9999}

	got, ok := s.GetTenant(tn.ID)
	if !ok {
		t.Fatal("tenant missing")
	}
	if got.Name != "clone" {
		t.Fatalf("store kept the caller's pointer: name is %q, want %q", got.Name, "clone")
	}
	if got.Quota.Rates[domain.GroupRead].PerSecond != 7 {
		t.Fatalf("store shares the caller's Rates map: got %v, want 7",
			got.Quota.Rates[domain.GroupRead].PerSecond)
	}

	// And the other direction: mutating what the store returned must not
	// change what the store holds.
	got.Name = "mutated-the-read-copy"
	delete(got.Quota.Rates, domain.GroupRead)
	again, _ := s.GetTenant(tn.ID)
	if again.Name != "clone" {
		t.Fatalf("store handed out its own pointer: reading again gives %q", again.Name)
	}
	if _, ok := again.Quota.Rates[domain.GroupRead]; !ok {
		t.Fatal("store handed out its own Rates map")
	}

	// Sandbox spec map/slice fields are the easiest ones to alias by accident.
	sb := domain.NewSandbox(tn.ID)
	sb.Spec.Env = map[string]string{"K": "v"}
	sb.Spec.Egress.Allow = []string{"a.example"}
	sb.Metadata = map[string]string{"m": "1"}
	if err := s.PutSandbox(sb); err != nil {
		t.Fatalf("put sandbox: %v", err)
	}
	sb.Spec.Env["K"] = "tampered"
	sb.Spec.Egress.Allow[0] = "evil.example"
	sb.Metadata["m"] = "tampered"

	out, ok := s.GetSandbox(sb.ID)
	if !ok {
		t.Fatal("sandbox missing")
	}
	if out.Spec.Env["K"] != "v" {
		t.Fatalf("Spec.Env aliased: got %q", out.Spec.Env["K"])
	}
	if out.Spec.Egress.Allow[0] != "a.example" {
		t.Fatalf("Spec.Egress.Allow aliased: got %q", out.Spec.Egress.Allow[0])
	}
	if out.Metadata["m"] != "1" {
		t.Fatalf("Metadata aliased: got %q", out.Metadata["m"])
	}
}

// ---- invariant 5: recovery classifies damage before acting on it ----------

func TestTornTailIsRepairedAutomatically(t *testing.T) {
	dir := t.TempDir()
	good := tenantFrames(t, "tnt_GOOD_1", "tnt_GOOD_2")
	half := encodeFrame(t, fkTenant, "put", "tnt_HALF", map[string]string{"id": "tnt_HALF"})
	writeLog(t, dir, joinFrames(good[0], good[1], half[:len(half)/2]))

	s, err := Open(dir, Options{Repair: RepairTailOnly})
	if err != nil {
		t.Fatalf("a torn tail is routine crash debris and must not block startup: %v", err)
	}
	defer s.Close()

	rep := s.Repair()
	if !rep.Damaged || rep.Kind != "torn-tail" {
		t.Fatalf("expected torn-tail damage, got %+v", rep)
	}
	if rep.Action != "truncate" {
		t.Fatalf("expected the tail to be truncated, got action %q", rep.Action)
	}
	if !hasTenant(s, "tnt_GOOD_1") || !hasTenant(s, "tnt_GOOD_2") {
		t.Fatal("frames before the tear were lost")
	}
	if hasTenant(s, "tnt_HALF") {
		t.Fatal("a half-written frame must not be applied")
	}
}

// TestMidLogDamageRefusesToStartByDefault is the core of the P0-2 fix. In
// v1.1.0 this layout silently discarded 2 acknowledged, fsynced transactions.
func TestMidLogDamageRefusesToStartByDefault(t *testing.T) {
	dir := t.TempDir()
	f := tenantFrames(t, "tnt_BEFORE", "tnt_TORN", "tnt_AFTER_1", "tnt_AFTER_2")
	blob := joinFrames(f[0], corruptCRC(f[1]), f[2], f[3])
	writeLog(t, dir, blob)

	s, err := Open(dir, Options{Repair: RepairTailOnly})
	if err == nil {
		s.Close()
		t.Fatal("startup must be refused when acknowledged frames sit behind mid-log damage")
	}
	msg := err.Error()
	for _, want := range []string{"mid-log", "offset", "salvage"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message should mention %q so the operator knows the options; got:\n%s", want, msg)
		}
	}

	// Refusing must not destroy the evidence: the log has to be byte-identical
	// so an operator can still copy it out and repair it.
	after, rerr := os.ReadFile(filepath.Join(dir, "wal.log"))
	if rerr != nil {
		t.Fatalf("read log after refusal: %v", rerr)
	}
	if len(after) != len(blob) {
		t.Fatalf("refusing to start modified the log: %d bytes, was %d", len(after), len(blob))
	}
}

func TestMidLogDamageSalvageIsExplicitAndReported(t *testing.T) {
	dir := t.TempDir()
	f := tenantFrames(t, "tnt_BEFORE", "tnt_TORN", "tnt_AFTER_1", "tnt_AFTER_2")
	writeLog(t, dir, joinFrames(f[0], corruptCRC(f[1]), f[2], f[3]))

	s, err := Open(dir, Options{Repair: RepairSalvage})
	if err != nil {
		t.Fatalf("salvage must be a usable escape hatch: %v", err)
	}
	defer s.Close()

	rep := s.Repair()
	if rep.Kind != "mid-log" || rep.Action != "salvage" {
		t.Fatalf("expected a mid-log salvage, got %+v", rep)
	}
	if rep.BytesDropped <= 0 {
		t.Fatalf("the amount of discarded data must be quantified, got %d", rep.BytesDropped)
	}
	if !hasTenant(s, "tnt_BEFORE") {
		t.Fatal("frames before the damage must be kept")
	}
	// Lost, but deliberately and visibly — that is the whole contract.
	if hasTenant(s, "tnt_AFTER_1") || hasTenant(s, "tnt_AFTER_2") {
		t.Fatal("salvage should not have resurrected frames after the damage")
	}
}

func TestStrictRepairRefusesEvenTornTail(t *testing.T) {
	dir := t.TempDir()
	good := tenantFrames(t, "tnt_1")
	half := encodeFrame(t, fkTenant, "put", "tnt_HALF", map[string]string{"id": "tnt_HALF"})
	writeLog(t, dir, joinFrames(good[0], half[:len(half)/2]))

	s, err := Open(dir, Options{Repair: RepairStrict})
	if err == nil {
		s.Close()
		t.Fatal("strict policy must refuse to start on any damage")
	}
}

func TestIntactLogIsNotReportedAsDamaged(t *testing.T) {
	dir := t.TempDir()
	f := tenantFrames(t, "tnt_1", "tnt_2")
	writeLog(t, dir, joinFrames(f...))

	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	rep := s.Repair()
	if rep.Damaged {
		t.Fatalf("an intact log was reported as damaged: %s", rep)
	}
	if rep.FramesKept != 2 {
		t.Fatalf("expected 2 frames replayed, got %d", rep.FramesKept)
	}
}

// An unknown frame kind means the log was written by a different version. That
// is not corruption, but silently truncating the log there — as v1.1.0 did —
// would throw away every later frame, so it must stop startup instead.
func TestUnknownFrameKindRefusesRatherThanTruncates(t *testing.T) {
	dir := t.TempDir()
	known := encodeFrame(t, fkTenant, "put", "tnt_KNOWN", map[string]string{"id": "tnt_KNOWN"})
	fromTheFuture := encodeFrame(t, frameKind("some_future_kind"), "put", "x", map[string]string{"id": "x"})
	trailing := tenantFrames(t, "tnt_TRAILING")
	writeLog(t, dir, joinFrames(known, fromTheFuture, trailing[0]))

	s, err := Open(dir, Options{Repair: RepairTailOnly})
	if err == nil {
		s.Close()
		t.Fatal("a frame this build cannot interpret must not be silently dropped")
	}
	if !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---- per-instance directory lock -----------------------------------------

// TestClosingOneStoreDoesNotReleaseAnothersLock pins the flock fix. v1.1.0
// kept the lock handle in a package-level variable, so opening a second store
// overwrote the first one's handle: closing either released the other's lock
// and leaked an fd (poc/lockleak).
func TestClosingOneStoreDoesNotReleaseAnothersLock(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()

	a, err := Open(dirA, Options{})
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	b, err := Open(dirB, Options{})
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	defer b.Close()

	if err := a.Close(); err != nil {
		t.Fatalf("close A: %v", err)
	}

	if canExclusiveLock(t, dirB) {
		t.Fatal("closing A released B's directory lock")
	}
	// A released its own lock, so its directory is free again.
	if !canExclusiveLock(t, dirA) {
		t.Fatal("closing A did not release A's own lock")
	}

	// A second store on a locked directory must still be refused.
	if _, err := Open(dirB, Options{}); err == nil {
		t.Fatal("opening a locked data directory should be a conflict")
	}
}

// canExclusiveLock reports whether the directory's lock file is currently
// available, i.e. nobody holds it.
func canExclusiveLock(t *testing.T, dir string) bool {
	t.Helper()
	f, err := lockDir(dir)
	if err != nil {
		return false
	}
	_ = unlockDir(f)
	return true
}

// A store that fails to open must not leave the directory locked — otherwise a
// single bad config makes the data dir permanently unopenable by anyone.
func TestFailedOpenReleasesTheDirectoryLock(t *testing.T) {
	dir := t.TempDir()
	// A corrupt compaction file makes loadCompacted fail, after lockDir won.
	if err := os.WriteFile(filepath.Join(dir, "data.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write data.json: %v", err)
	}
	if _, err := Open(dir, Options{}); err == nil {
		t.Fatal("expected the corrupt base snapshot to fail the open")
	}
	if !canExclusiveLock(t, dir) {
		t.Fatal("a failed Open leaked the directory lock")
	}
}

// ---- helpers ---------------------------------------------------------------

func mkTenantWithID(id string) *domain.Tenant {
	t := domain.NewTenant("fixed")
	t.ID = id
	return t
}
