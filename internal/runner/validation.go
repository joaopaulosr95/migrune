package runner

import (
	"context"
	"fmt"
	"log/slog"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// validation is a collection's validation options, as listCollections shows
// them, with MongoDB's defaults for the absent ones: no validator ({}),
// validationLevel strict and validationAction error.
type validation struct {
	Validator bson.D
	Level     string
	Action    string
}

func defaultValidation() validation {
	return validation{Validator: bson.D{}, Level: "strict", Action: "error"}
}

// collectionInfo is what listCollections says of a collection.
type collectionInfo struct {
	// Type is "collection", "view" or "timeseries".
	Type       string
	Validation validation
}

// readCollection returns what listCollections says of coll, or nil when it
// doesn't exist.
func readCollection(ctx context.Context, db *mongo.Database, coll string) (*collectionInfo, error) {
	cur, err := db.ListCollections(ctx, bson.D{{Key: "name", Value: coll}})
	if err != nil {
		return nil, err
	}
	var specs []struct {
		Type    string `bson:"type"`
		Options struct {
			Validator bson.D `bson:"validator"`
			Level     string `bson:"validationLevel"`
			Action    string `bson:"validationAction"`
		} `bson:"options"`
	}
	if err := cur.All(ctx, &specs); err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, nil
	}
	o := specs[0].Options
	v := defaultValidation()
	if o.Validator != nil {
		v.Validator = o.Validator
	}
	if o.Level != "" {
		v.Level = o.Level
	}
	if o.Action != "" {
		v.Action = o.Action
	}
	return &collectionInfo{Type: specs[0].Type, Validation: v}, nil
}

// current returns v's value of each option in set.
func (v validation) current(set bson.D) bson.D {
	out := make(bson.D, 0, len(set))
	for _, e := range set {
		var cur any
		switch e.Key {
		case "validator":
			cur = v.Validator
		case "validationLevel":
			cur = v.Level
		case "validationAction":
			cur = v.Action
		}
		out = append(out, bson.E{Key: e.Key, Value: cur})
	}
	return out
}

// unchanged reports whether a collMod setting set leaves v as it is,
// validators compared as migration.Canonical renders them.
func (v validation) unchanged(set bson.D) bool {
	for i, cur := range v.current(set) {
		if migration.Canonical(cur.Value) != migration.Canonical(set[i].Value) {
			return false
		}
	}
	return true
}

// modified returns v as a collMod setting set leaves it.
func (v validation) modified(set bson.D) validation {
	for _, e := range set {
		switch e.Key {
		case "validator":
			v.Validator, _ = e.Value.(bson.D)
			if v.Validator == nil {
				v.Validator = bson.D{}
			}
		case "validationLevel":
			v.Level, _ = e.Value.(string)
		case "validationAction":
			v.Action, _ = e.Value.(string)
		}
	}
	return v
}

// validationChange describes what a collModCollection setting set does to
// the validation have of coll, for the report. It has no index name.
func validationChange(action IndexAction, coll string, have validation, set bson.D) IndexChange {
	return IndexChange{Action: action, Collection: coll, Set: rawExtJSON(set), Before: rawExtJSON(have.current(set))}
}

// collModCollectionRejected returns why MongoDB would reject a
// collModCollection on coll, described by info, or nil.
func collModCollectionRejected(coll string, info *collectionInfo) error {
	switch {
	case info == nil:
		return fmt.Errorf("collection %s doesn't exist, so there is nothing to modify: create it in an earlier migration, or fix the name", coll)
	case info.Type != "" && info.Type != "collection":
		return fmt.Errorf("%s is a %s, which takes no validation options: MongoDB would reject the collMod (InvalidOptions)", coll, info.Type)
	}
	return nil
}

// runCollModCollection runs a collModCollection step: it reads the
// collection's validation, to report the values before and skip a no-op, then
// runs collMod with the step's set. check runs right before the collMod,
// which isn't sent when it fails.
func runCollModCollection(ctx context.Context, db *mongo.Database, s migration.Step, check func() error, log *slog.Logger) (IndexChange, error) {
	set, err := migration.StepSet(s)
	if err != nil {
		return IndexChange{}, err
	}
	info, err := readCollection(ctx, db, s.Collection)
	if err != nil {
		return IndexChange{}, fmt.Errorf("list collections: %w", err)
	}
	if err := collModCollectionRejected(s.Collection, info); err != nil {
		return IndexChange{}, err
	}
	have := info.Validation
	change := validationChange(IndexPresent, s.Collection, have, set)
	if have.unchanged(set) {
		log.Info("validation options already set", "collection", s.Collection, "set", extJSON(set))
		return change, nil
	}
	if err := check(); err != nil {
		return change, err
	}
	cmd := append(bson.D{{Key: "collMod", Value: s.Collection}}, set...)
	cmd = append(cmd, bson.E{Key: "writeConcern", Value: bson.D{{Key: "w", Value: "majority"}}})
	if err := db.RunCommand(ctx, cmd).Err(); err != nil {
		return change, err
	}
	change.Action = IndexModify
	log.Info("validation modified", "collection", s.Collection, "before", extJSON(have.current(set)), "set", extJSON(set))
	return change, nil
}

