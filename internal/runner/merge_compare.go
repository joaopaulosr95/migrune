package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// MaxMergeSamples is how many differing documents a merge step's comparison
// returns as samples.
const MaxMergeSamples = 5

// mergeTypeDepth is the depth of the nested type check in merge comparisons:
// how many levels below each top-level output field have their BSON types
// compared with the target's. 1 would compare the types of the field's direct
// children; deeper values are compared only by $eq and BSON size (see
// nestedTypeStages).
const mergeTypeDepth = 5

// mergeComparison is what compareMerge found.
type mergeComparison struct {
	// Diff counts the matched output documents with at least one field
	// that differs from the target's.
	Diff int64 `bson:"diff"`
	// Unmatched counts the output documents with no match in the target.
	Unmatched int64 `bson:"unmatched"`
	// TypeChanged counts the matched output documents with a field the
	// target has, not null, with another BSON type, or with a nested value
	// of another type in a field equal to the target's under $eq.
	TypeChanged int64 `bson:"typeChanged"`
	// DuplicateOn counts the output documents whose on fields are equal to
	// another output document's and match a target document; $merge
	// discards those with no match. Those with a null, missing or array on
	// field aren't counted.
	DuplicateOn      int64
	DuplicateMissing int64
	InvalidID        int64 `bson:"invalidID"`
	InvalidFieldName int64 `bson:"invalidFieldName"`
	// NullOn counts the output documents with an on field that is null or
	// missing.
	NullOn int64 `bson:"nullOn"`
	// ArrayOn counts the output documents with an on field that is an
	// array, which $merge rejects (error 51185), matched or not.
	ArrayOn int64 `bson:"arrayOn"`
	// IDMismatch counts the matched output documents whose _id differs from
	// their target's. Insert steps compare its stored BSON representation.
	IDMismatch int64 `bson:"idMismatch"`
	Samples    []MergeSample
}

// counts returns c's counts as a step's MergeCounts, without DiffCountAfter
// and CollationOnlyMatchCount, which other comparisons count.
func (c mergeComparison) counts() MergeCounts {
	return MergeCounts{DiffCountBefore: &c.Diff, UnmatchedCount: &c.Unmatched, TypeChangedCount: &c.TypeChanged,
		DuplicateOnCount: &c.DuplicateOn, NullOnCount: &c.NullOn, ArrayOnCount: &c.ArrayOn, IDMismatchCount: &c.IDMismatch}
}

type comparisonSample struct {
	Key   []bson.RawValue   `bson:"key"`
	Diffs []comparisonField `bson:"diffs"`
}

type comparisonField struct {
	Same       bool          `bson:"same"`
	Field      string        `bson:"field"`
	Before     bson.RawValue `bson:"before"`
	After      bson.RawValue `bson:"after"`
	BeforeType string        `bson:"beforeType"`
	AfterType  string        `bson:"afterType"`
	Changed    bool          `bson:"typeChanged"`
	Path       string        `bson:"path"`
	PathBefore string        `bson:"pathBeforeType"`
	PathAfter  string        `bson:"pathAfterType"`
}

