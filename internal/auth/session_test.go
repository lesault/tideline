package auth

import (
	"testing"
	"time"
)

func TestSessionCreateAndLookup(t *testing.T) {
	m := NewSessionManager(time.Hour)
	tok := m.Create(42)
	if tok == "" {
		t.Fatal("expected a non-empty session token")
	}
	uid, ok := m.UserID(tok)
	if !ok || uid != 42 {
		t.Fatalf("UserID(%q) = %d,%v; want 42,true", tok, uid, ok)
	}
}

func TestSessionTokensAreUnique(t *testing.T) {
	m := NewSessionManager(time.Hour)
	if m.Create(1) == m.Create(1) {
		t.Fatal("session tokens must be unique")
	}
}

func TestSessionUnknownTokenRejected(t *testing.T) {
	m := NewSessionManager(time.Hour)
	if _, ok := m.UserID("bogus"); ok {
		t.Fatal("unknown token should not resolve")
	}
}

func TestSessionDestroy(t *testing.T) {
	m := NewSessionManager(time.Hour)
	tok := m.Create(7)
	m.Destroy(tok)
	if _, ok := m.UserID(tok); ok {
		t.Fatal("destroyed session should not resolve")
	}
}

func TestSessionExpiry(t *testing.T) {
	m := NewSessionManager(-time.Minute) // already expired on creation
	tok := m.Create(7)
	if _, ok := m.UserID(tok); ok {
		t.Fatal("expired session should not resolve")
	}
}
