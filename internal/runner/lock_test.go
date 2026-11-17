package runner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

func TestAcquireConcurrent(t *testing.T) {
	db := testDB(t)
	ctx := testCtx(t)

	const runners = 8
	errs := make([]error, runners)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range runners {
		l := newTestLock(t, db, fmt.Sprintf("runner-%d", i), time.Second, time.Second)
		wg.Go(func() {
			<-start
			errs[i] = l.Acquire(ctx, 0)
		})
	}
	close(start)
	wg.Wait()

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner >= 0 {
				t.Fatalf("runner-%d and runner-%d both acquired the lock", winner, i)
			}
			winner = i
		case !errors.Is(err, ErrLockHeld):
			t.Fatalf("runner-%d: %v, want a lock held error", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no runner acquired the lock")
	}
	want := fmt.Sprintf("runner-%d", winner)
	for i, err := range errs {
		var held *LockHeldError
		if errors.As(err, &held) && held.Owner != want {
			t.Errorf("runner-%d was told the holder is %q, want %q", i, held.Owner, want)
		}
	}
	if doc := readLock(t, ctx, db); doc == nil || doc.Owner != want || len(doc.Token) != 32 ||
		doc.ExpiresAt.Sub(doc.LastHeartbeat) != 3*time.Second || !doc.AcquiredAt.Equal(doc.LastHeartbeat) {
		t.Errorf("lock document %+v, want owner %s with a token, expiring 3s after its heartbeat", doc, want)
	}
}

// TestLeaseClock checks the local lease with an injected clock: it ends
// exactly at the send time plus the expiry, at most maxLease, less the
// margin, and it keeps
// the clock's monotonic reading. Go compares two times on their monotonic
// readings when both have one, so a step of the wall clock, such as NTP
// setting it back, can't extend the lease. The public time API can't build
// a time whose wall and monotonic readings disagree, so the test checks the
// reading is kept instead of stepping the clock.
func TestLeaseClock(t *testing.T) {
	// The margin is 10s and maxLease 80s: with the default 90s TTL or a 1h
	// one, the lease ends 70s after the send time, before unlock's 80s
	// threshold. A 40s TTL is a 40s expiry, under the cap: 30s.
	base := time.Now()
	cases := []struct {
		name    string
		ttl     time.Duration
		elapsed time.Duration // on the monotonic clock, since the send time
		want    bool
	}{
		{name: "just renewed", ttl: 90 * time.Second, elapsed: 0},
		{name: "at the deadline", ttl: 90 * time.Second, elapsed: 70 * time.Second},
		{name: "past the deadline", ttl: 90 * time.Second, elapsed: 70*time.Second + time.Nanosecond, want: true},
		{name: "long past the deadline", ttl: 90 * time.Second, elapsed: time.Hour, want: true},
		{name: "long TTL at the cap", ttl: time.Hour, elapsed: 70 * time.Second},
		{name: "long TTL past the cap", ttl: time.Hour, elapsed: 70*time.Second + time.Nanosecond, want: true},
		{name: "short TTL at the deadline", ttl: 40 * time.Second, elapsed: 30 * time.Second},
		{name: "short TTL past the deadline", ttl: 40 * time.Second, elapsed: 30*time.Second + time.Nanosecond, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := base
			l := &Lock{interval: 10 * time.Second, ttl: tc.ttl, now: func() time.Time { return now }}
			if !l.leaseExpired() {
				t.Fatal("lease not expired before any renewal")
			}
			l.renewed(l.now())
			if !strings.Contains(l.lease.String(), " m=") {
				t.Fatalf("lease %s has no monotonic clock reading: a wall clock step would move it", l.lease)
			}
			now = base.Add(tc.elapsed)
			if got := l.leaseExpired(); got != tc.want {
				t.Errorf("leaseExpired() after %s = %v, want %v", tc.elapsed, got, tc.want)
			}
		})
	}
}

