package runner

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// ChangelogCollection holds one entry per migration outcome.
const ChangelogCollection = migration.ChangelogCollection

// ChangelogDeployment returns the digest's deployment for db: the UUID of
// its migrations_changelog collection, from listCollections, as a lowercase
// hyphenated hex string, or DeploymentNone when the collection doesn't
// exist. The server gives a collection a new UUID when it's created, so two
// deployments with the same database and replica set names have different
// ones; a snapshot clone keeps it. When the collection is missing, plan and
// apply create it (createChangelog) and read the UUID again, so their digests
// never say none.
func ChangelogDeployment(ctx context.Context, db *mongo.Database) (string, error) {
	_, deployment, err := readTarget(ctx, db)
	return deployment, err
}

// readTarget reads, with one listCollections, whether db has no collection
// but migrune's own, the lock and the changelog collections, which an
// earlier plan leaves behind, and its deployment (ChangelogDeployment).
func readTarget(ctx context.Context, db *mongo.Database) (onlyOwn bool, deployment string, err error) {
	specs, err := db.ListCollectionSpecifications(ctx, bson.D{{Key: "name", Value: bson.D{{Key: "$ne", Value: LockCollection}}}})
	if err != nil {
		return false, "", fmt.Errorf("list the collections of %s: %w", db.Name(), err)
	}
	onlyOwn = true
	deployment = DeploymentNone
	for _, spec := range specs {
		if spec.Name != ChangelogCollection {
			onlyOwn = false
			continue
		}
		u := spec.UUID
		if u == nil || u.Subtype != bson.TypeBinaryUUID || len(u.Data) != 16 {
			return false, "", fmt.Errorf("the %s collection of %s has no UUID in listCollections", ChangelogCollection, db.Name())
		}
		x := hex.EncodeToString(u.Data)
		deployment = x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
	}
	return onlyOwn, deployment, nil
}

// createChangelog creates the changelog collection of db, with write
// concern majority so its UUID, the digest's deployment, can't roll back.
// It does nothing when the collection exists.
func createChangelog(ctx context.Context, db *mongo.Database) error {
	err := db.Client().Database(db.Name(), options.Database().SetWriteConcern(writeconcern.Majority())).
		CreateCollection(ctx, ChangelogCollection)
	if err != nil && !isCode(err, codeNamespaceExists) {
		return fmt.Errorf("create the %s collection: %w", ChangelogCollection, err)
	}
	return nil
}

// codeNamespaceExists is the server's NamespaceExists error code: create on
// a collection that exists.
const codeNamespaceExists = 48

// namespaceState reports whether the changelog of db has any entry, of any
// status, for namespace, and the other namespaces it has entries for,
// sorted, "" standing for the default one. A missing changelog collection
// has no entries.
func namespaceState(ctx context.Context, db *mongo.Database, namespace string) (hasEntries bool, others []string, err error) {
	cur, err := changelogCollection(db).Aggregate(ctx, mongo.Pipeline{
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$namespace"}}}},
	})
	if err != nil {
		return false, nil, fmt.Errorf("read the changelog's namespaces: %w", err)
	}
	var groups []struct {
		Namespace *string `bson:"_id"`
	}
	if err := cur.All(ctx, &groups); err != nil {
		return false, nil, fmt.Errorf("read the changelog's namespaces: %w", err)
	}
	for _, g := range groups {
		// The default namespace's entries have no namespace field.
		name := ""
		if g.Namespace != nil {
			name = *g.Namespace
		}
		if name == namespace {
			hasEntries = true
		} else {
			others = append(others, name)
		}
	}
	slices.Sort(others)
	return hasEntries, others, nil
}

// InitError is apply's refusal, before it takes the lock, of a namespace
// without changelog entries when Config.Init isn't set (Init false), or of
// Config.Init on a namespace that has entries (Init true).
type InitError struct {
	Database  string
	Namespace string
	// Init is whether Config.Init was set.
	Init bool
	// Others are the other namespaces the changelog has entries for, as
	// namespaceState returns them. Set when Init is false.
	Others []string
}

func (e *InitError) Error() string {
	target := fmt.Sprintf("%s on database %s", namespacePhrase(e.Namespace), e.Database)
	if e.Init {
		return fmt.Sprintf("the changelog has entries for %s: --init is only for a namespace's first apply; remove it", target)
	}
	msg := fmt.Sprintf("the changelog has no entries for %s, so apply would run every migration", target)
	return msg + othersText(e.Others) + "; check --db and --namespace, and pass --init if this is the first apply"
}

// othersText is "; the changelog has entries for: <names>", naming the
// namespaces others (as namespaceState returns them), or "" when there are
// none.
func othersText(others []string) string {
	if len(others) == 0 {
		return ""
	}
	names := make([]string, len(others))
	for i, n := range others {
		names[i] = namespaceName(n)
	}
	return "; the changelog has entries for: " + strings.Join(names, ", ")
}

