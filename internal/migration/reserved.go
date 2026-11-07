package migration

import (
	"fmt"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The collections migrune keeps in the target database, next to the
// migrated ones.
const (
	// ChangelogCollection holds one entry per migration outcome.
	ChangelogCollection = "migrations_changelog"
	// LockCollection holds the migration lock, a single document.
	LockCollection = "migrations_lock"
)

// reservedMessage explains why a reserved collection is refused.
const reservedMessage = "migrations must not name migrune's own collections (" +
	ChangelogCollection + ", " + LockCollection + ") or MongoDB's system.* collections"

// IsReservedCollection reports whether migrations must not name coll:
// migrune's changelog and lock, and MongoDB's system.* collections. Writing
// to them could forge or erase changelog entries or break the lock; reading
// them from a pipeline could copy them elsewhere.
func IsReservedCollection(coll string) bool {
	return coll == ChangelogCollection || coll == LockCollection || strings.HasPrefix(coll, "system.")
}

// ReservedCollectionError reports the places in a migration file that name a
// reserved collection (see IsReservedCollection) where the schema can't see
// them: the from of $lookup and $graphLookup, the collection of $unionWith,
// and the target of a $merge or $out, at any depth of a pipeline. The schema
// already refuses a reserved step collection or top-level $merge.into.
type ReservedCollectionError struct {
	File       string
	Violations []Violation
}

func (e *ReservedCollectionError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: names a reserved collection:", e.File)
	for _, v := range e.Violations {
		fmt.Fprintf(&sb, "\n  %s: %s", v.Pointer, v.Message)
	}
	return sb.String()
}

// ReservedCollections returns where steps name a reserved collection: each
// step's collection, and every collection a pipeline reads or writes, at any
// depth (see ReservedCollectionError). Pointers are JSON pointers into the
// file.
func ReservedCollections(steps []Step) []Violation {
	var w reservedWalker
	for i, s := range steps {
		ptr := "/steps/" + strconv.Itoa(i)
		w.check(ptr+"/collection", s.Collection)
		if s.Update.Type == bson.TypeArray {
			if stages, ok := rawStages(s.Update.Array()); ok {
				w.pipeline(ptr+"/update", stages)
			}
		}
		w.pipeline(ptr+"/pipeline", s.Pipeline)
	}
	return w.out
}

type reservedWalker struct{ out []Violation }

func (w *reservedWalker) check(ptr, coll string) {
	if IsReservedCollection(coll) {
		w.out = append(w.out, Violation{Pointer: ptr, Message: fmt.Sprintf("%q is reserved: %s", coll, reservedMessage)})
	}
}

// target checks a collection given as a name, or as a document naming it in
// field (a $lookup from or $merge into with a database, for instance).
func (w *reservedWalker) target(ptr string, v bson.RawValue, field string) {
	switch v.Type {
	case bson.TypeString:
		w.check(ptr, v.StringValue())
	case bson.TypeEmbeddedDocument:
		if c, ok := v.Document().Lookup(field).StringValueOK(); ok {
			w.check(ptr+"/"+field, c)
		}
	}
}

func (w *reservedWalker) pipeline(ptr string, stages []bson.Raw) {
	for i, stage := range stages {
		elems, err := stage.Elements()
		if err != nil {
			continue
		}
		for _, e := range elems {
			sp := ptr + "/" + strconv.Itoa(i) + "/" + PointerToken(e.Key())
			v := e.Value()
			arg, isDoc := v.DocumentOK()
			switch e.Key() {
			case "$lookup", "$graphLookup":
				if isDoc {
					w.target(sp+"/from", arg.Lookup("from"), "coll")
					w.subPipeline(sp+"/pipeline", arg.Lookup("pipeline"))
				}
			case "$unionWith":
				if !isDoc {
					w.target(sp, v, "coll")
					break
				}
				w.target(sp+"/coll", arg.Lookup("coll"), "coll")
				w.subPipeline(sp+"/pipeline", arg.Lookup("pipeline"))
			case "$facet":
				if !isDoc {
					break
				}
				outputs, _ := arg.Elements()
				for _, o := range outputs {
					w.subPipeline(sp+"/"+PointerToken(o.Key()), o.Value())
				}
			case "$merge":
				if isDoc {
					w.target(sp+"/into", arg.Lookup("into"), "coll")
				} else {
					w.target(sp, v, "coll")
				}
			case "$out":
				w.target(sp, v, "coll")
			}
		}
	}
}

func (w *reservedWalker) subPipeline(ptr string, v bson.RawValue) {
	if v.Type != bson.TypeArray {
		return
	}
	if stages, ok := rawStages(v.Array()); ok {
		w.pipeline(ptr, stages)
	}
}

// rawStages returns the documents of a pipeline array, or false if one isn't
// a document.
func rawStages(a bson.RawArray) ([]bson.Raw, bool) {
	vals, err := a.Values()
	if err != nil {
		return nil, false
	}
	out := make([]bson.Raw, 0, len(vals))
	for _, v := range vals {
		d, ok := v.DocumentOK()
		if !ok {
			return nil, false
		}
		out = append(out, d)
	}
	return out, true
}
