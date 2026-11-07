package runner

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// mergeIndex is the unique index a $merge on fields other than _id matches
// through.
type mergeIndex struct {
	// Name is the index's name; empty when on is only _id.
	Name string
	// Collation is the collation the index and the pipeline share, as
	// relaxed Extended JSON, or "" for the simple collation.
	Collation string
}

// checkMergeIndex finds the unique index $merge needs on the target of a
// merge step whose on fields aren't only _id, before anything is written.
// Without one, $merge fails with error 51183 when it runs, whatever the
// pipeline outputs. MongoDB accepts an index that is unique, not partial,
// has exactly the on fields as its keys, in any order and direction, and
// has the collation the pipeline runs with, pipeline: the default collation
// of the collection it reads, source, as migrune sets none. A sparse or
// hidden index qualifies.
//
// In plan, the target's indexes are those the pending index migrations
// planned so far would leave. A collation a pending migration gives as
// written can't always be compared with the one listCollections expands
// with every default: when the fields they both set agree, the index
// qualifies, with a warning that apply may fail.
func (r *Run) checkMergeIndex(source, pipeline string, t mergeTarget, apply bool, log *slog.Logger) (mergeIndex, error) {
	if len(t.On) == 1 && t.On[0] == "_id" {
		return mergeIndex{}, nil
	}
	indexes, err := r.mergeTargetIndexes(t.Into, apply)
	if err != nil {
		return mergeIndex{}, fmt.Errorf("list the indexes of %s: %w", t.Into, err)
	}
	var near []string
	for _, ix := range indexes {
		if !sameFieldSet(ix.Keys, t.On) {
			continue
		}
		switch {
		case ix.Option("unique") != "true":
			near = append(near, fmt.Sprintf("%s isn't unique", ix.Name))
			continue
		case ix.Option("partialFilterExpression") != "":
			near = append(near, fmt.Sprintf("%s is partial", ix.Name))
			continue
		}
		have := ix.Option("collation")
		switch same, sure := sameCollationSpec(have, pipeline); {
		case same && sure:
			return mergeIndex{Name: ix.Name, Collation: have}, nil
		case same:
			log.Warn("the unique index for $merge has a collation plan can't compare with the pipeline's: apply may fail (error 51183)",
				"into", t.Into, "index", ix.Name, "indexCollation", have, "pipelineCollation", migration.OrNone(pipeline))
			return mergeIndex{Name: ix.Name, Collation: pipeline}, nil
		default:
			near = append(near, fmt.Sprintf("%s has %s", ix.Name, collationName(have)))
		}
	}
	msg := fmt.Sprintf("$merge on %s needs a unique, non-partial index on exactly %s in %s, with %s, "+
		"which the pipeline runs with as the default collation of %s; there is none, so $merge would fail (error 51183). "+
		"Create it in an earlier index migration",
		strings.Join(t.On, ", "), keysPattern(t.On), t.Into, collationName(pipeline), source)
	if len(near) > 0 {
		msg += "; on these fields, index " + strings.Join(near, ", index ")
	}
	return mergeIndex{}, fmt.Errorf("%s", msg)
}

// defaultCollation returns the default collation of coll, as relaxed
// Extended JSON, or "" for the simple collation or a collection that doesn't
// exist.
func (r *Run) defaultCollation(coll string) (string, error) {
	specs, err := r.db.ListCollectionSpecifications(r.ctx, bson.D{{Key: "name", Value: coll}})
	if err != nil || len(specs) == 0 {
		return "", err
	}
	v, err := specs[0].Options.LookupErr("collation")
	if err != nil {
		return "", nil
	}
	var collation bson.D
	if err := v.Unmarshal(&collation); err != nil {
		return "", err
	}
	return migration.Index{Options: bson.D{{Key: "collation", Value: collation}}}.Option("collation"), nil
}

// mergeTargetIndexes returns the indexes of coll: in plan, as the pending
// index migrations planned so far would leave them.
func (r *Run) mergeTargetIndexes(coll string, apply bool) ([]migration.Index, error) {
	if !apply {
		if indexes, ok := r.planIndexes[coll]; ok {
			return indexes, nil
		}
	}
	return listIndexes(r.ctx, r.db.Collection(coll))
}

