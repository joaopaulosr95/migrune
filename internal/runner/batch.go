package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// runBatched runs a data migration with a batchSize: its one updateMany or
// deleteMany step, in batches of m.BatchSize documents, each in a fenced
// transaction of its own that runs the batch twice and checks the second run
// changed nothing, as runData does for a whole migration.
//
// The batches are the _ids the step's filter matches, read once, in _id
// order, from one cursor outside any transaction (read concern majority, the
// step's hint and let; a batched step has no collation). Each batch runs the step with the filter
// {$and: [<filter>, {_id: {$in: <batch>}}]}: each _id is visited once, of
// whatever BSON type, and documents that still match the filter once
// changed aren't visited again. (A $gt on the last _id would skip _ids of
// other types: comparisons are type-bracketed.) The step's hint picks the
// cursor's index; the batches' writes go without it, so they find their
// documents by _id.
//
// apply commits each batch; the last one writes the Finished entry, with the
// step's counts summed over the batches and how many there were. When the
// filter matches nothing, one transaction writes it, with zero counts. A
// batch whose commit outcome stays unknown runs again, which its idempotence
// makes safe; the last one looks its entry up instead (see Run.commit). A
// failure is recorded as Errored, with how many batches committed: those
// stay committed. An interruption or a lost lock between batches writes no
// entry.
//
// plan counts the documents the filter matches and runs the first batch
// only, in a transaction it aborts: later batches can fail on documents plan
// never ran.
//
// The size guard of single-transaction migrations (checkTransactionSize)
// doesn't apply: no batch holds more than MaxTransactionDocs documents.
func (r *Run) runBatched(m migration.Migration, apply bool, log *slog.Logger, row *Row) error {
	// SetCurrent shares a mutex with transaction: call it before.
	if err := r.lock.SetCurrent(r.ctx, m.ID); err != nil {
		return r.lockErr(err)
	}
	s := m.Steps[0]
	size := m.BatchSize
	row.BatchSize = size
	sum := StepResult{Collection: s.Collection, Operation: s.Operation}
	// committed counts the batches apply committed; planned, those the
	// count expects, at least 1.
	var committed, planned int
	if apply {
		row.Batches = new(int64)
	}
	fail := func(err error, batch int) error {
		if apply {
			total := max(planned, batch)
			unit := "batches"
			if total == 1 {
				unit = "batch"
			}
			err = fmt.Errorf("%w; committed %d of %d %s", err, committed, total, unit)
		}
		var ie *InterruptedError
		if apply && !errors.As(err, &ie) {
			if werr := r.WriteErrored(m, r.stepErrors(err)); werr != nil {
				err = errors.Join(err, werr)
			}
		}
		return err
	}
	stepFailure := func(what string, err error) error {
		return r.cause(&StepFailure{Step: 0, Operation: s.Operation, Collection: s.Collection,
			Err: fmt.Errorf("%s: %w", what, err)})
	}

	total, err := r.countMatching(s, 0)
	if err != nil {
		return fail(stepFailure("count matching documents", err), 1)
	}
	planned = max(1, int((total+int64(size)-1)/int64(size)))
	cur, err := r.matchingIDs(s, size)
	if err != nil {
		return fail(stepFailure("read the matching _ids", err), 1)
	}
	defer func() {
		ctx, cancel := cleanupContext(r.ctx)
		defer cancel()
		_ = cur.Close(ctx)
	}()
	batch, err := nextIDs(r.ctx, cur, size)
	if err != nil {
		return fail(stepFailure("read the matching _ids", err), 1)
	}
	if !apply {
		checked := int64(len(batch))
		row.Matched, row.Checked = &total, &checked
	}
	log.Info("data migration runs in batches", "batchSize", size, "matching", total, "batches", planned)

	// The Finished entry's _id is chosen before the last transaction, so a
	// commit with an unknown outcome can be checked by looking it up.
	entry := bson.NewObjectID()
	for n := 1; ; n++ {
		// Read one batch ahead, to know which batch is the last.
		var next []bson.RawValue
		if apply {
			if next, err = nextIDs(r.ctx, cur, size); err != nil {
				return fail(stepFailure("read the matching _ids", err), n)
			}
		}
		last := len(next) == 0
		res, err := r.runBatch(m, batch, apply, last, n, &entry, sum, log)
		if res != nil {
			sum = addResults(sum, *res)
			if apply {
				committed = n
				*row.Batches = int64(n)
			}
		}
		if err != nil || !apply || last {
			step := DataStep{Index: 0, Operation: s.Operation, Collection: s.Collection,
				Matched: sum.MatchedCount, Modified: sum.ModifiedCount, Upserted: sum.UpsertedCount, Deleted: sum.DeletedCount}
			if res != nil || committed > 0 {
				row.Steps = []DataStep{step}
				what := "data step counts, summed over the committed batches"
				if !apply {
					what = "data step counts, from the first run of the first batch"
				}
				log.Info(what, step.logAttrs()...)
			}
		}
		if err != nil {
			return fail(err, n)
		}
		if !apply {
			log.Info("plan ran the first batch only: later batches can fail on documents plan didn't run",
				"checked", len(batch), "matching", total)
			return nil
		}
		if last {
			return nil
		}
		log.Info("batch committed", "batch", n, "of", max(planned, n+1), "documents", len(batch))
		batch = next
	}
}

