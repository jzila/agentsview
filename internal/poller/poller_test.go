package poller

import (
	"context"
	"errors"
	"maps"
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

// WaiterCount reports how many pending After calls are parked waiting for a
// future deadline. Tests use it to synchronize on the run loop having
// actually registered its next wait before advancing the clock, since
// advancing too early would compute the wrong deadline (After measures its
// duration from the clock's time when it is called, not from an earlier
// snapshot).
func (c *fakeClock) WaiterCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
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

// memStore is an in-memory StatusStore for round-trip tests.
type memStore struct {
	mu       sync.Mutex
	statuses map[string]Status
}

func newMemStore() *memStore {
	return &memStore{statuses: make(map[string]Status)}
}

func (m *memStore) LoadStatuses(context.Context) (map[string]Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Status, len(m.statuses))
	maps.Copy(out, m.statuses)
	return out, nil
}

func (m *memStore) SaveStatus(_ context.Context, status Status) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[status.Name] = status
	return nil
}

func TestJitterStaysWithinBounds(t *testing.T) {
	s := New(nil)
	max := 5 * time.Second
	for range 500 {
		d := s.jitter(max)
		assert.GreaterOrEqual(t, d, time.Duration(0))
		assert.Less(t, d, max)
	}
}

func TestJitterZeroMaxIsZero(t *testing.T) {
	s := New(nil)
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
	s := New(nil, WithClock(clock), withRand(func(time.Duration) time.Duration { return 0 }))

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
	s := New(nil, WithClock(clock), withRand(func(time.Duration) time.Duration { return 0 }))

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
	s := New(nil, WithClock(clock), withRand(func(max time.Duration) time.Duration { return max - 1 }))

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

// TestAttemptHeaderlessRetryAfterFallsBackToBackoff covers a roborev
// finding: a 429 (or similar) with no usable Retry-After header parses to
// a *RetryAfterError carrying a zero duration (see
// claude.parseRetryAfter), and treating that as an explicit "retry
// immediately" instruction skipped exponential backoff entirely, so
// repeated headerless failures would retry as fast as the scheduler loop
// allows instead of backing off. A non-positive RetryAfter must fall back
// to the normal ConsecutiveFailures-driven backoff, the same as any other
// error.
func TestAttemptHeaderlessRetryAfterFallsBackToBackoff(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	s := New(nil, WithClock(clock), withRand(func(time.Duration) time.Duration { return 0 }))

	j := &fakeJob{name: "job", interval: 16 * time.Minute, run: func(context.Context) error {
		return &RetryAfterError{RetryAfter: 0, Err: errors.New("rate limited, no retry-after header")}
	}}
	rj := &job{j: j, opts: Options{}, trigger: make(chan chan error, 1)}

	err := s.attempt(context.Background(), rj, false)
	require.Error(t, err)
	assert.Equal(t, 1, rj.status.ConsecutiveFailures)
	assert.Equal(t, base.Add(time.Minute), rj.status.NextRun,
		"a headerless Retry-After must use the normal backoff delay (interval/16), not an immediate retry")
}

// TestAttemptWithholdsTriggerDuringPendingRetryAfter covers a roborev
// finding: TriggerNow's bypassCooldown only bypasses Cooldown, but the
// prior implementation let it bypass a pending Retry-After wait the same
// way, so a manual trigger right after an upstream 429 could immediately
// repeat the same failing request. attempt now checks a pending
// Retry-After deadline unconditionally, before Cooldown, and returns
// ErrRetryAfterPending without running the job at all while it holds.
func TestAttemptWithholdsTriggerDuringPendingRetryAfter(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	s := New(nil, WithClock(clock), withRand(func(time.Duration) time.Duration { return 0 }))

	retryAfter := 90 * time.Second
	j := &fakeJob{name: "job", interval: time.Hour, run: func(context.Context) error {
		return &RetryAfterError{RetryAfter: retryAfter, Err: errors.New("rate limited")}
	}}
	rj := &job{j: j, opts: Options{}, trigger: make(chan chan error, 1)}

	err := s.attempt(context.Background(), rj, false)
	require.Error(t, err)
	require.EqualValues(t, 1, j.calls.Load())

	// A manual trigger (bypassCooldown=true) before the Retry-After wait
	// elapses must not run the job again.
	err = s.attempt(context.Background(), rj, true)
	var pending *ErrRetryAfterPending
	require.ErrorAs(t, err, &pending)
	assert.Equal(t, retryAfter, pending.Remaining)
	assert.EqualValues(t, 1, j.calls.Load(), "the job must not run again while the wait is pending")

	// Once the wait elapses, a trigger runs normally again.
	clock.Advance(retryAfter)
	err = s.attempt(context.Background(), rj, true)
	require.Error(t, err) // the fake job still fails every call
	assert.EqualValues(t, 2, j.calls.Load(), "the job runs again once the retry-after wait has elapsed")
}

// TestInitialDelayRecordsNextRunStatus covers a job without RunAtStart:
// before its first attempt ever runs, Status must already reflect the
// scheduled next run rather than a zero value for the whole first interval.
func TestInitialDelayRecordsNextRunStatus(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	s := New(nil, WithClock(clock), withRand(func(time.Duration) time.Duration { return 0 }))
	j := &fakeJob{name: "job", interval: time.Hour}
	rj := &job{j: j, opts: Options{}, trigger: make(chan chan error, 1)}

	delay := s.initialDelay(rj)
	assert.Equal(t, time.Hour, delay)
	assert.Equal(t, base.Add(time.Hour), rj.status.NextRun)
}

// TestSchedulerLoopAppliesBackoffAcrossTicks drives the real Start/run loop,
// not attempt directly: it proves the loop itself rearms its timer from the
// backoff delay an earlier failed attempt recorded in Status, and recovers
// once the job starts succeeding again.
func TestSchedulerLoopAppliesBackoffAcrossTicks(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	s := New(nil, WithClock(clock), withRand(func(time.Duration) time.Duration { return 0 }))

	var failing atomic.Bool
	failing.Store(true)
	j := &fakeJob{name: "job", interval: 16 * time.Minute, run: func(context.Context) error {
		if failing.Load() {
			return errors.New("boom")
		}
		return nil
	}}
	s.Register(j, Options{RunAtStart: true})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.Start(ctx)

	// RunAtStart fires the first (failing) attempt as soon as the loop
	// starts, with no clock advance needed.
	require.Eventually(t, func() bool { return j.calls.Load() == 1 }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		statuses := s.Status()
		return len(statuses) == 1 && statuses[0].ConsecutiveFailures == 1
	}, 2*time.Second, time.Millisecond)

	// Wait for the loop to actually register its next wait (backoffDelay
	// for one failure on a 16m interval is 1m; see TestBackoffDelayGrowsAndCaps)
	// before advancing, or the advance could race the loop computing its
	// deadline from a stale "now".
	require.Eventually(t, func() bool { return clock.WaiterCount() == 1 }, 2*time.Second, time.Millisecond)

	clock.Advance(59 * time.Second)
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int64(1), j.calls.Load(), "loop must wait out the full backoff delay")

	failing.Store(false)
	clock.Advance(time.Second)

	require.Eventually(t, func() bool { return j.calls.Load() == 2 }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		statuses := s.Status()
		return len(statuses) == 1 && statuses[0].ConsecutiveFailures == 0
	}, 2*time.Second, time.Millisecond)
}

