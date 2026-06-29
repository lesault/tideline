package auth

import (
	"strings"
	"testing"
)

func TestHashPasswordProducesVerifiableHash(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "" || strings.Contains(hash, "correct horse") {
		t.Fatalf("hash looks wrong: %q", hash)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("VerifyPassword rejected the correct password")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Fatal("VerifyPassword accepted a wrong password")
	}
}

func TestHashPasswordUsesRandomSalt(t *testing.T) {
	h1, _ := HashPassword("same")
	h2, _ := HashPassword("same")
	if h1 == h2 {
		t.Fatal("two hashes of the same password should differ (salt not random)")
	}
	// ...but both must still verify.
	if !VerifyPassword(h1, "same") || !VerifyPassword(h2, "same") {
		t.Fatal("both salted hashes should verify")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	for _, bad := range []string{"", "not-a-hash", "$argon2id$garbage"} {
		if VerifyPassword(bad, "anything") {
			t.Fatalf("malformed hash %q should not verify", bad)
		}
	}
}
