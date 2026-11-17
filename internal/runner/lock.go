package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

const (
	// LockCollection holds one lock document per namespace.
	LockCollection = migration.LockCollection
	// LockVersion is the format version every lock document is written
	// with, in its v field. A document without v, such as one
	// written by hand, reads as 1.
	LockVersion = 1
	// defaultLockID is the _id of the default namespace's lock document.
	defaultLockID = "migrations"
	// DefaultHeartbeatInterval is how often the lock holder refreshes the lock.
	DefaultHeartbeatInterval = 10 * time.Second
	// DefaultLockTTL is the shortest time a lock lives without a heartbeat. It
	// is above MongoDB's default transactionLifetimeLimitSeconds (60s), so the
	// lock can't expire while one of its holder's transactions is open.
	DefaultLockTTL = 90 * time.Second
	// MinLockTTL is the shortest lock TTL the command line accepts: above
	// MongoDB's default transaction lifetime limit of 60s.
	MinLockTTL = 61 * time.Second
	// expiryFactor is how many heartbeat intervals pass before a lock with no
	// heartbeat can be taken over, unless its TTL is longer.
	expiryFactor = 3
	// maxLeaseMargin is how long before the lock's expiry, counted from the
	// send time of the last heartbeat that succeeded, the runner stops
	// sending unfenced writes (index commands, $merge aggregates). For an
	// expiry under twice the margin, which only tests use, the margin is
	// half the expiry.
	maxLeaseMargin = 10 * time.Second
	// maxLease bounds the expiry the local lease counts from: unlock
	// removes a lock whose heartbeat is older than the server's transaction
	// lifetime plus two heartbeat intervals, at least this, so the holder
	// stops its unfenced writes a lease margin before unlock may let another
	// runner in, whatever --lock-ttl is.
	maxLease = DefaultTransactionLifetime + 2*DefaultHeartbeatInterval
	// lockWaitBackoff and maxLockWaitBackoff bound the pause between two
	// attempts to take a held lock with a lock wait.
	lockWaitBackoff    = time.Second
	maxLockWaitBackoff = 10 * time.Second
)

// ErrLockLost reports that this runner no longer owns the lock: another runner
// took it over, or no heartbeat succeeded in time to keep it.
var ErrLockLost = errors.New("migration lock lost")

// ErrLockHeld is matched (with errors.Is) by a *LockHeldError. The command
// line maps it to exit code 3.
var ErrLockHeld = errors.New("migration lock is held by another runner")

// LockHeldError reports that another runner holds the lock.
type LockHeldError struct {
	// Owner is the holder's runner id (--runner-id), a label: two runners
	// may share it.
	Owner         string
	Current       string
	LastHeartbeat time.Time
}

func (e *LockHeldError) Error() string {
	msg := fmt.Sprintf("migration lock is held by runner %q", e.Owner)
	if e.Current != "" {
		msg += fmt.Sprintf(" (applying %s)", e.Current)
	}
	if !e.LastHeartbeat.IsZero() {
		msg += fmt.Sprintf(", last heartbeat %s", e.LastHeartbeat.UTC().Format(time.RFC3339))
	}
	return msg
}

func (e *LockHeldError) Is(target error) bool { return target == ErrLockHeld }

// lockDoc is the lock document. Timestamps are server time.
type lockDoc struct {
	ID string `bson:"_id"`
	// V is the document's format version, LockVersion; 0 when missing,
	// which reads as 1.
	V int `bson:"v"`
	// Token is random, new on every acquisition: it alone says who owns
	// the lock.
	Token string `bson:"token"`
	// Owner is the holder's runner id, a label for people.
	Owner         string    `bson:"owner"`
	Current       *string   `bson:"current"`
	AcquiredAt    time.Time `bson:"acquiredAt"`
	LastHeartbeat time.Time `bson:"lastHeartbeat"`
	ExpiresAt     time.Time `bson:"expiresAt"`
}

// lockID is the _id of namespace's lock document: "migrations" for the
// default namespace, "migrations:<namespace>" otherwise. A namespace can't
// hold ":", so two namespaces never share a lock.
func lockID(namespace string) string {
	if namespace == "" {
		return defaultLockID
	}
	return defaultLockID + ":" + namespace
}

