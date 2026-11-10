package lint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/joaopaulosr95/migrune/internal/migration"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Index is an index definition, from the replay of the index migrations or
// from the expected-indexes file. Its definition and comparison are
// migration.Index's, which plan uses too.
type Index struct {
	migration.Index
	// File is the migration file that created the index, or the
	// expected-indexes file.
	File string
	// Pointer locates the createIndex step or the expected index in File.
	Pointer string
}

// conflicts reports whether MongoDB would reject creating ix under its name
// while other, with another name, is live on the same collection: the same
// key pattern, and none of migration.DistinctOptions tells them apart.
func (ix Index) conflicts(other Index) bool {
	return ix.Collection == other.Collection && ix.Name != other.Name && ix.SameDistinct(other.Index)
}

// replayIndexes replays the createIndex, dropIndex and collModIndex steps of
// migrations, in order, and returns the indexes left at the end, in creation
// order. MongoDB rejects a createIndex that reuses a live name for a
// different definition (index-name-reused), and one whose key pattern matches
// a live index under another name, unless their collation,
// partialFilterExpression, unique, sparse or wildcardProjection differ
// (index-conflict). Both are errors; the rejected index is not created. A collModIndex changes the live
// index's options, so later steps and the expected indexes see them; on an
// index the replay doesn't know it is a warning (unknown-index), and setting
// unique on an index that isn't prepareUnique is an error
// (unique-without-prepare), which leaves the index as it was.
func replayIndexes(migrations []migration.Migration) ([]Index, []Finding) {
	var (
		live     = map[indexKey]Index{}
		order    []indexKey
		findings []Finding
	)
	for _, m := range migrations {
		if m.Kind != migration.KindIndex {
			continue
		}
		// dropped are the indexes this migration dropped so far, with their
		// keys when the replay knows them.
		var dropped []Index
		for i, s := range m.Steps {
			ptr := "/steps/" + strconv.Itoa(i)
			switch s.Operation {
			case "createIndex":
				ix, err := stepIndex(m.Path, ptr, s)
				if err != nil {
					findings = append(findings, Finding{File: m.Path, Severity: Error, Rule: RuleSchema, Message: err.Error()})
					continue
				}
				if f, found := dropThenCreate(ptr, ix, dropped); found {
					findings = append(findings, f)
				}
				k := indexKey{ix.Collection, ix.Name}
				old, ok := live[k]
				if !ok {
					if f, found := conflictFinding(ptr, ix, live, order); found {
						findings = append(findings, f)
						continue
					}
					live[k] = ix
					order = append(order, k)
					continue
				}
				if !old.SameDefinition(ix.Index) {
					findings = append(findings, Finding{File: m.Path, Severity: Error, Rule: RuleIndexNameReused, Message: fmt.Sprintf(
						"%s: index %s on %s already exists with a different definition, created by %s: keys %s options %s here, keys %s options %s there. Pick another name, or drop it in an earlier migration",
						ptr, ix.Name, ix.Collection, filepath.Base(old.File), ix.KeysString(), ix.OptionsString(), old.KeysString(), old.OptionsString())})
				}
			case "collModIndex":
				if f, found := collMod(m.Path, ptr, s, live); found {
					findings = append(findings, f)
				}
			case "dropIndex":
				k := indexKey{s.Collection, s.Name}
				old, ok := live[k]
				if !ok {
					old = Index{Index: migration.Index{Collection: s.Collection, Name: s.Name}}
				}
				old.Pointer = ptr
				dropped = append(dropped, old)
				delete(live, k)
			}
		}
	}
	var out []Index
	for _, k := range order {
		if ix, ok := live[k]; ok {
			out = append(out, ix)
			// A name dropped and created again appears twice in order.
			delete(live, k)
		}
	}
	return out, findings
}

