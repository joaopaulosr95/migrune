package runner

import (
	"fmt"
	"log/slog"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// planIndex logs and adds to row.Indexes, step by step, how m would change the
// indexes as the pending migrations before it would leave them: it reads each
// collection's indexes with listIndexes once per run, then applies every
// planned step to them, so a migration sees what earlier pending ones create
// and drop. It writes nothing. A step MongoDB would reject (a name or key
// pattern already used by a different index) fails, and its change carries
// the reason.
func (r *Run) planIndex(m migration.Migration, log *slog.Logger, row *Row) error {
	if r.planIndexes == nil {
		r.planIndexes = map[string][]migration.Index{}
	}
	live := r.planIndexes
	for i, s := range m.Steps {
		fail := func(err error) error {
			return &StepFailure{Step: i, Operation: s.Operation, Collection: s.Collection, Err: err}
		}
		indexes, ok := live[s.Collection]
		if !ok {
			var err error
			if indexes, err = listIndexes(r.ctx, r.db.Collection(s.Collection)); err != nil {
				if rerr := r.Err(); rerr != nil {
					return rerr
				}
				return fail(fmt.Errorf("list indexes: %w", err))
			}
		}
		stepLog := log.With("step", i, "collection", s.Collection)
		switch s.Operation {
		case "createIndex":
			want, err := migration.StepIndex(s)
			if err != nil {
				return fail(err)
			}
			version, err := r.serverVersion()
			if err != nil {
				if rerr := r.Err(); rerr != nil {
					return rerr
				}
				return fail(err)
			}
			action, warning, err := planCreate(want, indexes, comparesHidden(version))
			if err != nil {
				c := indexChange(IndexCreate, s.Collection, want)
				c.Conflict = err.Error()
				row.Indexes = append(row.Indexes, c)
				return fail(err)
			}
			c := indexChange(action, s.Collection, want)
			c.Warning = warning
			if action == IndexCreate {
				if c.WouldExpire, err = r.wouldExpire(r.db.Collection(s.Collection), want); err != nil {
					if rerr := r.Err(); rerr != nil {
						return rerr
					}
					return fail(fmt.Errorf("count the documents the TTL index would delete: %w", err))
				}
			}
			row.Indexes = append(row.Indexes, c)
			switch action {
			case IndexPresent:
				stepLog.Info("index already present", "index", want.Name, "keys", extJSON(want.Keys))
			case IndexDiffers:
				stepLog.Warn("index already present with a collation plan can't compare: apply may fail", "index", want.Name, "warning", warning)
			default:
				attrs := []any{"index", want.Name, "keys", extJSON(want.Keys)}
				if len(want.Options) > 0 {
					attrs = append(attrs, "options", extJSON(want.Options))
				}
				if warning != "" {
					attrs = append(attrs, "warning", warning)
				}
				if c.WouldExpire != nil {
					attrs = append(attrs, "wouldExpire", *c.WouldExpire)
				}
				stepLog.Info("index to create", attrs...)
				if c.WouldExpire != nil && *c.WouldExpire > 0 {
					stepLog.Warn("the TTL index would delete documents as soon as it is built", "index", want.Name, "wouldExpire", *c.WouldExpire)
				}
				indexes = append(indexes, want)
			}
		case "dropIndex":
			j := slices.IndexFunc(indexes, func(ix migration.Index) bool { return ix.Name == s.Name })
			if j < 0 {
				stepLog.Info("index already absent", "index", s.Name)
				row.Indexes = append(row.Indexes, indexChange(IndexAbsent, s.Collection, migration.Index{Name: s.Name}))
				break
			}
			stepLog.Info("index to drop", "index", s.Name, "keys", extJSON(indexes[j].Keys))
			row.Indexes = append(row.Indexes, indexChange(IndexDrop, s.Collection, indexes[j]))
			indexes = slices.Delete(indexes, j, j+1)
		default:
			return fail(fmt.Errorf("operation %q is not an index operation", s.Operation))
		}
		live[s.Collection] = indexes
	}
	return nil
}

// planCreate returns what a createIndex of want would do while indexes exist
// on its collection: IndexPresent when an index with its name and definition
// exists, IndexCreate when it would be created. It fails when MongoDB would
// reject the createIndex: the name is taken by a different definition
// (IndexKeySpecsConflict), or another index has the same keys and distinct
// options (IndexOptionsConflict).
//
// listIndexes expands a collation with every default, and plan doesn't guess
// them: a field both collations set with different values is a different
// collation, but one that agrees on every shared field and lacks some of the
// existing one's can't be compared. The step is then IndexDiffers, or
// IndexCreate, with a warning that apply may fail.
//
// hidden tells whether the server compares hidden for an index of the same
// name (see comparesHidden): if not, an index that differs only by hidden is
// IndexPresent, and keeps its own.
func planCreate(want migration.Index, indexes []migration.Index, hidden bool) (IndexAction, string, error) {
	skip := []string{"collation"}
	if !hidden {
		skip = append(skip, "hidden")
	}
	for _, ix := range indexes {
		if ix.Name != want.Name {
			continue
		}
		if diff := want.DefinitionDiff(ix, skip...); diff != "" {
			return "", "", fmt.Errorf("index %s already exists with a different definition (%s): "+
				"MongoDB would reject the createIndex (IndexKeySpecsConflict); pick another name, or drop it in an earlier migration", want.Name, diff)
		}
		if same, sure := sameCollation(want, ix); !sure {
			return IndexDiffers, fmt.Sprintf("index %s exists with the collation %s, and the step gives %s: "+
				"plan can't tell whether MongoDB sees them as the same, so apply may fail (IndexKeySpecsConflict)",
				want.Name, migration.OrNone(ix.Option("collation")), migration.OrNone(want.Option("collation"))), nil
		} else if !same {
			return "", "", fmt.Errorf("index %s already exists with a different definition (collation %s, existing %s): "+
				"MongoDB would reject the createIndex (IndexKeySpecsConflict); pick another name, or drop it in an earlier migration",
				want.Name, migration.OrNone(want.Option("collation")), migration.OrNone(ix.Option("collation")))
		}
		return IndexPresent, "", nil
	}
	for _, ix := range indexes {
		if !want.SameDistinct(ix, "collation") {
			continue
		}
		switch same, sure := sameCollation(want, ix); {
		case !sure:
			return IndexCreate, fmt.Sprintf("index %s has the same keys and options as the existing index %s, but for a collation "+
				"plan can't compare (%s here, %s there): if MongoDB sees them as the same, apply fails (IndexOptionsConflict)",
				want.Name, ix.Name, migration.OrNone(want.Option("collation")), migration.OrNone(ix.Option("collation"))), nil
		case same:
			return "", "", fmt.Errorf("index %s has the same keys and options as the existing index %s: "+
				"MongoDB would reject the createIndex (IndexOptionsConflict); reuse %s, or drop it in an earlier migration", want.Name, ix.Name, ix.Name)
		}
	}
	return IndexCreate, "", nil
}

// comparesHidden reports whether a server of this version rejects a
// createIndex whose index exists under the same name and differs only by
// hidden (IndexOptionsConflict), as 8.0 and later do. 7.0 takes it as the
// existing index, which keeps its hidden. An index under another name
// conflicts on every version.
func comparesHidden(version string) bool {
	var major int
	if _, err := fmt.Sscanf(version, "%d.", &major); err != nil {
		return true
	}
	return major >= 8
}

// wouldExpire counts the documents of coll that the TTL index ix would delete
// once built: those whose indexed date is older than now less its
// expireAfterSeconds, among those its partialFilterExpression selects. It
// returns nil when ix isn't a TTL index: no expireAfterSeconds, or more than
// one key, which MongoDB never expires.
func (r *Run) wouldExpire(coll *mongo.Collection, ix migration.Index) (*int64, error) {
	var (
		ttl     float64
		isTTL   bool
		partial any
	)
	for _, e := range ix.Options {
		switch e.Key {
		case "expireAfterSeconds":
			switch v := e.Value.(type) {
			case int32:
				ttl, isTTL = float64(v), true
			case int64:
				ttl, isTTL = float64(v), true
			case float64:
				ttl, isTTL = v, true
			}
		case "partialFilterExpression":
			partial = e.Value
		}
	}
	if !isTTL || len(ix.Keys) != 1 {
		return nil, nil
	}
	// A $lt on a date matches dates only, and arrays holding an earlier one,
	// as the TTL monitor expires them.
	cutoff := time.Now().Add(-time.Duration(ttl * float64(time.Second)))
	filter := bson.D{{Key: ix.Keys[0].Key, Value: bson.D{{Key: "$lt", Value: cutoff}}}}
	if partial != nil {
		filter = bson.D{{Key: "$and", Value: bson.A{filter, partial}}}
	}
	n, err := coll.CountDocuments(r.ctx, filter)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// sameCollation compares want's collation with have's, as listIndexes
// returns it, as sameCollationSpec does: a field both set with different
// values is a different collation; sure is false when they agree on every
// field both set but one sets fields the other doesn't, which may only be
// defaults listIndexes filled in.
func sameCollation(want, have migration.Index) (same, sure bool) {
	return sameCollationSpec(want.Option("collation"), have.Option("collation"))
}

// sameCollationSpec compares two collations rendered by
// migration.Index.Option ("" for the simple collation). sure is false when
// they agree on every field they both set but one sets fields the other
// doesn't: listIndexes and listCollections fill in every default, while a
// collation from a migration file lists only what it sets.
func sameCollationSpec(a, b string) (same, sure bool) {
	if a == b {
		return true, true
	}
	if a == "" || b == "" {
		return false, true
	}
	var ma, mb map[string]any
	if bson.UnmarshalExtJSON([]byte(a), false, &ma) != nil || bson.UnmarshalExtJSON([]byte(b), false, &mb) != nil {
		return false, false
	}
	for k, v := range ma {
		if w, ok := mb[k]; ok && migration.Canonical(v) != migration.Canonical(w) {
			return false, true
		}
	}
	return true, false
}

// serverVersion returns the MongoDB version buildInfo reports, read once per
// run.
func (r *Run) serverVersion() (string, error) {
	if r.version != "" {
		return r.version, nil
	}
	var info struct {
		Version string `bson:"version"`
	}
	if err := r.db.RunCommand(r.ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil {
		return "", fmt.Errorf("read the MongoDB server version: %w", err)
	}
	r.version = info.Version
	return r.version, nil
}