// TestAcquire checks when runner B can take runner A's lock. Its cases
// run in parallel, each on its own database, with no fail point.
func TestAcquire(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// ttl is runner A's lock TTL; zero means 1s, so A's lock expires
		// after three of its 1s intervals.
		ttl time.Duration
		// expiry is how long after its heartbeat A's lock expires; zero
		// means 3s, three intervals.
		expiry time.Duration
		// setup runs with runner A holding the lock (1s interval) and
		// returns before B tries to acquire it.
		setup   func(t *testing.T, ctx context.Context, a *Lock)
		wantErr error
	}{
		{
			name:    "held and fresh",
			setup:   func(*testing.T, context.Context, *Lock) {},
			wantErr: ErrLockHeld,
		},
		{
			name: "held but fresh thanks to heartbeats",
			setup: func(t *testing.T, ctx context.Context, a *Lock) {
				for range 4 {
					time.Sleep(time.Second)
					if err := a.Heartbeat(ctx); err != nil {
						t.Fatal(err)
					}
				}
			},
			wantErr: ErrLockHeld,
		},
		{
			name:  "expired",
			setup: func(*testing.T, context.Context, *Lock) { time.Sleep(3500 * time.Millisecond) },
		},
		{
			// Three intervals are 3s, but the TTL floors the expiry.
			name:    "the TTL floors the expiry",
			ttl:     4 * time.Second,
			expiry:  4 * time.Second,
			setup:   func(*testing.T, context.Context, *Lock) {},
			wantErr: ErrLockHeld,
		},
		{
			name: "released",
			setup: func(t *testing.T, ctx context.Context, a *Lock) {
				if ok, err := a.Release(ctx); err != nil || !ok {
					t.Fatalf("release: %v %v", ok, err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			ctx := testCtx(t)
			ttl, expiry := cmp.Or(tc.ttl, time.Second), cmp.Or(tc.expiry, 3*time.Second)
			a := newTestLock(t, db, "runner-a", time.Second, ttl)
			b := newTestLock(t, db, "runner-b", 10*time.Second, 2*time.Minute)
			if err := a.Acquire(ctx, 0); err != nil {
				t.Fatal(err)
			}
			if doc := readLock(t, ctx, db); doc.ExpiresAt.Sub(doc.LastHeartbeat) != expiry {
				t.Fatalf("A's lock expires %s after its heartbeat, want %s", doc.ExpiresAt.Sub(doc.LastHeartbeat), expiry)
			}
			tc.setup(t, ctx, a)
			err := b.Acquire(ctx, 0)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
				t.Fatalf("B acquire: %v, want %v", err, tc.wantErr)
			}
			doc := readLock(t, ctx, db)
			if tc.wantErr != nil {
				var held *LockHeldError
				if !errors.As(err, &held) || held.Owner != "runner-a" {
					t.Errorf("B acquire: %v, want the holder named runner-a", err)
				}
				if doc.Owner != "runner-a" {
					t.Errorf("lock owner %q, want runner-a", doc.Owner)
				}
				return
			}
			if doc.Owner != "runner-b" || doc.ExpiresAt.Sub(doc.LastHeartbeat) != 2*time.Minute {
				t.Errorf("lock document %+v, want runner-b, expiring after its 2m TTL", doc)
			}
			// A was taken over: it can neither heartbeat nor release.
			if err := a.Heartbeat(ctx); !errors.Is(err, ErrLockLost) {
				t.Errorf("A heartbeat after takeover: %v, want ErrLockLost", err)
			}
			if ok, err := a.Release(ctx); ok || err != nil {
				t.Errorf("A release after takeover: %v %v, want false, nil", ok, err)
			}
		})
	}
}

