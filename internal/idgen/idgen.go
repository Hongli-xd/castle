// Package idgen generates prefixed, sortable-ish, unguessable identifiers.
//
// Format: <prefix>_<26 chars Crockford base32> — 130 bits of crypto-random
// payload. The prefix is load-bearing: Parse validates the kind, so passing
// a snapshot ID where a sandbox ID is expected is rejected at the boundary
// instead of corrupting state deeper in the stack.
package idgen

import (
	"crypto/rand"
	"fmt"
	"strings"
)

const (
	PrefixTenant   = "tnt"
	PrefixKey      = "ak"
	PrefixSandbox  = "sbx"
	PrefixSnapshot = "snp"
	PrefixTemplate = "tpl"
	PrefixEvent    = "evt"
	PrefixRequest  = "req"
)

const (
	rawLen  = 26                                 // 26 * 5 bits = 130 bits
	encoded = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32
)

// New returns a fresh ID with the given prefix, e.g. "sbx_01H9XQ...".
func New(prefix string) string {
	var b [rawLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure means the process entropy pool is broken;
		// refusing to mint weak IDs is the only safe move.
		panic("idgen: crypto/rand failed: " + err.Error())
	}
	var sb strings.Builder
	sb.Grow(len(prefix) + 1 + rawLen)
	sb.WriteString(prefix)
	sb.WriteByte('_')
	for i := 0; i < rawLen; i++ {
		sb.WriteByte(encoded[int(b[i])&0x1f])
	}
	return sb.String()
}

// Parse verifies that id carries the expected prefix and charset.
func Parse(prefix, id string) error {
	want := prefix + "_"
	if !strings.HasPrefix(id, want) {
		return fmt.Errorf("id %q does not have %q prefix", id, want)
	}
	raw := id[len(want):]
	if len(raw) != rawLen {
		return fmt.Errorf("id %q has wrong length", id)
	}
	for i := 0; i < len(raw); i++ {
		if strings.IndexByte(encoded, upper(raw[i])) < 0 {
			return fmt.Errorf("id %q contains invalid character %q", id, raw[i])
		}
	}
	return nil
}

func upper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - ('a' - 'A')
	}
	return c
}