func TestSchedulerTriggerNowBypassesCooldownOnce(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	store := newMemStore()
	// Persist a recent attempt so the steady loop would be inside cooldown.
	require.NoError(t, store.SaveStatus(context.Background(), Status{
		Name:        "job",
		LastAttempt: base.Add(-time.Minute),
	}))

	j := &fakeJob{name: "job", interval: time.Hour}
	s := New(store, WithClock(clock))
	s.Register(j, Options{Cooldown: 10 * time.Minute})

	ctx := t.Context()
	s.Start(ctx)

	err := s.TriggerNow("job")
	require.NoError(t, err)
	assert.Equal(t, int64(1), j.calls.Load(),
		"TriggerNow bypasses cooldown and runs immediately")

	// A second, non-triggered cooldown check right after must still gate
	// normally: directly exercise attempt() to confirm cooldown resumed.
	statuses := s.Status()
	require.Len(t, statuses, 1)
	assert.Equal(t, clock.Now(), statuses[0].LastAttempt)
}

// TestRegisterAfterStartIgnoresDuplicateName covers Register called with a
// name that already has a running loop: the second Job must never get its
// own loop, since that loop's own status and RunAtStart would be invisible
// to Status and TriggerNow while still running concurrently with the
// original.
func TestRegisterAfterStartIgnoresDuplicateName(t *testing.T) {
	var calls atomic.Int64
	newJob := func() *fakeJob {
		return &fakeJob{name: "job", interval: time.Hour, run: func(context.Context) error {
			calls.Add(1)
			return nil
		}}
	}
	s := New(nil)
	s.Register(newJob(), Options{})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.Start(ctx)

	// RunAtStart on the duplicate would run immediately if Register had
	// wrongly spawned a second loop for "job".
	s.Register(newJob(), Options{RunAtStart: true})
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, calls.Load())

	require.NoError(t, s.TriggerNow("job"))
	assert.Equal(t, int64(1), calls.Load())
	assert.Len(t, s.Status(), 1)
}

