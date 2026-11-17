package cmd

import (
	"context"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

// unlockThreshold logs the fallback at level: warn for unlock, which acts on
// it, info for status, where it only labels the heartbeat.
func (a *app) unlockThreshold(ctx context.Context, db *mongo.Database, level slog.Level) (threshold, lifetime time.Duration) {
	lifetime, err := runner.TransactionLifetime(ctx, db)
	if err != nil {
		lifetime = runner.DefaultTransactionLifetime
		a.log.Log(ctx, level, "can't read the server's transactionLifetimeLimitSeconds; assuming 60s, the default: if it was raised, unlock may remove the lock of a runner inside a transaction",
			"error", runner.RedactError(err, false))
	}
	return silenceThreshold(lifetime), lifetime
}

// silenceThreshold is the server's transaction lifetime plus two heartbeat
// intervals. The lifetime counts as at least the default: the holder's local
// lease is sized for it, so a lowered server limit can't let unlock in first.
func silenceThreshold(lifetime time.Duration) time.Duration {
	return max(lifetime, runner.DefaultTransactionLifetime) + 2*runner.DefaultHeartbeatInterval
}