// Lock is this runner's handle on its namespace's migration lock.
//
// Every timestamp is written and compared with the server's $$NOW, never the
// runner's clock. Each write by the holder sets expiresAt to $$NOW plus the
// expiry, the longer of three heartbeat intervals and the lock TTL; another
// runner may take the lock once expiresAt has passed.
//
// The lock is owned through a random token generated on each acquisition: the
// heartbeat, the fence and Release match on it, so two runners sharing a
// runner id never share the lock.
//
// Heartbeats pause while this runner's transaction holds the lock document,
// so Run.transaction refreshes the heartbeat right before it opens one. With
// a TTL above the server's transaction lifetime limit the lock can't expire
// while the transaction is open; the fence remains the backstop.
type Lock struct {
	coll *mongo.Collection
	// id is the lock document's _id (lockID).
	id       string
	owner    string
	interval time.Duration
	ttl      time.Duration
	log      *slog.Logger
	// sampleValues keeps servers' messages in the heartbeat's logs
	// (errorText).
	sampleValues bool
	// token is set by a successful Acquire.
	token string
	// now is the clock the local lease is measured with: time.Now, or a
	// test's clock.
	now func() time.Time

	// leaseMu guards lease. It is not mu, which a heartbeat holds while it
	// waits for the server: reading the lease never waits on the network.
	leaseMu sync.Mutex
	// lease is the local lease deadline: the send time of the last lock
	// write that succeeded outside a transaction, plus the expiry (at most
	// maxLease), less the lease margin. It keeps the send time's monotonic clock reading, so a
	// step of the wall clock (NTP) can't move it.
	lease time.Time

	// mu serialises writes this runner makes to the lock document outside a
	// transaction (heartbeat, current) with fenced transactions, so a
	// heartbeat never write-conflicts with this runner's own transaction.
	mu sync.Mutex
}

// NewLock returns a handle on namespace's lock in db for the runner owner.
// namespace is "" for the default namespace. interval and ttl must be whole
// numbers of seconds, at least one.
func NewLock(db *mongo.Database, namespace, owner string, interval, ttl time.Duration, log *slog.Logger) (*Lock, error) {
	if owner == "" {
		return nil, errors.New("lock: empty runner id")
	}
	if interval < time.Second || interval%time.Second != 0 {
		return nil, fmt.Errorf("lock: heartbeat interval %s must be a whole number of seconds", interval)
	}
	if ttl < time.Second || ttl%time.Second != 0 {
		return nil, fmt.Errorf("lock: TTL %s must be a whole number of seconds", ttl)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Lock{coll: lockCollection(db), id: lockID(namespace), owner: owner, interval: interval, ttl: ttl, log: log, now: time.Now}, nil
}

// lockCollection is db's lock collection, written and read with majority.
func lockCollection(db *mongo.Database) *mongo.Collection {
	return db.Collection(LockCollection, options.Collection().
		SetWriteConcern(writeconcern.Majority()).
		SetReadConcern(readconcern.Majority()))
}

// expiry is how long a lock written by this runner lives without a heartbeat:
// the longer of three intervals and the TTL.
func (l *Lock) expiry() time.Duration { return max(expiryFactor*l.interval, l.ttl) }

// leaseMargin is maxLeaseMargin, or half the expiry when that is shorter.
func (l *Lock) leaseMargin() time.Duration { return min(maxLeaseMargin, l.expiry()/2) }

// renewed records a lock write sent at sent, a reading of l.now, that
// succeeded outside a transaction.
func (l *Lock) renewed(sent time.Time) {
	l.leaseMu.Lock()
	defer l.leaseMu.Unlock()
	l.lease = sent.Add(min(l.expiry(), maxLease) - l.leaseMargin())
}

// leaseExpired reports whether the local lease deadline has passed: the lock
// may expire on the server soon, so no unfenced write may start. Both times
// carry monotonic clock readings, so the comparison ignores the wall clock.
func (l *Lock) leaseExpired() bool {
	l.leaseMu.Lock()
	defer l.leaseMu.Unlock()
	return l.now().After(l.lease)
}

// ownedFilter matches the lock document only while this runner owns it.
func (l *Lock) ownedFilter() bson.D {
	return bson.D{{Key: "_id", Value: l.id}, {Key: "token", Value: l.token}}
}

// expiresAt is the expression for the lock's new expiry: $$NOW plus the
// expiry.
func (l *Lock) expiresAt() bson.D {
	return bson.D{{Key: "$add", Value: bson.A{"$$NOW", l.expiry().Milliseconds()}}}
}

// lockVersion sets the lock document's v, LockVersion, in every write.
var lockVersion = bson.E{Key: "v", Value: bson.D{{Key: "$literal", Value: int32(LockVersion)}}}

// heartbeatUpdate refreshes lastHeartbeat and expiresAt with the server's
// clock.
func (l *Lock) heartbeatUpdate() mongo.Pipeline {
	return mongo.Pipeline{{{Key: "$set", Value: bson.D{
		lockVersion,
		{Key: "lastHeartbeat", Value: "$$NOW"},
		{Key: "expiresAt", Value: l.expiresAt()},
	}}}}
}

// newToken returns a random 128-bit token, in hex.
func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails
	return hex.EncodeToString(b)
}

