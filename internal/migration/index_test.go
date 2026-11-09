package migration

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestCollModSet checks how a collModIndex's set changes an index, as
// listIndexes shows it afterwards on every supported MongoDB version.
func TestCollModSet(t *testing.T) {
	d := func(kv ...any) bson.D {
		var out bson.D
		for i := 0; i < len(kv); i += 2 {
			out = append(out, bson.E{Key: kv[i].(string), Value: kv[i+1]})
		}
		return out
	}
	ix := func(opts ...any) Index {
		return Index{Collection: "events", Name: "createdAt_1", Keys: d("createdAt", int32(1)), Options: d(opts...)}
	}
	cases := []struct {
		name string
		have Index
		set  bson.D
		// after is the options after, current the index's values of set
		// before.
		after, current string
		unchanged      bool
	}{
		{name: "add a TTL", have: ix(), set: d("expireAfterSeconds", int32(3600)),
			after: `{"expireAfterSeconds": 3600}`, current: `{"expireAfterSeconds": null}`},
		{name: "change a TTL", have: ix("expireAfterSeconds", int32(60)), set: d("expireAfterSeconds", int32(3600)),
			after: `{"expireAfterSeconds": 3600}`, current: `{"expireAfterSeconds": 60}`},
		{name: "the same TTL, as another number type", have: ix("expireAfterSeconds", int64(60)), set: d("expireAfterSeconds", 60.0),
			after: `{"expireAfterSeconds": 60}`, current: `{"expireAfterSeconds": 60}`, unchanged: true},
		{name: "hide", have: ix(), set: d("hidden", true),
			after: `{"hidden": true}`, current: `{"hidden": false}`},
		{name: "hide a hidden index", have: ix("hidden", true), set: d("hidden", true),
			after: `{"hidden": true}`, current: `{"hidden": true}`, unchanged: true},
		{name: "unhide", have: ix("hidden", true), set: d("hidden", false),
			after: `{}`, current: `{"hidden": true}`},
		{name: "unhide a visible index", have: ix(), set: d("hidden", false),
			after: `{}`, current: `{"hidden": false}`, unchanged: true},
		{name: "prepareUnique", have: ix(), set: d("prepareUnique", true),
			after: `{"prepareUnique": true}`, current: `{"prepareUnique": false}`},
		{name: "prepareUnique on a unique index is a no-op", have: ix("unique", true), set: d("prepareUnique", true),
			after: `{"unique": true}`, current: `{"prepareUnique": true}`, unchanged: true},
		{name: "unique drops prepareUnique", have: ix("prepareUnique", true), set: d("unique", true),
			after: `{"unique": true}`, current: `{"unique": false}`},
		{name: "unique on a unique index", have: ix("unique", true), set: d("unique", true),
			after: `{"unique": true}`, current: `{"unique": true}`, unchanged: true},
		{name: "unique on the _id index", have: Index{Name: "_id_", Keys: d("_id", int32(1))}, set: d("unique", true),
			after: `{"unique": true}`, current: `{"unique": true}`, unchanged: true},
		{name: "several options, one already set", have: ix("hidden", true, "expireAfterSeconds", int32(60)),
			set:   d("hidden", true, "expireAfterSeconds", int32(30)),
			after: `{"expireAfterSeconds": 30, "hidden": true}`, current: `{"expireAfterSeconds": 60, "hidden": true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			after := tc.have.Modified(tc.set)
			if got := Canonical(after.Options); got != tc.after {
				t.Errorf("Modified options %s, want %s", got, tc.after)
			}
			if got := Canonical(tc.have.Current(tc.set)); got != tc.current {
				t.Errorf("Current %s, want %s", got, tc.current)
			}
			if got := tc.have.Unchanged(tc.set); got != tc.unchanged {
				t.Errorf("Unchanged %v, want %v", got, tc.unchanged)
			}
			if after.KeysString() != tc.have.KeysString() || after.Name != tc.have.Name {
				t.Errorf("Modified changed the keys or name: %+v", after)
			}
		})
	}
}

// TestDefinitionDiff checks which options count when two indexes are
// compared, for each index type, as listIndexes shows the existing one on
// every supported MongoDB version.
func TestDefinitionDiff(t *testing.T) {
	d := func(kv ...any) bson.D {
		var out bson.D
		for i := 0; i < len(kv); i += 2 {
			out = append(out, bson.E{Key: kv[i].(string), Value: kv[i+1]})
		}
		return out
	}
	ix := func(keys bson.D, opts ...any) Index {
		return Index{Collection: "items", Name: "ix", Keys: keys, Options: d(opts...)}
	}
	wildcard := d("$**", int32(1))
	text := d("_fts", "text", "_ftsx", int32(1))
	cases := []struct {
		name       string
		want, have Index
		// diff is "" when they are the same definition.
		diff string
	}{
		{name: "wildcard, the projection nested or dotted, true or 1, _id excluded",
			want: ix(wildcard, "wildcardProjection", d("a", d("b", true), "_id", false)),
			have: ix(wildcard, "wildcardProjection", d("a.b", int32(1)))},
		{name: "wildcard, another projection",
			want: ix(wildcard, "wildcardProjection", d("a", int32(1))),
			have: ix(wildcard, "wildcardProjection", d("a", int32(1), "b", int32(1))),
			diff: `wildcardProjection {"a": 1}, existing {"a": 1, "b": 1}`},
		{name: "wildcard, an exclusion against an inclusion",
			want: ix(wildcard, "wildcardProjection", d("a", int32(0))),
			have: ix(wildcard, "wildcardProjection", d("a", int32(1))),
			diff: `wildcardProjection {"a": 0}, existing {"a": 1}`},
		{name: "wildcard, no projection", want: ix(wildcard), have: ix(wildcard, "wildcardProjection", d("a", int32(1))),
			diff: `wildcardProjection none, existing {"a": 1}`},
		{name: "text, the defaults listIndexes fills in",
			want: ix(d("body", "text")),
			have: ix(text, "weights", d("body", int32(1)), "default_language", "english", "language_override", "language", "textIndexVersion", int32(3))},
		{name: "text, another language_override",
			want: ix(d("body", "text"), "language_override", "lang"),
			have: ix(text, "weights", d("body", int32(1)), "default_language", "english", "language_override", "language"),
			diff: `language_override "lang", existing "language"`},
		{name: "text, another textIndexVersion",
			want: ix(d("body", "text"), "textIndexVersion", int32(2)),
			have: ix(text, "weights", d("body", int32(1)), "default_language", "english", "language_override", "language", "textIndexVersion", int32(3))},
		{name: "2d, the same bounds as other numbers",
			want: ix(d("loc", "2d"), "bits", int64(20), "min", -180.0, "max", int32(180)),
			have: ix(d("loc", "2d"), "bits", int32(20), "min", int32(-180), "max", int32(180))},
		{name: "2d, the default bits spelled out, which MongoDB compares as given",
			want: ix(d("loc", "2d"), "bits", int32(26)), have: ix(d("loc", "2d")), diff: "bits 26, existing none"},
		{name: "2dsphere, another 2dsphereIndexVersion",
			want: ix(d("area", "2dsphere")), have: ix(d("area", "2dsphere"), "2dsphereIndexVersion", int32(4))},
		{name: "hashed, background and prepareUnique",
			want: ix(d("tenant", "hashed"), "background", true), have: ix(d("tenant", "hashed"), "prepareUnique", true)},
		{name: "TTL, another number type",
			want: ix(d("at", int32(1)), "expireAfterSeconds", int64(60)), have: ix(d("at", int32(1)), "expireAfterSeconds", int32(60))},
		{name: "partial, a false boolean is none",
			want: ix(d("a", int32(1)), "partialFilterExpression", d("a", d("$gt", int32(1)))),
			have: ix(d("a", int32(1)), "partialFilterExpression", d("a", d("$gt", int32(1))), "sparse", false)},
		{name: "storageEngine left out",
			want: ix(d("a", int32(1))), have: ix(d("a", int32(1)), "storageEngine", d("wiredTiger", d("configString", "block_compressor=zlib"))),
			diff: `storageEngine none, existing {"wiredTiger": {"configString": "block_compressor=zlib"}}`},
		{name: "the simple collation is none",
			want: ix(d("a", int32(1)), "collation", d("locale", "simple")), have: ix(d("a", int32(1)))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.want.DefinitionDiff(tc.have); got != tc.diff {
				t.Errorf("DefinitionDiff = %q, want %q", got, tc.diff)
			}
			if got := tc.want.SameDefinition(tc.have); got != (tc.diff == "") {
				t.Errorf("SameDefinition = %v, want %v", got, tc.diff == "")
			}
		})
	}
}
