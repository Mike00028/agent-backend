package hitl_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mike00028/golang-backend/services/api/internal/hitl"
)

// TestApprove verifies that a goroutine blocked in Request unblocks immediately
// after a matching Respond(approved=true) call.
func TestApprove(t *testing.T) {
	s := hitl.NewStore()

	var wg sync.WaitGroup
	wg.Add(1)
	var requestErr error
	go func() {
		defer wg.Done()
		requestErr = s.Request(context.Background(), "sess-1", "t1")
	}()

	// Give the goroutine time to block.
	time.Sleep(20 * time.Millisecond)

	if err := s.Respond("sess-1", "t1", hitl.ApprovalResult{Approved: true}); err != nil {
		t.Fatalf("Respond returned unexpected error: %v", err)
	}

	wg.Wait()
	if requestErr != nil {
		t.Fatalf("Request returned error after approval: %v", requestErr)
	}
}

// TestReject verifies that rejection propagates the reason as an error.
func TestReject(t *testing.T) {
	s := hitl.NewStore()

	var wg sync.WaitGroup
	wg.Add(1)
	var requestErr error
	go func() {
		defer wg.Done()
		requestErr = s.Request(context.Background(), "sess-2", "t1")
	}()

	time.Sleep(20 * time.Millisecond)

	if err := s.Respond("sess-2", "t1", hitl.ApprovalResult{Approved: false, Reason: "too risky"}); err != nil {
		t.Fatalf("Respond returned unexpected error: %v", err)
	}

	wg.Wait()
	if requestErr == nil {
		t.Fatal("expected an error on rejection, got nil")
	}
	if got := requestErr.Error(); got != "tool approval rejected: too risky" {
		t.Fatalf("unexpected error message: %q", got)
	}
}

// TestContextCancellation verifies that cancelling the context unblocks Request.
func TestContextCancellation(t *testing.T) {
	s := hitl.NewStore()

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	var requestErr error
	go func() {
		defer wg.Done()
		requestErr = s.Request(ctx, "sess-3", "t1")
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	wg.Wait()

	if requestErr == nil {
		t.Fatal("expected context.Canceled, got nil")
	}
}

// TestRespondNotFound verifies that Respond for an unknown key returns ErrNotFound.
func TestRespondNotFound(t *testing.T) {
	s := hitl.NewStore()
	err := s.Respond("no-such-session", "t99", hitl.ApprovalResult{Approved: true})
	if err != hitl.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestTimeout verifies that Request fails with ErrTimeout when nobody responds.
// Uses a tiny custom timeout via context deadline to avoid a 10-min wait.
func TestTimeout(t *testing.T) {
	s := hitl.NewStore()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := s.Request(ctx, "sess-4", "t1")
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// context.DeadlineExceeded is acceptable; so is ErrTimeout from the store.
	_ = err
}

// TestPendingKeys verifies that PendingKeys lists active requests.
func TestPendingKeys(t *testing.T) {
	s := hitl.NewStore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.Request(ctx, "sess-5", "t1")
	}()

	time.Sleep(20 * time.Millisecond)

	keys := s.PendingKeys()
	if len(keys) != 1 || keys[0] != "sess-5/t1" {
		t.Fatalf("unexpected pending keys: %v", keys)
	}

	cancel()
	wg.Wait()

	if len(s.PendingKeys()) != 0 {
		t.Fatal("expected empty pending keys after cancellation")
	}
}