// Acquire takes the lock with a new token: it succeeds when there is no lock
// or when the lock has expired. When another runner holds it, Acquire tries
// again with backoff until wait has passed, then returns a *LockHeldError
// naming the holder's runner id. A wait of 0 tries once. When ctx is done
// while waiting, Acquire returns the last *LockHeldError.
func (l *Lock) Acquire(ctx context.Context, wait time.Duration) error {
	token := newToken()
	deadline := time.Now().Add(wait)
	backoff := lockWaitBackoff
	for {
		err := l.tryAcquire(ctx, token)
		if !errors.Is(err, ErrLockHeld) {
			return err
		}
		left := time.Until(deadline)
		if left <= 0 {
			if wait > 0 {
				return fmt.Errorf("%w; gave up after waiting %s", err, wait)
			}
			return err
		}
		pause := min(backoff, left)
		l.log.Info("migration lock is held by another runner, waiting", "error", err, "retryIn", pause, "waitLeft", left.Round(time.Second))
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
		backoff = min(2*backoff, maxLockWaitBackoff)
	}
}

// tryAcquire makes one attempt to take the lock with token.
//
// It is one findOneAndUpdate with upsert on the lock's _id. MongoDB doesn't
// allow $expr in an upsert's query, so the check lives in the update pipeline:
// the document is rewritten for this runner only when it may take it (no
// document, an expired one, or one with this token, from an earlier attempt
// of the same Acquire), and is otherwise left as it is and returned to name
// the holder. A document with a v above LockVersion, written by a newer
// migrune, is never taken, even expired: tryAcquire fails with a
// *FormatError. CheckFormats refuses it before, and this keeps a newer runner
// that wrote one since from having its lock overwritten.
func (l *Lock) tryAcquire(ctx context.Context, token string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// A new document has no token and no expiresAt, which sorts before any
	// date. $literal keeps a runner id starting with "$" from being read as
	// a field path.
	me := bson.D{{Key: "$literal", Value: l.owner}}
	mayTake := bson.D{{Key: "$and", Value: bson.A{
		bson.D{{Key: "$lte", Value: bson.A{bson.D{{Key: "$ifNull", Value: bson.A{"$v", 1}}}, LockVersion}}},
		bson.D{{Key: "$or", Value: bson.A{
			bson.D{{Key: "$eq", Value: bson.A{"$token", token}}},
			bson.D{{Key: "$lt", Value: bson.A{"$expiresAt", "$$NOW"}}},
		}}},
	}}}
	ifTaken := func(taken, kept any) bson.D {
		return bson.D{{Key: "$cond", Value: bson.A{"$" + acquireField, taken, kept}}}
	}
	update := mongo.Pipeline{
		{{Key: "$set", Value: bson.D{{Key: acquireField, Value: mayTake}}}},
		{{Key: "$set", Value: bson.D{
			{Key: "v", Value: ifTaken(int32(LockVersion), "$v")},
			{Key: "token", Value: ifTaken(token, "$token")},
			{Key: "owner", Value: ifTaken(me, "$owner")},
			{Key: "current", Value: ifTaken(nil, "$current")},
			{Key: "acquiredAt", Value: ifTaken("$$NOW", "$acquiredAt")},
			{Key: "lastHeartbeat", Value: ifTaken("$$NOW", "$lastHeartbeat")},
			{Key: "expiresAt", Value: ifTaken(l.expiresAt(), "$expiresAt")},
		}}},
		{{Key: "$unset", Value: acquireField}},
	}
	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)
	filter := bson.D{{Key: "_id", Value: l.id}}

	for range 3 {
		sent := l.now()
		var doc lockDoc
		err := l.coll.FindOneAndUpdate(ctx, filter, update, opts).Decode(&doc)
		if mongo.IsDuplicateKeyError(err) {
			// Two runners raced to insert the first lock document and the
			// other one won: read it to name the holder.
			err = l.coll.FindOne(ctx, filter).Decode(&doc)
			if errors.Is(err, mongo.ErrNoDocuments) {
				continue // released in between: try again
			}
		}
		if err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}
		if doc.Token != token {
			if doc.V > LockVersion {
				return &FormatError{What: "migration lock", Found: doc.V, Known: LockVersion}
			}
			held := &LockHeldError{Owner: doc.Owner, LastHeartbeat: doc.LastHeartbeat}
			if doc.Current != nil {
				held.Current = *doc.Current
			}
			return held
		}
		l.token = token
		l.renewed(sent)
		l.log.Info("migration lock acquired", "runner", l.owner, "lock", l.id, "heartbeatInterval", l.interval, "lockTTL", l.ttl)
		return nil
	}
	return fmt.Errorf("acquire migration lock: %w", ErrLockHeld)
}

