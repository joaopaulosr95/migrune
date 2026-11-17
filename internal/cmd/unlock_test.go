package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

func TestUnlockUsage(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		env     map[string]string
		message string
	}{
		{name: "missing token", args: []string{"unlock", "--uri", "mongodb://localhost", "--db", "app"}, message: "--token is required"},
		{name: "missing uri", args: []string{"unlock", "--db", "app", "--token", "inspected"}, message: "--uri is required"},
		{name: "missing db", args: []string{"unlock", "--uri", "mongodb://localhost", "--token", "inspected"}, message: "--db is required"},
		{name: "invalid namespace", args: []string{"unlock", "--uri", "mongodb://localhost", "--db", "app", "--token", "inspected", "--namespace", "API"},
			message: `--namespace: invalid namespace "API"`},
		{name: "token from env", args: []string{"unlock", "--uri", "invalid", "--db", "app"},
			env: map[string]string{"MIGRUNE_TOKEN": "inspected"}, message: "--uri is not a valid MongoDB connection string"},
		{name: "token flag over env", args: []string{"unlock", "--uri", "mongodb://localhost", "--db", "app", "--token", ""},
			env: map[string]string{"MIGRUNE_TOKEN": "inspected"}, message: "--token is required"},
		{name: "no dir flag", args: []string{"unlock", "--dir", "unused"}, message: "unknown flag: --dir"},
		{name: "no force flag", args: []string{"unlock", "--force"}, message: "unknown flag: --force"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MIGRUNE_URI", "")
			t.Setenv("MIGRUNE_DB", "")
			t.Setenv("MIGRUNE_NAMESPACE", "")
			t.Setenv("MIGRUNE_TOKEN", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var stdout, stderr bytes.Buffer
			_, err := Execute(context.Background(), tc.args, &stdout, &stderr)
			if !errors.As(err, new(*UsageError)) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("got %v, want a usage error containing %q", err, tc.message)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout %q, want none", stdout.String())
			}
		})
	}
}

func TestUnlock(t *testing.T) {
	cases := []struct {
		name         string
		id           string
		token        string
		namespace    string
		heartbeatAgo time.Duration
		held         bool
		failed       bool
		message      string
		logs         []string
		remaining    int64
	}{
		{name: "deleted", id: "migrations", token: "inspected", heartbeatAgo: 2 * time.Minute,
			logs: []string{`level=WARN msg="migration lock removed"`, "runner=holder", "token=inspected", "current=", "heartbeatAge=", "expired=false"}},
		{name: "fresh", id: "migrations", token: "inspected", held: true, failed: true,
			message: "may be inside a transaction; retry after ", remaining: 1},
		{name: "other namespace", id: "migrations:billing", token: "inspected", heartbeatAgo: 2 * time.Minute, failed: true,
			message: `rerun with --namespace "billing"`, remaining: 1},
		{name: "default namespace", id: "migrations", token: "inspected", namespace: "billing", heartbeatAgo: 2 * time.Minute, failed: true,
			message: `rerun with --namespace ""`, remaining: 1},
		{name: "other token", id: "migrations", token: "new-acquisition", heartbeatAgo: 2 * time.Minute, held: true, failed: true,
			message: "run status again", remaining: 1},
		{name: "gone", logs: []string{"no lock has this token"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uri, db := testTarget(t)
			if tc.id != "" {
				putLock(t, db, tc.id, tc.token, tc.heartbeatAgo, 10*time.Minute)
			}
			t.Setenv("MIGRUNE_URI", uri)
			t.Setenv("MIGRUNE_DB", db.Name())
			t.Setenv("MIGRUNE_NAMESPACE", "")
			args := []string{"unlock", "--token", "inspected"}
			if tc.namespace != "" {
				args = append(args, "--namespace", tc.namespace)
			}
			var stdout, stderr bytes.Buffer
			_, err := Execute(context.Background(), args, &stdout, &stderr)
			if (err != nil) != tc.failed || errors.Is(err, runner.ErrLockHeld) != tc.held || errors.As(err, new(*UsageError)) {
				t.Fatalf("unlock: %v, want failure %v, lock held %v, no usage error", err, tc.failed, tc.held)
			}
			if tc.message != "" && !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error %q doesn't contain %q", err, tc.message)
			}
			for _, want := range tc.logs {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr %q doesn't contain %q", stderr.String(), want)
				}
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout %q, want none", stdout.String())
			}
			n, err := db.Collection(runner.LockCollection).CountDocuments(context.Background(), bson.D{})
			if err != nil {
				t.Fatal(err)
			}
			if n != tc.remaining {
				t.Errorf("%d locks remain, want %d", n, tc.remaining)
			}
			if n := len(entries(t, db)); n != 0 {
				t.Errorf("%d changelog entries, want none", n)
			}
		})
	}
}

