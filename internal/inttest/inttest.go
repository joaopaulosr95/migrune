// Package inttest keeps integration test packages from sharing the test
// replica set. Some integration tests change server-wide state: failCommand
// and other fail points, a secondary's replication, the replica set config,
// a primary step-down. When go test runs packages in parallel, as it does
// without -p 1, one package's change breaks another's tests. Each test
// process registers in a dedicated database, and a process that finds
// another one registered fails its integration tests at once, saying to run
// with -p 1, as task test:int does.
package inttest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

const (
	// Database holds one document per test process using the replica set.
	Database   = "migrune_inttest"
	collection = "processes"
	// ttl is how long a process's document lives without a heartbeat, so
	// a process that was killed holds no one up for longer.
	ttl       = 10 * time.Second
	heartbeat = 2 * time.Second
)

var (
	once sync.Once
	// claimErr is the outcome of this process's registration.
	claimErr error
	// release stops the heartbeat and removes this process's document.
	release func()
)

// Exclusive fails t unless this process is the only test process using the
// replica set at uri. The first call registers the process, waiting up to
// the registration TTL for another process's document to go, in case that
// process was killed; later calls return the same outcome at once. Call it
// before an integration test touches the server, and Release from TestMain.
func Exclusive(t testing.TB, uri string) {
	t.Helper()
	once.Do(func() { claimErr = claim(uri) })
	if claimErr != nil {
		t.Fatal(claimErr)
	}
}

// Release removes this process's registration, if it made one. Call it from
// TestMain once the tests have run, so the next package needn't wait for the
// registration to expire.
func Release() {
	if release != nil {
		release()
	}
}

// processDoc is a test process's registration.
type processDoc struct {
	ID        string    `bson:"_id"`
	Host      string    `bson:"host"`
	PID       int       `bson:"pid"`
	ExpiresAt time.Time `bson:"expiresAt"`
}

func claim(uri string) error {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return fmt.Errorf("inttest: connect: %w", err)
	}
	coll := client.Database(Database).Collection(collection, options.Collection().
		SetWriteConcern(writeconcern.Majority()).
		SetReadConcern(readconcern.Majority()))
	host, _ := os.Hostname()
	id := rand.Text()
	// expiresAt uses the server's clock, like the migration lock.
	refresh := func(ctx context.Context) error {
		_, err := coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}}, mongo.Pipeline{{{Key: "$set", Value: bson.D{
			{Key: "host", Value: host},
			{Key: "pid", Value: os.Getpid()},
			{Key: "expiresAt", Value: bson.D{{Key: "$add", Value: bson.A{"$$NOW", ttl.Milliseconds()}}}},
		}}}}, options.UpdateOne().SetUpsert(true))
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := refresh(ctx); err != nil {
		_ = client.Disconnect(ctx)
		return fmt.Errorf("inttest: register this test process: %w", err)
	}

	// Heartbeats keep the registration alive until Release. A failed one,
	// say during a step-down, is retried at the next.
	hctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(heartbeat)
		defer tick.Stop()
		for {
			select {
			case <-hctx.Done():
				return
			case <-tick.C:
				rctx, rcancel := context.WithTimeout(hctx, heartbeat)
				_ = refresh(rctx)
				rcancel()
			}
		}
	}()
	release = func() {
		stop()
		<-done
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = coll.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}})
		_ = client.Disconnect(ctx)
	}

	// Another live process: wait for its registration to go, as a killed
	// process's does once it expires.
	others := bson.D{
		{Key: "_id", Value: bson.D{{Key: "$ne", Value: id}}},
		{Key: "$expr", Value: bson.D{{Key: "$gt", Value: bson.A{"$expiresAt", "$$NOW"}}}},
	}
	deadline := time.Now().Add(ttl + heartbeat)
	for {
		var other processDoc
		err := coll.FindOne(ctx, others).Decode(&other)
		switch {
		case errors.Is(err, mongo.ErrNoDocuments):
			return nil
		case err != nil:
			return fmt.Errorf("inttest: look for other test processes: %w", err)
		case time.Now().After(deadline):
			return fmt.Errorf("another test process (pid %d on %s) is using the test replica set. "+
				"Integration test packages must run one at a time, with go test -p 1, as task test:int does: "+
				"some tests change server-wide state (fail points, replication, the replica set config, a step-down)",
				other.PID, other.Host)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