// namespacePhrase names a namespace in a sentence.
func namespacePhrase(namespace string) string {
	if namespace == "" {
		return "the default namespace"
	}
	return "namespace " + namespace
}

// Status is the outcome a changelog entry records. There is no running status:
// what is running lives in the lock document.
type Status string

const (
	StatusFinished Status = "Finished"
	StatusErrored  Status = "Errored"
)

// Entry is a changelog document.
type Entry struct {
	ID bson.ObjectID `bson:"_id"`
	// V is the entry's format version, EntryVersion.
	V int `bson:"v"`
	// Namespace is the namespace the entry belongs to. The default
	// namespace writes no field, as entries written before namespaces
	// existed.
	Namespace   string `bson:"namespace,omitempty"`
	MigrationID string `bson:"migrationId"`
	Checksum    string `bson:"checksum"`
	// CreatedAt is server time ($$NOW) when the entry was written.
	CreatedAt  time.Time  `bson:"createdAt"`
	Runner     string     `bson:"runner"`
	ApprovedBy ApprovedBy `bson:"approvedBy"`
	Status     Status     `bson:"status"`
	// Migration is the full migration as applied.
	Migration bson.Raw `bson:"migration"`
	// Results has one item per step, in file order. Finished entries only.
	Results []StepResult `bson:"results,omitempty"`
	// Errors is set on Errored entries only.
	Errors []StepError `bson:"errors,omitempty"`
	// Batches is, on the Finished entry of a migration with a batchSize,
	// how many batches it ran, each in a transaction of its own: 1 when
	// its filter matched nothing. Absent otherwise.
	Batches int `bson:"batches,omitempty"`
}

// StepResult is what MongoDB returned for one step. Only the fields that apply
// to the step's operation are set.
type StepResult struct {
	Collection    string `bson:"collection"`
	Operation     string `bson:"operation"`
	MatchedCount  *int64 `bson:"matchedCount,omitempty"`
	ModifiedCount *int64 `bson:"modifiedCount,omitempty"`
	UpsertedCount *int64 `bson:"upsertedCount,omitempty"`
	DeletedCount  *int64 `bson:"deletedCount,omitempty"`
	IndexName     string `bson:"indexName,omitempty"`
	// Set is, for a collModIndex or collModCollection step, the options it
	// set, as in the file.
	Set         bson.Raw `bson:"set,omitempty"`
	MergeCounts `bson:",inline"`
}

// StepError is one error of an Errored entry. Step is the index of the failed
// step in file order, or nil when the error isn't tied to a step.
type StepError struct {
	Step    *int   `bson:"step,omitempty"`
	Message string `bson:"message"`
}

func changelogCollection(db *mongo.Database) *mongo.Collection {
	return db.Collection(ChangelogCollection, options.Collection().
		SetWriteConcern(writeconcern.Majority()).
		SetReadConcern(readconcern.Majority()))
}

// ChangelogIndexName is the name of the index apply creates on the
// changelog, ChangelogIndexKeys.
const ChangelogIndexName = "namespace_status_createdAt"

// ChangelogIndexKeys serve Finished's read: the namespace and status
// equalities, then its sort.
var ChangelogIndexKeys = bson.D{
	{Key: "namespace", Value: 1},
	{Key: "status", Value: 1},
	{Key: "createdAt", Value: 1},
	{Key: "_id", Value: 1},
}

// ensureChangelogIndex creates the changelog index, ChangelogIndexName. It
// does nothing when an index with that name and those keys exists, and fails
// when the name is taken by an index with other keys.
func ensureChangelogIndex(ctx context.Context, db *mongo.Database) error {
	_, err := changelogCollection(db).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    ChangelogIndexKeys,
		Options: options.Index().SetName(ChangelogIndexName),
	})
	if err != nil {
		return fmt.Errorf("create the changelog index %s: %w", ChangelogIndexName, err)
	}
	return nil
}

// namespaceFilter matches the changelog entries of namespace: those with no
// namespace field (or a null one) for the default namespace.
func namespaceFilter(namespace string) bson.E {
	if namespace == "" {
		return bson.E{Key: "namespace", Value: nil}
	}
	return bson.E{Key: "namespace", Value: namespace}
}

// snapshot is the migration file as applied, kept in every entry so the audit
// survives the file being deleted.
func snapshot(m migration.Migration) (bson.Raw, error) {
	raw, err := bson.Marshal(struct {
		ID          string           `bson:"id"`
		Description string           `bson:"description"`
		BatchSize   int              `bson:"batchSize,omitempty"`
		Steps       []migration.Step `bson:"steps"`
	}{m.ID, m.Description, m.BatchSize, m.Steps})
	if err != nil {
		return nil, fmt.Errorf("snapshot migration %s: %w", m.ID, err)
	}
	return raw, nil
}

// EntryVersion is the format version every changelog entry is written with,
// in its v field. An entry without v reads as 1. CheckFormats refuses a
// changelog holding a higher one; when to raise it is in CONTRIBUTING.md.
const EntryVersion = 1