// acquireField is a temporary field tryAcquire's pipeline computes and
// removes.
const acquireField = "_migruneAcquire"

// Heartbeat refreshes the lock's lastHeartbeat and expiresAt, waiting at most
// one heartbeat interval for the server. It returns ErrLockLost when this
// runner no longer owns the lock.
func (l *Lock) Heartbeat(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, l.interval)
	defer cancel()
	return l.update(ctx, l.heartbeatUpdate())
}

// SetCurrent records the migration this runner is applying in the lock
// document, and refreshes the heartbeat. An empty id clears it.
func (l *Lock) SetCurrent(ctx context.Context, migrationID string) error {
	var current any
	if migrationID != "" {
		current = bson.D{{Key: "$literal", Value: migrationID}}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.update(ctx, mongo.Pipeline{{{Key: "$set", Value: bson.D{
		lockVersion,
		{Key: "current", Value: current},
		{Key: "lastHeartbeat", Value: "$$NOW"},
		{Key: "expiresAt", Value: l.expiresAt()},
	}}}})
}

// update writes the lock document outside a transaction, if this runner owns
// it, and renews the local lease. The caller holds mu.
func (l *Lock) update(ctx context.Context, update mongo.Pipeline) error {
	sent := l.now()
	res, err := l.coll.UpdateOne(ctx, l.ownedFilter(), update)
	if err != nil {
		return fmt.Errorf("refresh migration lock: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrLockLost
	}
	l.renewed(sent)
	return nil
}

// Fence must run inside a transaction (ctx carries the session). It writes the
// lock document only if this runner still owns it, so a runner whose lock was
// taken over can never commit: when the lock has a new token the write
// matches nothing and Fence returns ErrLockLost, and when a takeover commits
// while the transaction is open the two writes to the lock document conflict
// and the transaction aborts with a write conflict (a transient transaction
// error: a new attempt fences again, and matches nothing if the lock was
// really taken over). Either way the caller must abort the transaction.
//
// Run Fence as the transaction's first operation: from then on the lock
// document is held by the transaction, and a takeover waits for it to end.
func (l *Lock) Fence(ctx context.Context) error {
	if sess := mongo.SessionFromContext(ctx); sess == nil || !sess.TransactionRunning() {
		return errors.New("lock: fence called outside a transaction")
	}
	res, err := l.coll.UpdateOne(ctx, l.ownedFilter(), l.heartbeatUpdate())
	if err != nil {
		return fmt.Errorf("fence migration lock: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrLockLost
	}
	return nil
}

// Release deletes the lock document if this runner owns it. It reports whether
// a document was deleted; releasing a lock that has another token does
// nothing.
func (l *Lock) Release(ctx context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	res, err := l.coll.DeleteOne(ctx, l.ownedFilter())
	if err != nil {
		return false, fmt.Errorf("release migration lock: %w", err)
	}
	if res.DeletedCount == 0 {
		l.log.Warn("migration lock not released: this runner no longer owns it", "runner", l.owner)
		return false, nil
	}
	l.log.Info("migration lock released", "runner", l.owner)
	return true, nil
}

// StartHeartbeat refreshes the lock every interval until ctx is done or the
// returned stop function is called. When the lock is lost, or a heartbeat
// fails once the local lease deadline has passed, it calls lost with an error
// wrapping ErrLockLost and stops. stop waits for the goroutine to exit.
func (l *Lock) StartHeartbeat(ctx context.Context, lost context.CancelCauseFunc) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(l.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			err := l.Heartbeat(ctx)
			switch {
			case err == nil:
				l.log.Debug("migration lock heartbeat", "runner", l.owner)
			case ctx.Err() != nil:
				return
			case errors.Is(err, ErrLockLost):
				l.log.Error("migration lock lost: another runner took it over", "runner", l.owner)
				lost(ErrLockLost)
				return
			case l.leaseExpired():
				l.log.Error("migration lock lost: no heartbeat succeeded within the local lease",
					"runner", l.owner, "error", errorText(err, l.sampleValues))
				lost(fmt.Errorf("%w: %w", ErrLockLost, err))
				return
			default:
				l.log.Warn("migration lock heartbeat failed, retrying", "runner", l.owner, "error", errorText(err, l.sampleValues))
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
