package agent

import (
	"context"
	"sync"
)

// semaphore is a resizable counting semaphore.
//
// It is what makes `/agents max <int>` a *global* bound rather than a per-run
// one: every Run of a given scheduler, including one started recursively by a
// worker, draws from the same pool of slots.
type semaphore struct {
	mu    sync.Mutex
	cond  *sync.Cond
	limit int
	held  int
}

func newSemaphore(limit int) *semaphore {
	if limit < 1 {
		limit = DefaultMaxConcurrency
	}
	s := &semaphore{limit: limit}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// tryAcquire takes a slot if one is free.
func (s *semaphore) tryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held >= s.limit {
		return false
	}
	s.held++
	return true
}

// acquire waits for a slot, or for ctx to end.
func (s *semaphore) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Cancellation has to wake the waiter; sync.Cond has no select form.
	stop := context.AfterFunc(ctx, s.cond.Broadcast)
	defer stop()

	s.mu.Lock()
	defer s.mu.Unlock()
	for s.held >= s.limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.cond.Wait()
	}
	s.held++
	return nil
}

// release returns a slot.
func (s *semaphore) release() {
	s.mu.Lock()
	if s.held > 0 {
		s.held--
	}
	s.mu.Unlock()
	s.cond.Broadcast()
}

// reclaim takes a slot back without waiting.
//
// It is used by a task that gave up its slot to run a nested plan and has now
// finished waiting. Queueing behind new work would mean a parent waiting for
// a slot it already owned, which is how nested schedulers deadlock. The cost is
// that the limit can be briefly exceeded by the number of nested runs returning
// at once, which is bounded by the spawn depth.
func (s *semaphore) reclaim() {
	s.mu.Lock()
	s.held++
	s.mu.Unlock()
}

// setLimit resizes the semaphore, waking anyone who can now proceed.
func (s *semaphore) setLimit(n int) {
	s.mu.Lock()
	s.limit = n
	s.mu.Unlock()
	s.cond.Broadcast()
}

func (s *semaphore) limitOf() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit
}

// permitKey marks a context as running inside a scheduler slot.
type permitKey struct{}

func withPermit(ctx context.Context) context.Context {
	return context.WithValue(ctx, permitKey{}, true)
}

func holdsPermit(ctx context.Context) bool {
	held, _ := ctx.Value(permitKey{}).(bool)
	return held
}
