package hitl

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ApprovalTimeout is how long the executor waits for a human response before
// failing the task automatically.
const ApprovalTimeout = 10 * time.Minute

// ErrTimeout is returned when no approval arrives within ApprovalTimeout.
var ErrTimeout = errors.New("HITL approval timed out")

// ErrNotFound is returned when POST /agent/approve targets a key with no
// pending request (already resolved, wrong IDs, or never registered).
var ErrNotFound = errors.New("no pending approval for session/task")

// ApprovalResult is sent from the HTTP handler to the waiting executor goroutine.
type ApprovalResult struct {
	Approved bool
	Reason   string // optional rejection reason shown to the user
}

// Store holds one buffered channel per pending approval, keyed by
// "<sessionID>/<taskID>".  All methods are safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	pending map[string]chan ApprovalResult
}

// NewStore creates an empty Store.
func NewStore() *Store {
	return &Store{pending: make(map[string]chan ApprovalResult)}
}

// Request registers a pending approval for (sessionID, taskID) and blocks
// until a human responds via Respond, the context is cancelled, or
// ApprovalTimeout elapses.
//
// Returns nil if the human approved, an error otherwise.
// The channel is cleaned up before this function returns regardless of outcome.
func (s *Store) Request(ctx context.Context, sessionID, taskID string) error {
	key := sessionID + "/" + taskID
	ch := make(chan ApprovalResult, 1)

	s.mu.Lock()
	s.pending[key] = ch
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
	}()

	select {
	case result := <-ch:
		if !result.Approved {
			if result.Reason != "" {
				return fmt.Errorf("tool approval rejected: %s", result.Reason)
			}
			return errors.New("tool approval rejected")
		}
		return nil
	case <-time.After(ApprovalTimeout):
		return ErrTimeout
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Respond delivers a human decision to the goroutine blocked in Request.
// Returns ErrNotFound if nobody is waiting for that key.
func (s *Store) Respond(sessionID, taskID string, result ApprovalResult) error {
	key := sessionID + "/" + taskID

	s.mu.Lock()
	ch, ok := s.pending[key]
	s.mu.Unlock()

	if !ok {
		return ErrNotFound
	}

	select {
	case ch <- result:
		return nil
	default:
		// Channel is full — a concurrent Respond call already filled it.
		return errors.New("approval already responded")
	}
}

// PendingKeys returns all currently pending "<sessionID>/<taskID>" keys.
// Intended for debugging / admin dashboards only.
func (s *Store) PendingKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.pending))
	for k := range s.pending {
		keys = append(keys, k)
	}
	return keys
}
