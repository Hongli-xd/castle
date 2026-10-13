package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
)

var frameCRCTable = crc32.MakeTable(crc32.Castagnoli)

const (
	frameHeaderLen = 8 // [len u32][crc32 u32]
	maxFrameLen    = 64 * 1024 * 1024
)

// appendLocked serializes one mutation as a frame and fsyncs before returning.
//
// The caller MUST hold s.mu for writing. That is not a style preference: the
// order of frames on disk and the order in which the caller publishes the
// change into the in-memory maps have to happen inside the same critical
// section. If they can interleave (append outside the lock), a crash replays a
// different history than the one the process was serving, and an
// acknowledged write is silently lost.
//
// In v1.1.0 the Put*/AppendEvent family called this outside the lock while
// Update*/Delete* called it inside — a 4.2% acknowledged-write loss rate under
// concurrency (poc/walrace). Every mutation now takes the same path.
func (s *Store) appendLocked(k frameKind, op, id string, data any) error {
	if s.poisoned != nil {
		return s.poisoned
	}
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return apperr.InternalErr("marshal wal frame: %v", err)
		}
		raw = b
	}
	payload, err := json.Marshal(frame{K: k, Op: op, ID: id, Data: raw})
	if err != nil {
		return apperr.InternalErr("marshal wal envelope: %v", err)
	}
	buf := make([]byte, frameHeaderLen+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(payload)))
	binary.BigEndian.PutUint32(buf[4:8], crc32.Checksum(payload, frameCRCTable))
	copy(buf[8:], payload)

	// Offset our frame will occupy. Captured BEFORE the write: we hold the
	// write lock and are the only writer, so the current size is the append
	// point. (v1.1.0 read the size *after* writing, which made the rollback
	// below a no-op truncate-to-self and let a torn frame through.)
	before := s.walSize()

	n, err := s.wal.Write(buf)
	if err != nil {
		return s.rewindLocked(before, err, n, len(buf))
	}
	if n != len(buf) {
		return s.rewindLocked(before, nil, n, len(buf))
	}
	if !s.opts.DisableSync {
		if err := s.wal.Sync(); err != nil {
			// The bytes are in the file but not durable. Returning an error
			// without rewinding would let a later restart replay a mutation
			// the caller was told had failed — a phantom write. Rewind.
			return s.rewindLocked(before, nil, n, len(buf))
		}
	}
	return nil
}

// rewindLocked discards everything at or after `before`, so the WAL ends on a
// frame boundary and no later append can sit behind garbage. It is only called
// with the write lock held.
//
// If the rewind itself fails we can no longer promise that disk matches what
// we acknowledge, so the store poisons itself: further writes fail, /readyz
// reports it, and the operator restarts onto a clean rewind rather than
// running on a log whose tail is unknown.
func (s *Store) rewindLocked(before int64, cause error, n, want int) error {
	// The WAL handle is opened O_APPEND, so the next write lands at the new
	// end regardless of the file offset. ftruncate is enough.
	terr := s.wal.Truncate(before)
	if terr == nil && !s.opts.DisableSync {
		// Without this sync a crash could resurrect the pre-truncate length,
		// turning a reported failure back into a durable phantom frame.
		terr = s.wal.Sync()
	}
	if terr != nil {
		s.poisoned = apperr.InternalErr(
			"wal is in an unknown state: a write failed and the rollback to offset %d also failed (%v); refusing further writes",
			before, terr)
		return s.poisoned
	}
	if cause != nil {
		return apperr.InternalErr("wal write: %v", cause)
	}
	return apperr.InternalErr("wal short write (%d/%d bytes)", n, want)
}

func (s *Store) walSize() int64 {
	fi, err := s.wal.Stat()
	if err != nil {
		return 0
	}
	return fi.Size()
}

