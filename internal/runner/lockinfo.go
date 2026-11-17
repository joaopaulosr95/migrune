package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// DefaultTransactionLifetime is MongoDB's default transactionLifetimeLimitSeconds.
const DefaultTransactionLifetime = 60 * time.Second

// TransactionLifetime reads the server's transactionLifetimeLimitSeconds.
func TransactionLifetime(ctx context.Context, db *mongo.Database) (time.Duration, error) {
	var res struct {
		Seconds int64 `bson:"transactionLifetimeLimitSeconds"`
	}
	err := db.Client().Database("admin").RunCommand(ctx, bson.D{
		{Key: "getParameter", Value: 1},
		{Key: "transactionLifetimeLimitSeconds", Value: 1},
	}).Decode(&res)
	if err != nil {
		return 0, fmt.Errorf("read transactionLifetimeLimitSeconds: %w", err)
	}
	return time.Duration(res.Seconds) * time.Second, nil
}

// LockInfo is a namespace's lock, with ages and expiry from the server's clock.
type LockInfo struct {
	ID            string
	Namespace     string // "" for the default namespace
	Owner         string
	Current       string // "" when current is null or absent
	Token         string
	AcquiredAt    time.Time
	LastHeartbeat time.Time
	ExpiresAt     time.Time
	HeartbeatAge  time.Duration
	Expired       bool
	UnlockAfter   time.Time
	Stale         bool // heartbeat age is greater than the threshold
}

// ReadLock returns namespace's lock, or nil when there is none. It only reads.
func ReadLock(ctx context.Context, db *mongo.Database, namespace string, threshold time.Duration) (*LockInfo, error) {
	cur, err := lockCollection(db).Aggregate(ctx, mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "_id", Value: lockID(namespace)}}}},
		{{Key: "$addFields", Value: bson.D{{Key: "now", Value: "$$NOW"}}}},
	})
	if err != nil {
		return nil, fmt.Errorf("read migration lock: %w", err)
	}
	var docs []struct {
		Lock lockDoc   `bson:",inline"`
		Now  time.Time `bson:"now"`
	}
	if err := cur.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("read migration lock: %w", err)
	}
	if len(docs) == 0 {
		return nil, nil
	}
	return lockInfo(docs[0].Lock, docs[0].Now, threshold), nil
}

func lockInfo(doc lockDoc, now time.Time, threshold time.Duration) *LockInfo {
	info := &LockInfo{
		ID:            doc.ID,
		Owner:         doc.Owner,
		Token:         doc.Token,
		AcquiredAt:    doc.AcquiredAt,
		LastHeartbeat: doc.LastHeartbeat,
		ExpiresAt:     doc.ExpiresAt,
		HeartbeatAge:  now.Sub(doc.LastHeartbeat),
		Expired:       doc.ExpiresAt.Before(now),
		UnlockAfter:   doc.LastHeartbeat.Add(threshold),
	}
	if namespace, ok := strings.CutPrefix(doc.ID, defaultLockID+":"); ok {
		info.Namespace = namespace
	}
	if doc.Current != nil {
		info.Current = *doc.Current
	}
	info.Stale = info.HeartbeatAge > threshold
	return info
}

type UnlockOutcome int

const (
	UnlockDeleted        UnlockOutcome = iota // the lock was removed
	UnlockFresh                               // this namespace's lock has the token
	UnlockOtherNamespace                      // the token is another namespace's lock
	UnlockOtherToken                          // this namespace's lock has another token
	UnlockGone                                // no lock has the token, and the namespace has none
)

type UnlockResult struct {
	Outcome UnlockOutcome
	// Lock is the deleted document (Deleted), this namespace's lock (Fresh,
	// OtherToken), the token's lock (OtherNamespace), or nil (Gone). Only the
	// deleted document's HeartbeatAge and Expired use the local clock.
	Lock *LockInfo
}

// Unlock deletes namespace's lock if it has token and its heartbeat is older
// than threshold, by the server's clock. The delete waits at most timeout
// behind an open transaction.
func Unlock(ctx context.Context, db *mongo.Database, namespace, token string, threshold, timeout time.Duration) (UnlockResult, error) {
	coll := lockCollection(db)
	deleteCtx, cancel := context.WithTimeout(ctx, timeout)
	var doc lockDoc
	err := coll.FindOneAndDelete(deleteCtx, bson.D{
		{Key: "_id", Value: lockID(namespace)},
		{Key: "token", Value: token},
		{Key: "$expr", Value: bson.D{{Key: "$lt", Value: bson.A{
			"$lastHeartbeat",
			bson.D{{Key: "$subtract", Value: bson.A{"$$NOW", threshold.Milliseconds()}}},
		}}}},
	}).Decode(&doc)
	cancel()
	if err == nil {
		return UnlockResult{Outcome: UnlockDeleted, Lock: lockInfo(doc, time.Now(), threshold)}, nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return UnlockResult{}, fmt.Errorf("unlock migration lock: %w", err)
	}

	info, err := ReadLock(ctx, db, namespace, threshold)
	if err != nil {
		return UnlockResult{}, err
	}
	if info != nil && info.Token == token {
		return UnlockResult{Outcome: UnlockFresh, Lock: info}, nil
	}
	err = coll.FindOne(ctx, bson.D{{Key: "token", Value: token}}).Decode(&doc)
	if err == nil {
		otherNamespace := ""
		if ns, ok := strings.CutPrefix(doc.ID, defaultLockID+":"); ok {
			otherNamespace = ns
		}
		other, err := ReadLock(ctx, db, otherNamespace, threshold)
		if err != nil {
			return UnlockResult{}, err
		}
		if other != nil && other.Token == token {
			return UnlockResult{Outcome: UnlockOtherNamespace, Lock: other}, nil
		}
	} else if !errors.Is(err, mongo.ErrNoDocuments) {
		return UnlockResult{}, fmt.Errorf("find migration lock by token: %w", err)
	}
	if info != nil {
		return UnlockResult{Outcome: UnlockOtherToken, Lock: info}, nil
	}
	return UnlockResult{Outcome: UnlockGone}, nil
}
