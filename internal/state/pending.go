// internal/state/pending.go
package state

import (
	"sync"
	"time"

	"github.com/DanielFillol/Jarvis/internal/jira"
)

// PendingIssue represents a Jira draft awaiting additional
// information (project and/or issue type) before it can be created.  A
// PendingIssue is keyed by channel and thread timestamp.  It stores
// timestamps and original text to properly attribute the origin of the
// command when the card is ultimately created.
type PendingIssue struct {
	CreatedAt    time.Time
	Channel      string
	ThreadTs     string
	OriginTs     string
	OriginalText string
	Source       string // "thread_based" or "explicit"
	Draft        jira.IssueDraft
	Drafts       []jira.IssueDraft // multi-card flow: queue of drafts awaiting confirmation
	NeedProject  bool
	NeedType     bool
}

// Store maintains a mapping of pending issues per thread.  Entries
// expire after a configurable TTL.
type Store struct {
	mu       sync.Mutex
	byThread map[string]*PendingIssue
	ttl      time.Duration
}

// NewStore creates a new pending store with the given time-to-live.  A
// zero or negative duration disables expiration entirely.
func NewStore(ttl time.Duration) *Store {
	return &Store{byThread: make(map[string]*PendingIssue), ttl: ttl}
}

func (s *Store) key(channel, threadTs string) string {
	return channel + ":" + threadTs
}

// Save records a pending issue in the store.  Any existing entry for
// the same channel/thread is overwritten.
func (s *Store) Save(p *PendingIssue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byThread[s.key(p.Channel, p.ThreadTs)] = p
}

// Load retrieves a pending issue by channel and thread.  If the entry
// has expired it is removed and nil is returned.  A copy of the
// underlying struct is returned to avoid accidental mutation.
func (s *Store) Load(channel, threadTs string) *PendingIssue {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.key(channel, threadTs)
	p := s.byThread[key]
	if p == nil {
		return nil
	}
	if s.ttl > 0 && time.Since(p.CreatedAt) > s.ttl {
		delete(s.byThread, key)
		return nil
	}
	cp := *p
	return &cp
}

// Delete removes a pending issue from the store.
func (s *Store) Delete(channel, threadTs string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byThread, s.key(channel, threadTs))
}
