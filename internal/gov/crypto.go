package gov

import (
	"crypto/sha256"
	"crypto/subtle"
)

func sha256Sum(data []byte) [sha256.Size]byte {
	return sha256.Sum256(data)
}

// constEq is a constant-time comparison for secrets stored as hex/base64.
func constEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