// loadCompacted restores the last compaction snapshot, if any.
func (s *Store) loadCompacted() error {
	blob, err := os.ReadFile(s.compactedPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return apperr.InternalErr("read data.json: %v", err)
	}
	var all struct {
		Tenants   []*domain.Tenant     `json:"tenants"`
		Keys      []*domain.APIKey     `json:"keys"`
		Sandboxes []*domain.Sandbox    `json:"sandboxes"`
		Snapshots []*domain.Snapshot   `json:"snapshots"`
		Templates []*domain.Template   `json:"templates"`
		Usages    []*domain.Usage      `json:"usages"`
		Events    []*domain.AuditEvent `json:"events"`
	}
	if err := json.Unmarshal(blob, &all); err != nil {
		// A corrupted compaction file is fatal: it is the base of truth.
		return apperr.New(apperr.Internal, fmt.Sprintf("data.json is corrupt: %v", err))
	}
	for _, t := range all.Tenants {
		s.tenants[t.ID] = t
	}
	for _, k := range all.Keys {
		s.keys[k.ID] = k
		s.keysByPre[k.Prefix] = k
	}
	for _, sb := range all.Sandboxes {
		s.sandboxes[sb.ID] = sb
	}
	for _, sn := range all.Snapshots {
		s.snapshots[sn.ID] = sn
	}
	for _, t := range all.Templates {
		s.templates[t.ID] = t
	}
	for _, u := range all.Usages {
		s.usages[u.TenantID] = u
	}
	if len(all.Events) > s.opts.MaxEvents {
		all.Events = all.Events[len(all.Events)-s.opts.MaxEvents:]
	}
	s.events = all.Events
	return nil
}

// RepairPolicy decides what the store does when the log is damaged.
type RepairPolicy int

const (
	// RepairTailOnly (default) truncates a torn tail — the ordinary crash
	// case, where the last frame was mid-write when the process died — and
	// REFUSES to start when the damage is in the middle of the log. A silent
	// truncate there discards transactions that were acknowledged and
	// fsynced, which is exactly the failure a WAL exists to prevent.
	RepairTailOnly RepairPolicy = iota
	// RepairSalvage truncates at the first damaged frame wherever it is.
	// Everything after the damage is discarded, silently and deliberately.
	// This is the v1.1.0 behaviour, kept as an explicit opt-in for operators
	// who would rather lose the tail than stay down.
	RepairSalvage
	// RepairStrict refuses to start on any damage at all, torn tail included.
	// For deployments that would rather page a human than auto-repair.
	RepairStrict
)

func (p RepairPolicy) String() string {
	switch p {
	case RepairSalvage:
		return "salvage"
	case RepairStrict:
		return "strict"
	default:
		return "tail-only"
	}
}

// RepairReport describes what recovery found and did with the log. It is
// surfaced at startup and via Store.Repair so an operator can tell a routine
// crash recovery from real log damage without reading the log file.
type RepairReport struct {
	Damaged      bool
	Kind         string // "torn-tail" | "mid-log" | ""
	Reason       string // human-readable cause at the damage offset
	Action       string // "none" | "truncate" | "salvage" | "refuse"
	AtOffset     int64
	FramesKept   int
	BytesKept    int64
	BytesDropped int64
}

func (r RepairReport) String() string {
	if !r.Damaged {
		return "intact"
	}
	return fmt.Sprintf("%s at offset %d (%s): %s, kept %d frames/%d bytes, dropped %d bytes",
		r.Kind, r.AtOffset, r.Reason, r.Action, r.FramesKept, r.BytesKept, r.BytesDropped)
}

// replayAndRepair reads wal.log, applies every intact frame, and decides what
// to do about the first damaged one.
//
// Damage is classified before it is acted on. A torn tail is routine: the
// write that was in flight when the process died is not a committed
// transaction, so dropping it is correct. Damage in the middle is not
// routine — the frames after it were written and fsynced by a process that
// was still running, so they were acknowledged and must not disappear
// without a human saying so.
func (s *Store) replayAndRepair() error {
	blob, err := os.ReadFile(s.walPath())
	if os.IsNotExist(err) {
		s.repair = RepairReport{Action: "none"}
		return nil
	}
	if err != nil {
		return apperr.InternalErr("read wal: %v", err)
	}

	off, frames := 0, 0
	for off < len(blob) {
		fr, size, ok, reason := decodeFrameAt(blob, off)
		if !ok {
			return s.handleDamage(blob, off, frames, reason)
		}
		if err := s.applyFrame(fr); err != nil {
			return s.handleDamage(blob, off, frames, "frame cannot be applied: "+err.Error())
		}
		off += size
		frames++
	}
	s.repair = RepairReport{Action: "none", FramesKept: frames, BytesKept: int64(off)}
	return nil
}