// planCollection returns the collection coll as the pending steps planned so
// far leave it: read with listCollections once per run, then changed by each
// planned collModCollection. A collection the server lacks exists once an
// earlier planned createIndex creates it, with the default validation.
func (r *Run) planCollection(coll string) (*collectionInfo, error) {
	if info, ok := r.planCollections[coll]; ok {
		return info, nil
	}
	info, err := readCollection(r.ctx, r.db, coll)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	if info == nil && r.planCreated[coll] {
		// A createIndex creates a missing collection.
		info = &collectionInfo{Type: "collection", Validation: defaultValidation()}
	}
	if r.planCollections == nil {
		r.planCollections = map[string]*collectionInfo{}
	}
	r.planCollections[coll] = info
	return info, nil
}

// planCreates records that a planned createIndex creates an index on coll,
// which has none: coll may be missing on the server, and exists after it.
func (r *Run) planCreates(coll string) {
	if r.planCreated == nil {
		r.planCreated = map[string]bool{}
	}
	r.planCreated[coll] = true
	if info, ok := r.planCollections[coll]; ok && info == nil {
		r.planCollections[coll] = &collectionInfo{Type: "collection", Validation: defaultValidation()}
	}
}

// planCollModCollection plans a collModCollection step s and returns its
// change; with err set, the change, if any, carries the conflict. A step
// whose collection already has every value is present.
// One that changes it needs the collMod action; when the validation after it
// applies (a validator, and a level other than off), plan counts the
// documents that don't match the validator in nonMatching and warns about
// them.
func (r *Run) planCollModCollection(s migration.Step, log *slog.Logger) (*IndexChange, error) {
	set, err := migration.StepSet(s)
	if err != nil {
		return nil, err
	}
	info, err := r.planCollection(s.Collection)
	if err != nil {
		return nil, err
	}
	if err := collModCollectionRejected(s.Collection, info); err != nil {
		c := IndexChange{Action: IndexModify, Collection: s.Collection, Set: rawExtJSON(set), Conflict: err.Error()}
		return &c, err
	}
	have := info.Validation
	c := validationChange(IndexModify, s.Collection, have, set)
	if have.unchanged(set) {
		c.Action = IndexPresent
		log.Info("validation options already set", "set", extJSON(set))
		return &c, nil
	}
	switch ok, err := r.collModAllowed(s.Collection); {
	case err != nil:
		return nil, err
	case !ok:
		err := fmt.Errorf("the database user lacks the collMod action on %s.%s, which readWrite doesn't grant, "+
			"so apply would fail: grant it with a role of its own", r.db.Name(), s.Collection)
		c.Conflict = err.Error()
		return &c, err
	}
	after := have.modified(set)
	if len(after.Validator) > 0 && after.Level != "off" {
		n, err := r.db.Collection(s.Collection).CountDocuments(r.ctx, bson.D{{Key: "$nor", Value: bson.A{after.Validator}}})
		if err != nil {
			return nil, fmt.Errorf("count the documents that don't match the validator: %w", err)
		}
		c.NonMatching = &n
		if n > 0 {
			c.Warning = invalidDocumentsWarning(s.Collection, n, after)
		}
	}
	attrs := []any{"before", extJSON(have.current(set)), "set", extJSON(set)}
	if c.NonMatching != nil {
		attrs = append(attrs, "nonMatching", *c.NonMatching)
	}
	log.Info("validation to modify", attrs...)
	if c.Warning != "" {
		log.Warn(c.Warning)
	}
	info.Validation = after
	return &c, nil
}

// invalidDocumentsWarning explains what becomes of the n documents of coll
// that don't match the validator of v, which applies.
func invalidDocumentsWarning(coll string, n int64, v validation) string {
	match := "don't match"
	if n == 1 {
		match = "doesn't match"
	}
	docs := fmt.Sprintf("%d %s of %s %s the new validator: MongoDB keeps them as they are, and", n, plural(int(n), "document"), coll, match)
	switch {
	case v.Action == "warn":
		return docs + " with validationAction warn it accepts every write, logging those that don't match"
	case v.Level == "moderate":
		return docs + " with validationLevel moderate it lets updates to them through; " +
			"it rejects inserts, and updates to matching documents, that don't match"
	default:
		return docs + " rejects every update to them that leaves them not matching: " +
			"fix them in a data migration first, or start with validationLevel moderate"
	}
}
