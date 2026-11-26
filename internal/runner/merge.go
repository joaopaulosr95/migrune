package runner

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// runMerge runs a merge migration. Each step is an aggregate pipeline
// ending in $merge, which MongoDB doesn't allow in a transaction, so the steps
// run one by one, in file order, outside one.
//
// Plan compares each step against the current database. Apply writes and
// checks each step immediately. If any step inserts, it also checks the
// earlier steps against the final state before recording Finished.
func (r *Run) runMerge(m migration.Migration, apply bool, log *slog.Logger, row *Row) error {
	if err := r.lock.SetCurrent(r.ctx, m.ID); err != nil {
		return r.lockErr(err)
	}
	results := make([]StepResult, len(m.Steps))
	// Assemble the row once, including partial observations on any failure.
	// A terminal recheck replaces a step's after-counts in results.
	defer func() {
		for _, res := range results {
			row.add(res.MergeCounts)
			if res.Insert != nil {
				row.InsertSteps = append(row.InsertSteps, *res.Insert)
			}
		}
	}()
	hasInsert := false
	for i, s := range m.Steps {
		terminal := hasInsert && i == len(m.Steps)-1
		res, err := r.runMergeStep(i, s, apply, terminal, log.With("step", i, "collection", s.Collection), row)
		results[i] = res
		if err != nil {
			return r.stepFailed(m, i, s, err, apply)
		}
		hasInsert = hasInsert || res.Insert != nil
	}
	if !apply {
		return nil
	}
	if hasInsert {
		for i, s := range m.Steps[:len(m.Steps)-1] {
			pre, target, _ := splitMerge(s) // the immediate check already parsed it
			if err := r.verifyMerge(s, pre, target, &results[i], true, log.With("step", i, "collection", s.Collection)); err != nil {
				return r.stepFailed(m, i, s, err, apply)
			}
		}
	}
	return r.writeFinishedAlone(m, results)
}

// runMergeStep compares, and with apply merges and compares again, one step.
func (r *Run) runMergeStep(i int, s migration.Step, apply, terminal bool, log *slog.Logger, row *Row) (StepResult, error) {
	res := StepResult{Collection: s.Collection, Operation: s.Operation}
	pre, target, err := splitMerge(s)
	if err != nil {
		return res, err
	}
	if target.Insert {
		res.Insert = &InsertStep{Step: i, Into: target.Into}
	}
	index, err := r.prepareMerge(s, target, res.Insert, apply, log)
	if res.Insert != nil && res.Insert.TargetExistsBefore != nil {
		msg := "insert target is present"
		if !*res.Insert.TargetExistsBefore {
			msg = "insert target is absent; the first insert can create it"
		}
		log.Info(msg, "into", target.Into, "targetExistsBefore", *res.Insert.TargetExistsBefore)
	}
	if err != nil {
		return res, err
	}
	before, err := r.compareMerge(s.Collection, pre, target, log)
	if err != nil {
		return res, fmt.Errorf("compare with %s: %w", target.Into, err)
	}
	res.MergeCounts = before.counts()
	if res.Insert != nil {
		res.UnmatchedCount = nil
		res.Insert.PendingBefore = &before.Unmatched
		res.Insert.DiffCountBefore = &before.Diff
	}
	if index.Collation != "" {
		n, err := r.collationOnlyMatches(s.Collection, pre, target, index, log)
		if err != nil {
			return res, fmt.Errorf("count the documents matching %s only through the collation of index %s: %w", target.Into, index.Name, err)
		}
		res.CollationOnlyMatchCount = &n
	}
	for _, sm := range before.Samples {
		sm.Step = i
		row.Samples = append(row.Samples, sm)
		log.Info("merge sample: a document that differs from its target", sampleAttrs(sm)...)
	}
	if target.Insert {
		log.Info("insert comparison", "into", target.Into, "pendingBefore", before.Unmatched, "diffCountBefore", before.Diff)
	} else {
		log.Info("merge comparison", "into", target.Into, "on", strings.Join(target.On, ","),
			"diffCount", before.Diff, "unmatchedCount", before.Unmatched, "typeChangedCount", before.TypeChanged,
			"duplicateOnCount", before.DuplicateOn, "nullOnCount", before.NullOn, "arrayOnCount", before.ArrayOn,
			"idMismatchCount", before.IDMismatch)
	}
	if err := before.validate(target, log); err != nil {
		return res, err
	}
	if !apply {
		return res, nil
	}

	// $merge writes outside any transaction, unfenced.
	if err := r.checkLease(); err != nil {
		return res, err
	}
	if err := r.aggregate(s.Collection, toPipeline(s.Pipeline), log, nil); err != nil {
		return res, fmt.Errorf("merge into %s: %w", target.Into, err)
	}
	log.Info("merge done", "into", target.Into)
	err = r.verifyMerge(s, pre, target, &res, terminal, log)
	return res, err
}

