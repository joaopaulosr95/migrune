// Package runner runs migrations against a database: plan and apply (pending
// set, double-run data migrations, report), the
// global lock with its heartbeat and fence, the changelog and
// graceful shutdown.
package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// CleanupTimeout bounds the cleanup after a run stops: aborting the open
// transaction and releasing the lock.
const CleanupTimeout = 5 * time.Second

// InterruptedError reports that the run stopped because its context was
// cancelled from outside, typically by SIGINT or SIGTERM. An
// interrupted run writes no changelog entry. The command line maps it to exit
// code 128 + the signal number.
type InterruptedError struct {
	// Cause is the context's cancellation cause, e.g. the signal received.
	Cause error
}

func (e *InterruptedError) Error() string { return "interrupted: " + e.Cause.Error() }

func (e *InterruptedError) Unwrap() error { return e.Cause }

// ApprovedBy is who approved the run, recorded in the changelog.
type ApprovedBy struct {
	Name  string `bson:"name" json:"name"`
	Email string `bson:"email" json:"email"`
}

// Options configure a Run.
type Options struct {
	// RunnerID identifies this runner in the lock, the changelog and the
	// logs. Empty means DefaultRunnerID().
	RunnerID string
	// HeartbeatInterval defaults to DefaultHeartbeatInterval.
	HeartbeatInterval time.Duration
	// LockTTL is the shortest time the lock lives without a heartbeat; it
	// expires after the longer of this and three heartbeat intervals. It
	// defaults to DefaultLockTTL and should stay above the server's
	// transactionLifetimeLimitSeconds.
	LockTTL time.Duration
	// LockWait is how long Start keeps trying, with backoff, to take a
	// lock another runner holds. 0 tries once.
	LockWait   time.Duration
	ApprovedBy ApprovedBy
	Logger     *slog.Logger
}

// DefaultRunnerID returns the host name, the process id and a random suffix.
func DefaultRunnerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), rand.Text()[:8])
}

// Run is one plan or apply run holding the migration lock.
type Run struct {
	client     *mongo.Client
	db         *mongo.Database
	lock       *Lock
	changelog  *mongo.Collection
	runnerID   string
	approvedBy ApprovedBy
	log        *slog.Logger

	ctx           context.Context
	cancel        context.CancelCauseFunc
	stopHeartbeat func()
}

// Start acquires the lock on db, waiting up to o.LockWait for it, and starts
// its heartbeat. When another runner holds the lock it returns an error
// matching ErrLockHeld. The returned Run's Context is cancelled when ctx is
// (an interruption) or when the lock is lost (cause ErrLockLost). Call Close
// when done, whatever the outcome.
func Start(ctx context.Context, db *mongo.Database, o Options) (*Run, error) {
	if o.RunnerID == "" {
		o.RunnerID = DefaultRunnerID()
	}
	if o.HeartbeatInterval == 0 {
		o.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if o.LockTTL == 0 {
		o.LockTTL = DefaultLockTTL
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	log := o.Logger.With("runner", o.RunnerID)
	lock, err := NewLock(db, o.RunnerID, o.HeartbeatInterval, o.LockTTL, o.Logger)
	if err != nil {
		return nil, err
	}
	if err := lock.Acquire(ctx, o.LockWait); err != nil {
		return nil, interruption(ctx, err)
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	return &Run{
		client:        db.Client(),
		db:            db,
		lock:          lock,
		changelog:     changelogCollection(db),
		runnerID:      o.RunnerID,
		approvedBy:    o.ApprovedBy,
		log:           log,
		ctx:           runCtx,
		cancel:        cancel,
		stopHeartbeat: lock.StartHeartbeat(runCtx, cancel),
	}, nil
}

// RunnerID returns the id this run holds the lock under.
func (r *Run) RunnerID() string { return r.runnerID }

// Err classifies why the run's context is done: nil while it is live, an
// error wrapping ErrLockLost when the lock was lost, and an
// *InterruptedError otherwise.
func (r *Run) Err() error {
	if r.ctx.Err() == nil {
		return nil
	}
	cause := context.Cause(r.ctx)
	if errors.Is(cause, ErrLockLost) {
		return cause
	}
	return &InterruptedError{Cause: cause}
}

// Close stops the heartbeat and releases the lock if this runner still owns
// it. It runs on its own context bounded by CleanupTimeout, so it works after
// an interruption.
func (r *Run) Close() error {
	r.stopHeartbeat()
	defer r.cancel(context.Canceled)
	ctx, cancel := cleanupContext(r.ctx)
	defer cancel()
	_, err := r.lock.Release(ctx)
	return err
}

// lockErr cancels the run when err reports the lock lost, and returns the
// run's own error when it is done, or err.
func (r *Run) lockErr(err error) error {
	if errors.Is(err, ErrLockLost) {
		r.cancel(err)
	}
	return r.cause(err)
}

// cleanupContext returns a context that survives ctx's cancellation, bounded
// by CleanupTimeout.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), CleanupTimeout)
}

// interruption returns an *InterruptedError when ctx was cancelled, and err
// otherwise.
func interruption(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return &InterruptedError{Cause: context.Cause(ctx)}
	}
	return err
}

// cause returns the run's own error (interrupted, lock lost) when its context
// is done, and err otherwise.
func (r *Run) cause(err error) error {
	if rerr := r.Err(); rerr != nil {
		return rerr
	}
	return err
}
