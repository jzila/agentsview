// Package poller is a small, reusable background-job scheduler. It owns the
// behavior every hand-rolled ticker loop in this codebase otherwise
// reimplements: jitter, a cooldown recorded before each attempt, capped
// exponential backoff on consecutive failures, honoring a job-supplied
// Retry-After, and context cancellation. Read docs/agents/background-work.md
// before adding a new Job.
package poller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"math/rand/v2"
	"sync"
	"time"
)

// Job is one background task the Scheduler drives on an interval.
type Job interface {
	// Name is a stable identifier used for status and TriggerNow lookups
	// (e.g. "pricing-refresh").
	Name() string
	// Interval is the steady-state period between successful attempts.
	Interval() time.Duration
	// Run performs one attempt. It must be safe to call again after an
	// error and must return promptly once ctx is canceled.
	Run(ctx context.Context) error
}

// RetryAfterError lets a Job's Run tell the Scheduler exactly how long to
// wait before the next attempt, overriding jitter and backoff for that one
// cycle. Return it after a 429 (or similar) response carries a Retry-After
// hint.
type RetryAfterError struct {
	RetryAfter time.Duration
	Err        error
}

func (e *RetryAfterError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("retry after %s: %v", e.RetryAfter, e.Err)
	}
	return fmt.Sprintf("retry after %s", e.RetryAfter)
}

func (e *RetryAfterError) Unwrap() error { return e.Err }

// Options configures how the Scheduler drives one registered Job.
type Options struct {
	// Jitter adds a random delay in [0, Jitter) before each scheduled
	// attempt, spreading load when several jobs share an interval.
	Jitter time.Duration
	// Cooldown is the minimum time between attempts, recorded before the
	// attempt starts so a failed attempt still observes it (the
	// internal/pricingrefresh pattern). A tick landing inside the
	// cooldown window is skipped rather than run early.
	Cooldown time.Duration
	// RunAtStart runs the job once, as soon as its loop starts, instead
	// of waiting for the first interval tick. It still observes
	// Cooldown against a status restored from a StatusStore.
	RunAtStart bool
}

