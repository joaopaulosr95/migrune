package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

// Transactions are retried on these error labels, and give up after these
// bounds.
const (
	labelTransient     = "TransientTransactionError"
	labelUnknownCommit = "UnknownTransactionCommitResult"
	// maxTransactionRetries is how many times a transaction that failed
	// with a transient error runs again.
	maxTransactionRetries = 3
	// transactionBackoff is the pause before the first new attempt; it
	// doubles for each one after.
	transactionBackoff = 200 * time.Millisecond
	// maxCommitRetries is how many times a commit whose outcome is unknown
	// is sent again.
	maxCommitRetries = 3
	// CommitTimeout bounds a commit, retries included. The commit runs on a
	// context that an interruption doesn't cancel, so it always finishes.
	CommitTimeout = 30 * time.Second
)

// transaction runs fn in one transaction with read concern snapshot
// and write concern majority. The heartbeat is refreshed right before the
// transaction starts, since heartbeats wait while it is open: the lock's full
// expiry, at least its TTL, is left for the transaction. The fence is its
// first operation, so a runner that lost the lock can't commit. When commit
// is false (plan), or fn fails, the transaction is aborted on a cleanup
// context, so an interruption still aborts it at once.
//
// A transaction that fails with a TransientTransactionError (a write
// conflict, an election) runs again, fn included, up to
// maxTransactionRetries times with backoff while the run is live, each time
// in a new session with a fresh heartbeat and fence. A fence that matches
// nothing never runs again: the lock is lost, and the run is cancelled.
//
// The commit runs on a context the run's cancellation doesn't reach, bounded
// by CommitTimeout, so an interruption during the commit lets it finish and
// transaction reports what happened. A commit whose outcome is unknown is
// sent again; see commit. entry, when set, is the _id of the changelog entry
// fn writes: when a commit's outcome stays unknown, the entry being there
// tells it committed.
//
// Errors are classified like Err: when the run was interrupted or lost its
// lock, transaction returns that, whatever fn returned.
func (r *Run) transaction(fn func(ctx context.Context) error, commit bool, entry *bson.ObjectID) error {
	for attempt := 0; ; attempt++ {
		if err := r.Err(); err != nil {
			return err
		}
		err := r.transactionAttempt(fn, commit, entry)
		if err == nil {
			return nil
		}
		if rerr := r.Err(); rerr != nil {
			return rerr
		}
		if attempt == maxTransactionRetries || !hasErrorLabel(err, labelTransient) {
			return err
		}
		pause := transactionBackoff << attempt
		r.log.Warn("transient transaction error: running the transaction again",
			"attempt", attempt+2, "retryIn", pause, "error", err)
		t := time.NewTimer(pause)
		select {
		case <-r.ctx.Done():
			t.Stop()
			return r.Err()
		case <-t.C:
		}
	}
}

// transactionAttempt runs fn once in a new session and transaction, after a
// heartbeat and the fence.
func (r *Run) transactionAttempt(fn func(ctx context.Context) error, commit bool, entry *bson.ObjectID) error {
	// Heartbeats wait while the transaction holds the lock document, so they
	// never conflict with it.
	r.lock.mu.Lock()
	defer r.lock.mu.Unlock()
	if err := r.lock.update(r.ctx, r.lock.heartbeatUpdate()); err != nil {
		return r.lockErr(err)
	}

	sess, err := r.client.StartSession()
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	defer func() {
		ctx, cancel := cleanupContext(r.ctx)
		defer cancel()
		sess.EndSession(ctx)
	}()
	err = sess.StartTransaction(options.Transaction().
		SetReadConcern(readconcern.Snapshot()).
		SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		return fmt.Errorf("start transaction: %w", err)
	}
	sctx := mongo.NewSessionContext(r.ctx, sess)
	err = r.lock.Fence(sctx)
	if errors.Is(err, ErrLockLost) {
		r.cancel(err)
	}
	if err == nil {
		err = fn(sctx)
	}
	if err == nil && commit {
		return r.commit(sess, entry)
	}
	ctx, cancel := cleanupContext(r.ctx)
	defer cancel()
	if abortErr := sess.AbortTransaction(ctx); abortErr != nil && err == nil {
		err = fmt.Errorf("abort transaction: %w", abortErr)
	}
	return err
}

// commit commits sess's transaction on a context the run's cancellation
// doesn't reach. A commit that fails with UnknownTransactionCommitResult is
// sent again, up to maxCommitRetries times; MongoDB makes that safe. If the
// outcome is still unknown, the session is killed, so the transaction can't
// commit later, and the changelog entry with _id entry is looked up: there,
// the transaction committed; missing, it didn't. Without an entry an unknown
// outcome is an error.
func (r *Run) commit(sess *mongo.Session, entry *bson.ObjectID) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), CommitTimeout)
	defer cancel()
	err := sess.CommitTransaction(ctx)
	for i := 0; i < maxCommitRetries && err != nil && hasErrorLabel(err, labelUnknownCommit) && ctx.Err() == nil; i++ {
		r.log.Warn("transaction commit outcome unknown: committing again", "error", err)
		err = sess.CommitTransaction(ctx)
	}
	if err == nil {
		return nil
	}
	if !hasErrorLabel(err, labelUnknownCommit) && ctx.Err() == nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	if entry == nil {
		return fmt.Errorf("commit transaction: outcome unknown: %w", err)
	}
	lctx, lcancel := context.WithTimeout(context.WithoutCancel(r.ctx), CommitTimeout)
	defer lcancel()
	if kerr := killSession(lctx, r.client, sess); kerr != nil {
		r.log.Warn("could not kill the session of a commit with an unknown outcome", "error", kerr)
	}
	// A linearizable read sees the commit if it landed on the primary and
	// can't be rolled back; a majority read could miss one still
	// replicating.
	changelog := r.db.Collection(ChangelogCollection,
		options.Collection().SetReadConcern(readconcern.Linearizable()))
	n, lerr := changelog.CountDocuments(lctx, bson.D{{Key: "_id", Value: *entry}})
	switch {
	case lerr != nil:
		return fmt.Errorf("commit transaction: outcome unknown, and reading the changelog to find out failed: %w",
			errors.Join(err, lerr))
	case n > 0:
		r.log.Warn("transaction commit outcome was unknown: its changelog entry is there, so it committed", "error", err)
		return nil
	default:
		return fmt.Errorf("commit transaction: outcome unknown, and its changelog entry is missing, so it didn't commit: %w", err)
	}
}

// killSession kills the operations running in sess, and aborts its open
// transaction.
func killSession(ctx context.Context, client *mongo.Client, sess *mongo.Session) error {
	return client.Database("admin").RunCommand(ctx, bson.D{{Key: "killSessions", Value: bson.A{sess.ID()}}}).Err()
}

// hasErrorLabel reports whether err carries the server or driver error label.
func hasErrorLabel(err error, label string) bool {
	var le mongo.LabeledError
	return errors.As(err, &le) && le.HasErrorLabel(label)
}
