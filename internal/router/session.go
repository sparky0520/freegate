package router

import (
	"sync"
	"time"
)

type sessionEntry struct {
	bucketKey string
	expiresAt time.Time
}

// SessionStore maintains session stickiness with a 30-minute TTL.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]sessionEntry
}

// NewSessionStore creates an initialized SessionStore.
func NewSessionStore() *SessionStore {
	return &SessionStore{
		sessions: make(map[string]sessionEntry),
	}
}

// GetStickyBucket returns the bucket key for a session if not expired.
func (s *SessionStore) GetStickyBucket(sessionID string, now time.Time) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.sessions[sessionID]
	if !ok || now.After(entry.expiresAt) {
		return "", false
	}
	return entry.bucketKey, true
}

// SetStickyBucket records or refreshes the bucket key for a session with a 30 min TTL.
func (s *SessionStore) SetStickyBucket(sessionID, bucketKey string, now time.Time) {
	if sessionID == "" || bucketKey == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sessions[sessionID] = sessionEntry{
		bucketKey: bucketKey,
		expiresAt: now.Add(30 * time.Minute),
	}
}
