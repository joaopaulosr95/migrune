package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/inttest"
	"github.com/joaopaulosr95/migrune/internal/runner"
)

// dropStale's first step is a deleteMany, the first delete command the run
// sends: the fail point blocks it. markChecked runs after it.
const (
	dropStale   = "2026-11-01-004-drop-stale"
	markChecked = "2026-11-01-006-mark-checked"
)

// TestSIGTERMDuringDataMigration sends SIGTERM to a migrune process while a
// data migration's transaction is open. The transaction is aborted,
// the migration stays pending, the next one is skipped, the lock is
// released, the report says interrupted, plan writes no digest and the exit
// code is 143.
func TestSIGTERMDuringDataMigration(t *testing.T) {
	bin, uri, client := sigtermSetup(t)

	for _, command := range []string{"plan", "apply"} {
		t.Run(command, func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t, client)
			if _, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "stale", Value: true}}); err != nil {
				t.Fatal(err)
			}
			dir := fixtureDir(t, "data", dropStale, markChecked)

			// Block this process's first delete for 3s, well under the
			// 5s cleanup budget: the abort waits for it at most that long.
			app := "migrune-sigterm-" + rand.Text()[:8]
			fired := failPoint(t, ctx, client, bson.D{{Key: "times", Value: 1}}, bson.D{
				{Key: "failCommands", Value: bson.A{"delete"}},
				{Key: "blockConnection", Value: true},
				{Key: "blockTimeMS", Value: 3000},
				{Key: "appName", Value: app},
			})
			t.Cleanup(func() { failPoint(t, context.Background(), client, "off", nil) })

			digest := filepath.Join(t.TempDir(), "plan.digest")
			args := []string{command, "--dir", dir, "-o", "json"}
			if command == "apply" {
				args = append(args, "--no-digest", "--init")
			}
			if command == "plan" {
				args = append(args, "--digest-out", digest)
			}
			var stdout, stderr bytes.Buffer
			cmd := exec.Command(bin, args...)
			cmd.Env = append(os.Environ(), "MIGRUNE_URI="+withAppName(uri, app), "MIGRUNE_DB="+db.Name())
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill() }()

			// The delete is held: the transaction is open.
			waitFailPoint(t, ctx, client, fired+1, &stderr)
			start := time.Now()
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			err := cmd.Wait()
			t.Logf("stderr:\n%s", stderr.String())
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 143 {
				t.Fatalf("exit: %v, want status 143", err)
			}
			if d := time.Since(start); d > runner.CleanupTimeout+2*time.Second {
				t.Errorf("shutdown took %s", d)
			}

			var rep runner.Report
			if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
				t.Fatalf("report: %v\n%s", err, stdout.String())
			}
			if len(rep.Migrations) != 2 || rep.Migrations[0].ID != dropStale || rep.Migrations[0].Status != runner.RowInterrupted ||
				rep.Migrations[1].ID != markChecked || rep.Migrations[1].Status != runner.RowSkipped ||
				rep.Totals.Interrupted != 1 || rep.Totals.Skipped != 1 {
				t.Errorf("report %+v, want the migration interrupted and the next one skipped", rep)
			}
			if !strings.Contains(stderr.String(), "migration interrupted") {
				t.Error("no interruption in the logs")
			}

			if n := count(t, ctx, db.Collection("items"), bson.D{{Key: "stale", Value: true}}); n != 1 {
				t.Errorf("%d stale items, want 1: the transaction wasn't aborted", n)
			}
			if n := count(t, ctx, db.Collection("items"), bson.D{{Key: "checked", Value: true}}); n != 0 {
				t.Errorf("%d items checked, want none: the next migration ran", n)
			}
			if n := count(t, ctx, db.Collection(runner.ChangelogCollection), bson.D{}); n != 0 {
				t.Errorf("%d changelog entries, want none: the migration must stay pending", n)
			}
			if n := count(t, ctx, db.Collection(runner.LockCollection), bson.D{}); n != 0 {
				t.Errorf("lock not released")
			}
			if _, err := os.Stat(digest); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("interrupted plan wrote a digest: %v", err)
			}
		})
	}
}