// collMod applies a collModIndex step s, at ptr in the migration file path,
// to its index in live, and returns the finding it raises, if any.
func collMod(path, ptr string, s migration.Step, live map[indexKey]Index) (Finding, bool) {
	set, err := migration.StepSet(s)
	if err != nil {
		return Finding{File: path, Severity: Error, Rule: RuleSchema, Message: ptr + ": " + err.Error()}, true
	}
	k := indexKey{s.Collection, s.Name}
	ix, ok := live[k]
	switch {
	case !ok:
		return Finding{File: path, Severity: Warning, Rule: RuleUnknownIndex, Message: fmt.Sprintf(
				"%s: no earlier migration creates index %s on %s, so lint can't check this collModIndex: make sure the index exists, "+
					"such as from a baseline; plan fails if it doesn't", ptr, s.Name, s.Collection)},

			true
	case slices.ContainsFunc(set, func(e bson.E) bool { return e.Key == "unique" }) &&
		ix.Option("unique") == "" && ix.Option("prepareUnique") == "":
		return Finding{File: path, Severity: Error, Rule: RuleUniqueUnprepared, Message: fmt.Sprintf(
				"%s: index %s on %s isn't prepareUnique, created by %s: MongoDB converts an index to unique only once prepareUnique is set on it. "+
					"Set prepareUnique in an earlier migration, remove the duplicates in a data migration, then set unique",
				ptr, s.Name, s.Collection, filepath.Base(ix.File))},

			true
	}
	ix.Index = ix.Modified(set)
	live[k] = ix
	return Finding{}, false
}

// dropThenCreate returns the drop-then-create finding for ix when the same
// migration dropped, earlier, an index of its collection with its name or its
// keys: if the createIndex fails, such as a unique index on duplicates, the
// collection is left without either index.
func dropThenCreate(ptr string, ix Index, dropped []Index) (Finding, bool) {
	for _, d := range dropped {
		if d.Collection != ix.Collection {
			continue
		}
		var same string
		switch {
		case d.Name == ix.Name:
			same = "name"
		case len(d.Keys) > 0 && d.KeysString() == ix.KeysString():
			same = "keys " + ix.KeysString()
		default:
			continue
		}
		return Finding{File: ix.File, Severity: Error, Rule: RuleDropThenCreate, Message: fmt.Sprintf(
				"%s: index %s on %s is created after %s drops index %s, with the same %s: if the createIndex fails, such as a unique index on duplicates, "+
					"the collection is left without either. Create the new index under a new name, and drop the old one in a later migration",
				ptr, ix.Name, ix.Collection, d.Pointer, d.Name, same)},

			true
	}
	return Finding{}, false
}

// indexKey identifies a live index during the replay: names are scoped to a
// collection.
type indexKey struct{ collection, name string }

// conflictFinding returns the index-conflict finding for ix if a live index
// conflicts with it. live is searched in creation order so the finding names
// the oldest conflicting index.
func conflictFinding(ptr string, ix Index, live map[indexKey]Index, order []indexKey) (Finding, bool) {
	for _, k := range order {
		old, ok := live[k]
		if !ok || !ix.conflicts(old) {
			continue
		}
		what := "the same definition"
		if ix.OptionsString() != old.OptionsString() {
			what = fmt.Sprintf("the same keys and different options (%s here, %s there)", ix.OptionsString(), old.OptionsString())
		}
		return Finding{File: ix.File, Severity: Error, Rule: RuleIndexConflict, Message: fmt.Sprintf(
				"%s: index %s on %s has %s as index %s, created by %s: keys %s. MongoDB rejects a second index on the same keys unless its collation, partialFilterExpression, unique, sparse or wildcardProjection differs. Reuse %s, or drop it in an earlier migration",
				ptr, ix.Name, ix.Collection, what, old.Name, filepath.Base(old.File), ix.KeysString(), old.Name)},

			true
	}
	return Finding{}, false
}

func stepIndex(path, ptr string, s migration.Step) (Index, error) {
	ix, err := migration.StepIndex(s)
	if err != nil {
		return Index{}, fmt.Errorf("%s: %w", ptr, err)
	}
	return Index{Index: ix, File: path, Pointer: ptr}, nil
}

// ExpectedIndexes is the list of indexes the application expects, read from
// an expected-indexes file.
type ExpectedIndexes struct {
	Path    string
	Indexes []Index
}