// runBatch runs one batch, the documents with the _ids ids, of m's step, in
// one fenced transaction: the step, the step again, and a check that the
// second run changed nothing. In apply it commits, and the last batch, n of
// them, writes the Finished entry with _id entry and the step's counts,
// before's plus this batch's. It returns the first run's counts when the
// transaction passed (and, in apply, committed), and nil otherwise. A batch
// that isn't the last runs again when its commit outcome stays unknown.
func (r *Run) runBatch(m migration.Migration, ids []bson.RawValue, apply, last bool, n int, entry *bson.ObjectID,
	before StepResult, log *slog.Logger) (*StepResult, error) {
	s := m.Steps[0]
	step, err := batchStep(s, ids)
	if err != nil {
		return nil, &StepFailure{Step: 0, Operation: s.Operation, Collection: s.Collection, Err: err}
	}
	bm := m
	bm.Steps = []migration.Step{step}
	var lookup *bson.ObjectID
	if apply && last {
		lookup = entry
	}
	for attempt := 0; ; attempt++ {
		var first StepResult
		err := r.transaction(func(ctx context.Context) error {
			res, err := runDataSteps(ctx, r.db, bm, false)
			if err != nil {
				return err
			}
			first = res[0]
			second, err := runDataSteps(ctx, r.db, bm, true)
			if err != nil {
				return err
			}
			if err := checkIdempotent(bm, second); err != nil {
				return err
			}
			if !apply || !last {
				return nil
			}
			return r.writeFinishedBatches(ctx, *entry, m, []StepResult{addResults(before, first)}, n)
		}, apply, lookup)
		if err == nil {
			return &first, nil
		}
		if errors.Is(err, errCommitUnknown) && attempt < maxCommitRetries {
			log.Warn("batch commit outcome unknown: running the batch again", "batch", n, "error", r.errText(err))
			continue
		}
		return nil, withLifetimeHint(err)
	}
}

// matchingIDs opens a cursor on the _ids of the documents s's filter
// matches, in _id order, size per batch, outside any transaction, with read
// concern majority and the step's hint and let.
func (r *Run) matchingIDs(s migration.Step, size int) (*mongo.Cursor, error) {
	o, err := parseDataOptions(s)
	if err != nil {
		return nil, err
	}
	coll := r.db.Collection(s.Collection, options.Collection().SetReadConcern(readconcern.Majority()))
	opts := options.Find().
		SetSort(bson.D{{Key: "_id", Value: 1}}).
		SetProjection(bson.D{{Key: "_id", Value: 1}}).
		SetBatchSize(int32(size)).
		SetHint(o.Hint).SetLet(o.Let)
	return coll.Find(r.ctx, s.Filter, opts)
}

// nextIDs reads up to n _ids from cur.
func nextIDs(ctx context.Context, cur *mongo.Cursor, n int) ([]bson.RawValue, error) {
	var ids []bson.RawValue
	for len(ids) < n && cur.Next(ctx) {
		id, err := cur.Current.LookupErr("_id")
		if err != nil {
			return nil, fmt.Errorf("a matching document has no _id: %w", err)
		}
		// The cursor reuses its buffer.
		id.Value = slices.Clone(id.Value)
		ids = append(ids, id)
	}
	return ids, cur.Err()
}

// batchStep returns s restricted to the documents with the _ids ids, and
// without its hint, so the batch finds them by _id.
func batchStep(s migration.Step, ids []bson.RawValue) (migration.Step, error) {
	in := make(bson.A, len(ids))
	for i, id := range ids {
		in[i] = id
	}
	filter, err := bson.Marshal(bson.D{{Key: "$and", Value: bson.A{
		s.Filter,
		bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: in}}}},
	}}})
	if err != nil {
		return s, fmt.Errorf("batch filter: %w", err)
	}
	s.Filter = filter
	if s.Options != nil {
		elems, err := s.Options.Elements()
		if err != nil {
			return s, fmt.Errorf("options: %w", err)
		}
		opts := bson.D{}
		for _, e := range elems {
			if e.Key() != "hint" {
				opts = append(opts, bson.E{Key: e.Key(), Value: e.Value()})
			}
		}
		if s.Options, err = bson.Marshal(opts); err != nil {
			return s, fmt.Errorf("options: %w", err)
		}
	}
	return s, nil
}

// addResults sums the counts of two results of the same step.
func addResults(a, b StepResult) StepResult {
	add := func(x, y *int64) *int64 {
		if x == nil && y == nil {
			return nil
		}
		var n int64
		if x != nil {
			n += *x
		}
		if y != nil {
			n += *y
		}
		return &n
	}
	a.MatchedCount = add(a.MatchedCount, b.MatchedCount)
	a.ModifiedCount = add(a.ModifiedCount, b.ModifiedCount)
	a.UpsertedCount = add(a.UpsertedCount, b.UpsertedCount)
	a.DeletedCount = add(a.DeletedCount, b.DeletedCount)
	return a
}
