package migration

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Index is an index definition, from a createIndex step, an expected-indexes
// file or listIndexes. lint's replay and plan both compare indexes with it, so
// they agree on what MongoDB would accept.
type Index struct {
	Collection string
	Name       string
	// Keys is the key pattern, in order.
	Keys bson.D
	// Options are the index options but name (and, from listIndexes, key,
	// ns and v), as given.
	Options bson.D
}

// IgnoredOptions are the fields of an index specification that don't
// count when two indexes are compared: the identity fields v, ns, key
// and name, and the options MongoDB ignores when it compares an index with
// an existing one (background, textIndexVersion, 2dsphereIndexVersion) or
// that are a step on the way to unique (prepareUnique). Every other option
// counts, whether a migration gives it or listIndexes returns it.
var IgnoredOptions = []string{"v", "ns", "key", "name", "background", "textIndexVersion", "2dsphereIndexVersion", "prepareUnique"}

// optionOrder is the order options are rendered in; the options it doesn't
// list come after, sorted by name.
var optionOrder = []string{
	"unique",
	"sparse",
	"partialFilterExpression",
	"expireAfterSeconds",
	"collation",
	"weights",
	"default_language",
	"language_override",
	"hidden",
}

// DistinctOptions are the options MongoDB lets differ between two indexes
// on the same key pattern: such indexes can coexist under different names.
// Any other option differing makes them conflict.
var DistinctOptions = []string{"unique", "sparse", "partialFilterExpression", "collation", "wildcardProjection"}

// StepIndex returns the index a createIndex step asks for.
func StepIndex(s Step) (Index, error) {
	var keys, opts bson.D
	if err := bson.Unmarshal(s.Keys, &keys); err != nil {
		return Index{}, fmt.Errorf("keys: %w", err)
	}
	if err := bson.Unmarshal(s.Options, &opts); err != nil {
		return Index{}, fmt.Errorf("options: %w", err)
	}
	ix := Index{Collection: s.Collection, Keys: keys}
	for _, e := range opts {
		if e.Key == "name" {
			ix.Name, _ = e.Value.(string)
			continue
		}
		ix.Options = append(ix.Options, e)
	}
	if ix.Name == "" {
		return Index{}, errors.New("createIndex must name its index in options.name")
	}
	return ix, nil
}

// SpecIndex returns the index listIndexes describes with spec, on coll.
func SpecIndex(coll string, spec bson.D) Index {
	ix := Index{Collection: coll}
	for _, e := range spec {
		switch e.Key {
		case "name":
			ix.Name, _ = e.Value.(string)
		case "key":
			ix.Keys, _ = e.Value.(bson.D)
		case "v", "ns":
		default:
			ix.Options = append(ix.Options, e)
		}
	}
	return ix
}

// StepSet returns the options a collModIndex or collModCollection step sets,
// in file order. The schema allows, at least one, expireAfterSeconds, hidden,
// prepareUnique and unique for collModIndex, and validator, validationLevel
// and validationAction for collModCollection.
func StepSet(s Step) (bson.D, error) {
	var set bson.D
	if err := bson.Unmarshal(s.Set, &set); err != nil {
		return nil, fmt.Errorf("set: %w", err)
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("%s must change at least one option in set", s.Operation)
	}
	return set, nil
}

// Modified returns ix as a collMod setting set leaves it, as listIndexes then
// shows it: expireAfterSeconds replaced or added; hidden added when true and
// removed when false; prepareUnique added unless ix is already unique; unique
// added, and prepareUnique removed with it.
func (ix Index) Modified(set bson.D) Index {
	out := ix
	out.Options = slices.Clone(ix.Options)
	for _, e := range set {
		switch e.Key {
		case "hidden":
			if b, _ := e.Value.(bool); !b {
				out.Options = withoutOption(out.Options, "hidden")
				continue
			}
		case "prepareUnique":
			if out.Option("unique") != "" {
				continue
			}
		case "unique":
			out.Options = withoutOption(out.Options, "prepareUnique")
		}
		if i := slices.IndexFunc(out.Options, func(o bson.E) bool { return o.Key == e.Key }); i >= 0 {
			out.Options[i].Value = e.Value
		} else {
			out.Options = append(out.Options, e)
		}
	}
	return out
}

func withoutOption(opts bson.D, name string) bson.D {
	return slices.DeleteFunc(opts, func(o bson.E) bool { return o.Key == name })
}

