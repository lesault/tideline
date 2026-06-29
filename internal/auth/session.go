package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// SessionManager is an in-memory store of login sessions. For a single-binary
// self-hosted app this is deliberately simple: sessions live in memory and are
// cleared on restart (users just log in again). The token is an opaque random
// string stored in a cookie.
type SessionManager struct {
	ttl      time.Duration
	mu       sync.RWMutex
	sessions map[string]sessionEntry
}

type sessionEntry struct {
	userID  int64
	expires time.Time
}

// NewSessionManager returns a manager whose sessions live for ttl.
func NewSessionManager(ttl time.Duration) *SessionManager {
	return &SessionManager{ttl: ttl, sessions: make(map[string]sessionEntry)}
}

// Create starts a session for userID and returns its token.
func (m *SessionManager) Create(userID int64) string {
	tok := newToken()
	m.mu.Lock()
	m.sessions[tok] = sessionEntry{userID: userID, expires: time.Now().Add(m.ttl)}
	m.mu.Unlock()
	return tok
}

// UserID resolves a token to its user, reporting false if unknown or expired.
func (m *SessionManager) UserID(token string) (int64, bool) {
	m.mu.RLock()
	e, ok := m.sessions[token]
	m.mu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		return 0, false
	}
	return e.userID, true
}

// Destroy ends a session (logout).
func (m *SessionManager) Destroy(token string) {
	m.mu.Lock()
	delete(m.sessions, token)
	m.mu.Unlock()
}

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
