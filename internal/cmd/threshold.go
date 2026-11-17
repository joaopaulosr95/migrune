package cmd

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

func (a *app) unlockThreshold(ctx context.Context, db *mongo.Database) (threshold, lifetime time.Duration) {
	lifetime, err := runner.TransactionLifetime(ctx, db)
	if err != nil {
		lifetime = runner.DefaultTransactionLifetime
		a.log.Warn("can't read the server's transactionLifetimeLimitSeconds; assuming 60s, the default: if it was raised, unlock may remove the lock of a runner inside a transaction",
			"error", runner.RedactError(err, false))
	}
	return lifetime + 2*runner.DefaultHeartbeatInterval, lifetime
}
