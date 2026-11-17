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
	return lifetime + 2*runner.DefaultHeartbeatInterval, lifetime
}
