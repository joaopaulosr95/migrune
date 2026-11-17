package runner

import (
	"context"
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
	doc, now := docs[0].Lock, docs[0].Now
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
	return info, nil
}
