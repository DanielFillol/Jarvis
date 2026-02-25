// internal/state/message_tracker.go
package state

import "sync"

// MessageTracker keeps a mapping from originTs (the user's triggering message)
// to botTs (the bot's reply) so that when a user deletes their message the bot
// can delete its own reply automatically.
type MessageTracker struct {
	mu   sync.RWMutex
	data map[string]string // key: channel+":"+originTs → botTs
}

// NewMessageTracker constructs an empty MessageTracker.
func NewMessageTracker() *MessageTracker {
	return &MessageTracker{data: make(map[string]string)}
}

func key(channel, originTs string) string { return channel + ":" + originTs }

// Track records that botTs is the bot reply to the user message at originTs.
func (t *MessageTracker) Track(channel, originTs, botTs string) {
	t.mu.Lock()
	t.data[key(channel, originTs)] = botTs
	t.mu.Unlock()
}

// Get returns the bot reply ts for a given origin message, or "" if not found.
func (t *MessageTracker) Get(channel, originTs string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.data[key(channel, originTs)]
}

// Delete removes a tracked entry.
func (t *MessageTracker) Delete(channel, originTs string) {
	t.mu.Lock()
	delete(t.data, key(channel, originTs))
	t.mu.Unlock()
}
