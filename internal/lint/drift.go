package lint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// DriftResult is what Drift found.
type DriftResult struct {
	Findings []Finding
	// Replayed is how many applied migrations were replayed.
	Replayed int
	// Pending is how many *.json files in the directory have no Finished
	// entry: they aren't applied, so Drift leaves them out.
	Pending int
}

// Drift compares the indexes the applied migrations of a namespace leave
// with the indexes on the server: index drift.
//
// applied maps each migration Finished in the namespace to its latest
// Finished entry, with the steps it stored (runner.FinishedSnapshots): the
// migration as applied. Their createIndex, dropIndex and collModIndex steps
// are replayed in id order, as lint replays the files; the replay's own
// findings are lint's to report and are left out. The indexes left are
// matched with server (see matchIndexes): an applied index missing on the
// server, or there with other behaviour options, is an error; an index on
// the server that no applied migration creates, or one under another name,
// is a warning. Behaviour options compare as in lint, but collation, which
// listIndexes expands with every default: see sameOnServer. collations are
// the collections' default collations.
//
// dir is the migrations directory. A Finished migration whose file changed
// is a checksum error, and one whose file is missing an applied-missing
// error, as in lint; both are still replayed from their snapshot. Drift
// returns an error only when dir or a file in it can't be read.
func Drift(dir string, applied map[string]migration.Applied, server []migration.Index, collations map[string]bson.D) (DriftResult, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return DriftResult{}, fmt.Errorf("read migrations directory: %w", err)
	}
	var (
		res   DriftResult
		files []migration.Migration
		stems = map[string]bool{}
	)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".json")
		stems[stem] = true
		if _, ok := applied[stem]; !ok {
			res.Pending++
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := migration.ReadFile(path)
		if errors.Is(err, migration.ErrNotRegular) || errors.Is(err, migration.ErrTooLarge) {
			res.Findings = append(res.Findings, loadFindings(path, err)...)
			continue
		}
		if err != nil {
			return DriftResult{}, err
		}
		files = append(files, migration.Migration{ID: stem, Path: path, Checksum: migration.Checksum(raw)})
	}
	res.Findings = append(res.Findings, compareChecksums(files, applied)...)
	res.Findings = append(res.Findings, appliedMissing(dir, stems, applied)...)

	snapshots := make([]migration.Migration, 0, len(applied))
	for id, a := range applied {
		// The replay names a migration by its File, here its id.
		snapshots = append(snapshots, migration.Migration{ID: id, Path: id, Kind: migration.StepsKind(a.Steps), Steps: a.Steps})
	}
	slices.SortFunc(snapshots, func(a, b migration.Migration) int { return strings.Compare(a.ID, b.ID) })
	res.Replayed = len(snapshots)
	replayed, _ := replayIndexes(snapshots)

	got := make([]Index, len(server))
	for i, ix := range server {
		got[i] = Index{Index: ix}
	}
	for _, d := range matchIndexes(replayed, got, sameOnServer(collations)) {
		res.Findings = append(res.Findings, driftFinding(d))
	}
	sortFindings(res.Findings)
	return res, nil
}

func driftFinding(d indexDiff) Finding {
	w, g := d.want, d.got
	switch d.rule {
	case RuleIndexMissing:
		return Finding{Collection: w.Collection, Severity: Error, Rule: d.rule, Message: fmt.Sprintf(
			"index %s, created by %s with keys %s options %s, is not on the server",
			w.Name, w.File, w.KeysString(), w.OptionsString())}
	case RuleIndexOptions:
		return Finding{Collection: w.Collection, Severity: Error, Rule: d.rule, Message: fmt.Sprintf(
			"index %s has keys %s with options %s on the server, but the applied migrations leave index %s, created by %s, with options %s",
			g.Name, g.KeysString(), g.OptionsString(), w.Name, w.File, w.OptionsString())}
	case RuleIndexRenamed:
		return Finding{Collection: w.Collection, Severity: Warning, Rule: d.rule, Message: fmt.Sprintf(
			"index %s on the server matches index %s, created by %s, by definition, under a different name",
			g.Name, w.Name, w.File)}
	default: // RuleIndexExtra
		return Finding{Collection: g.Collection, Severity: Warning, Rule: d.rule, Message: fmt.Sprintf(
			"index %s, keys %s options %s, is on the server but no applied migration of this namespace creates it: "+
				"another namespace, the application or a person may have; drop it if nothing needs it",
			g.Name, g.KeysString(), g.OptionsString())}
	}
}

// sameOnServer returns how drift compares the options of an index
// the applied migrations leave, want, with an index listIndexes returns, got,
// on the same keys: as lint does, but for collation, which listIndexes
// expands with every default. Every collation field want gives must have the
// same value in got's; the fields only got has count as defaults. When want
// gives no collation, got must have none, or its collection's default
// collation from collations, which it inherits.
func sameOnServer(collations map[string]bson.D) func(want, got Index) bool {
	return func(want, got Index) bool {
		for _, name := range migration.OptionNames(want.Index, got.Index) {
			if name != "collation" && want.Option(name) != got.Option(name) {
				return false
			}
		}
		w, g := want.Collation(), got.Collation()
		if w == nil {
			def := migration.Index{Options: bson.D{{Key: "collation", Value: collations[got.Collection]}}}.Collation()
			return g == nil || def != nil && migration.Canonical(g) == migration.Canonical(def)
		}
		return g != nil && collationWithin(w, g)
	}
}

// collationWithin reports whether every field of the collation given has the
// same value in expanded, a collation as listIndexes returns it, with every
// default filled in. The fields only expanded has count as defaults, so one
// set to a non-default value outside given goes unnoticed.
func collationWithin(given, expanded bson.D) bool {
	for _, e := range given {
		i := slices.IndexFunc(expanded, func(f bson.E) bool { return f.Key == e.Key })
		if i < 0 || migration.Canonical(expanded[i].Value) != migration.Canonical(e.Value) {
			return false
		}
	}
	return true
}