// Current returns, for each option in set, ix's value of it: nil when ix has
// no expireAfterSeconds, false for an absent boolean. unique is true on the
// _id index, which listIndexes doesn't mark unique, and prepareUnique is true
// on a unique index, which already rejects duplicates: MongoDB takes setting
// either as a no-op.
func (ix Index) Current(set bson.D) bson.D {
	unique := ix.Option("unique") != "" || ix.IsID()
	out := make(bson.D, 0, len(set))
	for _, e := range set {
		v, ok := ix.option(e.Key)
		switch e.Key {
		case "expireAfterSeconds":
			if !ok {
				v = nil
			}
		case "unique", "prepareUnique":
			v = unique || (e.Key == "prepareUnique" && v == true)
		default:
			b, _ := v.(bool)
			v = b
		}
		out = append(out, bson.E{Key: e.Key, Value: v})
	}
	return out
}

// Unchanged reports whether a collMod setting set leaves ix as it is: every
// option already has its value, numbers compared by value.
func (ix Index) Unchanged(set bson.D) bool {
	for i, cur := range ix.Current(set) {
		if Canonical(cur.Value) != Canonical(set[i].Value) {
			return false
		}
	}
	return true
}

// IsText reports whether ix is a text index.
func (ix Index) IsText() bool {
	return slices.ContainsFunc(ix.Keys, func(e bson.E) bool { return e.Key == "_fts" || e.Value == "text" })
}

// IsID reports whether ix is the _id index.
func (ix Index) IsID() bool {
	return ix.Name == "_id_" || ix.KeysString() == `{"_id": 1}`
}

