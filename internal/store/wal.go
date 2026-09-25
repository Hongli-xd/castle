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
)

// append serializes one mutation as a frame and fsyncs before returning.
// The write happens under the caller's lock (Store.mu write-side or init).
func (s *Store) append(k frameKind, op, id string, data any) error {
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

	n, err := s.wal.Write(buf)
	if err != nil {
		// A partial write leaves a torn tail; rewind so the log stays valid
		// and later appends do not sit behind garbage.
		_ = s.wal.Truncate(int64(s.walSize()))
		return apperr.InternalErr("wal write: %v", err)
	}
	if n != len(buf) {
		_ = s.wal.Truncate(int64(s.walSize()))
		return apperr.InternalErr("wal short write (%d/%d bytes)", n, len(buf))
	}
	if !s.opts.DisableSync {
		if err := s.wal.Sync(); err != nil {
			return apperr.InternalErr("wal fsync: %v", err)
		}
	}
	return nil
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
	if len(all.Events) > s.opts.MaxEvents {
		all.Events = all.Events[len(all.Events)-s.opts.MaxEvents:]
	}
	s.events = all.Events
	return nil
}

// replayAndRepair reads wal.log, applies every intact frame, and truncates
// the file at the first torn frame. Anything after damage is untrusted —
// including frames that "look" valid — because ordering guarantees died
// with the torn write.
func (s *Store) replayAndRepair() error {
	blob, err := os.ReadFile(s.walPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return apperr.InternalErr("read wal: %v", err)
	}
	good := 0
	off := 0
	for off+frameHeaderLen <= len(blob) {
		ln := binary.BigEndian.Uint32(blob[off : off+4])
		crc := binary.BigEndian.Uint32(blob[off+4 : off+8])
		if ln > 64*1024*1024 || off+8+int(ln) > len(blob) {
			break // torn or absurd length
		}
		payload := blob[off+8 : off+8+int(ln)]
		if crc32.Checksum(payload, frameCRCTable) != crc {
			break // torn frame
		}
		var fr frame
		if err := json.Unmarshal(payload, &fr); err != nil {
			break
		}
		if err := s.applyFrame(&fr); err != nil {
			break
		}
		off += 8 + int(ln)
		good = off
	}
	if good < len(blob) {
		if err := os.Truncate(s.walPath(), int64(good)); err != nil {
			return apperr.InternalErr("truncate torn wal tail: %v", err)
		}
	}
	return nil
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

var lockFile *os.File

func lockDir(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return apperr.InternalErr("open lock file: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return apperr.ConflictErr("data directory is locked by another sandboxd process (%v)", err)
	}
	lockFile = f
	return nil
}

func unlockDir() error {
	if lockFile == nil {
		return nil
	}
	_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	_ = lockFile.Close()
	lockFile = nil
	return nil
}

// UptimeNote exists so the package keeps a compile-time link to time even if
// future refactors drop direct use above (kept intentionally: compaction
// timestamps land in the log line emitted by the daemon).
var _ = time.Now
