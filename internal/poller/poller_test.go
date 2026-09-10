package poller

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock is a manually-advanced Clock for deterministic scheduler tests.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	deadline time.Time
	ch       chan time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	deadline := c.now.Add(d)
	if !deadline.After(c.now) {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{deadline: deadline, ch: ch})
	return ch
}

// Advance moves the clock forward by d, firing any waiters whose deadline
// has now passed.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	remaining := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.deadline.After(c.now) {
			w.ch <- w.deadline
		} else {
			remaining = append(remaining, w)
		}
	}
	c.waiters = remaining
}

// fakeJob is a Job whose Run is driven by a test-supplied function.
type fakeJob struct {
	name     string
	interval time.Duration
	calls    atomic.Int64
	run      func(ctx context.Context) error
}

func (j *fakeJob) Name() string            { return j.name }
func (j *fakeJob) Interval() time.Duration { return j.interval }
func (j *fakeJob) Run(ctx context.Context) error {
	j.calls.Add(1)
	if j.run == nil {
		return nil
	}
	return j.run(ctx)
}

func TestJitterStaysWithinBounds(t *testing.T) {
	s := New()
	max := 5 * time.Second
	for range 500 {
		d := s.jitter(max)
		assert.GreaterOrEqual(t, d, time.Duration(0))
		assert.Less(t, d, max)
	}
}

func TestJitterZeroMaxIsZero(t *testing.T) {
	s := New()
	assert.Zero(t, s.jitter(0))
}

func TestBackoffDelayGrowsAndCaps(t *testing.T) {
	interval := 16 * time.Minute
	assert.Equal(t, interval, backoffDelay(interval, 0))
	assert.Equal(t, time.Minute, backoffDelay(interval, 1))
	assert.Equal(t, 2*time.Minute, backoffDelay(interval, 2))
	assert.Equal(t, 4*time.Minute, backoffDelay(interval, 3))
	assert.Equal(t, 8*time.Minute, backoffDelay(interval, 4))
	// Doubling would exceed interval from here on; backoff caps at it.
	assert.Equal(t, interval, backoffDelay(interval, 5))
	assert.Equal(t, interval, backoffDelay(interval, 50))
}

func TestAttemptCooldownSkipsRun(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	s := New(WithClock(clock), withRand(func(time.Duration) time.Duration { return 0 }))

	j := &fakeJob{name: "job", interval: time.Hour}
	rj := &job{j: j, opts: Options{Cooldown: 10 * time.Minute}, trigger: make(chan chan error, 1)}
	rj.status = Status{Name: j.name, LastAttempt: base.Add(-5 * time.Minute)}

	err := s.attempt(context.Background(), rj, false)
	require.NoError(t, err)
	assert.Zero(t, j.calls.Load(), "cooldown must skip the run")
	assert.Equal(t, base.Add(5*time.Minute), rj.status.NextRun,
		"next run scheduled for when cooldown clears")

	clock.Advance(10 * time.Minute)
	err = s.attempt(context.Background(), rj, false)
	require.NoError(t, err)
	assert.Equal(t, int64(1), j.calls.Load(), "cooldown elapsed, run proceeds")
}

func TestAttemptBackoffAndResetOnSuccess(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	s := New(WithClock(clock), withRand(func(time.Duration) time.Duration { return 0 }))

	wantErr := errors.New("boom")
	failures := 0
	j := &fakeJob{name: "job", interval: 16 * time.Minute, run: func(context.Context) error {
		if failures < 3 {
			failures++
			return wantErr
		}
		return nil
	}}
	rj := &job{j: j, opts: Options{}, trigger: make(chan chan error, 1)}

	// Three failing attempts: backoff grows 1m, 2m, 4m.
	err := s.attempt(context.Background(), rj, false)
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, 1, rj.status.ConsecutiveFailures)
	assert.Equal(t, clock.Now().Add(time.Minute), rj.status.NextRun)
	clock.Advance(time.Minute)

	err = s.attempt(context.Background(), rj, false)
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, 2, rj.status.ConsecutiveFailures)
	assert.Equal(t, clock.Now().Add(2*time.Minute), rj.status.NextRun)
	clock.Advance(2 * time.Minute)

	err = s.attempt(context.Background(), rj, false)
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, 3, rj.status.ConsecutiveFailures)
	assert.Equal(t, clock.Now().Add(4*time.Minute), rj.status.NextRun)
	clock.Advance(4 * time.Minute)

	// Fourth attempt succeeds: failures reset, schedule returns to interval.
	err = s.attempt(context.Background(), rj, false)
	require.NoError(t, err)
	assert.Equal(t, 0, rj.status.ConsecutiveFailures)
	assert.Empty(t, rj.status.LastError)
	assert.Equal(t, clock.Now(), rj.status.LastSuccess)
	assert.Equal(t, clock.Now().Add(16*time.Minute), rj.status.NextRun)
}

