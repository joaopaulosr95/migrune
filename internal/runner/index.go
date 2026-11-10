package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// Server error codes index steps handle.
const (
	codeNamespaceNotFound          = 26
	codeIndexNotFound              = 27
	codeCannotConvertIndexToUnique = 359
)

// runIndex runs an index migration. Index builds can't run in a
// multi-document transaction, so the steps run one by one, in file order,
// outside one; MongoDB makes each idempotent. With apply, the Finished entry
// is written after every step succeeded, in a fenced transaction of its own;
// a failure is recorded as Errored, unless the run was
// interrupted.
//
// plan writes nothing: it reads each collection's indexes with listIndexes
// and works out the diff, step by step, failing on a step that MongoDB would
// reject.
//
// Either way, each step's change is logged and goes into row.Indexes.
func (r *Run) runIndex(m migration.Migration, apply bool, log *slog.Logger, row *Row) error {
	if err := r.lock.SetCurrent(r.ctx, m.ID); err != nil {
		return r.lockErr(err)
	}
	if !apply {
		return r.planIndex(m, log, row)
	}
	results := make([]StepResult, len(m.Steps))
	for i, s := range m.Steps {
		res, change, err := runIndexStep(r.ctx, r.db, s, r.checkLease, log.With("step", i))
		if err != nil {
			// When interrupted or the lock is lost, the server finishes or
			// abandons a build on its own.
			return r.stepFailed(m, i, s, err, true)
		}
		results[i] = res
		row.Indexes = append(row.Indexes, change)
	}
	return r.writeFinishedAlone(m, results)
}

// runIndexStep runs one createIndex, dropIndex, collModIndex or
// collModCollection step and returns what it did to the index, or to the
// collection's validation. A dropIndex of an index or a collection that
// doesn't exist succeeds; a collModIndex of one fails, and so does a
// collModCollection of a missing collection. A collModIndex or
// collModCollection that finds every value it sets already set sends
// nothing. check runs
// right before the createIndexes, dropIndexes or collMod command, which isn't
// sent when it fails.
func runIndexStep(ctx context.Context, db *mongo.Database, s migration.Step, check func() error, log *slog.Logger) (StepResult, IndexChange, error) {
	out := StepResult{Collection: s.Collection, Operation: s.Operation}
	var change IndexChange
	majority := bson.E{Key: "writeConcern", Value: bson.D{{Key: "w", Value: "majority"}}}
	switch s.Operation {
	case "createIndex":
		spec, name, err := indexSpec(s)
		if err != nil {
			return out, change, err
		}
		want, err := migration.StepIndex(s)
		if err != nil {
			return out, change, err
		}
		out.IndexName = name
		if err := check(); err != nil {
			return out, change, err
		}
		var res struct {
			Before int    `bson:"numIndexesBefore"`
			After  int    `bson:"numIndexesAfter"`
			Note   string `bson:"note"`
		}
		err = db.RunCommand(ctx, bson.D{
			{Key: "createIndexes", Value: s.Collection},
			{Key: "indexes", Value: bson.A{spec}},
			majority,
		}).Decode(&res)
		if err != nil {
			return out, change, err
		}
		if res.Before == res.After {
			log.Info("index already present", "collection", s.Collection, "index", name)
			change = indexChange(IndexPresent, s.Collection, want)
		} else {
			log.Info("index created", "collection", s.Collection, "index", name)
			change = indexChange(IndexCreate, s.Collection, want)
		}
	case "dropIndex":
		out.IndexName = s.Name
		// MongoDB reports success for a missing index on an existing
		// collection, so look first to log what happened.
		indexes, err := listIndexes(ctx, db.Collection(s.Collection))
		if err != nil {
			return out, change, fmt.Errorf("list indexes: %w", err)
		}
		absent := indexChange(IndexAbsent, s.Collection, migration.Index{Name: s.Name})
		j := slices.IndexFunc(indexes, func(ix migration.Index) bool { return ix.Name == s.Name })
		if j < 0 {
			log.Info("index already absent", "collection", s.Collection, "index", s.Name)
			return out, absent, nil
		}
		if err := check(); err != nil {
			return out, change, err
		}
		err = db.RunCommand(ctx, bson.D{
			{Key: "dropIndexes", Value: s.Collection},
			{Key: "index", Value: s.Name},
			majority,
		}).Err()
		switch {
		case err == nil:
			log.Info("index dropped", "collection", s.Collection, "index", s.Name)
			change = indexChange(IndexDrop, s.Collection, indexes[j])
		case isCode(err, codeIndexNotFound, codeNamespaceNotFound):
			log.Info("index already absent", "collection", s.Collection, "index", s.Name)
			change = absent
		default:
			return out, change, err
		}
	case "collModIndex":
		out.IndexName, out.Set = s.Name, s.Set
		set, err := migration.StepSet(s)
		if err != nil {
			return out, change, err
		}
		// Look first, to report the values before and skip a no-op.
		indexes, err := listIndexes(ctx, db.Collection(s.Collection))
		if err != nil {
			return out, change, fmt.Errorf("list indexes: %w", err)
		}
		j := slices.IndexFunc(indexes, func(ix migration.Index) bool { return ix.Name == s.Name })
		if j < 0 {
			return out, change, missingIndexError(s.Collection, s.Name)
		}
		change = collModChange(IndexPresent, s.Collection, indexes[j], set)
		if indexes[j].Unchanged(set) {
			log.Info("index options already set", "collection", s.Collection, "index", s.Name, "set", extJSON(set))
			return out, change, nil
		}
		if err := check(); err != nil {
			return out, change, err
		}
		err = db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: s.Collection},
			{Key: "index", Value: append(bson.D{{Key: "name", Value: s.Name}}, set...)},
			majority,
		}).Err()
		if err != nil {
			return out, change, err
		}
		change.Action = IndexModify
		log.Info("index modified", "collection", s.Collection, "index", s.Name,
			"before", extJSON(indexes[j].Current(set)), "set", extJSON(set))
	case "collModCollection":
		out.Set = s.Set
		change, err := runCollModCollection(ctx, db, s, check, log)
		return out, change, err
	default:
		return out, change, fmt.Errorf("operation %q is not an index operation", s.Operation)
	}
	return out, change, nil
}