// sameFieldSet reports whether keys has exactly the fields in on, in any
// order.
func sameFieldSet(keys bson.D, on []string) bool {
	if len(keys) != len(on) {
		return false
	}
	for _, k := range keys {
		if !slices.Contains(on, k.Key) {
			return false
		}
	}
	return true
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

// collationName describes a collation rendered by migration.Index.Option.
func collationName(c string) string {
	if c == "" {
		return "the simple collation"
	}
	return "the collation " + c
}

// keysPattern renders on as an index key pattern, e.g. {"sku": 1}.
func keysPattern(on []string) string {
	parts := make([]string, len(on))
	for i, f := range on {
		parts[i] = fmt.Sprintf("%q: 1", f)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// collationOnlyMatches counts the output documents of pre, the pipeline of
// a merge step without its $merge stage, that have no exact match in the
// target but match one through ix's non-simple collation, such as "ABC"
// against "abc" under a case-insensitive one. The comparison counts them as
// unmatched, but MongoDB 8.3 and later merge them (see mergesThroughCollation).
//
// The pipeline runs without a collation option, so with the source's default
// collation, which ix shares, as $merge runs it: the $lookup then matches
// through that collation. A match is exact when each on value is equal under
// the simple collation: strings byte for byte, other values with $eq, which
// a collation doesn't change, except for strings nested in an embedded
// document, which then count as exact.
func (r *Run) collationOnlyMatches(source string, pre []bson.Raw, t mergeTarget, ix mergeIndex, log *slog.Logger) (int64, error) {
	var out []struct {
		N int64 `bson:"n"`
	}
	pipeline := append(toPipeline(pre), collationOnlyStages(t)...)
	err := r.aggregate(source, pipeline, log, func(ctx context.Context, cur *mongo.Cursor) error {
		return cur.All(ctx, &out)
	})
	if err != nil || len(out) == 0 {
		return 0, err
	}
	n := out[0].N
	if n == 0 {
		return 0, nil
	}
	version, err := r.serverVersion()
	if err != nil {
		return n, err
	}
	effect := "discards them, as plan counts them: unmatched"
	if mergesThroughCollation(version) {
		effect = "merges them, rewriting the target's on fields to the output's values, though plan counts them as unmatched"
	}
	log.Warn("output documents match a target only through the collation of the unique index on the on fields: MongoDB "+version+" "+effect+
		"; output the on values as the target stores them, or give the index the simple collation",
		"into", t.Into, "on", strings.Join(t.On, ","), "index", ix.Name, "collation", ix.Collation,
		"collationOnlyMatchCount", n, "serverVersion", version)
	return n, nil
}

// collationOnlyStages are the stages collationOnlyMatches appends to a merge
// pipeline without its $merge stage: they look up each output document's
// target as $merge matches it, and count those whose on values differ from
// the target's under the simple collation.
func collationOnlyStages(t mergeTarget) []any {
	type m = bson.M
	type a = bson.A
	let := bson.D{}
	match := a{}
	project := bson.D{{Key: "_id", Value: 0}}
	exact := a{}
	for i, f := range t.On {
		v := fmt.Sprintf("k%d", i)
		let = append(let, bson.E{Key: v, Value: "$o." + f})
		match = append(match, m{"$eq": a{"$" + f, "$$" + v}})
		project = append(project, bson.E{Key: v, Value: "$" + f})
		exact = append(exact, simpleEqual("$o."+f, m{"$first": "$t." + v}))
	}
	return []any{
		m{"$replaceWith": m{"o": "$$ROOT"}},
		m{"$lookup": bson.D{
			{Key: "from", Value: t.Into},
			{Key: "let", Value: let},
			{Key: "pipeline", Value: a{
				m{"$match": m{"$expr": m{"$and": match}}},
				m{"$limit": 1},
				m{"$project": project},
			}},
			{Key: "as", Value: "t"},
		}},
		m{"$match": m{"t.0": m{"$exists": true}}},
		m{"$match": m{"$expr": m{"$not": a{m{"$and": exact}}}}},
		m{"$count": "n"},
	}
}

// simpleEqual is an expression telling whether x and y are equal under the
// simple collation, whatever the pipeline's: two strings are compared byte
// for byte with $strLenBytes and $indexOfBytes, which no collation changes;
// other values with $eq.
func simpleEqual(x, y any) bson.M {
	type m = bson.M
	type a = bson.A
	isString := func(v any) m { return m{"$eq": a{m{"$type": v}, "string"}} }
	return m{"$cond": a{
		m{"$and": a{isString(x), isString(y)}},
		m{"$and": a{
			m{"$eq": a{m{"$strLenBytes": x}, m{"$strLenBytes": y}}},
			m{"$eq": a{m{"$indexOfBytes": a{x, y}}, 0}},
		}},
		m{"$eq": a{x, y}},
	}}
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

// mergesThroughCollation reports whether $merge on MongoDB version, as
// buildInfo reports it, matches an output document with a target whose on
// values are equal only under the collation of the unique index on the on
// fields: from 8.3 it does, and merges it; 7.0 to 8.2 discard it, as tested
// on 7.0.45, 8.0.34, 8.2.12, 8.3.13 and 9.0.4. Before 8.3, $merge didn't
// pass the collation on to its updates (SERVER-115813, fixed in 8.3.0 with no
// backport listed). A version that doesn't parse is taken as a recent one.
func mergesThroughCollation(version string) bool {
	var major, minor int
	if _, err := fmt.Sscanf(version, "%d.%d", &major, &minor); err != nil {
		return true
	}
	return major > 8 || major == 8 && minor >= 3
}