// compareMerge runs pre, the pipeline of a merge step without its $merge
// stage, on coll, followed by a comparison with the target, and counts the
// output documents that match, differ or have invalid identities:
//
//   - an output document matches the target document whose on fields are
//     equal to its own; whenNotMatched chooses whether to insert or discard
//     an unmatched row;
//   - a matched document differs when one of its top-level fields differs
//     from the target's: missing from the target, another BSON type, or
//     another value. Fields absent from the output are ignored. Values are
//     compared whole, so a nested document or array differs when any part
//     of it does. Numbers are type-strict: int 1, long 1 and double 1.0
//     differ;
//   - a matched document has a type change when one of its fields exists in
//     the target, not null, with another BSON type. A missing or null field
//     isn't a type change, only a diff; a non-null field set to null is;
//   - it also has a type change when a field equal to the target's under $eq
//     (which compares numbers by value, whatever their type) has a nested
//     value of another BSON type, up to mergeTypeDepth levels down (see
//     nestedTypeStages): {n: long 5} against {n: double 5.0}; and when a
//     nested document lacks a key the target has, not null, which $merge
//     would drop: {a: {b}} against {a: {b, c}} is a change at a.c to type
//     "missing".
//
// Nested values are otherwise compared with $eq and their BSON size, since
// aggregation has no byte-wise comparison: below the type check's depth, a
// nested int and a nested long or double of the same value differ, while a
// nested long and a nested double of the same value compare equal.
//
// It also returns up to MaxMergeSamples differing documents, those with a type
// change first.
func (r *Run) compareMerge(coll string, pre []bson.Raw, t mergeTarget, log *slog.Logger) (mergeComparison, error) {
	var out mergeComparison
	var facet []struct {
		Counts     []mergeComparison `bson:"counts"`
		Duplicates []struct {
			Matched int64 `bson:"matched"`
		} `bson:"duplicates"`
		Samples []comparisonSample `bson:"samples"`
	}
	pipeline := append(toPipeline(pre), comparisonStages(t, mergeTypeDepth)...)
	// The simple collation compares strings byte by byte, whatever the
	// collection's default collation: under a case-insensitive one, $eq
	// would hide a merge that only changes case.
	simple := options.Aggregate().SetCollation(&options.Collation{Locale: "simple"})
	if t.Insert {
		return r.compareInsertRows(coll, pipeline, t, log, simple)
	}
	pipeline = append(pipeline, discardFacets())
	err := r.aggregate(coll, pipeline, log, func(ctx context.Context, cur *mongo.Cursor) error {
		return cur.All(ctx, &facet)
	}, simple)
	if err != nil {
		return out, err
	}
	if len(facet) != 1 {
		return out, fmt.Errorf("comparison returned %d documents, want 1", len(facet))
	}
	if len(facet[0].Counts) == 1 {
		out = facet[0].Counts[0]
	}
	if len(facet[0].Duplicates) == 1 {
		out.DuplicateOn = facet[0].Duplicates[0].Matched
	}
	for _, sample := range facet[0].Samples {
		out.Samples = append(out.Samples, r.mergeSample(sample, t))
	}
	return out, nil
}

func (r *Run) mergeSample(s comparisonSample, t mergeTarget) MergeSample {
	key := bson.D{}
	for j, f := range t.On {
		if j < len(s.Key) {
			key = append(key, bson.E{Key: f, Value: s.Key[j]})
		}
	}
	sm := MergeSample{Key: rawExtJSON(key)}
	for _, d := range s.Diffs {
		f := MergeField{Field: d.Field, BeforeType: d.BeforeType, AfterType: d.AfterType, TypeChanged: d.Changed}
		if d.Path != "" {
			f.Path, f.BeforeType, f.AfterType = d.Path, d.PathBefore, d.PathAfter
		}
		if r.sampleValues {
			var cutBefore, cutAfter bool
			f.After, cutAfter = sampleValue(d.After)
			if d.Before.Type != 0 {
				f.Before, cutBefore = sampleValue(d.Before)
			}
			f.Truncated = cutBefore || cutAfter
		}
		sm.Fields = append(sm.Fields, f)
	}
	return sm
}