// Status is the observable state of one registered Job.
type Status struct {
	Name                string    `json:"name"`
	LastAttempt         time.Time `json:"last_attempt,omitzero"`
	LastSuccess         time.Time `json:"last_success,omitzero"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	NextRun             time.Time `json:"next_run,omitzero"`
}

// StatusStore persists Status across daemon restarts. internal/db
// implements it against the SQLite-only poller_status table; see
// docs/agents/storage.md.
type StatusStore interface {
	LoadStatuses(ctx context.Context) (map[string]Status, error)
	SaveStatus(ctx context.Context, status Status) error
}

// Clock abstracts time so the Scheduler can be driven deterministically in
// tests. Production code uses realClock, the default.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives the current time once d has
	// elapsed, mirroring time.After.
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// job bundles one registration's Job, Options, and live state.
type job struct {
	j    Job
	opts Options

	mu     sync.Mutex
	status Status

	trigger chan chan error
}

// Scheduler runs registered Jobs on their own goroutines, applying jitter,
// cooldown, backoff, and Retry-After uniformly, and persists Status through
// an optional StatusStore.
type Scheduler struct {
	clock   Clock
	store   StatusStore
	logf    func(format string, args ...any)
	randSrc func(max time.Duration) time.Duration

	mu      sync.Mutex
	jobs    map[string]*job
	order   []string
	started bool
	ctx     context.Context
	wg      sync.WaitGroup
}

// SchedulerOption configures a Scheduler at construction time.
type SchedulerOption func(*Scheduler)

// WithClock overrides the Scheduler's Clock. Tests use this to inject a fake
// clock; production code should not need it.
func WithClock(clock Clock) SchedulerOption {
	return func(s *Scheduler) { s.clock = clock }
}

// withRand overrides jitter generation for deterministic tests.
func withRand(randSrc func(max time.Duration) time.Duration) SchedulerOption {
	return func(s *Scheduler) { s.randSrc = randSrc }
}

// New creates a Scheduler. store may be nil to disable status persistence
// (status is then kept in memory only, for the process lifetime).
func New(store StatusStore, opts ...SchedulerOption) *Scheduler {
	s := &Scheduler{
		clock:   realClock{},
		store:   store,
		logf:    log.Printf,
		jobs:    make(map[string]*job),
		randSrc: defaultJitter,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func defaultJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max)))
}

func (s *Scheduler) jitter(max time.Duration) time.Duration {
	if s.randSrc == nil {
		return defaultJitter(max)
	}
	return s.randSrc(max)
}

// Register adds a Job to the Scheduler. If the Scheduler is already started
// (Start has been called), the job's loop starts immediately; otherwise it
// starts when Start runs.
//
// Registering a name that already has a running loop (Start has been called
// for it) is ignored, logged, and returns without replacing the existing
// job: replacing the map entry while its original loop kept running would
// leave that loop's Job and Options orphaned but still executing, invisible
// to Status and TriggerNow, alongside a second loop for the replacement.
// Registering the same name twice before Start is unaffected; the later
// call simply wins, since no loop has started yet.
func (s *Scheduler) Register(j Job, opts Options) {
	s.mu.Lock()
	name := j.Name()
	if _, exists := s.jobs[name]; exists && s.started {
		s.mu.Unlock()
		s.logf("poller: %q is already registered and running; ignoring duplicate Register", name)
		return
	}
	rj := &job{
		j:       j,
		opts:    opts,
		status:  Status{Name: name},
		trigger: make(chan chan error, 1),
	}
	if _, exists := s.jobs[name]; !exists {
		s.order = append(s.order, name)
	}
	s.jobs[name] = rj
	started := s.started
	ctx := s.ctx
	s.mu.Unlock()

	if started {
		s.loadOneStatus(ctx, rj)
		s.wg.Add(1)
		go s.run(ctx, rj)
	}
}

// Start loads persisted status for every registered job and spawns each
// job's loop. It returns immediately; job loops stop once ctx is canceled.
// A job registered after Start starts immediately (see Register). A second
// call to Start is ignored and logged: every already-registered job already
// has a running loop, so spawning another round would run each job's loop
// twice.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		s.logf("poller: Start called more than once; ignoring")
		return
	}
	s.started = true
	s.ctx = ctx
	jobs := make([]*job, 0, len(s.jobs))
	for _, name := range s.order {
		jobs = append(jobs, s.jobs[name])
	}
	s.mu.Unlock()

	s.loadStatuses(ctx)
	for _, rj := range jobs {
		s.wg.Add(1)
		go s.run(ctx, rj)
	}
}

// Wait blocks until every started job loop has returned. It is intended for
// tests driving cancellation; production callers rely on ctx instead.
func (s *Scheduler) Wait() {
	s.wg.Wait()
}

func (s *Scheduler) loadStatuses(ctx context.Context) {
	if s.store == nil {
		return
	}
	statuses, err := s.store.LoadStatuses(ctx)
	if err != nil {
		s.logf("poller: loading persisted status: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, status := range statuses {
		if rj, ok := s.jobs[name]; ok {
			rj.mu.Lock()
			rj.status = status
			rj.mu.Unlock()
		}
	}
}

func (s *Scheduler) loadOneStatus(ctx context.Context, rj *job) {
	if s.store == nil {
		return
	}
	statuses, err := s.store.LoadStatuses(ctx)
	if err != nil {
		s.logf("poller: loading persisted status: %v", err)
		return
	}
	if status, ok := statuses[rj.j.Name()]; ok {
		rj.mu.Lock()
		rj.status = status
		rj.mu.Unlock()
	}
}

func (s *Scheduler) saveStatus(ctx context.Context, status Status) {
	if s.store == nil {
		return
	}
	if err := s.store.SaveStatus(ctx, status); err != nil {
		s.logf("poller: persisting status for %q: %v", status.Name, err)
	}
}

// Status returns a snapshot of every registered job's Status, in
// registration order.
func (s *Scheduler) Status() []Status {
	s.mu.Lock()
	names := make([]string, len(s.order))
	copy(names, s.order)
	jobs := make(map[string]*job, len(s.jobs))
	maps.Copy(jobs, s.jobs)
	s.mu.Unlock()

	out := make([]Status, 0, len(names))
	for _, name := range names {
		rj := jobs[name]
		rj.mu.Lock()
		out = append(out, rj.status)
		rj.mu.Unlock()
	}
	return out
}

// ErrUnknownJob is returned by TriggerNow for a name that isn't registered
// and started.
var ErrUnknownJob = errors.New("poller: unknown job")

// TriggerNow runs a registered job immediately, bypassing its Cooldown for
// this one attempt, and waits for the attempt to finish. It returns the
// job's error, if any. Calling TriggerNow for a name that hasn't started its
// loop yet returns ErrUnknownJob.
//
// TriggerNow observes the Scheduler's own context (set by Start) both when
// enqueueing the request and while awaiting its response. The job's loop
// exits as soon as that context is canceled and stops draining the trigger
// channel, so without this a call queued during shutdown, or made any time
// after it, would block forever waiting for a response nobody will ever
// send. Once canceled, TriggerNow returns the context's error instead.
func (s *Scheduler) TriggerNow(name string) error {
	s.mu.Lock()
	rj, ok := s.jobs[name]
	started := s.started
	ctx := s.ctx
	s.mu.Unlock()
	if !ok || !started {
		return ErrUnknownJob
	}
	if ctx == nil {
		ctx = context.Background()
	}

	resp := make(chan error, 1)
	select {
	case rj.trigger <- resp:
	case <-ctx.Done():
		return ctx.Err()
	default:
		return fmt.Errorf("poller: %q already has a trigger pending", name)
	}

	select {
	case err := <-resp:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run is one job's loop. It sleeps for the next scheduled delay (subject to
// jitter/backoff/Retry-After), or wakes early on a TriggerNow request, until
// ctx is canceled.
func (s *Scheduler) run(ctx context.Context, rj *job) {
	defer s.wg.Done()

	delay := s.initialDelay(rj)
	for {
		select {
		case <-ctx.Done():
			return
		case resp := <-rj.trigger:
			err := s.attempt(ctx, rj, true)
			select {
			case resp <- err:
			default:
			}
		case <-s.clock.After(delay):
			// The error (if any) is already recorded in Status and logged
			// by attempt itself; the steady-tick path has no caller to
			// report it to.
			_ = s.attempt(ctx, rj, false)
		}

		if ctx.Err() != nil {
			return
		}

		rj.mu.Lock()
		next := rj.status.NextRun
		rj.mu.Unlock()
		delay = s.delayUntil(next)
	}
}

func (s *Scheduler) delayUntil(next time.Time) time.Duration {
	if next.IsZero() {
		return 0
	}
	d := max(next.Sub(s.clock.Now()), 0)
	return d
}

// initialDelay picks the delay before a job's first attempt and, unless
// RunAtStart fires it immediately, records that decision in Status.NextRun
// so a caller reading Status before the first attempt completes still sees
// when the job is scheduled to run next, rather than a zero value for the
// whole first interval.
func (s *Scheduler) initialDelay(rj *job) time.Duration {
	rj.mu.Lock()
	status := rj.status
	rj.mu.Unlock()
	if !status.NextRun.IsZero() {
		return s.delayUntil(status.NextRun)
	}
	if rj.opts.RunAtStart {
		return 0
	}
	delay := rj.j.Interval() + s.jitter(rj.opts.Jitter)
	status.NextRun = s.clock.Now().Add(delay)
	s.setStatus(rj, status)
	return delay
}

// attempt runs one Job.Run call, unless Cooldown gates it, and updates and
// persists Status. bypassCooldown is set only for a TriggerNow call.
func (s *Scheduler) attempt(ctx context.Context, rj *job, bypassCooldown bool) error {
	now := s.clock.Now()

	rj.mu.Lock()
	status := rj.status
	rj.mu.Unlock()
	// Status.Name always tracks the job's own name, regardless of how the
	// status was seeded (Register always sets it, but a status restored
	// from a store keyed by name is authoritative too).
	status.Name = rj.j.Name()

	if !bypassCooldown && rj.opts.Cooldown > 0 && !status.LastAttempt.IsZero() {
		elapsed := now.Sub(status.LastAttempt)
		if elapsed < rj.opts.Cooldown {
			status.NextRun = now.Add(rj.opts.Cooldown - elapsed + s.jitter(rj.opts.Jitter))
			s.setStatus(rj, status)
			s.saveStatus(ctx, status)
			return nil
		}
	}

	status.LastAttempt = now
	s.setStatus(rj, status)
	s.saveStatus(ctx, status)

	runErr := rj.j.Run(ctx)

	now2 := s.clock.Now()
	if runErr == nil {
		status.LastSuccess = now2
		status.LastError = ""
		status.ConsecutiveFailures = 0
		status.NextRun = now2.Add(rj.j.Interval() + s.jitter(rj.opts.Jitter))
	} else {
		status.LastError = runErr.Error()
		status.ConsecutiveFailures++
		if retryAfter, ok := errors.AsType[*RetryAfterError](runErr); ok {
			status.NextRun = now2.Add(retryAfter.RetryAfter)
		} else {
			delay := backoffDelay(rj.j.Interval(), status.ConsecutiveFailures)
			status.NextRun = now2.Add(delay + s.jitter(rj.opts.Jitter))
		}
		if ctx.Err() == nil {
			s.logf("poller: %s: %v", rj.j.Name(), runErr)
		}
	}
	s.setStatus(rj, status)
	s.saveStatus(ctx, status)
	return runErr
}

func (s *Scheduler) setStatus(rj *job, status Status) {
	rj.mu.Lock()
	rj.status = status
	rj.mu.Unlock()
}

// backoffBaseFraction sets the first backoff step relative to interval, so
// backoff behaves the same way for a 30-minute job and a 24-hour job:
// failures grow the delay from interval/16 by doubling, capped at interval.
const backoffBaseFraction = 16

func backoffDelay(interval time.Duration, failures int) time.Duration {
	if failures <= 0 || interval <= 0 {
		return interval
	}
	base := interval / backoffBaseFraction
	if base <= 0 {
		return interval
	}
	d := base
	// Cap the loop itself: 32 doublings overflows nothing meaningful and
	// always exceeds any realistic interval long before that point.
	for i := 1; i < failures && i < 32; i++ {
		d *= 2
		if d >= interval {
			return interval
		}
	}
	if d > interval {
		d = interval
	}
	return d
}
