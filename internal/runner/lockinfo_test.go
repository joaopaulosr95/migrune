package runner

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
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