// comparisonStages are the stages compareMerge appends to a merge pipeline
// without its $merge stage. Each output document is wrapped as o, so its own
// fields can't collide with the comparison's, and its target, if any, is
// looked up into t. depth is how many levels below each top-level field the
// nested type check goes (see nestedTypeStages).
func comparisonStages(t mergeTarget, depth int) []any {
	type m = bson.M
	type a = bson.A
	let := bson.D{}
	match := a{}
	key := a{}
	nullOn := a{}
	arrayOn := a{}
	for i, f := range t.On {
		v := fmt.Sprintf("k%d", i)
		let = append(let, bson.E{Key: v, Value: "$o." + f})
		match = append(match, m{"$eq": a{"$" + f, "$$" + v}})
		key = append(key, "$o."+f)
		nullOn = append(nullOn, m{"$in": a{m{"$type": "$o." + f}, a{"missing", "null"}}})
		arrayOn = append(arrayOn, m{"$isArray": "$o." + f})
	}
	// Discard on a business key may select a target with a different _id.
	// Insert steps compare the raw BSON IDs in the cursor.
	var idMismatch any = m{"$literal": false}
	if !slices.Contains(t.On, "_id") {
		idMismatch = m{"$and": a{
			m{"$gt": a{m{"$size": "$t"}, 0}},
			m{"$ne": a{m{"$type": "$o._id"}, "missing"}},
			m{"$ne": a{"$o._id", m{"$first": "$t._id"}}},
		}}
	}
	// The target's value of the output field f, or missing.
	target := m{"$first": m{"$filter": m{"input": "$ta", "as": "x", "cond": m{"$eq": a{"$$x.k", "$$f.k"}}}}}
	// equal: the same top-level type and equal under $eq, which compares
	// nested numbers by value whatever their type.
	equal := m{"$and": a{
		m{"$eq": a{m{"$type": "$$f.v"}, m{"$type": "$$tf.v"}}},
		m{"$eq": a{"$$f.v", "$$tf.v"}},
	}}
	// same: equal with the same BSON size too, which catches a nested int
	// against a long or double.
	same := m{"$and": a{
		equal,
		m{"$eq": a{m{"$bsonSize": m{"v": "$$f.v"}}, m{"$bsonSize": m{"v": "$$tf.v"}}}},
	}}
	// A target field that is missing or null takes any value without a type
	// change.
	typeChanged := m{"$and": a{
		m{"$not": a{m{"$in": a{m{"$type": "$$tf.v"}, a{"missing", "null"}}}}},
		m{"$ne": a{m{"$type": "$$f.v"}, m{"$type": "$$tf.v"}}},
	}}
	fields := m{"$map": m{
		"input": m{"$objectToArray": "$o"},
		"as":    "f",
		"in": m{"$let": m{
			"vars": m{"tf": target},
			"in": bson.D{
				{Key: "field", Value: "$$f.k"},
				{Key: "before", Value: "$$tf.v"},
				{Key: "after", Value: "$$f.v"},
				{Key: "beforeType", Value: m{"$type": "$$tf.v"}},
				{Key: "afterType", Value: m{"$type": "$$f.v"}},
				{Key: "typeChanged", Value: typeChanged},
				{Key: "equal", Value: equal},
				{Key: "same", Value: same},
			},
		}},
	}}
	// The first nested type change found in the field d, shallowest first,
	// or missing.
	nested := m{"$first": m{"$filter": m{"input": "$nt", "as": "x", "cond": m{"$eq": a{"$$x.f", "$$d.field"}}}}}
	found := m{"$ne": a{m{"$type": "$$n"}, "missing"}}
	ifFound := func(v any) any { return m{"$cond": a{found, v, "$$REMOVE"}} }
	var before, after any = "$$d.before", "$$d.after"
	keepField := m{"$not": a{"$$d.same"}}
	hasDiff := m{"$gt": a{m{"$size": "$diffs"}, 0}}
	if t.Insert {
		// Keep the ID's position in samples. Its potentially large BSON
		// values travel separately and the runner checks their exact bytes.
		isID := m{"$eq": a{"$$d.field", "_id"}}
		keepField = m{"$or": a{isID, keepField}}
		hasDiff = m{"$in": a{false, "$diffs.same"}}
		before = m{"$cond": a{isID, "$$REMOVE", before}}
		after = m{"$cond": a{isID, "$$REMOVE", after}}
	}
	diffs := m{"$map": m{
		"input": "$fs",
		"as":    "d",
		"in": m{"$let": m{
			"vars": m{"n": nested},
			"in": bson.D{
				{Key: "field", Value: "$$d.field"},
				{Key: "before", Value: before},
				{Key: "after", Value: after},
				{Key: "beforeType", Value: "$$d.beforeType"},
				{Key: "afterType", Value: "$$d.afterType"},
				{Key: "typeChanged", Value: m{"$or": a{"$$d.typeChanged", found}}},
				{Key: "path", Value: ifFound("$$n.p")},
				{Key: "pathBeforeType", Value: ifFound(m{"$type": "$$n.b"})},
				{Key: "pathAfterType", Value: ifFound(m{"$type": "$$n.a"})},
				{Key: "same", Value: m{"$and": a{"$$d.same", m{"$not": a{found}}}}},
			},
		}},
	}}
	project := m{
		"o":          1,
		"key":        key,
		"matched":    m{"$gt": a{m{"$size": "$t"}, 0}},
		"ta":         m{"$objectToArray": m{"$ifNull": a{m{"$first": "$t"}, m{}}}},
		"nullOn":     m{"$or": nullOn},
		"arrayOn":    m{"$or": arrayOn},
		"idMismatch": idMismatch,
	}
	if t.Insert {
		project["targetID"] = m{"$first": "$t._id"}
		project["invalidFieldName"] = m{"$anyElementTrue": a{m{"$map": m{
			"input": m{"$objectToArray": "$o"}, "as": "field",
			"in": m{"$or": a{m{"$eq": a{"$$field.k", ""}}, m{"$regexMatch": m{"input": "$$field.k", "regex": `^\$|\.`}}}},
		}}}}
	}
	stages := []any{
		m{"$replaceWith": m{"o": "$$ROOT"}},
		m{"$lookup": bson.D{
			{Key: "from", Value: t.Into},
			{Key: "let", Value: let},
			{Key: "pipeline", Value: a{
				m{"$match": m{"$expr": m{"$and": match}}},
				m{"$limit": 1},
			}},
			{Key: "as", Value: "t"},
		}},
		m{"$project": project},
		m{"$set": m{"fs": fields}},
		m{"$unset": a{"o", "ta"}},
	}
	stages = append(stages, nestedTypeStages(depth)...)
	return append(stages,
		m{"$set": m{"diffs": m{"$filter": m{"input": diffs, "as": "d", "cond": keepField}}}},
		m{"$unset": a{"fs", "nq", "nt"}},
		m{"$set": m{
			"diff":        m{"$and": a{"$matched", hasDiff}},
			"typeChanged": m{"$and": a{"$matched", m{"$anyElementTrue": a{"$diffs.typeChanged"}}}},
		}},
	)
}

