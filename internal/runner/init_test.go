package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// hasEntries reports whether the changelog of db has an entry for namespace.
func hasEntries(t *testing.T, ctx context.Context, db *mongo.Database, namespace string) bool {
	t.Helper()
	has, _, err := namespaceState(ctx, db, namespace)
	if err != nil {
		t.Fatal(err)
	}
	return has
}

// TestInit checks a namespace's first apply needs Config.Init, and that
// Init is refused once the namespace has entries, Errored ones included,
// both before anything is written.
func TestInit(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	ms := load(t, fixtureDir(t, "data", setFlag))
	apply := func(namespace string, init bool, digest *DigestInput) (*Report, error) {
		t.Helper()
		return Execute(ctx, db, ms, Options{Namespace: namespace, Logger: testLogger(t)}, Config{Mode: ModeApply, Init: init, Digest: digest})
	}
	collections := func() []string {
		t.Helper()
		names, err := db.ListCollectionNames(ctx, bson.D{})
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(names)
		return names
	}

	// An empty database: refused without --init, before any write.
	rep, err := apply("", false, nil)
	var ie *InitError
	if !errors.As(err, &ie) || ie.Init || rep != nil {
		t.Fatalf("apply on an empty database: %v, report %v, want an InitError", err, rep)
	}
	want := "the changelog has no entries for the default namespace on database " + db.Name() +
		", so apply would run every migration; check --db and --namespace, and pass --init if this is the first apply"
	if err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}
	if got := collections(); len(got) != 0 {
		t.Fatalf("collections %v after the refused apply, want none", got)
	}

	// plan reports the first apply, in its report, its log and its digest.
	var logs bytes.Buffer
	planned, err := Execute(ctx, db, ms, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))}, Config{Mode: ModePlan})
	if err != nil {
		t.Fatal(err)
	}
	if !planned.FirstApply || !planned.DigestInput.FirstApply || !strings.Contains(planned.DigestInput.Text(), "\ndeployment "+planned.DigestInput.Deployment+"\nfirst-apply\n") ||
		!deploymentPattern.MatchString(planned.DigestInput.Deployment) {
		t.Errorf("plan: firstApply %v, digest:\n%s", planned.FirstApply, planned.DigestInput.Text())
	}
	if !strings.Contains(logs.String(), `level=WARN msg="first apply for the default namespace on database `+db.Name()+": ") {
		t.Errorf("plan logs:\n%s\nwant the first apply warning", logs.String())
	}

	// With --init and the plan's digest, it applies.
	rep, err = apply("", true, planned.DigestInput)
	if err != nil || !rep.FirstApply || !slices.Equal(statuses(rep), []string{setFlag + ":applied"}) {
		t.Fatalf("apply --init: %v, report %+v", err, rep)
	}

	// --init left in place: refused on the next apply.
	_, err = apply("", true, nil)
	want = "the changelog has entries for the default namespace on database " + db.Name() +
		": --init is only for a namespace's first apply; remove it"
	if !errors.As(err, &ie) || !ie.Init || err.Error() != want {
		t.Fatalf("second apply --init: %v, want %q", err, want)
	}
	if rep, err := apply("", false, nil); err != nil || rep.FirstApply || len(rep.Migrations) != 0 {
		t.Fatalf("second apply: %v, report %+v", err, rep)
	}
	if planned, err := execute(t, ctx, db, ms, Options{}, Config{Mode: ModePlan}); err != nil || planned.FirstApply ||
		strings.Contains(planned.DigestInput.Text(), "first-apply") {
		t.Fatalf("plan after the first apply: %v, report %+v", err, planned)
	}

	// A namespace with Errored entries only has entries.
	failing := load(t, fixtureDir(t, "data", incCounter))
	seedItems(t, ctx, db)
	failed := func(init bool, logger *slog.Logger) error {
		t.Helper()
		_, err := Execute(ctx, db, failing, Options{Namespace: "api", Logger: logger}, Config{Mode: ModeApply, Init: init})
		return err
	}
	var me *MigrationError
	logs.Reset()
	if err := failed(true, slog.New(slog.NewTextHandler(&logs, nil))); !errors.As(err, &me) {
		t.Fatalf("apply --init of a failing migration: %v, want it to fail", err)
	}
	// Its warning names the namespace the changelog already has.
	if !strings.Contains(logs.String(), `level=WARN msg="first apply for namespace api on database `+db.Name()+
		` (--init); the changelog has entries for: (default)"`) {
		t.Errorf("apply --init logs:\n%s\nwant the first apply warning naming (default)", logs.String())
	}
	if got := entryStatuses(changelog(t, ctx, db)); !slices.Equal(got, []string{setFlag + ":Finished", incCounter + ":Errored"}) {
		t.Fatalf("changelog %v", got)
	}
	if err := failed(false, testLogger(t)); !errors.As(err, &me) {
		t.Errorf("apply after an Errored entry: %v, want the migration to fail again, not an InitError", err)
	}
	if err := failed(true, testLogger(t)); !errors.As(err, &ie) || !ie.Init {
		t.Errorf("apply --init after an Errored entry: %v, want an InitError", err)
	}

	// A mistyped namespace next to existing ones: the error lists them.
	_, err = apply("apii", false, nil)
	want = "the changelog has no entries for namespace apii on database " + db.Name() +
		", so apply would run every migration; the changelog has entries for: (default), api; " +
		"check --db and --namespace, and pass --init if this is the first apply"
	if !errors.As(err, &ie) || err.Error() != want {
		t.Fatalf("apply in a mistyped namespace: %v, want %q", err, want)
	}
}

// TestFirstApplyDigest checks a plan made for a namespace's first apply
// doesn't match once the namespace has entries.
func TestFirstApplyDigest(t *testing.T) {
	ctx := testCtx(t)
	db := testDB(t)
	dir := fixtureDir(t, "data", setFlag)
	planned, err := execute(t, ctx, db, load(t, dir), Options{}, Config{Mode: ModePlan})
	if err != nil || !planned.DigestInput.FirstApply {
		t.Fatalf("plan: %v, report %+v", err, planned)
	}
	// Another apply in between, of a migration the plan didn't see.
	if _, err := execute(t, ctx, db, load(t, fixtureDir(t, "data", seed)), Options{}, Config{Mode: ModeApply}); err != nil {
		t.Fatal(err)
	}
	addFixture(t, dir, "data", seed)
	_, err = execute(t, ctx, db, load(t, dir), Options{}, Config{Mode: ModeApply, Digest: planned.DigestInput})
	var dm *DigestMismatchError
	if !errors.As(err, &dm) || !slices.Contains(dm.Differences(),
		"the plan was made for the namespace's first apply, and the changelog has entries for it now") {
		t.Fatalf("apply with the first apply's plan: %v, want a mismatch naming the first apply", err)
	}
}
