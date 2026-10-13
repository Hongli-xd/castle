package service

// Exec idempotency. The industry survey's §5.1 is the forcing function: MCP
// became a stateless protocol in mid-2026, so a client that loses an exec
// response has no transport-level way to know whether the command ran — it
// must retry, and the runtime must make the retry safe. Without a key, a
// retried exec runs twice, which for an agent means two files written, two
// e-mails sent, two charges.
//
// The contract mirrors the Stripe idempotency key that every agent already
// knows how to issue: the client supplies an opaque key; the first call
// executes and remembers its result; every later call with the same key
// returns the remembered result with `replayed: true` and does not run the
// command again. Two concurrent calls with the same key single-flight into one
// execution. Reusing a key with *different* parameters is a client bug and is
// refused with a conflict rather than silently answered with a stale result.
//
// The cache is in-memory and bounded by both size and TTL. That is the right
// durability for this layer: the failure an idempotency key recovers from is a
// lost *response*, which is re-sent within seconds. Persisting full exec
// output (up to MaxExecOutput, 8 MiB) into the WAL would turn one retry
// pattern into an fsync amplifier and an unbounded disk. A daemon restart
// clears the cache; the sandboxes themselves survive via the reconciler, and a
// post-restart retry is a fresh execution — the same boundary Stripe draws
// between "retry within the window" and "reconstruct from scratch".

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/runtime"
)

const (
	idemDefaultMaxEntries = 4096
	idemDefaultTTL        = 10 * time.Minute
	idemMaxKeyLen         = 128
)

type idemEntry struct {
	result  *runtime.ExecResult
	digest  string
	seq     uint64
	expires time.Time
}

type idemCall struct {
	done   chan struct{}
	result *runtime.ExecResult
	digest string
	err    error
}

type idemRegistry struct {
	mu         sync.Mutex
	entries    map[string]*idemEntry
	inFlight   map[string]*idemCall
	seq        uint64
	maxEntries int
	ttl        time.Duration
	now        func() time.Time
}

func newIdemRegistry(maxEntries int, ttl time.Duration) *idemRegistry {
	if maxEntries <= 0 {
		maxEntries = idemDefaultMaxEntries
	}
	if ttl <= 0 {
		ttl = idemDefaultTTL
	}
	return &idemRegistry{
		entries:    map[string]*idemEntry{},
		inFlight:   map[string]*idemCall{},
		maxEntries: maxEntries,
		ttl:        ttl,
		now:        time.Now,
	}
}

func cloneExecResult(r *runtime.ExecResult) *runtime.ExecResult {
	if r == nil {
		return nil
	}
	c := *r
	c.Stdout = append([]byte(nil), r.Stdout...)
	c.Stderr = append([]byte(nil), r.Stderr...)
	return &c
}

// validateIdempotencyKey bounds client input before it becomes a map key. The
// key is attacker-chosen, so it must be length-bounded and NUL-free; any other
// character is acceptable because the key is opaque.
func validateIdempotencyKey(k string) error {
	if k == "" {
		return apperr.Invalid("idempotency_key must not be empty when provided")
	}
	if len(k) > idemMaxKeyLen {
		return apperr.Invalid("idempotency_key must be at most %d bytes", idemMaxKeyLen)
	}
	if strings.IndexByte(k, 0) >= 0 {
		return apperr.Invalid("idempotency_key must not contain NUL")
	}
	return nil
}

// execFingerprint canonicalizes the parameters that define "the same command".
// encoding/json sorts map keys, so env ordering cannot make two identical
// requests hash differently.
func execFingerprint(req runtime.ExecRequest) string {
	b, _ := json.Marshal(struct {
		Argv      []string          `json:"argv"`
		Env       map[string]string `json:"env"`
		User      string            `json:"user"`
		WorkDir   string            `json:"workdir"`
		TimeoutNs int64             `json:"timeout_ns"`
		MaxOutput int               `json:"max_output"`
	}{req.Argv, req.Env, req.User, req.WorkDir, int64(req.Timeout), req.MaxOutput})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// run executes fn exactly once per key. It returns the result and whether that
// result was a replay (served from a prior execution) rather than a fresh
// execution by this caller. fn runs outside any lock; the registry only
// coordinates.
func (r *idemRegistry) run(ctx context.Context, key, digest string, fn func() (*runtime.ExecResult, error)) (*runtime.ExecResult, bool, error) {
	for {
		r.mu.Lock()
		if e, ok := r.entries[key]; ok && r.now().Before(e.expires) {
			res, dg := cloneExecResult(e.result), e.digest
			r.mu.Unlock()
			if dg != digest {
				return nil, true, reusedKeyConflict()
			}
			return res, true, nil
		}
		if c, ok := r.inFlight[key]; ok {
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, false, apperr.From(ctx.Err())
			case <-c.done:
				if c.err != nil {
					return nil, true, c.err
				}
				if c.digest != digest {
					return nil, true, reusedKeyConflict()
				}
				return cloneExecResult(c.result), true, nil
			}
		}
		c := &idemCall{done: make(chan struct{})}
		r.inFlight[key] = c
		r.mu.Unlock()

		res, err := fn()
		c.result, c.err, c.digest = res, err, digest
		close(c.done)

		r.mu.Lock()
		delete(r.inFlight, key)
		if err == nil {
			r.storeLocked(key, digest, res)
		}
		r.mu.Unlock()

		return cloneExecResult(res), false, err
	}
}

func reusedKeyConflict() error {
	return apperr.ConflictErr("idempotency_key was already used with different parameters; a key names exactly one logical operation")
}

// storeLocked records a completed result, evicting expired then oldest entries
// so the table stays bounded regardless of how many keys a caller invents.
func (r *idemRegistry) storeLocked(key, digest string, res *runtime.ExecResult) {
	if len(r.entries) >= r.maxEntries {
		r.evictLocked()
	}
	r.seq++
	r.entries[key] = &idemEntry{
		result:  cloneExecResult(res),
		digest:  digest,
		seq:     r.seq,
		expires: r.now().Add(r.ttl),
	}
}

func (r *idemRegistry) evictLocked() {
	now := r.now()
	for k, e := range r.entries {
		if !now.Before(e.expires) {
			delete(r.entries, k)
		}
	}
	if len(r.entries) < r.maxEntries {
		return
	}
	var oldestKey string
	var oldestSeq uint64 = ^uint64(0)
	for k, e := range r.entries {
		if e.seq < oldestSeq {
			oldestSeq = e.seq
			oldestKey = k
		}
	}
	if oldestKey != "" {
		delete(r.entries, oldestKey)
	}
}
