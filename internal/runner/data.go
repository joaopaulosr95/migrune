package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// StepFailure is a failure tied to one step of a migration.
type StepFailure struct {
	// Step is the step's index in file order.
	Step       int
	Operation  string
	Collection string
	// Second is set when the failure comes from the second run of the
	// double-run.
	Second bool
	Err    error
}

func (e *StepFailure) Error() string {
	run := ""
	if e.Second {
		run = ", second run"
	}
	return fmt.Sprintf("/steps/%d (%s on %s%s): %v", e.Step, e.Operation, e.Collection, run, e.Err)
}

func (e *StepFailure) Unwrap() error { return e.Err }

// runData runs a data migration in one transaction: the steps, the
// same steps again, and a check that the second run changed nothing.
// When apply is set, the Finished entry is written in the same transaction
// and it commits; otherwise it aborts. The transaction runs again on a
// transient error (see Run.transaction). A failure is recorded as Errored
// when applying, unless the run was interrupted or lost its lock.
//
// The first run's counts, what the migration does, are logged and go into
// row.Steps, whether or not the migration then passes.
func (r *Run) runData(m migration.Migration, apply bool, log *slog.Logger, row *Row) error {
	// SetCurrent shares a mutex with transaction: call it before.
	if err := r.lock.SetCurrent(r.ctx, m.ID); err != nil {
		return r.lockErr(err)
	}
	// The entry's _id is chosen before the transaction, so a commit with an
	// unknown outcome can be checked by looking it up.
	entry := bson.NewObjectID()
	// first is the last attempt's first run, when it got that far.
	var first []StepResult
	err := r.transaction(func(ctx context.Context) error {
		first = nil
		res, err := runDataSteps(ctx, r.db, m, false)
		if err != nil {
			return err
		}
		first = res
		second, err := runDataSteps(ctx, r.db, m, true)
		if err != nil {
			return err
		}
		if err := checkIdempotent(m, second); err != nil {
			return err
		}
		if !apply {
			return nil
		}
		return r.WriteFinished(ctx, entry, m, first)
	}, apply, &entry)
	for i, res := range first {
		step := DataStep{Index: i, Operation: res.Operation, Collection: res.Collection,
			Matched: res.MatchedCount, Modified: res.ModifiedCount, Upserted: res.UpsertedCount, Deleted: res.DeletedCount}
		row.Steps = append(row.Steps, step)
		log.Info("data step counts, from the first run", step.logAttrs()...)
	}
	if err == nil {
		return nil
	}
	var ie *InterruptedError
	if apply && !errors.As(err, &ie) {
		if werr := r.WriteErrored(m, r.stepErrors(err)); werr != nil {
			err = errors.Join(err, werr)
		}
	}
	return err
}

// runDataSteps runs every step of m once, in file order.
func runDataSteps(ctx context.Context, db *mongo.Database, m migration.Migration, second bool) ([]StepResult, error) {
	results := make([]StepResult, len(m.Steps))
	for i, s := range m.Steps {
		res, err := runDataStep(ctx, db.Collection(s.Collection), s)
		if err != nil {
			return nil, &StepFailure{Step: i, Operation: s.Operation, Collection: s.Collection, Second: second, Err: err}
		}
		results[i] = res
	}
	return results, nil
}

// checkIdempotent fails when the second run reported any modified, upserted
// or deleted document.
func checkIdempotent(m migration.Migration, second []StepResult) error {
	for i, res := range second {
		var changes []string
		for _, c := range []struct {
			n    *int64
			verb string
		}{{res.ModifiedCount, "modified"}, {res.UpsertedCount, "upserted"}, {res.DeletedCount, "deleted"}} {
			if c.n != nil && *c.n > 0 {
				changes = append(changes, fmt.Sprintf("%s %d", c.verb, *c.n))
			}
		}
		if len(changes) > 0 {
			s := m.Steps[i]
			return &StepFailure{Step: i, Operation: s.Operation, Collection: s.Collection, Second: true,
				Err: fmt.Errorf("not idempotent: running the migration again %s documents; "+
					"a step must change nothing once applied (filter on the target state, use $set instead of $inc)",
					strings.Join(changes, ", "))}
		}
	}
	return nil
}

// stepErrors turns a migration failure into the errors of its Errored
// changelog entry, whose messages are errorText's.
func (r *Run) stepErrors(err error) []StepError {
	msg := r.errText(err)
	var sf *StepFailure
	if errors.As(err, &sf) {
		step := sf.Step
		return []StepError{{Step: &step, Message: msg}}
	}
	return []StepError{{Message: msg}}
}

