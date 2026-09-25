// Package gov implements multi-tenant governance: authentication, rate
// limiting, quota accounting, and audit. The service layer composes these
// before any business logic runs.
package gov

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
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

// Limiter is a per-tenant, per-operation-group token bucket set. One shared
// map with composite keys keeps memory bounded; idle buckets are swept.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

func NewLimiter() *Limiter {
	l := &Limiter{buckets: map[string]*bucket{}, now: time.Now}
	go l.sweeper()
	return l
}

func bucketKey(tenantID, group string) string { return tenantID + "|" + group }

// Allow consumes one token. rates supplies the group's policy (tenant quota
// over platform default); when the group is unconfigured the request passes.
// Returns the retry-after seconds on denial.
func (l *Limiter) Allow(tenantID, group string, rate *domain.Rate) (bool, int) {
	if rate == nil || rate.PerSecond <= 0 {
		return true, 0
	}
	key := bucketKey(tenantID, group)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok || b.perSec != rate.PerSecond || b.burst != rate.Burst {
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

func (l *Limiter) sweeper() {
	t := time.NewTicker(10 * time.Minute)
	for range t.C {
		cutoff := time.Now().Add(-30 * time.Minute)
		l.mu.Lock()
		for k, b := range l.buckets {
			if b.lastSeen.Before(cutoff) {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
