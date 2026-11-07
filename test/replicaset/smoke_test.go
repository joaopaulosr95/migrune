// Package replicaset holds the smoke test for the integration test replica
// set described by docker-compose.yaml in this directory.
package replicaset

import (
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

func TestReplicaSetSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: skipped with -short")
	}
	uri := os.Getenv("MIGRUNE_TEST_URI")
	if uri == "" {
		t.Skip("integration test: MIGRUNE_TEST_URI is not set; run `task test:int`")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })

	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		t.Fatalf("ping primary: %v", err)
	}

	db := client.Database("migrune_test_"+rand.Text()[:12], options.Database().
		SetWriteConcern(writeconcern.Majority()))
	t.Cleanup(func() { _ = db.Drop(context.Background()) })

	res, err := db.Collection("smoke").InsertOne(ctx, bson.D{{Key: "ok", Value: true}})
	if err != nil {
		t.Fatalf("insert with w: majority: %v", err)
	}
	if res.InsertedID == nil {
		t.Fatal("insert returned no id")
	}
}
