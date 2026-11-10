package runner

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// planIndex logs and adds to row.Indexes, step by step, how m would change the
// indexes, and the validation of collections, as the pending migrations
// before it would leave them: it reads each collection's indexes with
// listIndexes (or, for a collModCollection, its options with listCollections)
// once per run, then applies every planned step to them, so a migration sees
// what earlier pending ones create, change and drop. It writes nothing. A
// step MongoDB would reject (a name or key pattern already used by a
// different index, a collModIndex of a missing index, a collModCollection of
// a missing collection or a view, or either that the database user lacks the
// collMod action for) fails, and its change carries the reason.
func (r *Run) planIndex(m migration.Migration, log *slog.Logger, row *Row) error {
	if r.planIndexes == nil {
		r.planIndexes = map[string][]migration.Index{}
		r.serverIndexes = map[string][]migration.Index{}
	}
	live := r.planIndexes
	for i, s := range m.Steps {
		fail := func(err error) error {
			return &StepFailure{Step: i, Operation: s.Operation, Collection: s.Collection, Err: err}
		}
		stepLog := log.With("step", i, "collection", s.Collection)
		if s.Operation == "collModCollection" {
			c, err := r.planCollModCollection(s, stepLog)
			if c != nil {
				row.Indexes = append(row.Indexes, *c)
			}
			if err != nil {
				if rerr := r.Err(); rerr != nil {
					return rerr
				}
				return fail(err)
			}
			continue
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
			r.serverIndexes[s.Collection] = slices.Clone(indexes)
		}
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
				if len(indexes) == 0 {
					r.planCreates(s.Collection)
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
		case "collModIndex":
			set, err := migration.StepSet(s)
			if err != nil {
				return fail(err)
			}
			conflict := func(c IndexChange, err error) error {
				c.Conflict = err.Error()
				row.Indexes = append(row.Indexes, c)
				return fail(err)
			}
			j := slices.IndexFunc(indexes, func(ix migration.Index) bool { return ix.Name == s.Name })
			if j < 0 {
				return conflict(IndexChange{Action: IndexModify, Collection: s.Collection, Name: s.Name, Set: rawExtJSON(set)},
					missingIndexError(s.Collection, s.Name))
			}
			have := indexes[j]
			c := collModChange(IndexModify, s.Collection, have, set)
			if have.Unchanged(set) {
				c.Action = IndexPresent
				row.Indexes = append(row.Indexes, c)
				stepLog.Info("index options already set", "index", s.Name, "set", extJSON(set))
				break
			}
			if err := collModRejected(have, set); err != nil {
				return conflict(c, err)
			}
			switch ok, err := r.collModAllowed(s.Collection); {
			case err != nil:
				if rerr := r.Err(); rerr != nil {
					return rerr
				}
				return fail(err)
			case !ok:
				return conflict(c, fmt.Errorf("the database user lacks the collMod action on %s.%s, which readWrite doesn't grant, "+
					"so apply would fail: grant it with a role of its own", r.db.Name(), s.Collection))
			}
			after := have.Modified(set)
			if lowersTTL(have, set) {
				if c.WouldExpire, err = r.wouldExpire(r.db.Collection(s.Collection), after); err != nil {
					if rerr := r.Err(); rerr != nil {
						return rerr
					}
					return fail(fmt.Errorf("count the documents the TTL would delete: %w", err))
				}
			}
			if hasOption(set, "prepareUnique") {
				c.Warning = fmt.Sprintf("once index %s is prepareUnique, MongoDB rejects every insert and update that would add a duplicate key to it: "+
					"make sure the application doesn't write any", s.Name)
			}
			if hasOption(set, "unique") && have.Option("unique") == "" {
				warning, dup, err := r.uniqueDryRun(s.Collection, have)
				if err != nil {
					if rerr := r.Err(); rerr != nil {
						return rerr
					}
					return fail(err)
				}
				if dup != nil {
					return conflict(c, dup)
				}
				c.Warning = warning
			}
			row.Indexes = append(row.Indexes, c)
			attrs := []any{"index", s.Name, "before", extJSON(have.Current(set)), "set", extJSON(set)}
			if c.WouldExpire != nil {
				attrs = append(attrs, "wouldExpire", *c.WouldExpire)
			}
			stepLog.Info("index to modify", attrs...)
			if c.WouldExpire != nil && *c.WouldExpire > 0 {
				stepLog.Warn("the new TTL would delete documents at the TTL monitor's next pass", "index", s.Name, "wouldExpire", *c.WouldExpire)
			}
			if c.Warning != "" {
				stepLog.Warn(c.Warning, "index", s.Name)
			}
			indexes[j] = after
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

// collModRejected returns why MongoDB would reject a collMod setting set on
// have, which exists and isn't left unchanged by it, or nil.
func collModRejected(have migration.Index, set bson.D) error {
	switch {
	case hasOption(set, "expireAfterSeconds") && have.IsID():
		return fmt.Errorf("the _id index can't have a TTL: MongoDB would reject the collMod (InvalidOptions)")
	case hasOption(set, "expireAfterSeconds") && len(have.Keys) != 1:
		return fmt.Errorf("index %s has the keys %s, and a TTL index has a single field: MongoDB would reject the collMod (InvalidOptions)",
			have.Name, have.KeysString())
	case setsTrue(set, "hidden") && have.IsID():
		return fmt.Errorf("the _id index can't be hidden: MongoDB would reject the collMod (BadValue)")
	case hasOption(set, "unique") && have.Option("unique") == "" && have.Option("prepareUnique") == "":
		return fmt.Errorf("index %s isn't prepareUnique: MongoDB converts an index to unique only once a collMod set prepareUnique on it "+
			"(InvalidOptions). Set prepareUnique in an earlier migration, remove the duplicates, then set unique", have.Name)
	}
	return nil
}

// collModAllowed reports whether the authenticated user may run collMod on
// coll, which readWrite doesn't allow, from the privileges connectionStatus
// lists. With no user authenticated, as on a server without access control,
// every command is allowed.
func (r *Run) collModAllowed(coll string) (bool, error) {
	var res struct {
		AuthInfo struct {
			Users      []bson.Raw `bson:"authenticatedUsers"`
			Privileges []struct {
				// DB and Collection are nil for a cluster resource.
				Resource struct {
					DB          *string `bson:"db"`
					Collection  *string `bson:"collection"`
					AnyResource bool    `bson:"anyResource"`
				} `bson:"resource"`
				Actions []string `bson:"actions"`
			} `bson:"authenticatedUserPrivileges"`
		} `bson:"authInfo"`
	}
	err := r.db.RunCommand(r.ctx, bson.D{{Key: "connectionStatus", Value: 1}, {Key: "showPrivileges", Value: true}}).Decode(&res)
	if err != nil {
		return false, fmt.Errorf("read the database user's privileges (connectionStatus): %w", err)
	}
	if len(res.AuthInfo.Users) == 0 {
		return true, nil
	}
	for _, p := range res.AuthInfo.Privileges {
		rs := p.Resource
		// An empty db or collection means any.
		on := rs.AnyResource || rs.DB != nil && rs.Collection != nil &&
			(*rs.DB == "" || *rs.DB == r.db.Name()) && (*rs.Collection == "" || *rs.Collection == coll)
		if on && slices.Contains(p.Actions, "collMod") {
			return true, nil
		}
	}
	return false, nil
}

// maxDuplicateSamples is how many duplicate keys a failed unique conversion
// shows.
const maxDuplicateSamples = 5

// uniqueDryRun checks whether MongoDB would convert have, an index of coll
// that plan sees as prepareUnique, to unique: collMod's dryRun lists the
// documents that share a key. dup describes the duplicates, if any, with
// their count and a few of their keys. When the server's index isn't
// prepareUnique yet, because an earlier pending step sets it, dryRun can't
// run: warning says so, and apply finds out.
func (r *Run) uniqueDryRun(coll string, have migration.Index) (warning string, dup, err error) {
	server := r.serverIndexes[coll]
	j := slices.IndexFunc(server, func(ix migration.Index) bool { return ix.Name == have.Name })
	if j < 0 || server[j].Option("prepareUnique") == "" || server[j].KeysString() != have.KeysString() {
		return fmt.Sprintf("plan can't check index %s for duplicate keys: an earlier pending step sets prepareUnique on it, "+
			"and MongoDB checks only once it is set. apply fails if the collection has any then", have.Name), nil, nil
	}
	err = r.db.RunCommand(r.ctx, bson.D{
		{Key: "collMod", Value: coll},
		{Key: "index", Value: bson.D{{Key: "name", Value: have.Name}, {Key: "unique", Value: true}}},
		{Key: "dryRun", Value: true},
	}).Err()
	var ce mongo.CommandError
	switch {
	case err == nil:
		return "", nil, nil
	case !errors.As(err, &ce) || ce.Code != codeCannotConvertIndexToUnique:
		return "", nil, fmt.Errorf("check index %s for duplicate keys (collMod dryRun): %w", have.Name, err)
	}
	var res struct {
		Violations []struct {
			IDs []any `bson:"ids"`
		} `bson:"violations"`
	}
	if err := bson.Unmarshal(ce.Raw, &res); err != nil {
		return "", nil, fmt.Errorf("read the duplicates of index %s: %w", have.Name, err)
	}
	docs := 0
	var samples []string
	for _, v := range res.Violations {
		docs += len(v.IDs)
		if len(samples) == maxDuplicateSamples || len(v.IDs) == 0 {
			continue
		}
		key, err := r.indexKey(coll, have, v.IDs[0])
		switch {
		case errors.Is(err, mongo.ErrNoDocuments):
			// Deleted since the dry run.
		case err != nil:
			return "", nil, fmt.Errorf("read a duplicate key of index %s: %w", have.Name, err)
		default:
			samples = append(samples, key)
		}
	}
	such := ""
	if len(samples) > 0 {
		such = ", such as " + strings.Join(samples, ", ")
	}
	return "", fmt.Errorf("index %s has %d duplicate %s, shared by %d documents%s: MongoDB would refuse to make it unique "+
		"(CannotConvertIndexToUnique). Remove the duplicates in a data migration first",
		have.Name, len(res.Violations), plural(len(res.Violations), "key"), docs, such), nil
}

// indexKey renders the fields of ix's keys of the document of coll whose _id
// is id, as relaxed Extended JSON.
func (r *Run) indexKey(coll string, ix migration.Index, id any) (string, error) {
	projection := bson.D{{Key: "_id", Value: 0}}
	for _, k := range ix.Keys {
		if k.Key == "_id" {
			projection = projection[1:]
		}
		projection = append(projection, bson.E{Key: k.Key, Value: 1})
	}
	var doc bson.Raw
	err := r.db.Collection(coll).FindOne(r.ctx, bson.D{{Key: "_id", Value: id}}, options.FindOne().SetProjection(projection)).Decode(&doc)
	if err != nil {
		return "", err
	}
	b, err := bson.MarshalExtJSON(doc, false, false)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// lowersTTL reports whether set adds a TTL to have or lowers it: the index
// would then delete documents it kept before.
func lowersTTL(have migration.Index, set bson.D) bool {
	want, ok := number(set, "expireAfterSeconds")
	if !ok {
		return false
	}
	cur, ok := number(have.Options, "expireAfterSeconds")
	return !ok || want < cur
}

func number(d bson.D, name string) (float64, bool) {
	for _, e := range d {
		if e.Key != name {
			continue
		}
		switch v := e.Value.(type) {
		case int32:
			return float64(v), true
		case int64:
			return float64(v), true
		case float64:
			return v, true
		}
	}
	return 0, false
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func hasOption(d bson.D, name string) bool {
	return slices.ContainsFunc(d, func(e bson.E) bool { return e.Key == name })
}

func setsTrue(d bson.D, name string) bool {
	return slices.ContainsFunc(d, func(e bson.E) bool { return e.Key == name && e.Value == true })
}