// ReadExpectedIndexes reads an expected-indexes file: a JSON array of
// {"collection", "name", "keys", "options"} objects, in Extended JSON.
// collection and keys are required.
func ReadExpectedIndexes(path string) (*ExpectedIndexes, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if !json.Valid(trimmed) {
		return nil, fmt.Errorf("%s: invalid JSON", path)
	}
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("%s: want a JSON array of indexes", path)
	}
	// Extended JSON must be a document at the top level.
	wrapped := append(append([]byte(`{"indexes":`), trimmed...), '}')
	var doc struct {
		Indexes []bson.Raw `bson:"indexes"`
	}
	if err := bson.UnmarshalExtJSON(wrapped, false, &doc); err != nil {
		return nil, fmt.Errorf("%s: invalid Extended JSON: %w", path, err)
	}
	ex := &ExpectedIndexes{Path: path}
	for i, r := range doc.Indexes {
		ix, err := expectedIndex(r)
		if err != nil {
			return nil, fmt.Errorf("%s: /%d: %w", path, i, err)
		}
		ix.File, ix.Pointer = path, "/"+strconv.Itoa(i)
		ex.Indexes = append(ex.Indexes, ix)
	}
	return ex, nil
}

func expectedIndex(r bson.Raw) (Index, error) {
	elems, err := r.Elements()
	if err != nil {
		return Index{}, err
	}
	var ix Index
	for _, e := range elems {
		v := e.Value()
		var ok bool
		switch e.Key() {
		case "collection":
			ix.Collection, ok = v.StringValueOK()
			ok = ok && ix.Collection != ""
		case "name":
			ix.Name, ok = v.StringValueOK()
		case "keys":
			ok = v.Type == bson.TypeEmbeddedDocument && bson.Unmarshal(v.Value, &ix.Keys) == nil && len(ix.Keys) > 0
		case "options":
			ok = v.Type == bson.TypeEmbeddedDocument && bson.Unmarshal(v.Value, &ix.Options) == nil
		default:
			return Index{}, fmt.Errorf("unknown field %q", e.Key())
		}
		if !ok {
			return Index{}, fmt.Errorf("invalid %s", e.Key())
		}
	}
	switch {
	case ix.Collection == "":
		return Index{}, fmt.Errorf("missing collection")
	case len(ix.Keys) == 0:
		return Index{}, fmt.Errorf("missing keys")
	}
	return ix, nil
}

// indexDiff is one way a list of indexes differs from the list it should
// match, as matchIndexes finds it.
type indexDiff struct {
	// rule is RuleIndexMissing, RuleIndexOptions, RuleIndexExtra or
	// RuleIndexRenamed.
	rule string
	// want is the index of the list to match; zero for RuleIndexExtra.
	want Index
	// got is the index of the compared list; zero for RuleIndexMissing.
	got Index
}

// matchIndexes matches the indexes got with the indexes want by collection,
// ordered keys and options, as same compares them; names are
// ignored for matching. It returns, in this order: the matches whose names
// differ, when want's is given (RuleIndexRenamed); each wanted index with no
// match on its keys (RuleIndexMissing) or with a match on its keys but not on
// its options (RuleIndexOptions); and each index of got left unmatched
// (RuleIndexExtra). The _id index is skipped on both sides.
func matchIndexes(want, got []Index, same func(want, got Index) bool) []indexDiff {
	var (
		diffs   []indexDiff
		used    = make([]bool, len(got))
		pending []Index
	)
	// find returns the unused index of got on w's keys, as same compares
	// them when exact; of several, the one sharing the most collation fields
	// with w, so an index whose collation only gives a locale doesn't take
	// the expected index that another one on the same keys matches in full.
	find := func(w Index, exact bool) int {
		best, shared := -1, -1
		for j, g := range got {
			if used[j] || g.IsID() || w.Collection != g.Collection || w.KeysString() != g.KeysString() || exact && !same(w, g) {
				continue
			}
			if n := sharedCollationFields(w, g); n > shared {
				best, shared = j, n
			}
		}
		return best
	}
	for _, w := range want {
		if w.IsID() {
			continue
		}
		j := find(w, true)
		if j < 0 {
			pending = append(pending, w)
			continue
		}
		used[j] = true
		if w.Name != "" && w.Name != got[j].Name {
			diffs = append(diffs, indexDiff{RuleIndexRenamed, w, got[j]})
		}
	}
	for _, w := range pending {
		j := find(w, false)
		if j < 0 {
			diffs = append(diffs, indexDiff{rule: RuleIndexMissing, want: w})
			continue
		}
		used[j] = true
		diffs = append(diffs, indexDiff{RuleIndexOptions, w, got[j]})
	}
	for j, g := range got {
		if !used[j] && !g.IsID() {
			diffs = append(diffs, indexDiff{rule: RuleIndexExtra, got: g})
		}
	}
	return diffs
}