// TestSIGTERMDuringCommit sends SIGTERM to a migrune apply while the server
// holds a data migration's commit (T2). The commit finishes, so the report
// says applied, the Finished entry is there, the lock is released and the
// exit code is still 143.
func TestSIGTERMDuringCommit(t *testing.T) {
	bin, uri, client := sigtermSetup(t)
	ctx := testCtx(t)
	db := testDB(t, client)
	if _, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "stale", Value: true}}); err != nil {
		t.Fatal(err)
	}
	dir := fixtureDir(t, "data", dropStale)

	// Hold this process's commit for 3s; it then runs.
	app := "migrune-commit-" + rand.Text()[:8]
	fired := failPoint(t, ctx, client, bson.D{{Key: "times", Value: 1}}, bson.D{
		{Key: "failCommands", Value: bson.A{"commitTransaction"}},
		{Key: "blockConnection", Value: true},
		{Key: "blockTimeMS", Value: 3000},
		{Key: "appName", Value: app},
	})
	t.Cleanup(func() { failPoint(t, context.Background(), client, "off", nil) })

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "apply", "--no-digest", "--init", "--dir", dir, "-o", "json")
	cmd.Env = append(os.Environ(), "MIGRUNE_URI="+withAppName(uri, app), "MIGRUNE_DB="+db.Name())
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	waitFailPoint(t, ctx, client, fired+1, &stderr)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	t.Logf("stderr:\n%s", stderr.String())
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 143 {
		t.Fatalf("exit: %v, want status 143", err)
	}

	var rep runner.Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("report: %v\n%s", err, stdout.String())
	}
	if len(rep.Migrations) != 1 || rep.Migrations[0].Status != runner.RowApplied || rep.Totals.Applied != 1 || rep.Totals.Interrupted != 0 {
		t.Errorf("report %+v, want the migration applied", rep)
	}
	if strings.Contains(stderr.String(), "migration interrupted") {
		t.Error("the logs claim an interruption")
	}
	if n := count(t, ctx, db.Collection("items"), bson.D{{Key: "stale", Value: true}}); n != 0 {
		t.Errorf("%d stale items, want none: the commit didn't land", n)
	}
	if n := count(t, ctx, db.Collection(runner.ChangelogCollection), bson.D{{Key: "status", Value: "Finished"}}); n != 1 {
		t.Errorf("%d Finished entries, want 1", n)
	}
	if n := count(t, ctx, db.Collection(runner.LockCollection), bson.D{}); n != 0 {
		t.Errorf("lock not released")
	}
}

// nameIndex creates the index name_1 on items.
const nameIndex = "2026-11-02-001-name-index"