func TestAttemptRetryAfterHonored(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	// A non-zero jitter source would prove itself unused by RetryAfter.
	s := New(WithClock(clock), withRand(func(max time.Duration) time.Duration { return max - 1 }))

	retryAfter := 3 * time.Minute
	j := &fakeJob{name: "job", interval: time.Hour, run: func(context.Context) error {
		return &RetryAfterError{RetryAfter: retryAfter, Err: errors.New("rate limited")}
	}}
	rj := &job{j: j, opts: Options{Jitter: 30 * time.Second}, trigger: make(chan chan error, 1)}

	err := s.attempt(context.Background(), rj, false)
	require.Error(t, err)
	assert.Equal(t, base.Add(retryAfter), rj.status.NextRun,
		"Retry-After is honored exactly, without jitter")
}

// TestSchedulerTriggerNowBypassesCooldownOnce covers TriggerNow forcing an
// attempt that the job's own Cooldown would otherwise gate: a first attempt
// (via TriggerNow, since a fresh job has no prior attempt to gate on)
// establishes LastAttempt, and a second TriggerNow made immediately after
// still runs instead of being skipped for cooldown.
func TestSchedulerTriggerNowBypassesCooldownOnce(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)

	j := &fakeJob{name: "job", interval: time.Hour}
	s := New(WithClock(clock))
	s.Register(j, Options{Cooldown: 10 * time.Minute})

	ctx := t.Context()
	s.Start(ctx)

	err := s.TriggerNow("job")
	require.NoError(t, err)
	require.Equal(t, int64(1), j.calls.Load())

	err = s.TriggerNow("job")
	require.NoError(t, err)
	assert.Equal(t, int64(2), j.calls.Load(),
		"TriggerNow bypasses cooldown and runs immediately")

	statuses := s.Status()
	require.Len(t, statuses, 1)
	assert.Equal(t, clock.Now(), statuses[0].LastAttempt)
}

func TestSchedulerCancellationStopsCleanly(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	j := &fakeJob{name: "job", interval: time.Hour}
	s := New(WithClock(clock))
	s.Register(j, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	cancel()

	done := make(chan struct{})
	go func() {
		s.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}
}

func TestErrUnknownJobBeforeStart(t *testing.T) {
	s := New()
	err := s.TriggerNow("nope")
	assert.ErrorIs(t, err, ErrUnknownJob)
}

// TestTriggerNowAfterShutdownReturnsPromptly covers a TriggerNow call made
// once the scheduler's context is already canceled and its job loop has
// fully exited (confirmed via Wait). Before observing ctx in TriggerNow,
// this enqueued into the job's trigger channel and then blocked forever on
// the response: nothing was left running to drain the channel or reply.
func TestTriggerNowAfterShutdownReturnsPromptly(t *testing.T) {
	j := &fakeJob{name: "job", interval: time.Hour}
	s := New()
	s.Register(j, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	cancel()
	s.Wait()

	done := make(chan error, 1)
	go func() { done <- s.TriggerNow("job") }()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("TriggerNow blocked forever after scheduler shutdown")
	}
}

// TestTriggerNowQueuedDuringShutdownReturnsPromptly covers the narrower race
// the same bug caused: a TriggerNow call enqueues its request while the job
// loop is still busy running an earlier triggered attempt, the scheduler is
// canceled while that attempt is in flight, and the loop exits without ever
// looping back around to notice the newly queued request.
func TestTriggerNowQueuedDuringShutdownReturnsPromptly(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var enteredOnce sync.Once
	j := &fakeJob{name: "job", interval: time.Hour, run: func(ctx context.Context) error {
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-release:
		case <-ctx.Done():
		}
		return ctx.Err()
	}}
	s := New()
	s.Register(j, Options{RunAtStart: true})

	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	// Let the RunAtStart attempt claim the job loop's goroutine.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("job never started")
	}

	// This TriggerNow can only be enqueued; the loop is busy inside attempt
	// and cannot select on the trigger channel again until it returns, by
	// which point the scheduler will already be canceled.
	queued := make(chan error, 1)
	go func() { queued <- s.TriggerNow("job") }()
	time.Sleep(10 * time.Millisecond)

	cancel()
	close(release)

	select {
	case err := <-queued:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("TriggerNow queued during shutdown blocked forever")
	}
	s.Wait()
}
