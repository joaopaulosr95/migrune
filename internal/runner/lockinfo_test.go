package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

func TestReadTransactionLifetime(t *testing.T) {
	db := testDB(t)
	ctx := testCtx(t)
	got, err := TransactionLifetime(ctx, db)
	if err != nil || got != 60*time.Second {
		t.Fatalf("TransactionLifetime = %s, %v, want 60s", got, err)
	}
	if DefaultTransactionLifetime != 60*time.Second {
		t.Errorf("DefaultTransactionLifetime = %s, want 60s", DefaultTransactionLifetime)
	}

	admin := db.Client().Database("admin")
	var was struct {
		Was int64 `bson:"was"`
	}
	err = admin.RunCommand(ctx, bson.D{{Key: "setParameter", Value: 1}, {Key: "transactionLifetimeLimitSeconds", Value: 7}}).Decode(&was)
	if err != nil {
		t.Fatalf("set transactionLifetimeLimitSeconds: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := admin.RunCommand(ctx, bson.D{{Key: "setParameter", Value: 1}, {Key: "transactionLifetimeLimitSeconds", Value: was.Was}}).Err()
		if err != nil {
			t.Errorf("restore transactionLifetimeLimitSeconds to %d: %v", was.Was, err)
		}
	})

	got, err = TransactionLifetime(ctx, db)
	if err != nil || got != 7*time.Second {
		t.Fatalf("TransactionLifetime = %s, %v, want 7s", got, err)
	}
}

func TestUnlock(t *testing.T) {
	const threshold = 30 * time.Second
	cases := []struct {
		name          string
		namespace     string
		lockNamespace string
		token         string
		heartbeatAgo  time.Duration
		want          UnlockOutcome
	}{
		{name: "deleted", token: "token-a", heartbeatAgo: 45 * time.Second, want: UnlockDeleted},
		{name: "fresh", token: "token-a", heartbeatAgo: 5 * time.Second, want: UnlockFresh},
		{name: "other namespace", lockNamespace: "billing", token: "token-a", heartbeatAgo: 45 * time.Second, want: UnlockOtherNamespace},
		{name: "default namespace", namespace: "billing", token: "token-a", heartbeatAgo: 45 * time.Second, want: UnlockOtherNamespace},
		{name: "other token", token: "token-b", heartbeatAgo: 45 * time.Second, want: UnlockOtherToken},
		{name: "gone", want: UnlockGone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			ctx := testCtx(t)
			if tc.token != "" {
				_, err := db.Collection(LockCollection).UpdateOne(ctx, bson.D{{Key: "_id", Value: lockID(tc.lockNamespace)}},
					mongo.Pipeline{{{Key: "$set", Value: bson.D{
						{Key: "owner", Value: "holder"},
						{Key: "token", Value: tc.token},
						{Key: "current", Value: "migration-a"},
						{Key: "acquiredAt", Value: "$$NOW"},
						{Key: "lastHeartbeat", Value: bson.D{{Key: "$subtract", Value: bson.A{"$$NOW", tc.heartbeatAgo.Milliseconds()}}}},
						{Key: "expiresAt", Value: bson.D{{Key: "$add", Value: bson.A{"$$NOW", int64(time.Minute / time.Millisecond)}}}},
					}}}}, options.UpdateOne().SetUpsert(true))
				if err != nil {
					t.Fatalf("write lock: %v", err)
				}
			}

			got, err := Unlock(ctx, db, tc.namespace, "token-a", threshold, 10*time.Second)
			if err != nil || got.Outcome != tc.want {
				t.Fatalf("Unlock = %+v, %v, want outcome %v", got, err, tc.want)
			}
			if tc.want == UnlockGone {
				if got.Lock != nil {
					t.Fatalf("Unlock lock = %+v, want nil", got.Lock)
				}
				return
			}
			if got.Lock == nil {
				t.Fatal("Unlock lock = nil, want the holder's lock")
			}
			if got.Lock.ID != lockID(tc.lockNamespace) || got.Lock.Namespace != tc.lockNamespace || got.Lock.Token != tc.token || got.Lock.Owner != "holder" || got.Lock.Current != "migration-a" {
				t.Errorf("Unlock lock = %+v, want namespace %q, token %q, owner holder, current migration-a", got.Lock, tc.lockNamespace, tc.token)
			}
			if got.Lock.AcquiredAt.IsZero() || !got.Lock.UnlockAfter.Equal(got.Lock.LastHeartbeat.Add(threshold)) {
				t.Errorf("Unlock lock times = %+v", got.Lock)
			}
			remaining, err := ReadLock(ctx, db, tc.lockNamespace, threshold)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == UnlockDeleted {
				if remaining != nil {
					t.Fatalf("lock after Unlock = %+v, want nil", remaining)
				}
			} else if remaining == nil || remaining.Token != tc.token {
				t.Fatalf("lock after Unlock = %+v, want token %q", remaining, tc.token)
			}
		})
	}
}

