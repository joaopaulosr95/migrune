package cmd

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

// errInvalidURI is the usage error for a --uri the driver can't parse. The
// driver's message is left out: it may quote parts of the URI, credentials
// included.
var errInvalidURI = &UsageError{Err: errors.New("--uri is not a valid MongoDB connection string")}

// connect returns the target database, once it has checked that no
// changelog entry or lock document is newer than this migrune understands
// (runner.CheckFormats): every command that connects checks the formats
// before it reads or writes anything else. The
// returned function disconnects, on its own short context so it works after
// an interruption. It logs the database, never the URI.
func (a *app) connect(ctx context.Context, c connFlags) (*mongo.Database, func(), error) {
	client, err := mongo.Connect(options.Client().ApplyURI(c.uri))
	if err != nil {
		return nil, nil, errInvalidURI
	}
	a.log.Info("connecting", "db", c.db)
	disconnect := func() {
		ctx, cancel := context.WithTimeout(context.Background(), runner.CleanupTimeout)
		defer cancel()
		_ = client.Disconnect(ctx)
	}
	db := client.Database(c.db)
	if err := runner.CheckFormats(ctx, db); err != nil {
		disconnect()
		return nil, nil, err
	}
	return db, disconnect, nil
}
