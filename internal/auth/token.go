// Package auth authenticates API requests against the tokens table and scopes
// what an authenticated caller can see.
//
// Tokens are bearer secrets of the form kiln_<hex>. Only their SHA-256 hash is
// stored, so a database read cannot recover a usable credential.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// TokenPrefix marks kiln tokens so a leaked one is recognizable in scanners
// and log scrubbing.
const TokenPrefix = "kiln_"

// NewToken mints a bearer token, returning the plaintext (shown once, never
// stored) and the hash that goes in the tokens table.
func NewToken() (plain, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("auth: generate token: %w", err)
	}
	plain = TokenPrefix + hex.EncodeToString(b[:])
	return plain, HashToken(plain), nil
}

// HashToken maps a plaintext token to its stored form.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// looksLikeToken filters obvious garbage before hashing, without leaking
// timing about stored values: the comparison against the database is by hash.
func looksLikeToken(plain string) bool {
	return strings.HasPrefix(plain, TokenPrefix) && len(plain) > len(TokenPrefix)
}