func TestUnlockLiveTransaction(t *testing.T) {
	db := testDB(t)
	ctx := testCtx(t)
	l := newTestLock(t, db, "holder", time.Second, 61*time.Second)
	if err := l.Acquire(ctx, 0); err != nil {
		t.Fatal(err)
	}
	sess := startFencedTransaction(t, ctx, db, l)
	fencedAt := time.Now()
	time.Sleep(2500 * time.Millisecond)
	done := make(chan unlockReply, 1)
	go func() {
		res, err := Unlock(ctx, db, "", l.token, DefaultTransactionLifetime+2*time.Second, 10*time.Second)
		done <- unlockReply{res, err}
	}()
	select {
	case got := <-done:
		if got.err != nil || got.result.Outcome != UnlockFresh || got.result.Lock == nil || got.result.Lock.Token != l.token {
			t.Fatalf("Unlock = %+v, %v, want fresh holder", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Unlock waited behind a live transaction")
	}
	time.Sleep(time.Until(fencedAt.Add(3 * time.Second)))
	if err := sess.CommitTransaction(ctx); err != nil {
		t.Fatal(err)
	}
	if doc := readLock(t, ctx, db); doc == nil || doc.Token != l.token {
		t.Fatalf("lock after commit = %+v, want holder's lock", doc)
	}
}

func TestUnlockWaitsForTransaction(t *testing.T) {
	cases := []struct {
		name      string
		threshold time.Duration
		wait      bool
	}{
		{name: "waits and rechecks", threshold: 2 * time.Second, wait: true},
		{name: "fresh does not wait", threshold: 60 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			ctx := testCtx(t)
			l := newTestLock(t, db, "holder", time.Second, 61*time.Second)
			if err := l.Acquire(ctx, 0); err != nil {
				t.Fatal(err)
			}
			time.Sleep(3 * time.Second)
			sess := startFencedTransaction(t, ctx, db, l)
			fencedAt := time.Now()
			time.Sleep(time.Second)
			done := make(chan unlockReply, 1)
			go func() {
				res, err := Unlock(ctx, db, "", l.token, tc.threshold, 10*time.Second)
				done <- unlockReply{res, err}
			}()

			var got unlockReply
			if tc.wait {
				select {
				case got = <-done:
					t.Fatalf("Unlock returned before commit: %+v, %v", got.result, got.err)
				case <-time.After(time.Until(fencedAt.Add(4 * time.Second))):
				}
			} else {
				select {
				case got = <-done:
				case <-time.After(time.Second):
					t.Fatal("Unlock waited with a fresh committed heartbeat")
				}
				time.Sleep(time.Until(fencedAt.Add(4 * time.Second)))
			}
			if err := sess.CommitTransaction(ctx); err != nil {
				t.Fatal(err)
			}
			if tc.wait {
				select {
				case got = <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("Unlock did not return after commit")
				}
			}
			if got.err != nil || got.result.Outcome != UnlockFresh || got.result.Lock == nil || got.result.Lock.Token != l.token {
				t.Fatalf("Unlock = %+v, %v, want fresh holder", got.result, got.err)
			}
			if doc := readLock(t, ctx, db); doc == nil || doc.Token != l.token {
				t.Fatalf("lock after commit = %+v, want holder's lock", doc)
			}
		})
	}
}

func TestUnlockTimeout(t *testing.T) {
	db := testDB(t)
	ctx := testCtx(t)
	l := newTestLock(t, db, "holder", time.Second, 61*time.Second)
	if err := l.Acquire(ctx, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	sess := startFencedTransaction(t, ctx, db, l)
	fencedAt := time.Now()
	time.Sleep(time.Second)
	done := make(chan unlockReply, 1)
	go func() {
		res, err := Unlock(ctx, db, "", l.token, 2*time.Second, time.Second)
		done <- unlockReply{res, err}
	}()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.DeadlineExceeded) && !mongo.IsTimeout(got.err) {
			t.Fatalf("Unlock = %+v, %v, want timeout", got.result, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Unlock did not respect its 1s timeout")
	}
	time.Sleep(time.Until(fencedAt.Add(4 * time.Second)))
	if err := sess.CommitTransaction(ctx); err != nil {
		t.Fatal(err)
	}
	if doc := readLock(t, ctx, db); doc == nil || doc.Token != l.token {
		t.Fatalf("lock after timeout = %+v, want holder's lock", doc)
	}
}

type unlockReply struct {
	result UnlockResult
	err    error
}

func startFencedTransaction(t *testing.T, ctx context.Context, db *mongo.Database, l *Lock) *mongo.Session {
	t.Helper()
	sess, err := db.Client().StartSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.EndSession(context.Background()) })
	if err := sess.StartTransaction(options.Transaction().
		SetReadConcern(readconcern.Snapshot()).
		SetWriteConcern(writeconcern.Majority())); err != nil {
		t.Fatal(err)
	}
	if err := l.Fence(mongo.NewSessionContext(ctx, sess)); err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestReadLock(t *testing.T) {
	const threshold = 30 * time.Second
	cases := []struct {
		name           string
		id             string
		namespace      string
		heartbeatAgo   time.Duration
		expiresIn      time.Duration
		current        any
		omitAcquiredAt bool
		wantNone       bool
		wantStale      bool
		wantExpired    bool
	}{
		{name: "no document", wantNone: true},
		{name: "fresh", id: "migrations", heartbeatAgo: 5 * time.Second, expiresIn: time.Minute, current: "migration-a"},
		{name: "stale but not expired", id: "migrations", heartbeatAgo: 45 * time.Second, expiresIn: time.Minute, wantStale: true},
		{name: "stale and expired", id: "migrations", heartbeatAgo: 120 * time.Second, expiresIn: -30 * time.Second, wantStale: true, wantExpired: true},
		{name: "billing namespace", id: "migrations:billing", namespace: "billing", heartbeatAgo: 5 * time.Second, expiresIn: time.Minute},
		{name: "billing does not read default", id: "migrations", namespace: "billing", wantNone: true},
		{name: "default does not read billing", id: "migrations:billing", wantNone: true},
		{name: "null current and missing acquiredAt", id: "migrations", heartbeatAgo: 5 * time.Second, expiresIn: time.Minute, omitAcquiredAt: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			ctx := testCtx(t)
			if tc.id != "" {
				fields := bson.D{
					{Key: "owner", Value: "runner-a"},
					{Key: "token", Value: "token-a"},
					{Key: "current", Value: tc.current},
					{Key: "lastHeartbeat", Value: bson.D{{Key: "$subtract", Value: bson.A{"$$NOW", tc.heartbeatAgo.Milliseconds()}}}},
					{Key: "expiresAt", Value: bson.D{{Key: "$add", Value: bson.A{"$$NOW", tc.expiresIn.Milliseconds()}}}},
				}
				if !tc.omitAcquiredAt {
					fields = append(fields, bson.E{Key: "acquiredAt", Value: "$$NOW"})
				}
				_, err := db.Collection(LockCollection).UpdateOne(ctx, bson.D{{Key: "_id", Value: tc.id}},
					mongo.Pipeline{{{Key: "$set", Value: fields}}}, options.UpdateOne().SetUpsert(true))
				if err != nil {
					t.Fatalf("write lock: %v", err)
				}
			}

			got, err := ReadLock(ctx, db, tc.namespace, threshold)
			if err != nil {
				t.Fatalf("ReadLock: %v", err)
			}
			if tc.wantNone {
				if got != nil {
					t.Fatalf("ReadLock = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("ReadLock = nil, want a lock")
			}
			if got.ID != tc.id || got.Namespace != tc.namespace || got.Owner != "runner-a" || got.Token != "token-a" {
				t.Errorf("ReadLock = %+v, want id %q, namespace %q, owner runner-a, token token-a", got, tc.id, tc.namespace)
			}
			wantCurrent, _ := tc.current.(string)
			if got.Current != wantCurrent || got.AcquiredAt.IsZero() != tc.omitAcquiredAt {
				t.Errorf("current = %q, acquiredAt = %s, want current %q, missing acquiredAt %t", got.Current, got.AcquiredAt, wantCurrent, tc.omitAcquiredAt)
			}
			if got.Stale != tc.wantStale || got.Expired != tc.wantExpired {
				t.Errorf("stale = %t, expired = %t, want %t, %t", got.Stale, got.Expired, tc.wantStale, tc.wantExpired)
			}
			if got.HeartbeatAge < tc.heartbeatAgo || got.HeartbeatAge >= tc.heartbeatAgo+2*time.Second {
				t.Errorf("heartbeat age = %s, want [%s, %s)", got.HeartbeatAge, tc.heartbeatAgo, tc.heartbeatAgo+2*time.Second)
			}
			if want := got.LastHeartbeat.Add(threshold); !got.UnlockAfter.Equal(want) {
				t.Errorf("unlockAfter = %s, want %s", got.UnlockAfter, want)
			}
			if got.ExpiresAt.Sub(got.LastHeartbeat) != tc.heartbeatAgo+tc.expiresIn {
				t.Errorf("expiresAt = %s, want %s after heartbeat", got.ExpiresAt, tc.heartbeatAgo+tc.expiresIn)
			}
		})
	}
}