// TestSIGTERMDuringIndexBuild sends SIGTERM to a migrune apply while the
// server builds an index. The run exits without waiting for the
// build, reports the migration interrupted, writes no changelog entry and
// releases the lock; the next run succeeds.
func TestSIGTERMDuringIndexBuild(t *testing.T) {
	bin, uri, client := sigtermSetup(t)
	ctx := testCtx(t)
	db := testDB(t, client)

	// The primary's build of an index on items hangs on this document
	// until the fail point is turned off; builds elsewhere aren't affected.
	marker := rand.Text()
	if _, err := db.Collection("items").InsertOne(ctx, bson.D{{Key: "name", Value: "a"}, {Key: "marker", Value: marker}}); err != nil {
		t.Fatal(err)
	}
	const fp = "hangIndexBuildDuringCollectionScanPhaseBeforeInsertion"
	configure := func(mode string, data bson.D) {
		cmd := bson.D{{Key: "configureFailPoint", Value: fp}, {Key: "mode", Value: mode}}
		if data != nil {
			cmd = append(cmd, bson.E{Key: "data", Value: data})
		}
		if err := client.Database("admin").RunCommand(context.Background(), cmd).Err(); err != nil {
			t.Fatalf("configure fail point: %v", err)
		}
	}
	configure("alwaysOn", bson.D{{Key: "fieldsToMatch", Value: bson.D{{Key: "marker", Value: marker}}}})
	t.Cleanup(func() { configure("off", nil) })

	dir := fixtureDir(t, "index", nameIndex)
	apply := func() (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
		var stdout, stderr bytes.Buffer
		cmd := exec.Command(bin, "apply", "--no-digest", "--init", "--dir", dir, "-o", "json")
		cmd.Env = append(os.Environ(), "MIGRUNE_URI="+uri, "MIGRUNE_DB="+db.Name())
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, &stdout, &stderr
	}

	cmd, stdout, stderr := apply()
	deadline := time.Now().Add(30 * time.Second)
	for !indexBuildRunning(t, ctx, client, db.Name(), "items") {
		if time.Now().After(deadline) {
			t.Fatalf("index build never started\nstderr:\n%s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	t.Logf("stderr:\n%s", stderr.String())
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 143 {
		t.Fatalf("exit: %v, want status 143", err)
	}
	if d := time.Since(start); d > runner.CleanupTimeout {
		t.Errorf("shutdown took %s: it waited for the build", d)
	}
	if !indexBuildRunning(t, ctx, client, db.Name(), "items") {
		t.Error("the index build is gone: the runner must leave it to the server")
	}

	var rep runner.Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("report: %v\n%s", err, stdout.String())
	}
	if len(rep.Migrations) != 1 || rep.Migrations[0].Kind != "index" || rep.Migrations[0].Status != runner.RowInterrupted {
		t.Errorf("report %+v, want the index migration interrupted", rep)
	}
	if !strings.Contains(stderr.String(), "migration interrupted") {
		t.Error("no interruption in the logs")
	}
	if n := count(t, ctx, db.Collection(runner.ChangelogCollection), bson.D{}); n != 0 {
		t.Errorf("%d changelog entries, want none: the migration must stay pending", n)
	}
	if n := count(t, ctx, db.Collection(runner.LockCollection), bson.D{}); n != 0 {
		t.Errorf("lock not released")
	}

	// The server finishes the build; the next run's identical createIndex
	// waits for it or does nothing, and records the migration.
	configure("off", nil)
	cmd, stdout, stderr = apply()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("next apply: %v\nstderr:\n%s", err, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil || len(rep.Migrations) != 1 || rep.Migrations[0].Status != runner.RowApplied {
		t.Errorf("next apply report %+v, %v", rep, err)
	}
	if n := count(t, ctx, db.Collection(runner.ChangelogCollection), bson.D{{Key: "migrationId", Value: nameIndex}, {Key: "status", Value: "Finished"}}); n != 1 {
		t.Errorf("%d Finished entries, want 1", n)
	}
	n, err := db.Collection("items").Indexes().ListSpecifications(ctx)
	if err != nil || !slices.ContainsFunc(n, func(s mongo.IndexSpecification) bool { return s.Name == "name_1" }) {
		t.Errorf("indexes %+v, %v: want name_1", n, err)
	}
}

// copyLabel is a merge migration that copies each item's name into label.
const copyLabel = "2026-11-03-001-copy-label"

// TestSIGTERMDuringMerge sends SIGTERM to a migrune apply while the server
// runs a merge migration's $merge. The run cancels the aggregate,
// reports the migration interrupted, writes no changelog entry and releases
// the lock; the next run settles the merge.
func TestSIGTERMDuringMerge(t *testing.T) {
	bin, uri, client := sigtermSetup(t)
	ctx := testCtx(t)
	db := testDB(t, client)
	items := db.Collection("items")
	if _, err := items.InsertMany(ctx, []any{
		bson.D{{Key: "_id", Value: 1}, {Key: "name", Value: "a"}},
		bson.D{{Key: "_id", Value: 2}, {Key: "name", Value: "b"}},
		bson.D{{Key: "_id", Value: 3}, {Key: "name", Value: "c"}},
	}); err != nil {
		t.Fatal(err)
	}

	// The primary's $merge into this test's items hangs before writing,
	// until the fail point is turned off; merges into other namespaces
	// aren't affected. A hung $merge doesn't see interruptions until then.
	const fp = "hangWhileBuildingDocumentSourceMergeBatch"
	configure := func(mode string, data bson.D) {
		cmd := bson.D{{Key: "configureFailPoint", Value: fp}, {Key: "mode", Value: mode}}
		if data != nil {
			cmd = append(cmd, bson.E{Key: "data", Value: data})
		}
		if err := client.Database("admin").RunCommand(context.Background(), cmd).Err(); err != nil {
			t.Fatalf("configure fail point: %v", err)
		}
	}
	configure("alwaysOn", bson.D{{Key: "nss", Value: db.Name() + ".items"}})
	t.Cleanup(func() { configure("off", nil) })

	dir := fixtureDir(t, "merge", copyLabel)
	apply := func() (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
		var stdout, stderr bytes.Buffer
		cmd := exec.Command(bin, "apply", "--no-digest", "--init", "--dir", dir, "-o", "json")
		cmd.Env = append(os.Environ(), "MIGRUNE_URI="+uri, "MIGRUNE_DB="+db.Name())
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, &stdout, &stderr
	}

	cmd, stdout, stderr := apply()
	deadline := time.Now().Add(30 * time.Second)
	for !mergeHanging(t, ctx, client, db.Name()) {
		if time.Now().After(deadline) {
			t.Fatalf("merge never started\nstderr:\n%s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	t.Logf("stderr:\n%s", stderr.String())
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 143 {
		t.Fatalf("exit: %v, want status 143", err)
	}
	if d := time.Since(start); d > runner.CleanupTimeout+2*time.Second {
		t.Errorf("shutdown took %s", d)
	}

	var rep runner.Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("report: %v\n%s", err, stdout.String())
	}
	if len(rep.Migrations) != 1 || rep.Migrations[0].Kind != "merge" || rep.Migrations[0].Status != runner.RowInterrupted {
		t.Errorf("report %+v, want the merge migration interrupted", rep)
	}
	for _, message := range []string{"aggregate in progress cancelled", "merge migration interrupted", "fresh digest", "firstApply for --init"} {
		if !strings.Contains(stderr.String(), message) {
			t.Errorf("missing %q in interruption logs", message)
		}
	}
	if n := count(t, ctx, db.Collection(runner.ChangelogCollection), bson.D{}); n != 0 {
		t.Errorf("%d changelog entries, want none: the migration must stay pending", n)
	}
	if n := count(t, ctx, db.Collection(runner.LockCollection), bson.D{}); n != 0 {
		t.Errorf("lock not released")
	}

	// Released, the cancelled $merge stops without writing.
	configure("off", nil)
	deadline = time.Now().Add(30 * time.Second)
	for mergeRunning(t, ctx, client, db.Name()) {
		if time.Now().After(deadline) {
			t.Fatal("the cancelled merge is still running")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := count(t, ctx, items, bson.D{{Key: "label", Value: bson.D{{Key: "$exists", Value: true}}}}); n != 0 {
		t.Errorf("%d items labelled, want none: the aggregate wasn't cancelled", n)
	}

	// The next run settles the merge.
	cmd, stdout, stderr = apply()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("next apply: %v\nstderr:\n%s", err, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil || len(rep.Migrations) != 1 ||
		rep.Migrations[0].Status != runner.RowApplied || rep.Migrations[0].DiffCountAfter == nil || *rep.Migrations[0].DiffCountAfter != 0 {
		t.Errorf("next apply report %+v, %v", rep, err)
	}
	if n := count(t, ctx, items, bson.D{{Key: "$expr", Value: bson.D{{Key: "$eq", Value: bson.A{"$label", "$name"}}}}}); n != 3 {
		t.Errorf("%d items labelled, want 3", n)
	}
	if n := count(t, ctx, db.Collection(runner.ChangelogCollection), bson.D{{Key: "migrationId", Value: copyLabel}, {Key: "status", Value: "Finished"}}); n != 1 {
		t.Errorf("%d Finished entries, want 1", n)
	}
}

// conflict creates createdAt_-1, then name_1 on other keys than an
// existing name_1: its second step fails.
const conflict = "2026-11-02-004-conflict"

// TestSignalAfterRun sends SIGTERM to migrune once its run is over, while
// it is blocked writing the report to a full stdout pipe. The signal doesn't
// change the outcome: the exit code is 0 after a run that succeeded, and
// plan still writes its digest, or the failure's code after a run that
// failed. Nothing is reported interrupted.
func TestSignalAfterRun(t *testing.T) {
	bin, uri, client := sigtermSetup(t)
	cases := []struct {
		name, command string
		sub, id       string // the runner fixture testdata/<sub>/<id>.json
		conflict      bool   // create name_1 on {name: 1} first: the migration fails
		want          int
		status        runner.RowStatus
	}{
		{name: "plan succeeded", command: "plan", sub: "data", id: dropStale, status: runner.RowApplied},
		{name: "apply succeeded", command: "apply", sub: "data", id: dropStale, status: runner.RowApplied},
		{name: "plan failed", command: "plan", sub: "index", id: conflict, conflict: true, want: 1, status: runner.RowErrored},
		{name: "apply failed", command: "apply", sub: "index", id: conflict, conflict: true, want: 1, status: runner.RowErrored},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testCtx(t)
			db := testDB(t, client)
			items := db.Collection("items")
			if _, err := items.InsertOne(ctx, bson.D{{Key: "_id", Value: 1}, {Key: "name", Value: "a"}, {Key: "stale", Value: true}}); err != nil {
				t.Fatal(err)
			}
			if tc.conflict {
				if _, err := items.Indexes().CreateOne(ctx, mongo.IndexModel{
					Keys: bson.D{{Key: "name", Value: 1}}, Options: options.Index().SetName("name_1"),
				}); err != nil {
					t.Fatal(err)
				}
			}
			dir := fixtureDir(t, tc.sub, tc.id)
			digest := filepath.Join(t.TempDir(), "plan.digest")
			args := []string{tc.command, "--dir", dir, "-o", "json"}
			if tc.command == "apply" {
				args = append(args, "--no-digest", "--init")
			}
			if tc.command == "plan" {
				args = append(args, "--digest-out", digest)
			}

			stdout, w := fullPipe(t)
			var stderr syncBuffer
			cmd := exec.Command(bin, args...)
			cmd.Env = append(os.Environ(), "MIGRUNE_URI="+uri, "MIGRUNE_DB="+db.Name())
			cmd.Stdout, cmd.Stderr = w, &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill() }()
			_ = w.Close()

			// The run is over once the lock is released; the report's
			// write then blocks on the full pipe.
			deadline := time.Now().Add(30 * time.Second)
			for !strings.Contains(stderr.String(), "migration lock released") {
				if time.Now().After(deadline) {
					t.Fatalf("the lock was never released\nstderr:\n%s", stderr.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			time.Sleep(500 * time.Millisecond)
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatalf("signal: %v: migrune exited before writing its report", err)
			}
			out, rerr := io.ReadAll(stdout)
			err := cmd.Wait()
			t.Logf("stderr:\n%s", stderr.String())
			if rerr != nil {
				t.Fatal(rerr)
			}
			var exit *exec.ExitError
			switch {
			case tc.want == 0 && err != nil:
				t.Errorf("exit: %v, want status 0", err)
			case tc.want != 0 && (!errors.As(err, &exit) || exit.ExitCode() != tc.want):
				t.Errorf("exit: %v, want status %d", err, tc.want)
			}

			// The report follows the spaces that filled the pipe.
			var rep runner.Report
			if err := json.Unmarshal(out, &rep); err != nil {
				t.Fatalf("report: %v\n%s", err, bytes.TrimSpace(out))
			}
			if len(rep.Migrations) != 1 || rep.Migrations[0].Status != tc.status || rep.Totals.Interrupted != 0 {
				t.Errorf("report %+v, want the migration %s", rep, tc.status)
			}
			if strings.Contains(stderr.String(), "migration interrupted") {
				t.Error("the logs claim an interruption")
			}
			if tc.command == "plan" {
				_, err := os.Stat(digest)
				if written := err == nil; written != (tc.want == 0) {
					t.Errorf("digest written: %v (%v), want %v", written, err, tc.want == 0)
				}
			}
		})
	}
}

// fullPipe returns a pipe whose buffer is full of spaces, so the first
// write to w blocks until r is read. r is closed when the test ends.
func fullPipe(t *testing.T) (r, w *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	raw, err := w.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var ferr error
	err = raw.Control(func(fd uintptr) {
		// The pipe is non-blocking: write until it is full, in big
		// chunks then byte by byte, and make it blocking again for the
		// child.
		for _, size := range []int{4096, 1} {
			chunk := bytes.Repeat([]byte(" "), size)
			for {
				_, err := syscall.Write(int(fd), chunk)
				if errors.Is(err, syscall.EAGAIN) {
					break
				}
				if err != nil {
					ferr = err
					return
				}
			}
		}
		ferr = syscall.SetNonblock(int(fd), false)
	})
	if err = cmp.Or(err, ferr); err != nil {
		t.Fatalf("fill the pipe: %v", err)
	}
	return r, w
}

// syncBuffer is a bytes.Buffer a process can write to while the test reads
// it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var (
	buildOnce sync.Once
	// binDir holds the migrune binary the tests build; TestMain removes
	// it.
	binDir   string
	buildErr error
)

func TestMain(m *testing.M) {
	code := m.Run()
	inttest.Release()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// sigtermSetup skips an integration test without a replica set. Otherwise
// it returns the migrune binary, built once for all the tests, the replica
// set's URI and a client connected to it.
func sigtermSetup(t *testing.T) (bin, uri string, client *mongo.Client) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: skipped with -short")
	}
	uri = os.Getenv("MIGRUNE_TEST_URI")
	if uri == "" {
		t.Skip("integration test: MIGRUNE_TEST_URI is not set; run `task test:int`")
	}
	inttest.Exclusive(t, uri)
	bin = buildMigrune(t)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	return bin, uri, client
}

// buildMigrune returns the migrune binary, built once for all the tests.
func buildMigrune(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		if binDir, buildErr = os.MkdirTemp("", "migrune-test-"); buildErr != nil {
			return
		}
		if out, err := exec.Command("go", "build", "-o", filepath.Join(binDir, "migrune"), ".").CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("%w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("build migrune: %v", buildErr)
	}
	return filepath.Join(binDir, "migrune")
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// testDB returns a fresh database, dropped when the test ends.
func testDB(t *testing.T, client *mongo.Client) *mongo.Database {
	db := client.Database("migrune_test_" + rand.Text()[:12])
	t.Cleanup(func() { _ = db.Drop(context.Background()) })
	return db
}

// fixtureDir copies the runner's fixtures testdata/<sub>/<id>.json into a
// new directory.
func fixtureDir(t *testing.T, sub string, ids ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, id := range ids {
		raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "runner", "testdata", sub, id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// mergeHanging reports whether an aggregate on db.items is held by the merge
// fail point.
func mergeHanging(t *testing.T, ctx context.Context, client *mongo.Client, db string) bool {
	t.Helper()
	return currentOps(t, ctx, client, bson.D{
		{Key: "command.aggregate", Value: "items"}, {Key: "command.$db", Value: db},
		{Key: "failpointMsg", Value: "hangWhileBuildingDocumentSourceMergeBatch"},
	})
}

// mergeRunning reports whether an aggregate on db.items is in progress.
func mergeRunning(t *testing.T, ctx context.Context, client *mongo.Client, db string) bool {
	t.Helper()
	return currentOps(t, ctx, client, bson.D{{Key: "command.aggregate", Value: "items"}, {Key: "command.$db", Value: db}})
}

// currentOps reports whether an operation in progress matches filter.
func currentOps(t *testing.T, ctx context.Context, client *mongo.Client, filter bson.D) bool {
	t.Helper()
	cur, err := client.Database("admin").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$currentOp", Value: bson.D{}}},
		{{Key: "$match", Value: filter}},
	})
	if err != nil {
		t.Fatalf("currentOp: %v", err)
	}
	defer func() { _ = cur.Close(ctx) }()
	return cur.Next(ctx)
}

// indexBuildRunning reports whether a createIndexes on db.coll is in
// progress.
func indexBuildRunning(t *testing.T, ctx context.Context, client *mongo.Client, db, coll string) bool {
	t.Helper()
	cur, err := client.Database("admin").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$currentOp", Value: bson.D{}}},
		{{Key: "$match", Value: bson.D{{Key: "command.createIndexes", Value: coll}, {Key: "command.$db", Value: db}}}},
	})
	if err != nil {
		t.Fatalf("currentOp: %v", err)
	}
	defer func() { _ = cur.Close(ctx) }()
	return cur.Next(ctx)
}

// failPoint configures the failCommand fail point on the primary, and returns
// how many times it had fired before. It needs the server's test commands,
// which test/replicaset enables.
func failPoint(t *testing.T, ctx context.Context, client *mongo.Client, mode any, data bson.D) (fired int64) {
	t.Helper()
	cmd := bson.D{{Key: "configureFailPoint", Value: "failCommand"}, {Key: "mode", Value: mode}}
	if data != nil {
		cmd = append(cmd, bson.E{Key: "data", Value: data})
	}
	var res struct {
		Count int64 `bson:"count"`
	}
	if err := client.Database("admin").RunCommand(ctx, cmd).Decode(&res); err != nil {
		t.Fatalf("configure fail point: %v", err)
	}
	return res.Count
}

// waitFailPoint waits until the failCommand fail point has fired n times in
// total: the command it holds was sent.
func waitFailPoint(t *testing.T, ctx context.Context, client *mongo.Client, n int64, stderr *bytes.Buffer) {
	t.Helper()
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := client.Database("admin").RunCommand(wctx, bson.D{
		{Key: "waitForFailPoint", Value: "failCommand"}, {Key: "timesEntered", Value: n},
	}).Err()
	if err != nil {
		t.Fatalf("the fail point was never hit: %v\nstderr:\n%s", err, stderr.String())
	}
}

func withAppName(uri, app string) string {
	switch {
	case strings.Contains(uri, "?"):
		return uri + "&appName=" + app
	case strings.HasSuffix(uri, "/"):
		return uri + "?appName=" + app
	default:
		return uri + "/?appName=" + app
	}
}

func count(t *testing.T, ctx context.Context, coll *mongo.Collection, filter any) int64 {
	t.Helper()
	n, err := coll.CountDocuments(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