func runDataStep(ctx context.Context, coll *mongo.Collection, s migration.Step) (StepResult, error) {
	out := StepResult{Collection: s.Collection, Operation: s.Operation}
	o, err := parseDataOptions(s)
	if err != nil {
		return out, err
	}
	switch s.Operation {
	case "updateOne", "updateMany":
		update, err := updateValue(s.Update)
		if err != nil {
			return out, err
		}
		var res *mongo.UpdateResult
		// Options left nil aren't sent.
		if s.Operation == "updateOne" {
			opts := options.UpdateOne().SetArrayFilters(o.ArrayFilters).SetCollation(o.Collation).SetHint(o.Hint).SetLet(o.Let)
			if o.Upsert != nil {
				opts.SetUpsert(*o.Upsert)
			}
			res, err = coll.UpdateOne(ctx, s.Filter, update, opts)
		} else {
			opts := options.UpdateMany().SetArrayFilters(o.ArrayFilters).SetCollation(o.Collation).SetHint(o.Hint).SetLet(o.Let)
			if o.Upsert != nil {
				opts.SetUpsert(*o.Upsert)
			}
			res, err = coll.UpdateMany(ctx, s.Filter, update, opts)
		}
		if err != nil {
			return out, err
		}
		out.MatchedCount = &res.MatchedCount
		out.ModifiedCount = &res.ModifiedCount
		out.UpsertedCount = &res.UpsertedCount
	case "deleteMany":
		opts := options.DeleteMany().SetCollation(o.Collation).SetHint(o.Hint).SetLet(o.Let)
		res, err := coll.DeleteMany(ctx, s.Filter, opts)
		if err != nil {
			return out, err
		}
		out.DeletedCount = &res.DeletedCount
	default:
		return out, fmt.Errorf("operation %q is not a data operation", s.Operation)
	}
	return out, nil
}

// updateValue returns a step's update document or update pipeline in a form
// the driver accepts.
func updateValue(v bson.RawValue) (any, error) {
	switch v.Type {
	case bson.TypeEmbeddedDocument:
		return v.Document(), nil
	case bson.TypeArray:
		var stages []bson.Raw
		if err := v.Unmarshal(&stages); err != nil {
			return nil, fmt.Errorf("update pipeline: %w", err)
		}
		return stages, nil
	default:
		return nil, fmt.Errorf("update must be a document or a pipeline, got %s", v.Type)
	}
}

// dataOptions are the step options migrune passes to the driver; nil
// means not set.
type dataOptions struct {
	Upsert       *bool
	ArrayFilters []any
	Collation    *options.Collation
	Hint         any
	// Let is a bson.Raw.
	Let any
}

// allowedOptions lists the options each data operation accepts.
var allowedOptions = map[string][]string{
	"updateOne":  {"arrayFilters", "collation", "hint", "let", "upsert"},
	"updateMany": {"arrayFilters", "collation", "hint", "let", "upsert"},
	"deleteMany": {"collation", "hint", "let"},
}

func parseDataOptions(s migration.Step) (dataOptions, error) {
	var o dataOptions
	if s.Options == nil {
		return o, nil
	}
	elems, err := s.Options.Elements()
	if err != nil {
		return o, fmt.Errorf("options: %w", err)
	}
	allowed := allowedOptions[s.Operation]
	for _, e := range elems {
		key, v := e.Key(), e.Value()
		if !slices.Contains(allowed, key) {
			return o, fmt.Errorf("option %q is not supported for %s; supported options: %s", key, s.Operation, strings.Join(allowed, ", "))
		}
		switch key {
		case "upsert":
			b, ok := v.BooleanOK()
			if !ok {
				return o, fmt.Errorf("option upsert must be a boolean, got %s", v.Type)
			}
			o.Upsert = &b
		case "arrayFilters":
			var filters []bson.Raw
			if err := v.Unmarshal(&filters); err != nil {
				return o, fmt.Errorf("option arrayFilters must be an array of documents: %w", err)
			}
			o.ArrayFilters = make([]any, len(filters))
			for i, f := range filters {
				o.ArrayFilters[i] = f
			}
		case "collation":
			var c collation
			if err := v.Unmarshal(&c); err != nil {
				return o, fmt.Errorf("option collation must be a document: %w", err)
			}
			o.Collation = c.options()
		case "hint":
			switch v.Type {
			case bson.TypeString:
				o.Hint = v.StringValue()
			case bson.TypeEmbeddedDocument:
				o.Hint = v.Document()
			default:
				return o, fmt.Errorf("option hint must be an index name or a key pattern, got %s", v.Type)
			}
		case "let":
			doc, ok := v.DocumentOK()
			if !ok {
				return o, fmt.Errorf("option let must be a document, got %s", v.Type)
			}
			o.Let = doc
		}
	}
	return o, nil
}

// collation is a collation document as MongoDB spells its fields.
type collation struct {
	Locale          string `bson:"locale"`
	CaseLevel       bool   `bson:"caseLevel"`
	CaseFirst       string `bson:"caseFirst"`
	Strength        int    `bson:"strength"`
	NumericOrdering bool   `bson:"numericOrdering"`
	Alternate       string `bson:"alternate"`
	MaxVariable     string `bson:"maxVariable"`
	Normalization   bool   `bson:"normalization"`
	Backwards       bool   `bson:"backwards"`
}

func (c collation) options() *options.Collation {
	return &options.Collation{
		Locale:          c.Locale,
		CaseLevel:       c.CaseLevel,
		CaseFirst:       c.CaseFirst,
		Strength:        c.Strength,
		NumericOrdering: c.NumericOrdering,
		Alternate:       c.Alternate,
		MaxVariable:     c.MaxVariable,
		Normalization:   c.Normalization,
		Backwards:       c.Backwards,
	}
}