// TestStartCalledTwiceDoesNotDuplicateLoops covers a second Start call: every
// already-registered job already has a running loop, so a second round of
// goroutines would run each job's RunAtStart attempt twice.
func TestStartCalledTwiceDoesNotDuplicateLoops(t *testing.T) {
	var calls atomic.Int64
	j := &fakeJob{name: "job", interval: time.Hour, run: func(context.Context) error {
		calls.Add(1)
		return nil
	}}
	s := New(nil)
	s.Register(j, Options{RunAtStart: true})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.Start(ctx)
	s.Start(ctx)

	require.Eventually(t, func() bool {
		return calls.Load() > 0
	}, 2*time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(1), calls.Load())
}

func TestSchedulerCancellationStopsCleanly(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	j := &fakeJob{name: "job", interval: time.Hour}
	s := New(nil, WithClock(clock))
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

func TestSchedulerStatusPersistenceRoundTrip(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	store := newMemStore()

	j := &fakeJob{name: "job", interval: time.Hour}
	rj := &job{j: j, opts: Options{}, trigger: make(chan chan error, 1)}
	s := New(store, WithClock(clock))
	err := s.attempt(context.Background(), rj, false)
	require.NoError(t, err)

	// A fresh Scheduler backed by the same store restores the status.
	j2 := &fakeJob{name: "job", interval: time.Hour}
	s2 := New(store, WithClock(clock))
	s2.Register(j2, Options{})
	ctx := t.Context()
	s2.Start(ctx)

	statuses := s2.Status()
	require.Len(t, statuses, 1)
	assert.Equal(t, clock.Now(), statuses[0].LastSuccess)
	assert.Equal(t, base, statuses[0].LastAttempt)
}

// TestSchedulerRestoresRetryAfterUntilAcrossRestart covers a roborev
// finding: RetryAfterUntil lived only in an in-memory job field, so
// restoring persisted Status on a restart (loadStatuses/loadOneStatus)
// did not restore it, and a TriggerNow call made shortly after that
// restart -- but still within the original Retry-After wait -- would
// bypass NextRun and repeat the request anyway. RetryAfterUntil is now a
// Status field, restored the same way LastAttempt/NextRun already are.
func TestSchedulerRestoresRetryAfterUntilAcrossRestart(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	store := newMemStore()

	retryAfter := 5 * time.Minute
	j := &fakeJob{name: "job", interval: time.Hour, run: func(context.Context) error {
		return &RetryAfterError{RetryAfter: retryAfter, Err: errors.New("rate limited")}
	}}
	s := New(store, WithClock(clock))
	s.Register(j, Options{})
	s.Start(t.Context())

	err := s.TriggerNow("job")
	require.Error(t, err)
	require.EqualValues(t, 1, j.calls.Load())

	// Restart: a fresh Scheduler backed by the same store, well within
	// the original Retry-After wait.
	clock.Advance(time.Minute)
	j2 := &fakeJob{name: "job", interval: time.Hour, run: func(context.Context) error {
		return &RetryAfterError{RetryAfter: retryAfter, Err: errors.New("rate limited")}
	}}
	s2 := New(store, WithClock(clock))
	s2.Register(j2, Options{})
	s2.Start(t.Context())

	err = s2.TriggerNow("job")
	var pending *ErrRetryAfterPending
	require.ErrorAs(t, err, &pending)
	assert.EqualValues(t, 0, j2.calls.Load(),
		"a restored Retry-After deadline must block TriggerNow across a restart")
}

func TestErrUnknownJobBeforeStart(t *testing.T) {
	s := New(nil)
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
	s := New(nil)
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
	s := New(nil)
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
