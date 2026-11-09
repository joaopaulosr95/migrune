package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

func TestRunErr(t *testing.T) {
	signal := errors.New("terminated signal received")
	cases := []struct {
		name        string
		cause       error // nil: context still live
		interrupted bool
		lockLost    bool
	}{
		{name: "live"},
		{name: "signal", cause: signal, interrupted: true},
		{name: "plain cancel", cause: context.Canceled, interrupted: true},
		{name: "lock lost", cause: ErrLockLost, lockLost: true},
		{name: "lock lost wrapped", cause: fmt.Errorf("%w: server unreachable", ErrLockLost), lockLost: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			if tc.cause != nil {
				cancel(tc.cause)
			}
			err := (&Run{ctx: ctx}).Err()
			var ie *InterruptedError
			if got := errors.As(err, &ie); got != tc.interrupted {
				t.Errorf("Err() = %v, interrupted %v, want %v", err, got, tc.interrupted)
			}
			if got := errors.Is(err, ErrLockLost); got != tc.lockLost {
				t.Errorf("Err() = %v, lock lost %v, want %v", err, got, tc.lockLost)
			}
			if tc.cause == nil && err != nil {
				t.Errorf("Err() = %v on a live run", err)
			}
			if tc.interrupted && !errors.Is(err, tc.cause) {
				t.Errorf("Err() = %v does not wrap the cause %v", err, tc.cause)
			}
		})
	}
}

func TestCheckNamespace(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{name: "", ok: true},
		{name: "api", ok: true},
		{name: "billing-v2_jobs", ok: true},
		{name: "0", ok: true},
		{name: strings.Repeat("a", 64), ok: true},
		{name: strings.Repeat("a", 65)},
		{name: "Api"},
		{name: "api:jobs"},
		{name: "api jobs"},
		{name: "api.jobs"},
		{name: "ápi"},
		{name: "api\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckNamespace(tc.name)
			if (err == nil) != tc.ok {
				t.Fatalf("CheckNamespace(%q) = %v, want ok %v", tc.name, err, tc.ok)
			}
		})
	}
}

func TestLockID(t *testing.T) {
	for namespace, want := range map[string]string{"": "migrations", "api": "migrations:api", "billing-v2": "migrations:billing-v2"} {
		if got := lockID(namespace); got != want {
			t.Errorf("lockID(%q) = %q, want %q", namespace, got, want)
		}
	}
}

func TestWrittenCollections(t *testing.T) {
	merge := func(source, into string) migration.Step {
		stage, err := bson.Marshal(bson.D{{Key: "$merge", Value: bson.D{{Key: "into", Value: into}}}})
		if err != nil {
			t.Fatal(err)
		}
		return migration.Step{Collection: source, Operation: "aggregate", Pipeline: []bson.Raw{stage}}
	}
	cases := []struct {
		name  string
		steps []migration.Step
		want  []string
	}{
		{name: "data and index", steps: []migration.Step{{Collection: "a", Operation: "updateMany"}, {Collection: "b", Operation: "createIndex"}, {Collection: "a", Operation: "dropIndex"}},
			want: []string{"a", "b"}},
		{name: "merge writes its target", steps: []migration.Step{merge("sources", "items")}, want: []string{"items"}},
		{name: "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := writtenCollections(tc.steps); !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