func discardFacets() bson.M {
	type m = bson.M
	type a = bson.A
	counts := bson.D{
		{Key: "_id", Value: nil},
		{Key: "diff", Value: m{"$sum": m{"$cond": a{"$diff", 1, 0}}}},
		{Key: "unmatched", Value: m{"$sum": m{"$cond": a{"$matched", 0, 1}}}},
		{Key: "typeChanged", Value: m{"$sum": m{"$cond": a{"$typeChanged", 1, 0}}}},
		{Key: "nullOn", Value: m{"$sum": m{"$cond": a{"$nullOn", 1, 0}}}},
		{Key: "arrayOn", Value: m{"$sum": m{"$cond": a{"$arrayOn", 1, 0}}}},
		{Key: "idMismatch", Value: m{"$sum": m{"$cond": a{"$idMismatch", 1, 0}}}},
	}
	validKey := m{"nullOn": false, "arrayOn": false, "matched": true}
	perKey := m{"_id": "$key", "n": m{"$sum": 1}}
	totals := m{"_id": nil, "matched": m{"$sum": "$n"}}
	return m{"$facet": m{
		"counts": a{m{"$group": counts}},
		"duplicates": a{
			m{"$match": validKey},
			m{"$group": perKey},
			m{"$match": m{"n": m{"$gt": 1}}},
			m{"$group": totals},
		},
		"samples": a{
			m{"$match": m{"diff": true}},
			m{"$sort": m{"typeChanged": -1}},
			m{"$limit": MaxMergeSamples},
			m{"$project": m{"_id": 0, "key": 1, "diffs": 1}},
		},
	}}
}