// TestSameRunnerID checks the runner id is only a label: a second runner with
// the same id can't take a held lock, and once it takes the expired lock, the
// first runner can neither heartbeat nor release it (T1).
func TestSameRunnerID(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	ctx := testCtx(t)
	a := newTestLock(t, db, "deploy", time.Second, time.Second)
	b := newTestLock(t, db, "deploy", time.Second, time.Second)
	if err := a.Acquire(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := a.SetCurrent(ctx, "2026-11-05-001-add-archived-flag"); err != nil {
		t.Fatal(err)
	}
	var held *LockHeldError
	if err := b.Acquire(ctx, 0); !errors.As(err, &held) || held.Owner != "deploy" || held.Current != "2026-11-05-001-add-archived-flag" {
		t.Fatalf("B acquire while A holds the lock: %v, want held by deploy", err)
	}
	first := readLock(t, ctx, db)

	// A stops heartbeating; B takes the expired lock.
	eventually(t, 10*time.Second, "B to take the expired lock", func() bool { return b.Acquire(ctx, 0) == nil })
	taken := readLock(t, ctx, db)
	if taken.Owner != "deploy" || taken.Token == first.Token || taken.Current != nil || !taken.AcquiredAt.After(first.AcquiredAt) {
		t.Fatalf("lock after B took it %+v, want a new token, no current and a new acquiredAt (was %+v)", taken, first)
	}
	if err := a.Heartbeat(ctx); !errors.Is(err, ErrLockLost) {
		t.Errorf("A heartbeat: %v, want ErrLockLost", err)
	}
	if ok, err := a.Release(ctx); ok || err != nil {
		t.Errorf("A release: %v %v, want false, nil", ok, err)
	}
	if doc := readLock(t, ctx, db); doc == nil || doc.Token != taken.Token {
		t.Fatalf("lock after A released: %+v, want B's", doc)
	}
	if ok, err := b.Release(ctx); !ok || err != nil {
		t.Fatalf("B release: %v %v, want true, nil", ok, err)
	}
}

// TestLockWait checks Acquire with a wait takes the lock once its holder
// releases it within the wait, and gives up with a lock held error when the
// holder keeps it.
func TestLockWait(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		release time.Duration // 0: never
		wait    time.Duration
		wantErr error
	}{
		{name: "released within the wait", release: 1500 * time.Millisecond, wait: 20 * time.Second},
		{name: "held past the wait", wait: 2 * time.Second, wantErr: ErrLockHeld},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			ctx := testCtx(t)
			holder := newTestLock(t, db, "holder", 10*time.Second, time.Minute)
			waiter := newTestLock(t, db, "waiter", 10*time.Second, time.Minute)
			if err := holder.Acquire(ctx, 0); err != nil {
				t.Fatal(err)
			}
			if tc.release > 0 {
				time.AfterFunc(tc.release, func() { _, _ = holder.Release(context.Background()) })
			}
			start := time.Now()
			err := waiter.Acquire(ctx, tc.wait)
			took := time.Since(start)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
				t.Fatalf("acquire: %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if took < tc.wait || took > tc.wait+2*time.Second {
					t.Errorf("gave up after %s, want just after %s", took, tc.wait)
				}
				if doc := readLock(t, ctx, db); doc == nil || doc.Owner != "holder" {
					t.Errorf("lock %+v, want still the holder's", doc)
				}
				return
			}
			if took < tc.release {
				t.Errorf("acquired after %s, before the release at %s", took, tc.release)
			}
			if doc := readLock(t, ctx, db); doc == nil || doc.Owner != "waiter" {
				t.Errorf("lock %+v, want the waiter's", doc)
			}
		})
	}
}