// decodeFrameAt validates one frame without applying it: header sanity, CRC,
// and JSON shape. It is the only place that reads a frame off the log, so the
// tail-scan below and the replay loop can never disagree about validity.
func decodeFrameAt(blob []byte, off int) (*frame, int, bool, string) {
	if off+frameHeaderLen > len(blob) {
		return nil, 0, false, "trailing bytes shorter than a frame header"
	}
	ln := binary.BigEndian.Uint32(blob[off : off+4])
	crc := binary.BigEndian.Uint32(blob[off+4 : off+8])
	if ln > maxFrameLen {
		return nil, 0, false, fmt.Sprintf("frame length %d exceeds the %d byte limit", ln, maxFrameLen)
	}
	if off+frameHeaderLen+int(ln) > len(blob) {
		return nil, 0, false, fmt.Sprintf("frame claims %d bytes but only %d remain", ln, len(blob)-off-frameHeaderLen)
	}
	payload := blob[off+frameHeaderLen : off+frameHeaderLen+int(ln)]
	if crc32.Checksum(payload, frameCRCTable) != crc {
		return nil, 0, false, "frame crc mismatch"
	}
	var fr frame
	if err := json.Unmarshal(payload, &fr); err != nil {
		return nil, 0, false, "frame payload is not valid JSON: " + err.Error()
	}
	return &fr, frameHeaderLen + int(ln), true, ""
}

// damageScanWindow bounds how far past the damage we look for a surviving
// frame before calling it a tail tear. Cyclic corruption of a whole region
// should not turn startup into an O(n^2) scan.
const damageScanWindow = 1 << 20

// handleDamage classifies the damage at `off` and applies the repair policy.
func (s *Store) handleDamage(blob []byte, off, frames int, reason string) error {
	mid := s.intactFrameAfter(blob, off)
	kind := "torn-tail"
	if mid {
		kind = "mid-log"
	}
	rep := RepairReport{
		Damaged:      true,
		Kind:         kind,
		Reason:       reason,
		AtOffset:     int64(off),
		FramesKept:   frames,
		BytesKept:    int64(off),
		BytesDropped: int64(len(blob) - off),
	}

	// A torn tail is the expected crash signature; the policy decides whether
	// to auto-repair it. Mid-log damage is only ever truncated when the
	// operator has explicitly accepted tail loss.
	truncate := false
	switch s.opts.Repair {
	case RepairStrict:
		truncate = false
	case RepairSalvage:
		truncate = true
	default: // RepairTailOnly
		truncate = !mid
	}

	if !truncate {
		rep.Action = "refuse"
		s.repair = rep
		return apperr.New(apperr.Internal, fmt.Sprintf(
			"refusing to start: %s wal damage at offset %d (%s); %d bytes / %d intact frames precede it. "+
				"Restore from a backup (Store.Backup) and replay, or start with repair=salvage to "+
				"explicitly accept losing those bytes. (policy=%s, scanned_to=%d)",
			kind, off, reason, off, frames, s.opts.Repair, s.damageScanEnd(off)))
	}

	if err := os.Truncate(s.walPath(), int64(off)); err != nil {
		return apperr.InternalErr("truncate damaged wal tail at %d: %v", off, err)
	}
	syncDir(s.dir)
	if kind == "torn-tail" {
		rep.Action = "truncate"
	} else {
		rep.Action = "salvage"
	}
	s.repair = rep
	return nil
}

func (s *Store) damageScanEnd(off int) int {
	return off + damageScanWindow
}

// intactFrameAfter reports whether a decodable frame starts somewhere after
// `off`. The fast path is the byte right after the damaged frame's declared
// length, which is where the next frame boundary is when only the payload rotted.
// Only if that misses do we scan, and only within damageScanWindow.
func (s *Store) intactFrameAfter(blob []byte, off int) bool {
	if off+frameHeaderLen <= len(blob) {
		ln := int(binary.BigEndian.Uint32(blob[off : off+4]))
		if cand := off + frameHeaderLen + ln; cand > off && cand < len(blob) {
			if _, _, ok, _ := decodeFrameAt(blob, cand); ok {
				return true
			}
		}
	}
	end := off + 1 + damageScanWindow
	if end > len(blob) {
		end = len(blob)
	}
	for pos := off + 1; pos+frameHeaderLen <= end; pos++ {
		if _, _, ok, _ := decodeFrameAt(blob, pos); ok {
			return true
		}
	}
	return false
}