func (r *Run) prepareMerge(s migration.Step, target mergeTarget, obs *InsertStep, apply bool, log *slog.Logger) (mergeIndex, error) {
	if target.Insert {
		return mergeIndex{}, r.checkInsertCollections(s, target, obs)
	}
	collation, err := r.defaultCollation(s.Collection)
	if err != nil {
		return mergeIndex{}, fmt.Errorf("read the default collation of %s: %w", s.Collection, err)
	}
	index, err := r.checkMergeIndex(s.Collection, collation, target, apply, log)
	if err == nil {
		warnCollation(log, s.Collection, collation, target, index)
	}
	return index, err
}

func (before mergeComparison) validate(target mergeTarget, log *slog.Logger) error {
	if before.Unmatched > 0 && !target.Insert {
		log.Warn("output documents have no match in the target: $merge discards them; check the on fields",
			"into", target.Into, "on", strings.Join(target.On, ","), "unmatchedCount", before.Unmatched)
	}
	if before.InvalidFieldName > 0 {
		return fmt.Errorf("invalidFieldNameCount is %d: output documents for %s have an empty top-level name or one containing a dot or starting with $", before.InvalidFieldName, target.Into)
	}
	if before.InvalidID > 0 {
		return fmt.Errorf("invalidIDCount is %d: output documents for %s have an invalid _id (regex, undefined or an invalid field inside a document-valued _id)", before.InvalidID, target.Into)
	}
	if before.DuplicateMissing > 0 {
		return fmt.Errorf("duplicateMissingIDCount is %d: that many output documents share a missing _id in %s; output one document per _id", before.DuplicateMissing, target.Into)
	}
	on := strings.Join(target.On, ",")
	if before.NullOn > 0 {
		return fmt.Errorf("nullOnCount is %d: that many output documents have a null or missing on field (%s): "+
			"$merge would match them with a target whose field is null or missing, or with none; "+
			"output only documents with every on field set", before.NullOn, on)
	}
	if before.ArrayOn > 0 {
		return fmt.Errorf("arrayOnCount is %d: that many output documents have an array as an on field (%s): "+
			"$merge would fail on them part way through (error 51185); output one value per on field", before.ArrayOn, on)
	}
	if before.IDMismatch > 0 {
		if target.Insert {
			return fmt.Errorf("idMismatchCount is %d: an emitted _id matches a document in %s but has a different BSON representation; emit the stored _id exactly", before.IDMismatch, target.Into)
		}
		return fmt.Errorf("idMismatchCount is %d: that many output documents have an _id other than the _id of the document "+
			"they match in %s on %s: $merge can't change an _id and would fail part way through; "+
			"leave _id out of the output ({\"$project\": {\"_id\": 0, ...}})",
			before.IDMismatch, target.Into, on)
	}
	if before.TypeChanged > 0 {
		return fmt.Errorf("typeChangedCount is %d: the merge would change the BSON type of existing fields in %s, "+
			"or drop nested ones (type missing); neither is allowed in place: write the new type or shape to a new field instead",
			before.TypeChanged, target.Into)
	}
	if before.DuplicateOn > 0 {
		return fmt.Errorf("duplicateOnCount is %d: that many output documents share their on fields (%s) with another, "+
			"so $merge would write the same document in %s more than once and the last write would win; "+
			"make the pipeline output one document per on value", before.DuplicateOn, on, target.Into)
	}
	return nil
}