// KeysString renders the key pattern for comparison, in order, with numbers
// by value, so 1, 1.0 and NumberLong(1) are equal. A text index's keys are
// rendered as listIndexes lists them: its text fields become {"_fts":
// "text", "_ftsx": 1}, and their weights go into the weights option (see
// Option).
func (ix Index) KeysString() string {
	parts := make([]string, 0, len(ix.Keys))
	text := false
	for _, e := range ix.Keys {
		if e.Value == "text" && e.Key != "_fts" {
			if !text {
				parts = append(parts, `"_fts": "text"`, `"_ftsx": 1`)
				text = true
			}
			continue
		}
		parts = append(parts, strconv.Quote(e.Key)+": "+Canonical(e.Value))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// OptionsString renders the options for comparison, every one but
// IgnoredOptions, in a fixed order, leaving out the absent ones (see
// Option).
func (ix Index) OptionsString() string {
	return ix.optionsString(OptionNames(ix))
}

func (ix Index) optionsString(names []string) string {
	var parts []string
	for _, name := range names {
		if v := ix.Option(name); v != "" {
			parts = append(parts, strconv.Quote(name)+": "+v)
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// OptionNames returns the names of the options of ixs that count in a
// comparison, those a text index has by default included, without
// IgnoredOptions, in the order OptionsString renders them.
func OptionNames(ixs ...Index) []string {
	var names, rest []string
	add := func(name string) {
		if slices.Contains(IgnoredOptions, name) || slices.Contains(names, name) || slices.Contains(rest, name) {
			return
		}
		if slices.Contains(optionOrder, name) {
			names = append(names, name)
		} else {
			rest = append(rest, name)
		}
	}
	for _, ix := range ixs {
		if ix.IsText() {
			for _, name := range textDefaults {
				add(name)
			}
		}
		for _, e := range ix.Options {
			add(e.Key)
		}
	}
	slices.SortFunc(names, func(a, b string) int { return slices.Index(optionOrder, a) - slices.Index(optionOrder, b) })
	slices.Sort(rest)
	return append(names, rest...)
}

// textDefaults are the options listIndexes always shows on a text index,
// with MongoDB's default when createIndex doesn't give them.
var textDefaults = []string{"weights", "default_language", "language_override"}

// Option renders one option for comparison, or "" when it is absent. A false
// boolean is the same as an absent one, and so is a collation of just
// {locale: "simple"}, as in MongoDB. For a text index, weights holds every
// text field, 1 unless the weights option gives another, default_language is
// "english" and language_override "language" unless given: what listIndexes
// shows for the same index, and what MongoDB compares. A wildcardProjection
// is rendered as MongoDB compares it (see projection).
func (ix Index) Option(name string) string {
	v, ok := ix.option(name)
	switch {
	case ix.IsText() && name == "weights":
		return Canonical(ix.weights(v))
	case ix.IsText() && name == "default_language" && !ok:
		return Canonical("english")
	case ix.IsText() && name == "language_override" && !ok:
		return Canonical("language")
	case !ok:
		return ""
	}
	if b, isBool := v.(bool); isBool && !b {
		return ""
	}
	switch name {
	case "collation":
		if isSimpleCollation(v) {
			return ""
		}
	case "wildcardProjection":
		if d, isDoc := v.(bson.D); isDoc {
			return Canonical(projection(d, ""))
		}
	}
	return Canonical(v)
}

// projection returns a wildcardProjection as MongoDB compares two of them:
// every path dotted, so {a: {b: 1}} is {"a.b": 1}, each one included (1) or
// excluded (0) whatever the value's type, and _id left out when excluded,
// as wildcard indexes leave it out by default.
func projection(p bson.D, prefix string) bson.D {
	var out bson.D
	for _, e := range p {
		path := prefix + e.Key
		if sub, ok := e.Value.(bson.D); ok {
			out = append(out, projection(sub, path+".")...)
			continue
		}
		v := int32(0)
		if truthy(e.Value) {
			v = 1
		}
		if path == "_id" && v == 0 {
			continue
		}
		out = append(out, bson.E{Key: path, Value: v})
	}
	return out
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int32:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	}
	return true
}

func (ix Index) option(name string) (any, bool) {
	for _, e := range ix.Options {
		if e.Key == name {
			return e.Value, true
		}
	}
	return nil, false
}

// weights merges a text index's text fields, weighted 1, with the weights
// option given, w.
func (ix Index) weights(w any) bson.D {
	given, _ := w.(bson.D)
	out := slices.Clone(given)
	for _, e := range ix.Keys {
		if e.Value == "text" && e.Key != "_fts" && !slices.ContainsFunc(out, func(g bson.E) bool { return g.Key == e.Key }) {
			out = append(out, bson.E{Key: e.Key, Value: int32(1)})
		}
	}
	return out
}

// Collation returns ix's collation, or nil when it has none or just
// {locale: "simple"}, which is the same as none.
func (ix Index) Collation() bson.D {
	v, _ := ix.option("collation")
	d, _ := v.(bson.D)
	if isSimpleCollation(d) {
		return nil
	}
	return d
}

func isSimpleCollation(v any) bool {
	d, ok := v.(bson.D)
	return ok && len(d) == 1 && d[0].Key == "locale" && d[0].Value == "simple"
}

// SameDefinition reports whether ix and other have the same keys and
// options, collation included: every option but IgnoredOptions.
func (ix Index) SameDefinition(other Index) bool {
	return ix.KeysString() == other.KeysString() && ix.OptionsString() == other.OptionsString()
}

// DefinitionDiff describes the first way other differs from ix, in keys or in
// an option other than IgnoredOptions and those in skip, or returns "".
func (ix Index) DefinitionDiff(other Index, skip ...string) string {
	if ix.KeysString() != other.KeysString() {
		return fmt.Sprintf("keys %s, existing %s", ix.KeysString(), other.KeysString())
	}
	for _, name := range OptionNames(ix, other) {
		if slices.Contains(skip, name) {
			continue
		}
		if w, h := ix.Option(name), other.Option(name); w != h {
			return fmt.Sprintf("%s %s, existing %s", name, OrNone(w), OrNone(h))
		}
	}
	return ""
}

// OrNone returns s, or "none" when s is empty.
func OrNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// SameDistinct reports whether ix and other have the same keys and the same
// DistinctOptions but those in skip: MongoDB refuses to create one under
// another name while the other exists (IndexOptionsConflict).
func (ix Index) SameDistinct(other Index, skip ...string) bool {
	if ix.KeysString() != other.KeysString() {
		return false
	}
	for _, name := range DistinctOptions {
		if !slices.Contains(skip, name) && ix.Option(name) != other.Option(name) {
			return false
		}
	}
	return true
}

// Canonical renders a value for comparison: numbers as float64 in decimal
// notation, so 1, 1.0 and NumberLong(1) are equal, documents with their
// fields sorted, and everything else as relaxed Extended JSON.
func Canonical(v any) string {
	switch x := v.(type) {
	case int32:
		return strconv.FormatFloat(float64(x), 'f', -1, 64)
	case int64:
		return strconv.FormatFloat(float64(x), 'f', -1, 64)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bson.D:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, strconv.Quote(e.Key)+": "+Canonical(e.Value))
		}
		slices.Sort(parts)
		return "{" + strings.Join(parts, ", ") + "}"
	case bson.A:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, Canonical(e))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	s, err := ExtJSON(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return s
}

// ExtJSON renders a value, not only a document, as relaxed Extended JSON.
func ExtJSON(v any) (string, error) {
	b, err := bson.MarshalExtJSON(bson.D{{Key: "v", Value: v}}, false, false)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(b), `{"v":`), "}"), nil
}
