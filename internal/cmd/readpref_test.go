package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

// lagTag is the replica set member tag TestLaggingSecondary gives the
// secondary it pauses, so readPreferenceTags in the URI makes a
// secondary read land on it every time.
var lagTag = bson.D{{Key: "migrune-test", Value: "lagging"}}

// TestLaggingSecondary runs a data migration that upserts a document, then a
// merge migration that reads it, with readPreference=secondary in the URI
// and the secondary it names paused. Reads go to the primary anyway: the
// merge sees the new document, and the ignored readPreference is logged.
//
// It reconfigures the test replica set and stops a secondary's replication,
// both server-wide: test packages must not run in parallel (task test:int
// runs them one at a time; inttest fails them otherwise). Both are undone
// when the test ends, failed or not.
func TestLaggingSecondary(t *testing.T) {
	uri, db := testTarget(t)
	ctx := context.Background()
	client := db.Client()

	host := pickSecondary(t, client)
	setMemberTags(t, client, host, lagTag)
	if t.Failed() {
		t.FailNow()
	}
	t.Cleanup(func() { setMemberTags(t, client, host, nil) })
	member, err := mongo.Connect(options.Client().SetHosts([]string{host}).SetDirect(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = member.Disconnect(ctx)
	})
	stopSync := func(mode string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return member.Database("admin").RunCommand(ctx, bson.D{
			{Key: "configureFailPoint", Value: "rsSyncApplyStop"}, {Key: "mode", Value: mode},
		}).Err()
	}
	// Registered after the tags' cleanup, so it runs before it: the member
	// syncs again before the replica set is reconfigured back.
	t.Cleanup(func() {
		if err := stopSync("off"); err != nil {
			t.Errorf("turn rsSyncApplyStop off on %s: %v", host, err)
		}
	})
	if err := stopSync("alwaysOn"); err != nil {
		t.Fatalf("turn rsSyncApplyStop on on %s: %v", host, err)
	}

	lagURI := withQuery(uri, "readPreference=secondary&readPreferenceTags=migrune-test:lagging")
	t.Setenv("MIGRUNE_URI", lagURI)
	t.Setenv("MIGRUNE_DB", db.Name())
	var out, logs bytes.Buffer
	_, err = Execute(ctx, []string{"apply", "--no-digest", "--init", "--dir", "testdata/primary"}, &out, &logs)
	t.Logf("stdout:\n%s\nstderr:\n%s", out.String(), logs.String())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	const warning = `msg="migrune reads from the primary; the URI's readPreference secondary is ignored"`
	if n := strings.Count(logs.String(), warning); n != 1 {
		t.Errorf("logged the warning %d times, want once", n)
	}
	if strings.Contains(logs.String(), "migrune-test:lagging") || strings.Contains(out.String(), "migrune-test:lagging") {
		t.Error("the output quotes the URI")
	}
	var account struct {
		DisplayName string `bson:"displayName"`
	}
	if err := db.Collection("accounts").FindOne(ctx, bson.D{{Key: "_id", Value: "a1"}}).Decode(&account); err != nil {
		t.Fatal(err)
	}
	if account.DisplayName != "Ada" {
		t.Errorf("displayName = %q, want %q: the merge read a lagging secondary", account.DisplayName, "Ada")
	}
	for _, e := range entries(t, db) {
		if e.Status != runner.StatusFinished {
			t.Errorf("%s is %s, want Finished", e.MigrationID, e.Status)
		}
	}
}

// pickSecondary returns the host of a secondary of the test replica set.
func pickSecondary(t *testing.T, client *mongo.Client) string {
	t.Helper()
	var status struct {
		Members []struct {
			Name     string `bson:"name"`
			StateStr string `bson:"stateStr"`
		} `bson:"members"`
	}
	if err := client.Database("admin").RunCommand(context.Background(), bson.D{{Key: "replSetGetStatus", Value: 1}}).Decode(&status); err != nil {
		t.Fatal(err)
	}
	for _, m := range status.Members {
		if m.StateStr == "SECONDARY" {
			return m.Name
		}
	}
	t.Fatal("the test replica set has no secondary")
	return ""
}

// setMemberTags sets the tags of the member host, nil for none, with a
// replica set reconfig. It reports a failure with t.Errorf, so it can run in
// a cleanup.
func setMemberTags(t *testing.T, client *mongo.Client, host string, tags bson.D) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin := client.Database("admin")
	var res struct {
		Config bson.D `bson:"config"`
	}
	if err := admin.RunCommand(ctx, bson.D{{Key: "replSetGetConfig", Value: 1}}).Decode(&res); err != nil {
		t.Errorf("read the replica set config: %v", err)
		return
	}
	cfg := res.Config
	if tags == nil {
		tags = bson.D{}
	}
	for i, e := range cfg {
		switch e.Key {
		case "version":
			switch v := e.Value.(type) {
			case int32:
				cfg[i].Value = v + 1
			case int64:
				cfg[i].Value = v + 1
			}
		case "members":
			members := e.Value.(bson.A)
			for k, m := range members {
				if m := m.(bson.D); lookup(m, "host") == host {
					members[k] = setField(m, "tags", tags)
				}
			}
		}
	}
	if err := admin.RunCommand(ctx, bson.D{{Key: "replSetReconfig", Value: cfg}}).Err(); err != nil {
		t.Errorf("tag %s %v: %v", host, tags, err)
	}
}

// lookup returns the value of d's field key, nil without one.
func lookup(d bson.D, key string) any {
	for _, e := range d {
		if e.Key == key {
			return e.Value
		}
	}
	return nil
}

// setField sets d's field key to value, adding it if d has none.
func setField(d bson.D, key string, value any) bson.D {
	for i := range d {
		if d[i].Key == key {
			d[i].Value = value
			return d
		}
	}
	return append(d, bson.E{Key: key, Value: value})
}

// withQuery adds the query options to uri.
func withQuery(uri, query string) string {
	switch {
	case strings.Contains(uri, "?"):
		return uri + "&" + query
	case strings.Contains(strings.TrimPrefix(uri, "mongodb://"), "/"):
		return uri + "?" + query
	default:
		return uri + "/?" + query
	}
}