func TestUnlockThenApply(t *testing.T) {
	uri, db := testTarget(t)
	putLock(t, db, "migrations", "inspected", 2*time.Minute, 10*time.Minute)
	dir := migrationsDir(t, setFlag)
	args := []string{"apply", "--no-digest", "--init", "--dir", dir}
	if _, err := migrune(t, uri, db, args...); !errors.Is(err, runner.ErrLockHeld) {
		t.Fatalf("apply: %v, want lock held", err)
	}
	if _, err := migrune(t, uri, db, "unlock", "--token", "inspected"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	out, err := migrune(t, uri, db, args...)
	if err != nil || !hasRow(out, setFlag, "applied") {
		t.Fatalf("apply: %v\n%s", err, out)
	}
}

func TestUnlockThresholdFallback(t *testing.T) {
	uri, db := testTarget(t)
	ctx := context.Background()
	admin := db.Client().Database("admin")
	err := admin.RunCommand(ctx, bson.D{
		{Key: "configureFailPoint", Value: "failCommand"},
		{Key: "mode", Value: bson.D{{Key: "times", Value: 1}}},
		{Key: "data", Value: bson.D{
			{Key: "failCommands", Value: bson.A{"getParameter"}},
			{Key: "appName", Value: "migrune-fallback"},
			{Key: "errorCode", Value: 13},
		}},
	}).Err()
	if err != nil {
		t.Fatalf("configure fail point: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := admin.RunCommand(ctx, bson.D{
			{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: "off"},
		}).Err(); err != nil {
			t.Errorf("turn fail point off: %v", err)
		}
	})
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("appName", "migrune-fallback")
	u.RawQuery = query.Encode()
	t.Setenv("MIGRUNE_URI", u.String())
	t.Setenv("MIGRUNE_DB", db.Name())
	var stdout, stderr bytes.Buffer
	_, err = Execute(ctx, []string{"unlock", "--token", "inspected"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}
	for _, want := range []string{"transactionLifetimeLimitSeconds", "assuming 60s", "if it was raised", "no lock has this token"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q doesn't contain %q", stderr.String(), want)
		}
	}
	if n := strings.Count(stderr.String(), "assuming 60s"); n != 1 {
		t.Errorf("%d fallback warnings, want 1", n)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout %q, want none", stdout.String())
	}
}

func putLock(t *testing.T, db *mongo.Database, id, token string, heartbeatAgo, expiresIn time.Duration) {
	t.Helper()
	_, err := db.Collection(runner.LockCollection).UpdateOne(context.Background(), bson.D{{Key: "_id", Value: id}},
		mongo.Pipeline{{{Key: "$set", Value: bson.D{
			{Key: "v", Value: runner.LockVersion},
			{Key: "owner", Value: "holder"},
			{Key: "token", Value: token},
			{Key: "current", Value: setFlag},
			{Key: "acquiredAt", Value: bson.D{{Key: "$subtract", Value: bson.A{"$$NOW", heartbeatAgo.Milliseconds()}}}},
			{Key: "lastHeartbeat", Value: bson.D{{Key: "$subtract", Value: bson.A{"$$NOW", heartbeatAgo.Milliseconds()}}}},
			{Key: "expiresAt", Value: bson.D{{Key: "$add", Value: bson.A{"$$NOW", expiresIn.Milliseconds()}}}},
		}}}}, options.UpdateOne().SetUpsert(true))
	if err != nil {
		t.Fatalf("put lock: %v", err)
	}
}
