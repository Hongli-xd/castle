// Package gov implements multi-tenant governance: authentication, rate
// limiting, quota accounting, and audit. The service layer composes these
// before any business logic runs.
package gov

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sandcastle.dev/sandcastle/internal/apperr"
	"sandcastle.dev/sandcastle/internal/domain"
	"sandcastle.dev/sandcastle/internal/store"
)

// ---- authentication ---------------------------------------------------------

const (
	secretBytes  = 30                                 // 240 bits of entropy in the plaintext key
	secretCustom = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567" // Crockford-ish, no lookalikes
	prefixLen    = 16
)

// Authenticator validates API keys and mints new ones.
type Authenticator struct {
	store *store.Store
	now   func() time.Time
}

func NewAuthenticator(st *store.Store) *Authenticator {
	return &Authenticator{store: st, now: time.Now}
}

// MintKey generates a new key, persists only its hash, and returns the
// plaintext exactly once — callers display it to the operator and it is
// never stored, logged, or echoed anywhere else.
func (a *Authenticator) MintKey(tenantID, name string, scope domain.KeyScope, ttl time.Duration) (string, *domain.APIKey, error) {
	var rnd [secretBytes]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", nil, apperr.InternalErr("entropy pool failure: %v", err)
	}
	// base32-ish without padding for a CLI-friendly alphabet
	plaintext := "sk_" + base64.RawURLEncoding.EncodeToString(rnd[:])
	key := domain.NewAPIKey(tenantID, name, scope)
	key.Prefix = plaintext[:min(prefixLen, len(plaintext))]
	key.Hash = hashSecret(plaintext)
	if ttl > 0 {
		exp := a.now().Add(ttl)
		key.ExpiresAt = &exp
	}
	if err := a.store.PutKey(key); err != nil {
		return "", nil, err
	}
	return plaintext, key, nil
}

func hashSecret(plaintext string) string {
	sum := sha256Sum([]byte(plaintext))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Authenticate resolves a bearer/key secret to a live APIKey. Lookup goes
// through the stored prefix index; the hash comparison is constant-time so
// timing cannot bisect the keyspace.
func (a *Authenticator) Authenticate(secret string) (*domain.APIKey, error) {
	secret = strings.TrimSpace(secret)
	if len(secret) < 8 || !strings.HasPrefix(secret, "sk_") {
		return nil, apperr.Auth("missing or malformed API key")
	}
	prefix := secret[:min(prefixLen, len(secret))]
	candidate, ok := a.store.KeyByPrefix(prefix)
	if !ok {
		return nil, apperr.Auth("invalid API key")
	}
	if !constEq(hashSecret(secret), candidate.Hash) {
		return nil, apperr.Auth("invalid API key")
	}
	if !candidate.Valid(a.now()) {
		return nil, apperr.Auth("API key is expired or revoked")
	}
	return candidate, nil
}

// ExtractSecret pulls the key material out of a request, accepting both the
// Authorization: Bearer header and X-API-Key (E2B/Daytona compatibility).
func ExtractSecret(header http.Header) string {
	if v := header.Get("X-API-Key"); v != "" {
		return v
	}
	auth := header.Get("Authorization")
	if auth == "" {
		return ""
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return strings.TrimSpace(parts[1])
	}
	return auth
}

// ---- rate limiting ------------------------------------------------------------

type bucket struct {
	tokens   float64
	last     time.Time
	burst    int
	perSec   float64
	lastSeen time.Time
}

// Limiter is a per-identity, per-operation-group token bucket set. One shared
// map with composite keys keeps memory bounded; idle buckets are swept.
type Limiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	now      func() time.Time
	stopCh   chan struct{}
	stopOnce sync.Once
}

// maxBuckets bounds the bucket table. The sweeper alone is not enough: a
// caller that can mint unbounded identities (any client IP, any key id) would
// otherwise grow the map without limit between sweeps.
const maxBuckets = 200_000

func NewLimiter() *Limiter {
	l := &Limiter{buckets: map[string]*bucket{}, now: time.Now, stopCh: make(chan struct{})}
	go l.sweeper()
	return l
}

// Stop terminates the idle-bucket sweeper. Without this the goroutine outlives
// whatever created the Limiter: one leaked goroutine per Service, which in
// tests means one per test case and in production means the sweeper keeps
// running against a discarded map.
func (l *Limiter) Stop() {
	l.stopOnce.Do(func() { close(l.stopCh) })
}

// bucketKey namespaces identities apart.
//
// The identity is length-prefixed rather than merely separated by a delimiter.
// A delimiter only works while no caller ever passes a string containing it, and
// "no ID will ever contain this byte" is exactly the kind of reasoning that
// stops being true when a later caller starts keying on something new (a header,
// a path, a URL). With a length prefix, no pair of inputs can produce another
// pair's key, whatever they contain.
func bucketKey(identity, group string) string {
	return strconv.Itoa(len(identity)) + ":" + identity + "|" + group
}

// Allow consumes one token for (identity, group). rates supplies the group's
// policy (tenant quota over platform default); when the group is unconfigured
// the request passes. Returns the retry-after seconds on denial.
//
// `identity` must be non-empty and must actually identify the caller: passing
// an empty string (as v1.1.0 did for every platform-admin key) puts unrelated
// callers in one bucket, so one admin key's traffic starves all the others.
func (l *Limiter) Allow(identity, group string, rate *domain.Rate) (bool, int) {
	if rate == nil || rate.PerSecond <= 0 {
		return true, 0
	}
	if identity == "" {
		// Never share a bucket between unidentifiable callers; the caller is
		// expected to pass a namespace-qualified fallback identity.
		identity = "anonymous"
	}
	key := bucketKey(identity, group)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok || b.perSec != rate.PerSecond || b.burst != rate.Burst {
		if !ok && len(l.buckets) >= maxBuckets {
			l.evictOldestLocked(maxBuckets / 8)
		}
		b = &bucket{tokens: float64(rate.Burst), last: now, burst: rate.Burst, perSec: rate.PerSecond}
		l.buckets[key] = b
	}
	b.lastSeen = now
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * b.perSec
	if b.tokens > float64(b.burst) {
		b.tokens = float64(b.burst)
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	need := 1 - b.tokens
	retry := int(need/b.perSec + 0.999)
	if retry < 1 {
		retry = 1
	}
	return false, retry
}

// Len reports how many buckets are live — used by tests and by operators
// watching for bucket-table growth.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Sweep drops buckets idle for longer than idleTTL. Exported so tests can
// exercise eviction without waiting for the ticker.
func (l *Limiter) Sweep(idleTTL time.Duration) int {
	cutoff := l.now().Add(-idleTTL)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sweepLocked(cutoff)
}

func (l *Limiter) sweepLocked(cutoff time.Time) int {
	n := 0
	for k, b := range l.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(l.buckets, k)
			n++
		}
	}
	return n
}

// evictOldestLocked removes the `n` least-recently-seen buckets. Called only
// when the table is at its cap; amortized by evicting a slice of it at a time.
func (l *Limiter) evictOldestLocked(n int) {
	type entry struct {
		key  string
		seen time.Time
	}
	all := make([]entry, 0, len(l.buckets))
	for k, b := range l.buckets {
		all = append(all, entry{k, b.lastSeen})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seen.Before(all[j].seen) })
	if n > len(all) {
		n = len(all)
	}
	for _, e := range all[:n] {
		delete(l.buckets, e.key)
	}
}

func (l *Limiter) sweeper() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-l.stopCh:
			return
		case <-t.C:
			l.Sweep(30 * time.Minute)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
