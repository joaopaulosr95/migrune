package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/joaopaulosr95/migrune/internal/migration"
	"github.com/joaopaulosr95/migrune/internal/runner"
)

func TestStatusUsage(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		env     map[string]string
		message string
	}{
		{name: "missing uri", args: []string{"status", "--db", "app"}, message: "--uri is required"},
		{name: "missing db", args: []string{"status", "--uri", "mongodb://localhost"}, message: "--db is required"},
		{name: "bad output", args: []string{"status", "--uri", "mongodb://localhost", "--db", "app", "-o", "yaml"},
			message: `invalid --output "yaml"`},
		{name: "bad output from env", args: []string{"status", "--uri", "mongodb://localhost", "--db", "app"},
			env: map[string]string{"MIGRUNE_OUTPUT": "yaml"}, message: `invalid --output "yaml"`},
		{name: "invalid namespace", args: []string{"status", "--uri", "mongodb://localhost", "--db", "app", "--namespace", "API"},
			message: `--namespace: invalid namespace "API"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MIGRUNE_URI", "")
			t.Setenv("MIGRUNE_DB", "")
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
	t.Run("dir is optional", func(t *testing.T) {
		t.Setenv("MIGRUNE_DIR", "")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var stdout, stderr bytes.Buffer
		_, err := Execute(ctx, []string{"status", "--uri", "mongodb://127.0.0.1:1", "--db", "app"}, &stdout, &stderr)
		if err == nil || errors.As(err, new(*UsageError)) {
			t.Fatalf("got %v, want a connection failure, not a usage error", err)
		}
		if !strings.Contains(stderr.String(), "connecting") {
			t.Errorf("status without --dir didn't reach connect: %s", stderr.String())
		}
	})
}

func TestStatusInvalidDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, setFlag+".json"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, filepath.Join(dir, "missing")} {
		var stdout, stderr bytes.Buffer
		_, err := Execute(context.Background(), []string{"status", "--uri", "mongodb://127.0.0.1:1", "--db", "app", "--dir", path}, &stdout, &stderr)
		if err == nil || errors.As(err, new(*UsageError)) {
			t.Fatalf("got %v, want a directory load failure", err)
		}
		if stdout.Len() != 0 || strings.Contains(stderr.String(), "connecting") {
			t.Errorf("invalid directory produced output or connected: stdout %q, stderr %q", stdout.String(), stderr.String())
		}
	}
}

func TestStatus(t *testing.T) {
	uri, db := testTarget(t)
	ctx := context.Background()
	dir := migrationsDir(t, setFlag, seed)
	if _, err := migrune(t, uri, db, "apply", "--init", "--no-digest", "--dir", dir); err != nil {
		t.Fatal(err)
	}
	addMigration(t, dir, runnerFixtures("data"), incCounter)
	addMigration(t, dir, runnerFixtures("data"), dropStale)
	raw, err := bson.Marshal(bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	step := 0
	_, err = db.Collection(runner.ChangelogCollection).InsertOne(ctx, runner.Entry{
		ID: bson.NewObjectID(), V: runner.EntryVersion, MigrationID: incCounter, Checksum: "failed-sum",
		CreatedAt: time.Now().UTC(), Runner: "failed-runner", Status: runner.StatusErrored, Migration: raw,
		Errors: []runner.StepError{{Step: &step, Message: "stored document value"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := entries(t, db)
	beforeDB := dumpDB(t, db)
	changedDir := migrationsDir(t, setFlag, incCounter, dropStale)
	path := filepath.Join(changedDir, setFlag+".json")
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(file, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	hasStatus := func(table, id, status string) bool {
		for line := range strings.Lines(table) {
			f := strings.Fields(line)
			if len(f) >= 2 && f[0] == id && f[1] == status {
				return true
			}
		}
		return false
	}
	cases := []struct {
		name         string
		args         []string
		want         map[string]string
		sampleValues bool
	}{
		{name: "with dir", args: []string{"--dir", dir}, want: map[string]string{
			setFlag: "applied", seed: "applied", incCounter: "failed", dropStale: "pending",
		}},
		{name: "without dir", want: map[string]string{setFlag: "applied", seed: "applied", incCounter: "failed"}},
		{name: "empty dir", args: []string{"--dir", t.TempDir()}, want: map[string]string{setFlag: "missing", seed: "missing", incCounter: "failed"}},
		{name: "changed and missing", args: []string{"--dir", changedDir}, want: map[string]string{
			setFlag: "changed", seed: "missing", incCounter: "failed", dropStale: "pending",
		}},
		{name: "sample values", args: []string{"--sample-values"}, sampleValues: true,
			want: map[string]string{setFlag: "applied", seed: "applied", incCounter: "failed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := migrune(t, uri, db, append([]string{"status"}, tc.args...)...)
			if err != nil {
				t.Fatal(err)
			}
			for id, status := range tc.want {
				if !hasStatus(out, id, status) {
					t.Errorf("missing %s %s row:\n%s", id, status, out)
				}
			}
			if len(tc.want) == 3 && strings.Contains(out, dropStale) {
				t.Errorf("pending migration shown without a matching file: %s", out)
			}
			if strings.Contains(out, "stored document value") != tc.sampleValues || strings.Contains(out, "error recorded") == tc.sampleValues {
				t.Errorf("stored error visibility with --sample-values=%t: %s", tc.sampleValues, out)
			}
		})
	}
	if after := entries(t, db); !reflect.DeepEqual(after, before) {
		t.Error("status changed the changelog")
	}
	if after := dumpDB(t, db); after != beforeDB {
		t.Errorf("status changed the database:\nbefore %s\nafter %s", beforeDB, after)
	}
	// Keep the holder's heartbeat from changing the document between snapshots.
	holder, err := runner.Start(ctx, db, runner.Options{RunnerID: "deploy", HeartbeatInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	var lockBefore bson.Raw
	if err := db.Collection(runner.LockCollection).FindOne(ctx, bson.D{}).Decode(&lockBefore); err != nil {
		t.Fatal(err)
	}
	if out, err := migrune(t, uri, db, "status"); err != nil || !strings.Contains(out, `Lock: held by runner "deploy"`) {
		t.Fatalf("status with holder: %v\n%s", err, out)
	}
	var lockAfter bson.Raw
	if err := db.Collection(runner.LockCollection).FindOne(ctx, bson.D{}).Decode(&lockAfter); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lockBefore, lockAfter) {
		t.Error("status changed the held lock")
	}
	heldDB := dumpDB(t, db)
	invalid := t.TempDir()
	if err := os.WriteFile(filepath.Join(invalid, setFlag+".json"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := migrune(t, uri, db, "status", "--dir", invalid); err == nil || errors.As(err, new(*UsageError)) || out != "" {
		t.Errorf("invalid directory: %v, stdout %q", err, out)
	}
	if after := entries(t, db); !reflect.DeepEqual(after, before) {
		t.Error("status with a holder changed the changelog")
	}
	// Both reads, including the lock check, must stay in the selected namespace.
	if out, err := migrune(t, uri, db, "status", "--namespace", "billing", "-o", "json"); err != nil {
		t.Fatal(err)
	} else {
		var rep struct {
			Migrations []migrationState `json:"migrations"`
			Lock       *statusLock      `json:"lock"`
		}
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatal(err)
		}
		if rep.Migrations == nil || len(rep.Migrations) != 0 || rep.Lock != nil {
			t.Errorf("another namespace's state leaked: %s", out)
		}
	}
	if after := dumpDB(t, db); after != heldDB {
		t.Errorf("status changed the database with a holder:\nbefore %s\nafter %s", heldDB, after)
	}
}

func TestStatusEmptyDir(t *testing.T) {
	a := newApp(&bytes.Buffer{}, &bytes.Buffer{})
	finished := map[string]migration.Applied{"a": {ID: "a", Checksum: "sum"}}
	for _, tc := range []struct {
		name, dir, want string
	}{
		{name: "empty dir", dir: t.TempDir(), want: "missing"},
		{name: "without dir", want: "applied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := a.loadStatusFiles(tc.dir)
			if err != nil {
				t.Fatal(err)
			}
			states := migrationStates(files, finished, nil, false)
			if len(states) != 1 || states[0].Status != tc.want {
				t.Errorf("status with --dir %q = %+v, want %s", tc.dir, states, tc.want)
			}
		})
	}
}

func TestWriteStatusTable(t *testing.T) {
	at := time.Date(2026, 11, 5, 10, 12, 0, 0, time.UTC)
	step := 0
	const rows = "MIGRATION  STATUS   APPLIED AT            RUNNER\n" +
		"a          applied  2026-11-05T10:12:00Z  ci-41\n" +
		"b          failed   2026-11-05T10:12:00Z  ci-42\n"
	const hidden = "  step 0: error recorded (rerun with --sample-values to show it)\n"
	const pending = "c          pending\n"
	const fresh = "\nLock: held by runner \"deploy\" (applying c)\n" +
		"  token        token\n" +
		"  acquired     2026-11-05T10:10:02Z\n" +
		"  heartbeat    2026-11-05T10:12:00Z (47s ago; may be inside a transaction)\n" +
		"  unlock after 2026-11-05T10:13:20Z\n" +
		"  expires      2026-11-05T10:13:30Z (in 43s)\n"
	cases := []struct {
		name         string
		noDir        bool
		sampleValues bool
		noLock       bool
		stale        bool
		minimal      bool
		want         string
	}{
		{name: "fresh", want: "Database: app\nNamespace: billing\n\n" + rows + hidden + pending + fresh},
		{name: "without dir", noDir: true, want: "Database: app\nNamespace: billing\n\n" +
			"No --dir: pending, changed and missing migrations aren't shown.\n" + rows + hidden + fresh},
		{name: "sample values", sampleValues: true, want: "Database: app\nNamespace: billing\n\n" +
			rows + "  step 0: document value\n" + pending + fresh},
		{name: "stale and expired", stale: true, want: "Database: app\nNamespace: billing\n\n" + rows + hidden + pending +
			"\nLock: held by runner \"deploy\" (applying c)\n" +
			"  token        token\n" +
			"  acquired     2026-11-05T10:10:02Z\n" +
			"  heartbeat    2026-11-05T10:12:00Z (95s ago; holder stopped: unlock allowed)\n" +
			"  unlock after 2026-11-05T10:13:20Z\n" +
			"  expires      2026-11-05T10:13:23Z (12s ago, expired)\n"},
		{name: "no lock", noLock: true, want: "Database: app\nNamespace: billing\n\n" + rows + hidden + pending + "\nLock: not held\n"},
		{name: "hand-written lock", minimal: true, want: "Database: app\nNamespace: billing\n\n" + rows + hidden + pending +
			"\nLock: held by runner \"deploy\"\n" +
			"  token        token\n" +
			"  acquired     -\n" +
			"  heartbeat    2026-11-05T10:12:00Z (47s ago; may be inside a transaction)\n" +
			"  unlock after 2026-11-05T10:13:20Z\n" +
			"  expires      2026-11-05T10:13:30Z (in 43s)\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := []migration.Migration{{ID: "a", Checksum: "a"}, {ID: "b"}, {ID: "c"}}
			dir := "migrations"
			if tc.noDir {
				files, dir = nil, ""
			}
			rep := statusReport{Database: "app", Namespace: "billing", Dir: dir,
				Migrations: migrationStates(files,
					map[string]migration.Applied{"a": {ID: "a", Checksum: "a", AppliedAt: at, Runner: "ci-41"}},
					map[string]runner.ErroredEntry{"b": {ID: "b", CreatedAt: at, Runner: "ci-42",
						Errors: []runner.StepError{{Step: &step, Message: "document value"}}}}, tc.sampleValues)}
			lock := &runner.LockInfo{Owner: "deploy", Current: "c", Token: "token",
				AcquiredAt: at.Add(-118 * time.Second), LastHeartbeat: at, ExpiresAt: at.Add(90 * time.Second),
				HeartbeatAge: 47400 * time.Millisecond, UnlockAfter: at.Add(80 * time.Second)}
			if tc.stale {
				lock.HeartbeatAge, lock.Stale, lock.Expired = 95*time.Second, true, true
				lock.ExpiresAt = at.Add(83 * time.Second)
			}
			if tc.minimal {
				lock.Current, lock.AcquiredAt = "", time.Time{}
			}
			if tc.noLock {
				lock = nil
			}
			var out bytes.Buffer
			if err := writeStatus(&out, rep, lock, "table"); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tc.want {
				t.Errorf("table:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestWriteStatusTableEscapesCells(t *testing.T) {
	at := time.Date(2026, 11, 5, 10, 12, 0, 0, time.UTC)
	step := 0
	cases := []struct {
		name, id, runner, wantID, wantRunner string
	}{
		{name: "runner newline", id: "a", runner: "deploy\nextra", wantID: "a", wantRunner: `"deploy\nextra"`},
		{name: "runner tab", id: "a", runner: "deploy\textra", wantID: "a", wantRunner: `"deploy\textra"`},
		{name: "id newline", id: "a\nextra", runner: "deploy", wantID: `"a\nextra"`, wantRunner: "deploy"},
		{name: "id tab", id: "a\textra", runner: "deploy", wantID: `"a\textra"`, wantRunner: "deploy"},
		{name: "printable unicode", id: "migração", runner: "déploy", wantID: "migração", wantRunner: "déploy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := statusReport{Database: "app", Dir: "migrations", Migrations: []migrationState{
				{ID: tc.id, Status: "failed", FailedAt: &at, Runner: tc.runner,
					errors: []runner.StepError{{Step: &step, Message: "document value"}}},
				{ID: "b", Status: "applied", AppliedAt: &at, Runner: "ci-42"},
			}}
			var out bytes.Buffer
			if err := writeStatus(&out, rep, nil, "table"); err != nil {
				t.Fatal(err)
			}
			table := strings.Split(out.String(), "\n\n")[1]
			var lines []string
			for line := range strings.Lines(table) {
				lines = append(lines, strings.Join(strings.Fields(line), " "))
			}
			want := []string{
				"MIGRATION STATUS APPLIED AT RUNNER",
				tc.wantID + " failed 2026-11-05T10:12:00Z " + tc.wantRunner,
				"step 0: document value",
				"b applied 2026-11-05T10:12:00Z ci-42",
			}
			if !slices.Equal(lines, want) {
				t.Errorf("table lines %q, want %q", lines, want)
			}
		})
	}
}

func TestWriteStatusTableEscapesLock(t *testing.T) {
	lock := &runner.LockInfo{Owner: "deploy\nextra", Current: "a\nextra", Token: "token\textra"}
	var out bytes.Buffer
	if err := writeStatus(&out, statusReport{Database: "app", Dir: "migrations"}, lock, "table"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`Lock: held by runner "deploy\nextra" (applying "a\nextra")` + "\n",
		`  token        "token\textra"` + "\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("table missing %q:\n%s", want, out.String())
		}
	}
}

func TestWriteStatusJSON(t *testing.T) {
	at := time.Date(2026, 11, 5, 10, 12, 0, 0, time.FixedZone("local", -3*60*60))
	step := 0
	for _, sampleValues := range []bool{false, true} {
		for _, held := range []bool{false, true} {
			var lock *runner.LockInfo
			if held {
				lock = &runner.LockInfo{Owner: "deploy", Current: "a", Token: "token", AcquiredAt: at,
					LastHeartbeat: at, ExpiresAt: at.Add(90 * time.Second), HeartbeatAge: 47 * time.Second,
					UnlockAfter: at.Add(80 * time.Second)}
			}
			rep := statusReport{Version: "test", Database: "app", Namespace: "billing", Dir: "migrations",
				Migrations: migrationStates(nil, nil, map[string]runner.ErroredEntry{"a": {
					ID: "a", Checksum: "sum", CreatedAt: at, Runner: "deploy",
					Errors: []runner.StepError{{Step: &step, Message: "document value"}},
				}}, sampleValues)}
			var out bytes.Buffer
			if err := writeStatus(&out, rep, lock, "json"); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if keys := sortedKeys(got); !slices.Equal(keys, []string{"database", "dir", "lock", "migrations", "namespace", "version"}) {
				t.Errorf("report keys %v", keys)
			}
			if got["version"] != "test" || got["database"] != "app" || got["namespace"] != "billing" || got["dir"] != "migrations" {
				t.Errorf("report %v", got)
			}
			row := got["migrations"].([]any)[0].(map[string]any)
			keys := []string{"checksum", "failedAt", "failedStep", "id", "runner", "status"}
			if sampleValues {
				keys = append(keys, "error")
				slices.Sort(keys)
				if row["error"] != "document value" {
					t.Errorf("error %v", row["error"])
				}
			}
			if !slices.Equal(sortedKeys(row), keys) || row["failedStep"] != float64(0) || row["status"] != "failed" {
				t.Errorf("migration %v, want keys %v", row, keys)
			}
			if row["failedAt"] != "2026-11-05T13:12:00Z" {
				t.Errorf("failedAt %v, want UTC", row["failedAt"])
			}
			if !held {
				if got["lock"] != nil {
					t.Errorf("lock %v, want null", got["lock"])
				}
				continue
			}
			l := got["lock"].(map[string]any)
			if keys := sortedKeys(l); !slices.Equal(keys, []string{"acquiredAt", "current", "expired", "expiresAt", "heartbeatAgeMs", "lastHeartbeat", "owner", "token", "unlockAfter"}) {
				t.Errorf("lock keys %v", keys)
			}
			if l["heartbeatAgeMs"] != float64(47000) || l["owner"] != "deploy" || l["current"] != "a" || l["token"] != "token" || l["expired"] != false {
				t.Errorf("lock %v", l)
			}
			if l["acquiredAt"] != "2026-11-05T13:12:00Z" || l["lastHeartbeat"] != "2026-11-05T13:12:00Z" ||
				l["expiresAt"] != "2026-11-05T13:13:30Z" || l["unlockAfter"] != "2026-11-05T13:13:20Z" {
				t.Errorf("lock times %v, want UTC", l)
			}
		}
	}
	t.Run("without dir or migrations", func(t *testing.T) {
		var out bytes.Buffer
		rep := statusReport{Version: "test", Database: "app", Migrations: migrationStates(nil, nil, nil, false)}
		if err := writeStatus(&out, rep, nil, "json"); err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if keys := sortedKeys(got); !slices.Equal(keys, []string{"database", "lock", "migrations", "namespace", "version"}) {
			t.Errorf("report keys %v", keys)
		}
		if rows, ok := got["migrations"].([]any); !ok || len(rows) != 0 || got["namespace"] != "" || got["lock"] != nil {
			t.Errorf("empty status %v, want an empty array, default namespace and null lock", got)
		}
	})
}

func TestMigrationStates(t *testing.T) {
	at := time.Date(2026, 1, 4, 10, 12, 0, 0, time.UTC)
	later := at.Add(time.Hour)
	step0, step1 := 0, 1
	errors := []runner.StepError{
		{Message: "document value in first message"},
		{Step: &step0, Message: "step zero failure"},
		{Step: &step1, Message: "step one failure"},
	}
	hidden := []runner.StepError{{}, {Step: &step0}, {Step: &step1}}
	finished := map[string]migration.Applied{
		"a": {ID: "a", Checksum: "applied", AppliedAt: at, Runner: "runner-a"},
	}
	errored := map[string]runner.ErroredEntry{
		"a": {ID: "a", Checksum: "retry", CreatedAt: later, Runner: "runner-b", Errors: errors},
	}
	failed := migrationState{ID: "a", Status: "failed", Checksum: "retry", FailedAt: &later,
		Runner: "runner-b", FailedStep: &step0, errors: hidden}
	cases := []struct {
		name         string
		files        []migration.Migration
		finished     map[string]migration.Applied
		errored      map[string]runner.ErroredEntry
		sampleValues bool
		want         []migrationState
	}{
		{
			name: "applied", files: []migration.Migration{{ID: "a", Checksum: "applied"}}, finished: finished,
			want: []migrationState{{ID: "a", Status: "applied", Checksum: "applied", AppliedAt: &at, Runner: "runner-a"}},
		},
		{
			name: "changed", files: []migration.Migration{{ID: "a", Checksum: "edited"}}, finished: finished,
			want: []migrationState{{ID: "a", Status: "changed", Checksum: "applied", AppliedAt: &at, Runner: "runner-a"}},
		},
		{
			name: "missing", files: []migration.Migration{}, finished: finished,
			want: []migrationState{{ID: "a", Status: "missing", Checksum: "applied", AppliedAt: &at, Runner: "runner-a"}},
		},
		{
			name: "pending", files: []migration.Migration{{ID: "a", Checksum: "new"}},
			want: []migrationState{{ID: "a", Status: "pending", Checksum: "new"}},
		},
		{
			name: "failed instead of pending", files: []migration.Migration{{ID: "a", Checksum: "edited"}}, errored: errored,
			want: []migrationState{failed},
		},
		{
			name: "failed mutable rerun", files: []migration.Migration{{ID: "a", Checksum: "edited"}}, finished: finished, errored: errored,
			want: []migrationState{failed},
		},
		{
			name: "failed despite missing file", files: []migration.Migration{}, finished: finished, errored: errored,
			want: []migrationState{failed},
		},
		{
			name: "finished after error", files: []migration.Migration{{ID: "a", Checksum: "applied"}},
			finished: map[string]migration.Applied{"a": {ID: "a", Checksum: "applied", AppliedAt: later, Runner: "runner-a"}},
			errored:  map[string]runner.ErroredEntry{"a": {ID: "a", Checksum: "retry", CreatedAt: at, Runner: "runner-b", Errors: errors}},
			want:     []migrationState{{ID: "a", Status: "applied", Checksum: "applied", AppliedAt: &later, Runner: "runner-a"}},
		},
		{
			name: "equal timestamps keep finished", files: []migration.Migration{{ID: "a", Checksum: "applied"}}, finished: finished,
			errored: map[string]runner.ErroredEntry{"a": {ID: "a", CreatedAt: at, Errors: errors}},
			want:    []migrationState{{ID: "a", Status: "applied", Checksum: "applied", AppliedAt: &at, Runner: "runner-a"}},
		},
		{
			name: "without dir only applied and failed", errored: errored,
			finished: map[string]migration.Applied{
				"a": {ID: "a", Checksum: "applied", AppliedAt: at, Runner: "runner-a"},
				"b": {ID: "b", Checksum: "no-file", AppliedAt: at, Runner: "runner-c"},
			},
			want: []migrationState{failed, {ID: "b", Status: "applied", Checksum: "no-file", AppliedAt: &at, Runner: "runner-c"}},
		},
		{
			name: "sample values include first message and first step", errored: errored, sampleValues: true,
			want: []migrationState{{ID: "a", Status: "failed", Checksum: "retry", FailedAt: &later, Runner: "runner-b",
				FailedStep: &step0, Error: "document value in first message", errors: errors}},
		},
		{
			name:    "failed without step errors",
			errored: map[string]runner.ErroredEntry{"a": {ID: "a", Checksum: "retry", CreatedAt: later, Runner: "runner-b"}},
			want:    []migrationState{{ID: "a", Status: "failed", Checksum: "retry", FailedAt: &later, Runner: "runner-b"}},
		},
		{
			name: "sorted union", files: []migration.Migration{{ID: "c", Checksum: "c"}, {ID: "b", Checksum: "b"}},
			finished: map[string]migration.Applied{"d": {ID: "d", Checksum: "d", AppliedAt: at}}, errored: errored,
			want: []migrationState{failed, {ID: "b", Status: "pending", Checksum: "b"}, {ID: "c", Status: "pending", Checksum: "c"},
				{ID: "d", Status: "missing", Checksum: "d", AppliedAt: &at}},
		},
		{name: "empty", want: []migrationState{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := migrationStates(tc.files, tc.finished, tc.errored, tc.sampleValues)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("migrationStates = %+v, want %+v", got, tc.want)
			}
		})
	}
}
