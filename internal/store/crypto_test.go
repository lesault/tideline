package store

import "testing"

func TestEncryptFieldRoundTrips(t *testing.T) {
	key := deriveKey("a strong secret")
	enc, err := encryptField(key, "wallabag-password")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if enc == "wallabag-password" || enc == "" {
		t.Fatalf("ciphertext should not equal plaintext: %q", enc)
	}
	got, err := decryptField(key, enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "wallabag-password" {
		t.Fatalf("round-trip = %q, want original", got)
	}
}

func TestEncryptFieldUsesRandomNonce(t *testing.T) {
	key := deriveKey("k")
	a, _ := encryptField(key, "same")
	b, _ := encryptField(key, "same")
	if a == b {
		t.Fatal("two encryptions of the same value should differ (nonce)")
	}
}

func TestDecryptFieldPassesThroughLegacyPlaintext(t *testing.T) {
	// A value without the enc: prefix is treated as legacy plaintext.
	got, err := decryptField(deriveKey("k"), "plain-old-password")
	if err != nil {
		t.Fatalf("decrypt legacy: %v", err)
	}
	if got != "plain-old-password" {
		t.Fatalf("legacy passthrough = %q", got)
	}
}

func TestDecryptFieldFailsWithWrongKey(t *testing.T) {
	enc, _ := encryptField(deriveKey("right"), "secret")
	if _, err := decryptField(deriveKey("wrong"), enc); err == nil {
		t.Fatal("decrypt with the wrong key should fail")
	}
}
