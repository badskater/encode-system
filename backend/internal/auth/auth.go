// Package auth provides per-node token generation and hashing.
// Tokens are random 32-byte values; only SHA-256 hashes are stored.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
)

// NewToken generates a random agent token (64 hex chars).
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// HashToken hashes a token for storage. The constant shape keeps lookups
// simple; tokens are high-entropy so no slow KDF is needed.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// VerifyToken compares a presented token to a stored hash in constant time.
func VerifyToken(token, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(HashToken(token)), []byte(hash)) == 1
}
