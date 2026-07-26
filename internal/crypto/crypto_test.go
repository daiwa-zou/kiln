package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestSealOpenRoundTrip(t *testing.T) {
	key := testKey(t)
	for _, encoded := range []string{
		base64.StdEncoding.EncodeToString(key),
		base64.URLEncoding.EncodeToString(key),
		hex.EncodeToString(key),
		"  " + base64.StdEncoding.EncodeToString(key) + "\n", // env files carry whitespace
	} {
		k, err := NewKeyring(encoded)
		if err != nil {
			t.Fatalf("NewKeyring(%q): %v", encoded[:8], err)
		}
		secret := []byte("ghp_example_personal_access_token")
		ct, nonce, err := k.Seal(secret)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(ct, secret) {
			t.Fatal("ciphertext contains plaintext")
		}
		got, err := k.Open(ct, nonce)
		if err != nil || !bytes.Equal(got, secret) {
			t.Fatalf("Open = %q, %v", got, err)
		}
	}
}

func TestNoncesAreUnique(t *testing.T) {
	k, err := NewKeyring(hex.EncodeToString(testKey(t)))
	if err != nil {
		t.Fatal(err)
	}
	_, n1, _ := k.Seal([]byte("a"))
	_, n2, _ := k.Seal([]byte("a"))
	if bytes.Equal(n1, n2) {
		t.Fatal("two seals reused a nonce")
	}
}

func TestWrongKeyAndTamperingFailClosed(t *testing.T) {
	k1, _ := NewKeyring(hex.EncodeToString(testKey(t)))
	k2, _ := NewKeyring(hex.EncodeToString(testKey(t)))

	ct, nonce, err := k1.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k2.Open(ct, nonce); err == nil {
		t.Error("wrong master key opened a credential")
	}
	ct[0] ^= 0xff
	if _, err := k1.Open(ct, nonce); err == nil {
		t.Error("tampered ciphertext opened")
	}
}

func TestKeyValidation(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"short-hex":   hex.EncodeToString(make([]byte, 16)),
		"not-encoded": "definitely not a key!!",
	}
	for name, key := range cases {
		if _, err := NewKeyring(key); err == nil {
			t.Errorf("%s: NewKeyring accepted %q", name, key)
		}
	}
	// The error for a bad key must guide the operator.
	_, err := NewKeyring("")
	if err == nil || !strings.Contains(err.Error(), "KILN_MASTER_KEY") {
		t.Errorf("empty-key error does not name the setting: %v", err)
	}
}