func (r *Run) verifyMerge(s migration.Step, pre []bson.Raw, target mergeTarget, res *StepResult, terminal bool, log *slog.Logger) error {
	// Re-read metadata as well: a dropped source must not look like a
	// converged, empty projection.
	if target.Insert || terminal {
		if _, err := r.prepareMerge(s, target, nil, true, log); err != nil {
			return err
		}
	}
	after, err := r.compareMerge(s.Collection, pre, target, log)
	if err != nil {
		return fmt.Errorf("compare with %s after the merge: %w", target.Into, err)
	}
	res.DiffCountAfter = &after.Diff
	if target.Insert {
		res.Insert.PendingAfter = &after.Unmatched
		res.Insert.DiffCountAfter = &after.Diff
		log.Info("insert comparison after the merge", "into", target.Into, "pendingAfter", after.Unmatched, "diffCountAfter", after.Diff)
	} else {
		log.Info("merge comparison after the merge", "into", target.Into, "diffCountAfter", after.Diff)
	}
	if target.Insert || terminal {
		if err := after.validate(target, log); err != nil {
			return err
		}
	}
	if target.Insert && after.Unmatched > 0 {
		return fmt.Errorf("pendingAfter is %d: that many output documents still have no target in %s after the merge", after.Unmatched, target.Into)
	}
	if after.Diff > 0 {
		return fmt.Errorf("diffCountAfter is %d: documents in %s still differ from the output after the merge, "+
			"so it didn't settle and running it again would change them again", after.Diff, target.Into)
	}
	return nil
}

// mergeTarget is what a step's $merge stage writes to.
type mergeTarget struct {
	Into   string
	Insert bool
	// On are the fields matching output documents with target documents.
	On []string
}

// splitMerge returns a merge step's pipeline without its $merge stage, and
// that stage's target. The schema requires exactly one $merge, with into as a
// collection name and on, if set, as a field name or an array of distinct
// ones; lint requires it to be the last stage, which plan and apply check
// here.
func splitMerge(s migration.Step) ([]bson.Raw, mergeTarget, error) {
	var t mergeTarget
	n := len(s.Pipeline)
	stage, ok := s.Pipeline[n-1].Lookup("$merge").DocumentOK()
	if !ok {
		return nil, t, errors.New("the $merge stage must be the last stage of the pipeline")
	}
	t.Into, _ = stage.Lookup("into").StringValueOK()
	mode, _ := stage.Lookup("whenNotMatched").StringValueOK()
	t.Insert = mode == "insert"
	on, err := stage.LookupErr("on")
	switch {
	case err != nil:
		t.On = []string{"_id"}
	case on.Type == bson.TypeString:
		t.On = []string{on.StringValue()}
	default:
		vals, _ := on.Array().Values()
		for _, v := range vals {
			t.On = append(t.On, v.StringValue())
		}
	}
	return s.Pipeline[:n-1], t, nil
}

// toPipeline turns stages into a pipeline the driver accepts.
func toPipeline(stages []bson.Raw) []any {
	out := make([]any, len(stages))
	for i, st := range stages {
		out[i] = st
	}
	return out
}

// warnCollation logs one warning per merge step when source, the collection
// the step reads, has a non-simple default collation. $merge runs the
// pipeline under it, since an aggregate has a single collation and migrune
// sets none, while the comparison runs the same pipeline under the simple
// collation to compare strings byte by byte. Any expression comparing
// strings, in any stage, can then give another output, so the counts may not
// be what apply writes. The unique index $merge matches the on fields
// through, ix, has the same collation, and the warning then also lists what
// the comparison can't see through it.
func warnCollation(log *slog.Logger, source, collation string, t mergeTarget, ix mergeIndex) {
	if collation == "" {
		return
	}
	msg := "the collection the pipeline reads has a non-simple default collation: $merge runs the pipeline under it, " +
		"while plan's preview and the before and after counts run it under the simple collation, " +
		"so they may differ from what apply writes"
	attrs := []any{"source", source, "collation", collation}
	if ix.Collation != "" {
		msg += "; the unique index on the on fields has it too, and the comparison doesn't fully check it: " +
			"duplicateOnCount misses output documents whose on values are equal only under it; " +
			"documents that match a target only through it get no per-field diff or type check; " +
			"from MongoDB 8.3, $merge rewrites the target's stored on values to the output's; " +
			"collationOnlyMatchCount leaves out on values that are embedded documents"
		attrs = append(attrs, "into", t.Into, "on", strings.Join(t.On, ","), "index", ix.Name)
	}
	log.Warn(msg, attrs...)
}