// collModChange describes what a collModIndex setting set does to have, an
// index of coll, for the report.
func collModChange(action IndexAction, coll string, have migration.Index, set bson.D) IndexChange {
	c := IndexChange{Action: action, Collection: coll, Name: have.Name, Set: rawExtJSON(set), Before: rawExtJSON(have.Current(set))}
	if len(have.Keys) > 0 {
		c.Keys = rawExtJSON(have.Keys)
	}
	return c
}

// missingIndexError is the failure of a collModIndex whose index doesn't
// exist.
func missingIndexError(coll, name string) error {
	return fmt.Errorf("index %s doesn't exist on %s, so there is nothing to modify: create it in an earlier migration, or fix the name", name, coll)
}

// indexChange describes what a step does to ix, an index of coll, for the
// report.
func indexChange(action IndexAction, coll string, ix migration.Index) IndexChange {
	c := IndexChange{Action: action, Collection: coll, Name: ix.Name}
	if len(ix.Keys) > 0 {
		c.Keys = rawExtJSON(ix.Keys)
	}
	if len(ix.Options) > 0 {
		c.Options = rawExtJSON(ix.Options)
	}
	return c
}

// indexSpec returns the createIndexes entry for a createIndex step: its keys
// followed by its options as written, and the index name. The schema requires
// options.name.
func indexSpec(s migration.Step) (bson.D, string, error) {
	name, ok := s.Options.Lookup("name").StringValueOK()
	if !ok || name == "" {
		return nil, "", errors.New("createIndex must name its index in options.name")
	}
	elems, err := s.Options.Elements()
	if err != nil {
		return nil, "", fmt.Errorf("options: %w", err)
	}
	spec := bson.D{{Key: "key", Value: s.Keys}}
	for _, e := range elems {
		spec = append(spec, bson.E{Key: e.Key(), Value: e.Value()})
	}
	return spec, name, nil
}

func isCode(err error, codes ...int) bool {
	var se mongo.ServerError
	if !errors.As(err, &se) {
		return false
	}
	return slices.ContainsFunc(codes, se.HasErrorCode)
}

func listIndexes(ctx context.Context, coll *mongo.Collection) ([]migration.Index, error) {
	cur, err := coll.Indexes().List(ctx)
	if isCode(err, codeNamespaceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var specs []bson.D
	if err := cur.All(ctx, &specs); err != nil {
		if isCode(err, codeNamespaceNotFound) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]migration.Index, 0, len(specs))
	for _, spec := range specs {
		out = append(out, migration.SpecIndex(coll.Name(), spec))
	}
	return out, nil
}
