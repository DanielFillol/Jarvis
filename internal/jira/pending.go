package jira

import (
	"sync"
	"time"
)

// Store maintains a mapping of pending issues per thread.  Entries
// expire after a configurable TTL.
type Store struct {
	mu       sync.Mutex
	byThread map[string]*PendingIssue
	ttl      time.Duration
}

// NewStore creates a new pending Store with the given time-to-live.  A
// zero or negative duration disables expiration entirely.
func NewStore(ttl time.Duration) *Store {
	return &Store{byThread: make(map[string]*PendingIssue), ttl: ttl}
}

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
	Source       string       // "thread_based" or "explicit"
	Drafts       []IssueDraft // multi-card flow: queue of drafts awaiting confirmation
	NeedProject  bool
	NeedType     bool
}