func (s *Store) applyFrame(fr *frame) error {
	switch fr.K {
	case fkTenant:
		if fr.Op == "del" {
			delete(s.tenants, fr.ID)
			return nil
		}
		var t domain.Tenant
		if err := json.Unmarshal(fr.Data, &t); err != nil {
			return err
		}
		s.tenants[t.ID] = &t
	case fkKey:
		if fr.Op == "del" {
			if k, ok := s.keys[fr.ID]; ok {
				delete(s.keysByPre, k.Prefix)
			}
			delete(s.keys, fr.ID)
			return nil
		}
		var k domain.APIKey
		if err := json.Unmarshal(fr.Data, &k); err != nil {
			return err
		}
		if old, ok := s.keys[k.ID]; ok {
			delete(s.keysByPre, old.Prefix)
		}
		s.keys[k.ID] = &k
		s.keysByPre[k.Prefix] = &k
	case fkSandbox:
		if fr.Op == "del" {
			delete(s.sandboxes, fr.ID)
			return nil
		}
		var sb domain.Sandbox
		if err := json.Unmarshal(fr.Data, &sb); err != nil {
			return err
		}
		s.sandboxes[sb.ID] = &sb
	case fkSnapshot:
		if fr.Op == "del" {
			delete(s.snapshots, fr.ID)
			return nil
		}
		var sn domain.Snapshot
		if err := json.Unmarshal(fr.Data, &sn); err != nil {
			return err
		}
		s.snapshots[sn.ID] = &sn
	case fkTemplate:
		if fr.Op == "del" {
			delete(s.templates, fr.ID)
			return nil
		}
		var t domain.Template
		if err := json.Unmarshal(fr.Data, &t); err != nil {
			return err
		}
		s.templates[t.ID] = &t
	case fkUsage:
		if fr.Op == "del" {
			delete(s.usages, fr.ID)
			return nil
		}
		var u domain.Usage
		if err := json.Unmarshal(fr.Data, &u); err != nil {
			return err
		}
		s.usages[u.TenantID] = &u
	case fkEvent:
		var ev domain.AuditEvent
		if err := json.Unmarshal(fr.Data, &ev); err != nil {
			return err
		}
		s.events = append(s.events, &ev)
		if len(s.events) > s.opts.MaxEvents {
			s.events = s.events[len(s.events)-s.opts.MaxEvents:]
		}
	default:
		return fmt.Errorf("unknown frame kind %q", fr.K)
	}
	return nil
}

// Compact rewrites the base snapshot and truncates the log. Callers (the
// daemon's maintenance loop) invoke it periodically; it is safe under load
// because it runs under the full write lock.
func (s *Store) Compact() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Compaction is destructive: it rewrites data.json from in-memory state and
	// then truncates the log. If a failed write poisoned the WAL, in-memory
	// state may be missing a mutation that IS on disk, so compacting now would
	// turn a recoverable ambiguity into permanent loss.
	if s.poisoned != nil {
		return s.poisoned
	}
	blob, err := s.marshalAll()
	if err != nil {
		return err
	}
	tmp := s.compactedPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return apperr.InternalErr("compact create: %v", err)
	}
	if _, err := f.Write(blob); err != nil {
		f.Close()
		return apperr.InternalErr("compact write: %v", err)
	}
	if !s.opts.DisableSync {
		if err := f.Sync(); err != nil {
			f.Close()
			return apperr.InternalErr("compact fsync: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		return apperr.InternalErr("compact close: %v", err)
	}
	if err := os.Rename(tmp, s.compactedPath()); err != nil {
		return apperr.InternalErr("compact rename: %v", err)
	}
	syncDir(s.dir)
	if err := os.Truncate(s.walPath(), 0); err != nil {
		return apperr.InternalErr("compact truncate wal: %v", err)
	}
	if !s.opts.DisableSync {
		if h, err := os.OpenFile(s.walPath(), os.O_WRONLY, 0o600); err == nil {
			_ = h.Sync()
			h.Close()
		}
	}
	return nil
}

func syncDir(dir string) {
	h, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = h.Sync()
	h.Close()
}

// ---- single-writer directory lock -------------------------------------------
//
// The lock handle is owned by the Store, not by the package. v1.1.0 kept it in
// a package-level variable, so opening a second Store overwrote the first
// Store's handle: closing either one released the other's flock, and the
// original fd leaked. Reproduced in poc/lockleak.

func lockDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, apperr.InternalErr("open lock file: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, apperr.ConflictErr("data directory is locked by another sandboxd process (%v)", err)
	}
	return f, nil
}

func unlockDir(f *os.File) error {
	if f == nil {
		return nil
	}
	// Closing the descriptor releases the flock; the explicit LOCK_UN keeps
	// the ordering obvious and is harmless on a fd we are about to close.
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}

// UptimeNote exists so the package keeps a compile-time link to time even if
// future refactors drop direct use above (kept intentionally: compaction
// timestamps land in the log line emitted by the daemon).
var _ = time.Now