// WriteFinished records m as Finished, with the results of its steps, as the
// changelog entry with _id id. Call it inside the fenced transaction the
// entry commits with: a data migration's, or one of its own for index and
// merge migrations.
func (r *Run) WriteFinished(ctx context.Context, id bson.ObjectID, m migration.Migration, results []StepResult) error {
	if results == nil {
		results = []StepResult{}
	}
	return r.writeEntry(ctx, id, m, StatusFinished, bson.E{Key: "results", Value: results})
}

// writeFinishedBatches is WriteFinished for a migration with a batchSize,
// with the step's counts summed over its batches, and how many it ran.
func (r *Run) writeFinishedBatches(ctx context.Context, id bson.ObjectID, m migration.Migration, results []StepResult, batches int) error {
	return r.writeEntry(ctx, id, m, StatusFinished, bson.E{Key: "results", Value: results},
		bson.E{Key: "batches", Value: int32(batches)})
}

// writeFinishedAlone records m as Finished, with the results of its steps, as
// a new changelog entry, in a fenced transaction of its own: for index and
// merge migrations, whose steps run outside one.
func (r *Run) writeFinishedAlone(m migration.Migration, results []StepResult) error {
	entry := bson.NewObjectID()
	return r.transaction(func(ctx context.Context) error {
		return r.WriteFinished(ctx, entry, m, results)
	}, true, &entry)
}

// stepFailed returns the error for step i, s, of an index or merge migration
// m, which failed with err. When the run was interrupted or lost its lock,
// it returns that, and records nothing. Otherwise it returns a StepFailure
// and, with apply, records m as Errored with it.
func (r *Run) stepFailed(m migration.Migration, i int, s migration.Step, err error, apply bool) error {
	if rerr := r.Err(); rerr != nil {
		return rerr
	}
	sf := &StepFailure{Step: i, Operation: s.Operation, Collection: s.Collection, Err: err}
	if apply {
		if werr := r.WriteErrored(m, r.stepErrors(sf)); werr != nil {
			return errors.Join(sf, werr)
		}
	}
	return sf
}

// WriteErrored records m as Errored with errs, after it failed, in a fenced
// transaction of its own. It writes nothing when the run was interrupted or
// lost its lock: an interruption is not a failure and the migration stays
// pending, and a runner that lost its lock writes nothing. When the
// fence finds the lock lost, the run is cancelled, nothing is written and
// the error says so.
func (r *Run) WriteErrored(m migration.Migration, errs []StepError) error {
	if r.Err() != nil {
		return nil
	}
	if errs == nil {
		errs = []StepError{}
	}
	id := bson.NewObjectID()
	err := r.transaction(func(ctx context.Context) error {
		return r.writeEntry(ctx, id, m, StatusErrored, bson.E{Key: "errors", Value: errs})
	}, true, &id)
	if err != nil {
		return fmt.Errorf("no Errored changelog entry written for %s: %w", m.ID, err)
	}
	return nil
}

// writeEntry upserts the entry with _id id with an update pipeline, so
// createdAt comes from the server's $$NOW (see entrySet).
func (r *Run) writeEntry(ctx context.Context, id bson.ObjectID, m migration.Migration, status Status, outcome ...bson.E) error {
	set, err := r.entrySet(m, status, outcome...)
	if err != nil {
		return err
	}
	_, err = r.changelog.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: id}},
		mongo.Pipeline{{{Key: "$set", Value: set}}},
		options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("write %s changelog entry for %s: %w", status, m.ID, err)
	}
	r.log.Info("changelog entry written", "migration", m.ID, "status", string(status))
	return nil
}

// entrySet is the $set stage writeEntry writes an entry with: every field
// but _id, the outcome's last. Every client value goes through $literal, so
// operators in the migration snapshot aren't evaluated.
func (r *Run) entrySet(m migration.Migration, status Status, outcome ...bson.E) (bson.D, error) {
	snap, err := snapshot(m)
	if err != nil {
		return nil, err
	}
	lit := func(v any) bson.D { return bson.D{{Key: "$literal", Value: v}} }
	set := bson.D{{Key: "v", Value: lit(int32(EntryVersion))}}
	if r.namespace != "" {
		set = append(set, bson.E{Key: "namespace", Value: lit(r.namespace)})
	}
	set = append(set, bson.D{
		{Key: "migrationId", Value: lit(m.ID)},
		{Key: "checksum", Value: lit(m.Checksum)},
		{Key: "createdAt", Value: "$$NOW"},
		{Key: "runner", Value: lit(r.runnerID)},
		{Key: "approvedBy", Value: lit(r.approvedBy)},
		{Key: "status", Value: lit(status)},
		{Key: "migration", Value: lit(snap)},
	}...)
	for _, o := range outcome {
		set = append(set, bson.E{Key: o.Key, Value: lit(o.Value)})
	}
	return set, nil
}