// sameOptions reports whether a and b have the same options, every one but
// migration.IgnoredOptions.
func sameOptions(a, b Index) bool {
	return a.OptionsString() == b.OptionsString()
}

// sharedCollationFields counts the collation fields a and b both give, with
// the same value.
func sharedCollationFields(a, b Index) int {
	n := 0
	g := b.Collation()
	for _, e := range a.Collation() {
		i := slices.IndexFunc(g, func(f bson.E) bool { return f.Key == e.Key })
		if i >= 0 && migration.Canonical(g[i].Value) == migration.Canonical(e.Value) {
			n++
		}
	}
	return n
}

// sameExpected compares an expected index, want, with a replayed one, got:
// options exactly, except a collation as listIndexes shows it, recognised by
// its version field, which createIndex doesn't accept. listIndexes fills in
// every default, so that collation compares as drift does (collationWithin):
// every field the migrations give must have the same value in it.
func sameExpected(want, got Index) bool {
	w := want.Collation()
	if !slices.ContainsFunc(w, func(e bson.E) bool { return e.Key == "version" }) {
		return sameOptions(want, got)
	}
	for _, name := range migration.OptionNames(want.Index, got.Index) {
		if name != "collation" && want.Option(name) != got.Option(name) {
			return false
		}
	}
	g := got.Collation()
	return g != nil && collationWithin(g, w)
}

// compareIndexes matches the replayed indexes with the expected ones (see
// matchIndexes), with options compared as sameExpected does. A missing index or one
// whose options differ is an error; an extra or renamed index
// is a warning.
func compareIndexes(replayed []Index, expected *ExpectedIndexes) []Finding {
	var findings []Finding
	for _, d := range matchIndexes(expected.Indexes, replayed, sameExpected) {
		e, r := d.want, d.got
		switch d.rule {
		case RuleIndexRenamed:
			findings = append(findings, Finding{File: r.File, Severity: Warning, Rule: RuleIndexRenamed, Message: fmt.Sprintf(
				"%s: index %s on %s matches the expected index %s by definition, under a different name",
				r.Pointer, r.Name, r.Collection, e.Name)})
		case RuleIndexMissing:
			findings = append(findings, Finding{File: expected.Path, Severity: Error, Rule: RuleIndexMissing, Message: fmt.Sprintf(
				"%s: no migration creates the expected index %s on %s: keys %s options %s",
				e.Pointer, displayName(e), e.Collection, e.KeysString(), e.OptionsString())})
		case RuleIndexOptions:
			findings = append(findings, Finding{File: r.File, Severity: Error, Rule: RuleIndexOptions, Message: fmt.Sprintf(
				"%s: index %s on %s has keys %s with options %s, but the expected index %s has options %s",
				r.Pointer, r.Name, r.Collection, r.KeysString(), r.OptionsString(), displayName(e), e.OptionsString())})
		case RuleIndexExtra:
			findings = append(findings, Finding{File: r.File, Severity: Warning, Rule: RuleIndexExtra, Message: fmt.Sprintf(
				"%s: index %s on %s is not in %s; drop it if nothing needs it",
				r.Pointer, r.Name, r.Collection, filepath.Base(expected.Path))})
		}
	}
	return findings
}

func displayName(ix Index) string {
	if ix.Name == "" {
		return "(unnamed)"
	}
	return ix.Name
}