// nestedTypeStages compare the BSON types inside the top-level fields whose
// output and target have the same top-level type, depth levels down. Level 0
// is the top-level field itself, whose type the field comparison already
// checks; level 1 its direct children (the fields of a document, the
// elements of an array); level n the children of level n-1. Below depth,
// values are compared only by $eq and BSON size.
//
// Aggregation has no recursion, so the walk is unrolled breadth first, one
// pair of $set stages per level: nq holds the pairs of output and target
// values still to look into, {f: top-level field, p: dotted path, a: output
// value, b: target value, e: a and b are equal under $eq}, and nt collects
// the pairs whose types differ.
//
// Children pair up by key: a document's field names, an array's indexes. A
// document is walked whether or not its value changed. An array is walked
// only when its value is unchanged: once elements are added, removed or
// moved, the same index no longer means the same element, so a changed array
// stays a plain diff. A target value that is missing or null takes any type,
// as at the top level; its children aren't compared. A target key the output
// document lacks pairs with a missing output value, so dropping a non-null
// nested field is a change to type "missing".
//
// For example, with the target
//
//	{price: {amount: int 10, tags: ["a"]}, address: {city: "L", street: "R"}}
//
// and the output
//
//	{price: {amount: long 10, tags: ["a"]}, address: {city: "L"}}
//
// the walk goes:
//
//	start   nq: price (equal under $eq), address (not equal, but a document)
//	level 1 children: price.amount, price.tags, address.city, address.street
//	        nt += price.amount (int -> long), address.street (string -> missing)
//	        nq: price.tags (an equal array); the rest aren't containers or differ
//	level 2 children: price.tags.0, string on both sides: nothing to add
//	        nq: empty, so levels 3 to 5 do nothing
//
// The levels can't run in parallel: each one's pairs are the children of the
// pairs the level before kept. Within a level, one expression handles every
// pair of the document at once. The cost per document is quadratic in a
// document's width (children pair by name with a linear search) and in the
// pairs per level ($reduce with $concatArrays copies as it grows); both are
// negligible next to the pipeline and its $lookup into the target, unless a
// document has hundreds of fields or large unchanged arrays.
func nestedTypeStages(depth int) []any {
	type m = bson.M
	type a = bson.A
	// kv lists the children of a document or array as {k, v} pairs; an
	// array's keys are its indexes.
	kv := func(v string) any {
		return m{"$cond": a{
			m{"$isArray": v},
			m{"$map": m{
				"input": m{"$range": a{0, m{"$size": v}}},
				"as":    "i",
				"in":    m{"k": m{"$toString": "$$i"}, "v": m{"$arrayElemAt": a{v, "$$i"}}},
			}},
			m{"$objectToArray": v},
		}}
	}
	// The children of the pair x: one per output key, with the target's
	// value under the same key, then one per target key the output lacks,
	// with a missing output value, so a nested field the output drops shows
	// up as a change to type "missing". Arrays are only walked when equal,
	// so they never lack an index.
	children := m{"$let": m{
		"vars": m{"x": "$$this", "ka": kv("$$this.a"), "kb": kv("$$this.b")},
		"in": m{"$concatArrays": a{
			m{"$map": m{
				"input": "$$ka",
				"as":    "ea",
				"in": m{"$let": m{
					// ponytail: a linear search per child, quadratic in a
					// document's width; fine for the documents merges handle.
					"vars": m{"eb": m{"$first": m{"$filter": m{"input": "$$kb", "as": "y", "cond": m{"$eq": a{"$$y.k", "$$ea.k"}}}}}},
					"in": bson.D{
						{Key: "f", Value: "$$x.f"},
						{Key: "p", Value: m{"$concat": a{"$$x.p", ".", "$$ea.k"}}},
						{Key: "a", Value: "$$ea.v"},
						{Key: "b", Value: "$$eb.v"},
						{Key: "e", Value: m{"$eq": a{"$$ea.v", "$$eb.v"}}},
					},
				}},
			}},
			m{"$map": m{
				"input": m{"$filter": m{"input": "$$kb", "as": "eb", "cond": m{"$not": a{m{"$in": a{"$$eb.k", "$$ka.k"}}}}}},
				"as":    "eb",
				"in": bson.D{
					{Key: "f", Value: "$$x.f"},
					{Key: "p", Value: m{"$concat": a{"$$x.p", ".", "$$eb.k"}}},
					{Key: "b", Value: "$$eb.v"},
					{Key: "e", Value: false},
				},
			}},
		}},
	}}
	ta, tb := m{"$type": "$$x.a"}, m{"$type": "$$x.b"}
	changed := m{"$and": a{
		m{"$ne": a{ta, tb}},
		m{"$not": a{m{"$in": a{tb, a{"missing", "null"}}}}},
	}}
	walk := func(t, e any) any {
		return m{"$or": a{m{"$eq": a{t, "object"}}, m{"$and": a{m{"$eq": a{t, "array"}}, e}}}}
	}
	descend := m{"$and": a{m{"$eq": a{ta, tb}}, walk(ta, "$$x.e")}}
	stages := []any{m{"$set": m{
		"nt": a{},
		"nq": m{"$map": m{
			"input": m{"$filter": m{"input": "$fs", "as": "d", "cond": m{"$and": a{
				m{"$eq": a{"$$d.afterType", "$$d.beforeType"}}, walk("$$d.afterType", "$$d.equal"),
			}}}},
			"as": "d",
			"in": m{"f": "$$d.field", "p": "$$d.field", "a": "$$d.after", "b": "$$d.before", "e": "$$d.equal"},
		}},
	}}}
	for range depth {
		stages = append(stages,
			m{"$set": m{"nq": m{"$reduce": m{
				"input":        "$nq",
				"initialValue": a{},
				"in":           m{"$concatArrays": a{"$$value", children}},
			}}}},
			m{"$set": m{
				"nt": m{"$concatArrays": a{"$nt", m{"$filter": m{"input": "$nq", "as": "x", "cond": changed}}}},
				"nq": m{"$filter": m{"input": "$nq", "as": "x", "cond": descend}},
			}},
		)
	}
	return stages
}

// sampleValue renders a value as relaxed Extended JSON, to embed in the JSON
// report. One longer than MaxSampleValueChars characters becomes a JSON
// string of its first MaxSampleValueChars characters followed by "…", and
// cut is set.
func sampleValue(v bson.RawValue) (out json.RawMessage, cut bool) {
	s, err := migration.ExtJSON(v)
	if err != nil {
		b, _ := json.Marshal(err.Error())
		return b, false
	}
	if utf8.RuneCountInString(s) <= MaxSampleValueChars {
		return json.RawMessage(s), false
	}
	r := []rune(s)
	b, _ := json.Marshal(string(r[:MaxSampleValueChars]) + "…")
	return b, true
}

// sampleAttrs are the log attributes of a merge sample.
func sampleAttrs(s MergeSample) []any {
	attrs := []any{"key", string(s.Key)}
	for _, f := range s.Fields {
		attrs = append(attrs, "field."+f.Field, f.Describe())
	}
	return attrs
}
