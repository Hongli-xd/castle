// Package store persists domain entities behind a crash-safe write-ahead log.
//
// Design (see docs/DESIGN.md §6):
//   - every mutation is one WAL frame: [len u32][crc32 u32][json payload];
//     the frame is fsynced before the call returns;
//   - on open the store replays the log and truncates a torn tail detected
//     by CRC or short read — a transaction either fully exists or is absent;
//   - compaction rewrites a point-in-time snapshot (data.json) atomically
//     and truncates the log;
//   - an flock on <dir>/lock prevents two daemons from sharing a data dir;
//   - updates use compare-and-set on Version: callers pass the version they
//     read (BaseVersion), so "mutate three times in memory, fsync once" is
//     legal while concurrent writers are still detected precisely.
//
// The interface is deliberately narrow; a Postgres-backed implementation can
// replace this package without touching callers.
//
// # Invariants
//
// These are the rules that make the store crash-correct. They are enforced by
// TestAppendAlwaysUnderWriteLock and the recovery tests, not just by comment:
//
//  1. Every mutation appends its WAL frame AND publishes to the in-memory maps
//     inside the same s.mu write-lock window (appendLocked). Splitting those
//     two steps — as v1.1.0's Put* family did — lets a crash replay a history
//     the process never served, and silently loses acknowledged writes.
//  2. The store never keeps a caller-owned pointer and never hands out one of
//     its own: every boundary clones. Otherwise a caller mutating an object it
//     still holds would change persisted state with no WAL frame.
//  3. A failed write rewinds the log to the frame boundary it started at. An
//     fsync failure also rewinds, so a caller told "failed" cannot have its
//     mutation replayed as durable (a phantom write).
//  4. If a rewind fails, the store is poisoned: mutations fail fast and
//     Healthy() reports why, rather than running on a log of unknown shape.
//  5. Recovery distinguishes a torn tail (routine, droppable) from mid-log
//     damage (acknowledged and fsynced data, never dropped silently).
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
)

type Options struct {
	// MaxEvents caps in-memory audit events; older events fall out of query
	// results (the WAL on disk still holds them until compaction).
	MaxEvents int
	// DisableSync skips fsync — benchmarks and throwaway test stores only.
	DisableSync bool
	// Repair decides how recovery treats a damaged log. The zero value,
	// RepairTailOnly, is the safe default: torn tails are routine crash
	// debris, mid-log damage is not.
	Repair RepairPolicy
}

type Store struct {
	mu   sync.RWMutex
	dir  string
	wal  *os.File
	opts Options

	// lockFile is this store's flock handle. It must be per-instance: a
	// package-level handle lets one store's Close release another's lock.
	lockFile *os.File
	// poisoned is set when the WAL reached an unknown state (a write failed
	// and the rollback failed too). Every mutation then fails fast instead of
	// pretending the store is writable.
	poisoned error
	// repair records what recovery found at open time.
	repair RepairReport

	tenants   map[string]*domain.Tenant
	keys      map[string]*domain.APIKey
	keysByPre map[string]*domain.APIKey
	sandboxes map[string]*domain.Sandbox
	snapshots map[string]*domain.Snapshot
	templates map[string]*domain.Template
	usages    map[string]*domain.Usage
	events    []*domain.AuditEvent // append-only, capped
}

// Open prepares (or recovers) a store rooted at dir.
func Open(dir string, opts Options) (*Store, error) {
	if opts.MaxEvents <= 0 {
		opts.MaxEvents = 100_000
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, apperr.InternalErr("create data dir: %v", err)
	}
	lock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	s := &Store{
		dir:       dir,
		opts:      opts,
		lockFile:  lock,
		tenants:   map[string]*domain.Tenant{},
		keys:      map[string]*domain.APIKey{},
		keysByPre: map[string]*domain.APIKey{},
		sandboxes: map[string]*domain.Sandbox{},
		snapshots: map[string]*domain.Snapshot{},
		templates: map[string]*domain.Template{},
		usages:    map[string]*domain.Usage{},
	}
	// Any failure past this point must release the directory lock, otherwise
	// this process leaks it and holds the data dir hostage for its lifetime.
	opened := false
	defer func() {
		if !opened {
			_ = unlockDir(s.lockFile)
			s.lockFile = nil
		}
	}()
	if err := s.loadCompacted(); err != nil {
		return nil, err
	}
	if err := s.replayAndRepair(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.walPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, apperr.InternalErr("open wal: %v", err)
	}
	s.wal = f
	opened = true
	return s, nil
}

