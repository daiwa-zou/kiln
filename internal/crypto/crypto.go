// Package crypto seals connector credentials under the deployment master key.
//
// The contract is deliberately narrow: secrets are written through the API,
// stored only as AEAD ciphertext, and opened in exactly one place -- the
// worker, at sync time. Nothing here supports listing or exporting plaintext;
// a credential that needs reading back is a credential being misused.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Keyring performs AEAD sealing with the master key. AES-256-GCM with a
// per-secret random nonce; ciphertext and nonce land in the two bytea columns
// the credentials table has carried since migration 001.
type Keyring struct {
	aead cipher.AEAD
}

// NewKeyring parses the master key and builds the AEAD. The key must decode
// to exactly 32 bytes, accepted as base64 (standard or URL) or hex, so
// `openssl rand -base64 32` and `openssl rand -hex 32` both work verbatim.
func NewKeyring(masterKey string) (*Keyring, error) {
	key, err := decodeKey(strings.TrimSpace(masterKey))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: build aead: %w", err)
	}
	return &Keyring{aead: aead}, nil
}

// Seal encrypts a secret under a fresh random nonce.
func (k *Keyring) Seal(plaintext []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("crypto: nonce: %w", err)
	}
	return k.aead.Seal(nil, nonce, plaintext, nil), nonce, nil
}

// Open decrypts a stored secret. A wrong master key or tampered row fails
// authentication rather than yielding garbage.
func (k *Keyring) Open(ciphertext, nonce []byte) ([]byte, error) {
	plaintext, err := k.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: open credential: %w", err)
	}
	return plaintext, nil
}

func decodeKey(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("crypto: master key is empty; set KILN_MASTER_KEY (32 random bytes, base64 or hex)")
	}
	// Hex first, and only at the exact 64-character length: a 64-char hex
	// string is also decodable base64 (to the wrong 48 bytes), so charset
	// alone cannot disambiguate.
	if len(s) == 64 {
		if key, err := hex.DecodeString(s); err == nil {
			return key, nil
		}
	}
	for _, decode := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
	} {
		if key, err := decode(s); err == nil {
			if len(key) != 32 {
				return nil, fmt.Errorf("crypto: master key decodes to %d bytes, want 32", len(key))
			}
			return key, nil
		}
	}
	return nil, fmt.Errorf("crypto: master key is neither valid base64 nor hex")
}