func TestReleaseByNonOwner(t *testing.T) {
	db := testDB(t)
	ctx := testCtx(t)
	a := newTestLock(t, db, "runner-a", time.Second, time.Second)
	b := newTestLock(t, db, "runner-b", time.Second, time.Second)
	if err := a.Acquire(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := b.Release(ctx); ok || err != nil {
		t.Fatalf("B release: %v %v, want false, nil", ok, err)
	}
	if doc := readLock(t, ctx, db); doc == nil || doc.Owner != "runner-a" {
		t.Fatalf("lock after B release: %+v, want owned by runner-a", doc)
	}
	if ok, err := a.Release(ctx); !ok || err != nil {
		t.Fatalf("A release: %v %v, want true, nil", ok, err)
	}
	if doc := readLock(t, ctx, db); doc != nil {
		t.Fatalf("lock after A release: %+v, want none", doc)
	}
}

func TestHeartbeatLossCancelsContext(t *testing.T) {
	db := testDB(t)
	ctx := testCtx(t)
	run, err := Start(ctx, db, Options{RunnerID: "runner-a", HeartbeatInterval: time.Second, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	// A few heartbeats keep the lock fresh.
	first := readLock(t, ctx, db).LastHeartbeat
	eventually(t, 5*time.Second, "a heartbeat", func() bool {
		return readLock(t, ctx, db).LastHeartbeat.After(first)
	})
	if run.Err() != nil {
		t.Fatalf("run cancelled while holding the lock: %v", run.Err())
	}

	takeOver(t, ctx, db, "runner-b")
	select {
	case <-run.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("run context not cancelled after the lock was lost")
	}
	if err := run.Err(); !errors.Is(err, ErrLockLost) {
		t.Errorf("run.Err() = %v, want ErrLockLost", err)
	}
	if err := run.transaction(func(context.Context) error { return nil }, true, nil); !errors.Is(err, ErrLockLost) {
		t.Errorf("transaction after losing the lock: %v, want ErrLockLost", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if doc := readLock(t, ctx, db); doc == nil || doc.Owner != "runner-b" {
		t.Errorf("lock after close: %+v, want still owned by runner-b", doc)
	}
}

// TestZombieCannotCommit checks a runner whose lock was taken over can't
// commit a data migration's transaction.
func TestZombieCannotCommit(t *testing.T) {
	cases := []struct {
		name string
		// fenceFirst runs the fence before the data write, as
		// Run.transaction does; otherwise the fence runs last, from
		// WriteFinished.
		fenceFirst bool
	}{
		// The takeover happened before the transaction touched the lock:
		// the fence matches nothing.
		{name: "fence matches nothing", fenceFirst: true},
		// The takeover committed while the transaction was open, after its
		// snapshot: the fence write conflicts.
		{name: "fence write conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			ctx := testCtx(t)
			zombie := newTestLock(t, db, "zombie", 10*time.Second, time.Minute)
			if err := zombie.Acquire(ctx, 0); err != nil {
				t.Fatal(err)
			}
			data := db.Collection("projects")
			if _, err := data.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}}); err != nil {
				t.Fatal(err)
			}

			sess, err := db.Client().StartSession()
			if err != nil {
				t.Fatal(err)
			}
			defer sess.EndSession(context.Background())
			err = sess.StartTransaction(options.Transaction().
				SetReadConcern(readconcern.Snapshot()).
				SetWriteConcern(writeconcern.Majority()))
			if err != nil {
				t.Fatal(err)
			}
			sctx := mongo.NewSessionContext(ctx, sess)

			// Another runner takes the lock over, as if the zombie
			// had stopped heartbeating and its lock expired.
			takeover := func() { takeOver(t, ctx, db, "next") }
			write := func() {
				_, err := data.UpdateOne(sctx, bson.D{{Key: "_id", Value: 1}}, bson.D{{Key: "$set", Value: bson.D{{Key: "migrated", Value: true}}}})
				if err != nil {
					t.Fatal(err)
				}
			}

			if tc.fenceFirst {
				takeover()
				err = zombie.Fence(sctx)
			} else {
				write()
				takeover()
				err = zombie.Fence(sctx)
			}
			// A fence that matches nothing is a lost lock; a write
			// conflict is a transient error, and the retried
			// transaction's fence would match nothing.
			var ce mongo.CommandError
			conflict := errors.As(err, &ce) && ce.HasErrorCode(112) && ce.HasErrorLabel(labelTransient)
			if tc.fenceFirst && !errors.Is(err, ErrLockLost) || !tc.fenceFirst && (!conflict || errors.Is(err, ErrLockLost)) {
				t.Fatalf("fence: %v, want a lost lock %v, a transient write conflict %v", err, tc.fenceFirst, !tc.fenceFirst)
			}
			if tc.fenceFirst {
				write()
			}
			// Even a caller that ignores the fence can't commit once it
			// conflicted; one that respects it aborts.
			if !tc.fenceFirst {
				if err := sess.CommitTransaction(ctx); err == nil {
					t.Fatal("commit after a fence write conflict succeeded")
				}
			} else {
				if err := sess.AbortTransaction(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if n := countDocs(t, ctx, data, bson.D{{Key: "migrated", Value: true}}); n != 0 {
				t.Errorf("zombie's write is visible in %d documents", n)
			}
			if doc := readLock(t, ctx, db); doc.Owner != "next" {
				t.Errorf("lock owner %q, want next", doc.Owner)
			}
		})
	}
}

// TestFencedTransactionLostLock checks Run.transaction refuses to commit once
// the lock was taken over, before the heartbeat notices.
func TestFencedTransactionLostLock(t *testing.T) {
	db := testDB(t)
	ctx := testCtx(t)
	run, err := Start(ctx, db, Options{RunnerID: "zombie", HeartbeatInterval: 10 * time.Second, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	m := fixture(t, "2026-11-05-001-add-archived-flag")
	takeOver(t, ctx, db, "next")

	data := db.Collection("projects")
	err = run.transaction(func(sctx context.Context) error {
		t.Error("transaction body ran after the fence failed")
		_, err := data.InsertOne(sctx, bson.D{{Key: "_id", Value: 1}})
		if err != nil {
			return err
		}
		return run.WriteFinished(sctx, bson.NewObjectID(), m, nil)
	}, true, nil)
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("transaction: %v, want ErrLockLost", err)
	}
	if n := countDocs(t, ctx, data, bson.D{}); n != 0 {
		t.Errorf("%d data documents written", n)
	}
	if n := countDocs(t, ctx, db.Collection(ChangelogCollection), bson.D{}); n != 0 {
		t.Errorf("%d changelog entries written", n)
	}
}

// TestTransactionRefreshesHeartbeat checks Run.transaction refreshes the
// heartbeat before the transaction opens, outside it.
func TestTransactionRefreshesHeartbeat(t *testing.T) {
	db := testDB(t)
	ctx := testCtx(t)
	// A long interval: no timed heartbeat runs during the test.
	run, err := Start(ctx, db, Options{RunnerID: "runner-a", HeartbeatInterval: time.Minute, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	before := readLock(t, ctx, db).LastHeartbeat
	time.Sleep(50 * time.Millisecond)
	err = run.transaction(func(context.Context) error {
		// Read outside the transaction: the committed lock document.
		if doc := readLock(t, ctx, db); !doc.LastHeartbeat.After(before) {
			t.Errorf("lastHeartbeat when the transaction opened = %s, want after %s", doc.LastHeartbeat, before)
		}
		return nil
	}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
}

// TestLongTransactionKeepsLock checks a transaction open for longer than three
// heartbeat intervals, but less than the lock TTL, commits while another
// runner keeps trying to take the lock, and that runner never gets it.
func TestLongTransactionKeepsLock(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	ctx := testCtx(t)
	const (
		interval = time.Second
		ttl      = 6 * time.Second
		hold     = 4 * time.Second // > 3 × interval, < ttl
	)
	run, err := Start(ctx, db, Options{RunnerID: "runner-a", HeartbeatInterval: interval, LockTTL: ttl, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()

	// Runner B tries to acquire the lock until told to stop. Its acquire
	// waits while A's transaction holds the lock document, so each attempt
	// is bounded and retried.
	other := newTestLock(t, db, "runner-b", interval, time.Second)
	stop := make(chan struct{})
	var got []time.Time
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			actx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			err := other.Acquire(actx, 0)
			cancel()
			if err == nil {
				got = append(got, time.Now())
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	data := db.Collection("projects")
	err = run.transaction(func(sctx context.Context) error {
		if _, err := data.InsertOne(sctx, bson.D{{Key: "_id", Value: 1}}); err != nil {
			return err
		}
		time.Sleep(hold)
		return nil
	}, true, nil)
	// Keep B trying for a while after the commit, while A's paused
	// heartbeat catches up.
	time.Sleep(2 * interval)
	close(stop)
	wg.Wait()

	if err != nil {
		t.Fatalf("transaction held for %s: %v", hold, err)
	}
	if len(got) > 0 {
		t.Errorf("runner-b acquired the lock %d times, first at %s", len(got), got[0].Format(time.RFC3339Nano))
	}
	if err := run.Err(); err != nil {
		t.Errorf("run.Err() = %v, want the lock still held", err)
	}
	if doc := readLock(t, ctx, db); doc == nil || doc.Owner != "runner-a" {
		t.Errorf("lock document %+v, want owned by runner-a", doc)
	}
	if n := countDocs(t, ctx, data, bson.D{}); n != 1 {
		t.Errorf("%d data documents, want the committed one", n)
	}
}

func TestNewLock(t *testing.T) {
	db := (&mongo.Client{}).Database("unused")
	cases := []struct {
		interval, ttl time.Duration
		ok            bool
		expiry        time.Duration
	}{
		{interval: time.Second, ttl: time.Second, ok: true, expiry: 3 * time.Second},
		{interval: time.Second, ttl: 4 * time.Second, ok: true, expiry: 4 * time.Second},
		{interval: 10 * time.Second, ttl: DefaultLockTTL, ok: true, expiry: 90 * time.Second},
		{interval: 40 * time.Second, ttl: DefaultLockTTL, ok: true, expiry: 120 * time.Second},
		{interval: 0, ttl: time.Second},
		{interval: 500 * time.Millisecond, ttl: time.Second},
		{interval: 1500 * time.Millisecond, ttl: time.Second},
		{interval: -time.Second, ttl: time.Second},
		{interval: time.Second, ttl: 0},
		{interval: time.Second, ttl: 1500 * time.Millisecond},
		{interval: time.Second, ttl: -time.Minute},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("interval %s ttl %s", tc.interval, tc.ttl), func(t *testing.T) {
			l, err := NewLock(db, "", "runner-a", tc.interval, tc.ttl, nil)
			if (err == nil) != tc.ok {
				t.Fatalf("NewLock(%s, %s) error = %v, want ok %v", tc.interval, tc.ttl, err, tc.ok)
			}
			if err == nil && l.expiry() != tc.expiry {
				t.Errorf("expiry = %s, want %s", l.expiry(), tc.expiry)
			}
		})
	}
	if _, err := NewLock(db, "", "", time.Second, time.Second, nil); err == nil {
		t.Error("NewLock with an empty owner succeeded")
	}
}

func TestLockHeldError(t *testing.T) {
	beat := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		err  *LockHeldError
		want string
	}{
		{
			name: "idle holder",
			err:  &LockHeldError{Owner: "ci-42", LastHeartbeat: beat},
			want: `migration lock is held by runner "ci-42", last heartbeat 2026-11-07T10:00:00Z`,
		},
		{
			name: "holder applying a migration",
			err:  &LockHeldError{Owner: "ci-42", Current: "2026-11-05-001-add-archived-flag", LastHeartbeat: beat},
			want: `migration lock is held by runner "ci-42" (applying 2026-11-05-001-add-archived-flag), last heartbeat 2026-11-07T10:00:00Z`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
			if !errors.Is(tc.err, ErrLockHeld) {
				t.Error("errors.Is(err, ErrLockHeld) = false")
			}
		})
	}
}