func (s *Store) walPath() string       { return filepath.Join(s.dir, "wal.log") }
func (s *Store) compactedPath() string { return filepath.Join(s.dir, "data.json") }

// Repair reports what recovery found and did with the log at open time. It is
// advisory: an intact log reports Action "none".
func (s *Store) Repair() RepairReport {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repair
}

// Healthy returns the reason the store cannot accept writes, or nil. /readyz
// uses it so a poisoned store is taken out of rotation instead of serving
// writes that will all fail.
func (s *Store) Healthy() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.poisoned
}

// Close releases the WAL handle and the directory lock. Data is durable at
// every mutation boundary; Close only needs to be tidy. It is idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wal != nil {
		_ = s.wal.Close()
		s.wal = nil
	}
	if s.lockFile != nil {
		err := unlockDir(s.lockFile)
		s.lockFile = nil
		return err
	}
	return nil
}

// ---- entity records -------------------------------------------------------

type frameKind string

const (
	fkTenant   frameKind = "tenant"
	fkKey      frameKind = "key"
	fkSandbox  frameKind = "sandbox"
	fkSnapshot frameKind = "snapshot"
	fkTemplate frameKind = "template"
	fkUsage    frameKind = "usage"
	fkEvent    frameKind = "event"
)

type frame struct {
	K    frameKind       `json:"k"`
	Op   string          `json:"op"` // "put" | "del"
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ---- tenants --------------------------------------------------------------

// PutTenant inserts or replaces a tenant. The frame is appended and the map
// is updated inside the same write-lock window, so the log on disk and the
// in-memory view can never be ordered differently. The store keeps its own
// copy: a caller that mutates the pointer afterwards cannot change persisted
// state without going through UpdateTenant.
func (s *Store) PutTenant(t *domain.Tenant) error {
	if t.ID == "" {
		return apperr.InternalErr("tenant missing ID")
	}
	if t.Version == 0 {
		t.Version = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendLocked(fkTenant, "put", t.ID, t); err != nil {
		return err
	}
	s.tenants[t.ID] = cloneTenant(t)
	return nil
}

func (s *Store) GetTenant(id string) (*domain.Tenant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tenants[id]
	if !ok {
		return nil, false
	}
	return cloneTenant(t), true
}

// UpdateTenant runs fn under the write lock with CAS on baseVersion. fn may
// mutate t freely (maps included): the store operates on a private deep copy
// and discards it if fn fails or the CAS check fails.
func (s *Store) UpdateTenant(id string, baseVersion int64, fn func(t *domain.Tenant) error) (*domain.Tenant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.tenants[id]
	if !ok {
		return nil, apperr.NotFoundErr("tenant")
	}
	next := cloneTenant(cur)
	if err := fn(next); err != nil {
		return nil, err
	}
	if baseVersion >= 0 && cur.Version != baseVersion {
		return nil, apperr.ConflictErr("tenant was modified concurrently (have v%d, read v%d)", cur.Version, baseVersion)
	}
	next.Version = cur.Version + 1
	next.UpdatedAt = time.Now().UTC()
	if err := next.Validate(domain.Limits{MaxCPUMillicores: 1 << 30, MaxMemoryMiB: 1 << 30, MaxTimeoutSeconds: 1 << 30}); err != nil {
		return nil, err
	}
	if err := s.appendLocked(fkTenant, "put", id, next); err != nil {
		return nil, err
	}
	s.tenants[id] = next
	return cloneTenant(next), nil
}

func (s *Store) ListTenants() []*domain.Tenant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.Tenant, 0, len(s.tenants))
	for _, t := range s.tenants {
		out = append(out, cloneTenant(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func cloneTenant(t *domain.Tenant) *domain.Tenant {
	c := *t
	rates := make(map[string]*domain.Rate, len(t.Quota.Rates))
	for g, r := range t.Quota.Rates {
		rc := *r
		rates[g] = &rc
	}
	c.Quota.Rates = rates
	return &c
}

// ---- API keys ---------------------------------------------------------------

func (s *Store) PutKey(k *domain.APIKey) error {
	if k.ID == "" || k.Prefix == "" {
		return apperr.InternalErr("api key missing ID/prefix")
	}
	if k.Version == 0 {
		k.Version = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendLocked(fkKey, "put", k.ID, k); err != nil {
		return err
	}
	stored := cloneKey(k)
	s.keys[stored.ID] = stored
	s.keysByPre[stored.Prefix] = stored
	return nil
}

func (s *Store) GetKey(id string) (*domain.APIKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[id]
	if !ok {
		return nil, false
	}
	return cloneKey(k), true
}

func (s *Store) KeyByPrefix(prefix string) (*domain.APIKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keysByPre[prefix]
	if !ok {
		return nil, false
	}
	return cloneKey(k), true
}

// cloneKey copies an API key, including the pointer-valued timestamps: a
// shallow copy would share them, so a later write through either copy would be
// visible from the other.
func cloneKey(k *domain.APIKey) *domain.APIKey {
	c := *k
	c.ExpiresAt = cloneTimePtr(k.ExpiresAt)
	c.RevokedAt = cloneTimePtr(k.RevokedAt)
	c.LastUsedAt = cloneTimePtr(k.LastUsedAt)
	return &c
}

func cloneTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func (s *Store) UpdateKey(id string, baseVersion int64, fn func(k *domain.APIKey) error) (*domain.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.keys[id]
	if !ok {
		return nil, apperr.NotFoundErr("api key")
	}
	next := *cloneKey(cur)
	if err := fn(&next); err != nil {
		return nil, err
	}
	if baseVersion >= 0 && cur.Version != baseVersion {
		return nil, apperr.ConflictErr("api key was modified concurrently")
	}
	next.Version = cur.Version + 1
	if err := s.appendLocked(fkKey, "put", id, &next); err != nil {
		return nil, err
	}
	stored := cloneKey(&next)
	if cur.Prefix != stored.Prefix {
		delete(s.keysByPre, cur.Prefix)
	}
	s.keys[id] = stored
	s.keysByPre[stored.Prefix] = stored
	return cloneKey(stored), nil
}

func (s *Store) ListKeys(tenantID string) []*domain.APIKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*domain.APIKey
	for _, k := range s.keys {
		if k.TenantID == tenantID {
			out = append(out, cloneKey(k))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ---- sandboxes ------------------------------------------------------------

func (s *Store) PutSandbox(sb *domain.Sandbox) error {
	if sb.ID == "" {
		return apperr.InternalErr("sandbox missing ID")
	}
	if sb.Version == 0 {
		sb.Version = 1 // a persisted entity is at least at its first version
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendLocked(fkSandbox, "put", sb.ID, sb); err != nil {
		return err
	}
	s.sandboxes[sb.ID] = cloneSandbox(sb)
	return nil
}

func (s *Store) GetSandbox(id string) (*domain.Sandbox, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sb, ok := s.sandboxes[id]
	if !ok {
		return nil, false
	}
	return cloneSandbox(sb), true
}

// UpdateSandbox is the only sanctioned way to mutate a live sandbox. It
// deep-copies, applies fn, enforces the state machine (unless allowAnyState),
// checks CAS against baseVersion, appends and fsyncs, then publishes.
func (s *Store) UpdateSandbox(id string, baseVersion int64, fn func(sb *domain.Sandbox) error) (*domain.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.sandboxes[id]
	if !ok {
		return nil, apperr.NotFoundErr("sandbox")
	}
	next := cloneSandbox(cur)
	if err := fn(next); err != nil {
		return nil, err
	}
	if baseVersion >= 0 && cur.Version != baseVersion {
		return nil, apperr.ConflictErr("sandbox %s was modified concurrently", id)
	}
	next.Version = cur.Version + 1
	next.UpdatedAt = time.Now().UTC()
	if err := s.appendLocked(fkSandbox, "put", id, next); err != nil {
		return nil, err
	}
	s.sandboxes[id] = next
	return cloneSandbox(next), nil
}

func (s *Store) DeleteSandbox(id string, baseVersion int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.sandboxes[id]
	if !ok {
		return apperr.NotFoundErr("sandbox")
	}
	if baseVersion >= 0 && cur.Version != baseVersion {
		return apperr.ConflictErr("sandbox %s was modified concurrently", id)
	}
	if err := s.appendLocked(fkSandbox, "del", id, nil); err != nil {
		return err
	}
	delete(s.sandboxes, id)
	return nil
}

func (s *Store) ListSandboxes(tenantID string, state *domain.SandboxState) []*domain.Sandbox {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.Sandbox, 0, 8)
	for _, sb := range s.sandboxes {
		if tenantID != "" && sb.TenantID != tenantID {
			continue
		}
		if state != nil && sb.State != *state {
			continue
		}
		out = append(out, cloneSandbox(sb))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// ForEachSandbox runs fn over every sandbox under the read lock. The
// reconciler and the TTL sweeper use it; fn must not retain the pointer.
func (s *Store) ForEachSandbox(fn func(sb *domain.Sandbox) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sb := range s.sandboxes {
		c := *sb
		if !fn(&c) {
			return
		}
	}
}

// ForEachSnapshot and ForEachTemplate enumerate every record regardless of
// owner. ListSnapshots("") happens to mean "all", but ListTemplates("") means
// "public plus the empty tenant" — a difference that is easy to miss and
// dangerous to rely on. The reconciler's image pass must see every
// referenced image, so it asks for all of them explicitly.
func (s *Store) ForEachSnapshot(fn func(sn *domain.Snapshot) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sn := range s.snapshots {
		if !fn(cloneSnapshot(sn)) {
			return
		}
	}
}

func (s *Store) ForEachTemplate(fn func(t *domain.Template) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.templates {
		if !fn(cloneTemplate(t)) {
			return
		}
	}
}

// cloneSpec copies a SandboxSpec, including its map and slice fields. A plain
// struct copy would leave Env/Egress.Allow aliased to the caller.
func cloneSpec(sp domain.SandboxSpec) domain.SandboxSpec {
	c := sp
	if sp.Env != nil {
		c.Env = make(map[string]string, len(sp.Env))
		for k, v := range sp.Env {
			c.Env[k] = v
		}
	}
	if sp.Egress.Allow != nil {
		c.Egress.Allow = append([]string(nil), sp.Egress.Allow...)
	}
	return c
}

func cloneSandbox(sb *domain.Sandbox) *domain.Sandbox {
	c := *sb
	if sb.Metadata != nil {
		c.Metadata = make(map[string]string, len(sb.Metadata))
		for k, v := range sb.Metadata {
			c.Metadata[k] = v
		}
	}
	c.Spec = cloneSpec(sb.Spec)
	return &c
}

func cloneSnapshot(sn *domain.Snapshot) *domain.Snapshot {
	c := *sn
	c.Spec = cloneSpec(sn.Spec)
	return &c
}

func cloneTemplate(t *domain.Template) *domain.Template {
	c := *t
	c.Composition = append([]string(nil), t.Composition...)
	c.DefaultSpec = cloneSpec(t.DefaultSpec)
	return &c
}

func cloneEvent(ev *domain.AuditEvent) *domain.AuditEvent {
	c := *ev
	if ev.Detail != nil {
		c.Detail = make(map[string]string, len(ev.Detail))
		for k, v := range ev.Detail {
			c.Detail[k] = v
		}
	}
	return &c
}

// ---- snapshots / templates --------------------------------------------------

func (s *Store) PutSnapshot(sn *domain.Snapshot) error {
	if sn.ID == "" {
		return apperr.InternalErr("snapshot missing ID")
	}
	if sn.Version == 0 {
		sn.Version = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendLocked(fkSnapshot, "put", sn.ID, sn); err != nil {
		return err
	}
	s.snapshots[sn.ID] = cloneSnapshot(sn)
	return nil
}

func (s *Store) GetSnapshot(id string) (*domain.Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sn, ok := s.snapshots[id]
	if !ok {
		return nil, false
	}
	return cloneSnapshot(sn), true
}

func (s *Store) DeleteSnapshot(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.snapshots[id]; !ok {
		return apperr.NotFoundErr("snapshot")
	}
	if err := s.appendLocked(fkSnapshot, "del", id, nil); err != nil {
		return err
	}
	delete(s.snapshots, id)
	return nil
}

func (s *Store) ListSnapshots(tenantID string) []*domain.Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.Snapshot, 0, 8)
	for _, sn := range s.snapshots {
		if tenantID != "" && sn.TenantID != tenantID {
			continue
		}
		out = append(out, cloneSnapshot(sn))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// SnapshotBytesUsed sums snapshot storage for a tenant (quota accounting).
func (s *Store) SnapshotBytesUsed(tenantID string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total int64
	for _, sn := range s.snapshots {
		if sn.TenantID == tenantID {
			total += sn.SizeBytes
		}
	}
	return total
}

func (s *Store) SnapshotCount(tenantID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, sn := range s.snapshots {
		if sn.TenantID == tenantID {
			n++
		}
	}
	return n
}

func (s *Store) PutTemplate(t *domain.Template) error {
	if t.ID == "" {
		return apperr.InternalErr("template missing ID")
	}
	if t.Version == 0 {
		t.Version = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendLocked(fkTemplate, "put", t.ID, t); err != nil {
		return err
	}
	s.templates[t.ID] = cloneTemplate(t)
	return nil
}

func (s *Store) GetTemplate(id string) (*domain.Template, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.templates[id]
	if !ok {
		return nil, false
	}
	return cloneTemplate(t), true
}

func (s *Store) DeleteTemplate(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.templates[id]; !ok {
		return apperr.NotFoundErr("template")
	}
	if err := s.appendLocked(fkTemplate, "del", id, nil); err != nil {
		return err
	}
	delete(s.templates, id)
	return nil
}

func (s *Store) ListTemplates(tenantID string) []*domain.Template {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.Template, 0, 8)
	for _, t := range s.templates {
		if !t.Public && t.TenantID != tenantID {
			continue
		}
		out = append(out, cloneTemplate(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ---- usage (metering) -------------------------------------------------------

func cloneUsage(u *domain.Usage) *domain.Usage {
	if u == nil {
		return nil
	}
	c := *u
	return &c
}

// GetUsage returns a tenant's persisted usage record, or nil when none exists
// yet (a tenant with no sampled activity).
func (s *Store) GetUsage(tenantID string) (*domain.Usage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.usages[tenantID]
	if !ok {
		return nil, false
	}
	return cloneUsage(u), true
}

// UpdateUsage applies fn to a tenant's usage record, creating it on first
// sight. It is CAS-free: the metering loop is the sole writer, and its
// cadence (one frame per tenant per flush) makes a compare-and-set pointless —
// the only writer that can conflict with itself is a graceful shutdown flush.
func (s *Store) UpdateUsage(tenantID string, fn func(u *domain.Usage) error) (*domain.Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.usages[tenantID]
	if !ok {
		cur = domain.NewUsage(tenantID)
	}
	next := cloneUsage(cur)
	if err := fn(next); err != nil {
		return nil, err
	}
	next.Version = cur.Version + 1
	next.UpdatedAt = time.Now().UTC()
	if err := s.appendLocked(fkUsage, "put", tenantID, next); err != nil {
		return nil, err
	}
	s.usages[tenantID] = next
	return cloneUsage(next), nil
}

// ListUsage returns every persisted usage record, for platform-wide metering.
func (s *Store) ListUsage() []*domain.Usage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.Usage, 0, len(s.usages))
	for _, u := range s.usages {
		out = append(out, cloneUsage(u))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TenantID < out[j].TenantID })
	return out
}

// ---- events (append-only) ---------------------------------------------------

// AppendEvent persists one audit event. Events are never updated or deleted.
//
// This runs on the authentication-failure path, so it is the highest-frequency
// fsync in the product. Callers that can be reached without credentials must
// throttle before calling it (see httpapi's pre-auth limiter), or an
// unauthenticated flood turns into one fsync per request.
func (s *Store) AppendEvent(ev *domain.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendLocked(fkEvent, "put", ev.ID, ev); err != nil {
		return err
	}
	s.events = append(s.events, cloneEvent(ev))
	if len(s.events) > s.opts.MaxEvents {
		// drop oldest in bulk to avoid slice thrash
		drop := s.opts.MaxEvents / 10
		s.events = s.events[drop:]
	}
	return nil
}

// EventQuery filters audit events. Empty fields match everything. Before
// zero means "now". Results are newest-first, limited.
type EventQuery struct {
	TenantID  string
	SandboxID string
	Action    string
	Actor     string
	Before    time.Time
	Limit     int
}

func (s *Store) ListEvents(q EventQuery) []*domain.AuditEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 100
	}
	if q.Before.IsZero() {
		q.Before = time.Now().UTC().Add(time.Minute)
	}
	out := make([]*domain.AuditEvent, 0, q.Limit)
	for i := len(s.events) - 1; i >= 0 && len(out) < q.Limit; i-- {
		ev := s.events[i]
		if q.TenantID != "" && ev.TenantID != q.TenantID {
			continue
		}
		if q.SandboxID != "" && ev.SandboxID != q.SandboxID {
			continue
		}
		if q.Action != "" && ev.Action != q.Action {
			continue
		}
		if q.Actor != "" && ev.Actor != q.Actor {
			continue
		}
		if !ev.TS.Before(q.Before) {
			continue
		}
		out = append(out, cloneEvent(ev))
	}
	return out
}

// CountEventsSince counts matching events after t — used for abuse metrics.
func (s *Store) CountEventsSince(t time.Time, tenantID, action string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for i := len(s.events) - 1; i >= 0; i-- {
		ev := s.events[i]
		if ev.TS.Before(t) {
			break
		}
		if tenantID != "" && ev.TenantID != tenantID {
			continue
		}
		if action != "" && ev.Action != action {
			continue
		}
		n++
	}
	return n
}

// Stats summarizes the store for /metrics and startup logs.
type Stats struct {
	Tenants, Keys, Sandboxes, Snapshots, Templates, Events int
}

func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Stats{
		Tenants:   len(s.tenants),
		Keys:      len(s.keys),
		Sandboxes: len(s.sandboxes),
		Snapshots: len(s.snapshots),
		Templates: len(s.templates),
		Events:    len(s.events),
	}
}

// Backup copies a consistent point-in-time snapshot of all entities into a
// new directory (commercial ops: "copy the data dir" must not race writes).
func (s *Store) Backup(dir string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return apperr.InternalErr("backup mkdir: %v", err)
	}
	blob, err := s.marshalAll()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "data.json"), blob, 0o600); err != nil {
		return apperr.InternalErr("backup write: %v", err)
	}
	return nil
}

func (s *Store) marshalAll() ([]byte, error) {
	all := struct {
		Tenants   []*domain.Tenant     `json:"tenants"`
		Keys      []*domain.APIKey     `json:"keys"`
		Sandboxes []*domain.Sandbox    `json:"sandboxes"`
		Snapshots []*domain.Snapshot   `json:"snapshots"`
		Templates []*domain.Template   `json:"templates"`
		Usages    []*domain.Usage      `json:"usages"`
		Events    []*domain.AuditEvent `json:"events"`
	}{}
	for _, t := range s.tenants {
		all.Tenants = append(all.Tenants, t)
	}
	for _, k := range s.keys {
		all.Keys = append(all.Keys, k)
	}
	for _, sb := range s.sandboxes {
		all.Sandboxes = append(all.Sandboxes, sb)
	}
	for _, sn := range s.snapshots {
		all.Snapshots = append(all.Snapshots, sn)
	}
	for _, t := range s.templates {
		all.Templates = append(all.Templates, t)
	}
	for _, u := range s.usages {
		all.Usages = append(all.Usages, u)
	}
	all.Events = s.events
	blob, err := json.MarshalIndent(all, "", " ")
	if err != nil {
		return nil, apperr.InternalErr("marshal backup: %v", err)
	}
	return blob, nil
}

func init() { // fail fast if time handling is broken (paranoia, cheap)
	if time.Now().IsZero() {
		panic(fmt.Sprintf("store: system clock reported zero time"))
	}
}

func (s Stats) String() string {
	return fmt.Sprintf("tenants=%d keys=%d sandboxes=%d snapshots=%d templates=%d events=%d",
		s.Tenants, s.Keys, s.Sandboxes, s.Snapshots, s.Templates, s.Events)
}
