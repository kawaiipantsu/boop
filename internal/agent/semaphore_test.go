package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSemaphoreBounds(t *testing.T) {
	s := newSemaphore(2)
	if !s.tryAcquire() {
		t.Fatal("the first acquisition must succeed")
	}
	if !s.tryAcquire() {
		t.Fatal("the second acquisition must succeed")
	}
	if s.tryAcquire() {
		t.Fatal("tryAcquire() succeeded past the limit")
	}
	s.release()
	if !s.tryAcquire() {
		t.Fatal("a released slot must become available")
	}
}

func TestSemaphoreDefaultsToTheSpecLimit(t *testing.T) {
	for _, n := range []int{0, -1} {
		if got := newSemaphore(n).limitOf(); got != DefaultMaxConcurrency {
			t.Errorf("newSemaphore(%d).limitOf() = %d, want %d", n, got, DefaultMaxConcurrency)
		}
	}
}

func TestSemaphoreAcquireWaitsAndCancels(t *testing.T) {
	s := newSemaphore(1)
	if !s.tryAcquire() {
		t.Fatal("tryAcquire() = false on an empty semaphore")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire() = %v, want the deadline to be honoured", err)
	}

	// A slot freed by another goroutine wakes the waiter.
	got := make(chan error, 1)
	go func() { got <- s.acquire(context.Background()) }()
	time.Sleep(10 * time.Millisecond)
	s.release()
	select {
	case err := <-got:
		if err != nil {
			t.Errorf("acquire() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquire() did not wake when a slot was released")
	}
}

func TestSemaphoreSetLimitWakesWaiters(t *testing.T) {
	s := newSemaphore(1)
	if !s.tryAcquire() {
		t.Fatal("tryAcquire() = false")
	}
	got := make(chan error, 1)
	go func() { got <- s.acquire(context.Background()) }()
	time.Sleep(10 * time.Millisecond)
	s.setLimit(4)
	select {
	case err := <-got:
		if err != nil {
			t.Errorf("acquire() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("raising the limit did not release a waiter")
	}
	if s.limitOf() != 4 {
		t.Errorf("limitOf() = %d, want 4", s.limitOf())
	}
}

func TestSemaphoreReleaseNeverGoesNegative(t *testing.T) {
	s := newSemaphore(1)
	s.release()
	s.release()
	if !s.tryAcquire() {
		t.Fatal("tryAcquire() = false; over-releasing must not corrupt the count")
	}
	if s.tryAcquire() {
		t.Fatal("tryAcquire() succeeded past the limit after over-releasing")
	}
}

// TestSemaphoreUnderRace hammers the semaphore from many goroutines, which is
// how it is actually used: every dispatch decision touches it.
func TestSemaphoreUnderRace(t *testing.T) {
	s := newSemaphore(3)
	var mu sync.Mutex
	live, peak := 0, 0

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.acquire(context.Background()); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			live++
			if live > peak {
				peak = live
			}
			mu.Unlock()

			mu.Lock()
			live--
			mu.Unlock()
			s.release()
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			s.setLimit(1 + i%5)
		}
	}()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if peak > 5 {
		t.Errorf("peak concurrency = %d, want no more than the highest limit set (5)", peak)
	}
}

// TestSchedulerConcurrencyIsGlobalAcrossRuns pins the §11 wording: the bound is
// global, not per run.
func TestSchedulerConcurrencyIsGlobalAcrossRuns(t *testing.T) {
	tr := newTracker()
	s := NewScheduler(tr.exec(15*time.Millisecond, nil), 2)

	var wg sync.WaitGroup
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			tasks := []Task{
				{ID: string(rune('a'+r)) + "1"},
				{ID: string(rune('a'+r)) + "2"},
				{ID: string(rune('a'+r)) + "3"},
			}
			if _, err := s.Run(context.Background(), tasks); err != nil {
				t.Error(err)
			}
		}(r)
	}
	wg.Wait()

	peak, started, _, _ := tr.snapshot()
	if peak > 2 {
		t.Errorf("peak concurrency across runs = %d, want at most the global limit of 2", peak)
	}
	if len(started) != 9 {
		t.Errorf("started %d tasks, want all 9", len(started))
	}
}

// TestSchedulerNestedRunDoesNotDeadlock covers a worker that itself schedules
// work: the parent gives up its slot while it waits, so nesting cannot starve.
func TestSchedulerNestedRunDoesNotDeadlock(t *testing.T) {
	var s *Scheduler
	s = NewScheduler(TaskExecutorFunc(func(ctx context.Context, task Task) (TaskOutcome, error) {
		if task.ID == "child" {
			return TaskOutcome{Output: "leaf"}, nil
		}
		if _, err := s.Run(ctx, []Task{{ID: "child"}}); err != nil {
			return TaskOutcome{}, err
		}
		return TaskOutcome{Output: "parent"}, nil
	}), 1) // one slot: a parent that kept its slot would deadlock

	done := make(chan []TaskResult, 1)
	go func() {
		results, err := s.Run(context.Background(), []Task{{ID: "p1"}, {ID: "p2"}})
		if err != nil {
			t.Error(err)
		}
		done <- results
	}()

	select {
	case results := <-done:
		for _, r := range results {
			if r.Status != TaskComplete {
				t.Errorf("task %s = %s (%s), want complete", r.TaskID, r.Status, r.Error)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nested runs deadlocked")
	}
}
