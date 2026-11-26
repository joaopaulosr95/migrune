package runner

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// compareInsertRows consumes one source evaluation for the counts, identity
// checks and samples. MongoDB ranks the wrapped keys using its own BSON equality.
// Sorting by the same key keeps each group together across cursor batches.
func (r *Run) compareInsertRows(coll string, pipeline []any, t mergeTarget, log *slog.Logger, opts *options.AggregateOptionsBuilder) (mergeComparison, error) {
	pipeline = append(pipeline, insertComparisonStages(r.sampleValues)...)
	var out mergeComparison
	var group, matched, missing, extraDiff int64
	finishGroup := func() {
		if matched+missing > 1 {
			out.DuplicateOn += matched
			out.DuplicateMissing += missing
		}
		matched, missing = 0, 0
	}
	var typeSamples, otherSamples []MergeSample
	err := r.aggregate(coll, pipeline, log, func(ctx context.Context, cur *mongo.Cursor) error {
		for cur.Next(ctx) {
			var row struct {
				Sample      comparisonSample `bson:",inline"`
				Group       int64
				ReadIDs     bool
				Matched     bool
				Diff        bool
				TypeChanged bool
				Totals      *mergeComparison
				IDType      string
				NullOn      bool
				ArrayOn     bool
			}
			if err := cur.Decode(&row); err != nil {
				return err
			}
			if group != row.Group {
				finishGroup()
				group = row.Group
			}
			var id, targetID bson.RawValue
			if row.ReadIDs {
				source, err := readComparisonDocument(ctx, cur)
				if err != nil {
					return err
				}
				id = source.Lookup("_id")
				row.Sample.Key = []bson.RawValue{id}
				if row.Matched {
					target, err := readComparisonDocument(ctx, cur)
					if err != nil {
						return err
					}
					targetID = target.Lookup("_id")
					if r.sampleValues {
						for i := range row.Sample.Diffs {
							f := &row.Sample.Diffs[i]
							f.Before = target.Lookup(f.Field)
							f.After = source.Lookup(f.Field)
						}
					}
				}
			}
			mismatch := row.ReadIDs && row.Matched && !id.Equal(targetID)
			switch {
			case row.NullOn || row.ArrayOn:
				// Counted in the totals and excluded from duplicate groups.
			case id.Type == bson.TypeRegex || id.Type == bson.TypeUndefined ||
				(id.Type == bson.TypeEmbeddedDocument && !validStoredIDDocument(id.Document(), idDocument)):
				out.InvalidID++
			case row.Matched:
				matched++
				if mismatch {
					out.IDMismatch++
					// MongoDB can compare equal even when the immutable BSON differs.
					if !row.Diff {
						extraDiff++
						if len(otherSamples) < MaxMergeSamples {
							row.Sample.Diffs = []comparisonField{{Field: "_id", Before: targetID, After: id,
								BeforeType: row.IDType, AfterType: row.IDType}}
						}
					}
				}
			default:
				missing++
			}
			if row.Totals != nil {
				out.Diff, out.TypeChanged = row.Totals.Diff, row.Totals.TypeChanged
				out.Unmatched, out.NullOn, out.ArrayOn = row.Totals.Unmatched, row.Totals.NullOn, row.Totals.ArrayOn
				out.InvalidFieldName = row.Totals.InvalidFieldName
			}
			if len(row.Sample.Diffs) != 0 {
				for i := range row.Sample.Diffs {
					field := &row.Sample.Diffs[i]
					if field.Field == "_id" {
						if field.Same && !mismatch {
							row.Sample.Diffs = slices.Delete(row.Sample.Diffs, i, i+1)
						} else {
							field.Before, field.After = targetID, id
						}
						break
					}
				}
				samples := &otherSamples
				if row.TypeChanged {
					samples = &typeSamples
				}
				if len(*samples) < MaxMergeSamples {
					*samples = append(*samples, r.mergeSample(row.Sample, t))
				}
			}
		}
		return cur.Err()
	}, opts)
	finishGroup()
	out.Diff += extraDiff
	samples := append(typeSamples, otherSamples...)
	out.Samples = samples[:min(len(samples), MaxMergeSamples)]
	return out, err
}

// Source and target travel separately so a pair of legal documents fits in the
// cursor. Decode owns the BSON across batches, including while reading the pair.
func readComparisonDocument(ctx context.Context, cur *mongo.Cursor) (bson.Raw, error) {
	if !cur.Next(ctx) {
		if err := cur.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("insert comparison ended before its ID")
	}
	var doc bson.Raw
	if err := cur.Decode(&doc); err != nil {
		return nil, err
	}
	if doc.Lookup("_id").Type == 0 {
		return nil, errors.New("insert comparison returned a missing ID record")
	}
	return doc, nil
}

// insertComparisonStages keeps MongoDB's native identity groups and bounded
// samples in the same stream that supplies the original BSON IDs.
func insertComparisonStages(sampleValues bool) []any {
	type m = bson.M
	order := bson.D{{Key: "identity", Value: 1}}
	var pipeline []any
	var source, target any = m{"_id": "$identity.id"}, m{"_id": "$targetID"}
	if sampleValues {
		// Keep only fields that differ. Each side is a subset of one original
		// document and can travel with its own _id without exceeding 16 MiB.
		sampleFields := func(side string) m {
			value := "$$d." + side
			present := m{"$filter": m{"input": "$diffs", "as": "d", "cond": m{"$ne": bson.A{m{"$type": value}, "missing"}}}}
			fields := m{"$map": m{"input": present, "as": "d", "in": m{"k": "$$d.field", "v": value}}}
			return m{"$cond": bson.A{"$diff", m{"$arrayToObject": fields}, "$$REMOVE"}}
		}
		pipeline = append(pipeline, m{"$set": m{"after": sampleFields("after"), "before": sampleFields("before")}})
		source = m{"$cond": bson.A{"$sample", m{"$mergeObjects": bson.A{source, "$after"}}, source}}
		target = m{"$cond": bson.A{"$sample", m{"$mergeObjects": bson.A{target, "$before"}}, target}}
	}
	pipeline = append(pipeline, m{"$unset": bson.A{"diffs.before", "diffs.after"}})
	// Return at most five ordinary samples and five type changes. Both
	// running counts use the same sort as the native identity groups.
	runningCount := func(flag any) m {
		return m{"$sum": m{"$cond": bson.A{flag, 1, 0}}, "window": m{"documents": bson.A{"unbounded", "current"}}}
	}
	sample := m{"$and": bson.A{"$diff", m{"$or": bson.A{
		m{"$lte": bson.A{"$diffNumber", MaxMergeSamples}},
		m{"$and": bson.A{"$typeChanged", m{"$lte": bson.A{"$typeNumber", MaxMergeSamples}}}},
	}}}}
	neighbor := func(by int) m { return m{"$shift": m{"by": by, "output": "$identity"}} }
	// A native match for these same-type scalar IDs implies identical BSON.
	// Other matched IDs, and all document-valued IDs, need the raw pair.
	canonical := m{"$in": bson.A{"$idType", bson.A{"objectId", "string", "int", "long", "bool", "date", "timestamp"}}}
	raw := m{"$or": bson.A{
		m{"$in": bson.A{"$idType", bson.A{"object", "regex", "undefined"}}},
		m{"$and": bson.A{"$matched", m{"$or": bson.A{m{"$not": bson.A{canonical}}, m{"$ne": bson.A{"$idType", m{"$type": "$targetID"}}}}}}},
	}}
	totals := m{"diff": "$diffNumber", "typeChanged": "$typeNumber", "unmatched": "$unmatched",
		"nullOn": "$nulls", "arrayOn": "$arrays", "invalidFieldName": "$invalidNames"}
	metrics := m{"group": "$group", "idType": "$idType", "matched": "$matched", "diff": "$diff",
		"typeChanged": "$typeChanged", "nullOn": "$nullOn", "arrayOn": "$arrayOn", "readIDs": "$readIDs",
		"diffs":  m{"$cond": bson.A{"$sample", "$diffs", "$$REMOVE"}},
		"totals": m{"$cond": bson.A{"$last", totals, "$$REMOVE"}}}
	return append(pipeline,
		m{"$set": m{"identity": m{"id": m{"$first": "$key"}}, "idType": m{"$type": m{"$first": "$key"}}}},
		m{"$setWindowFields": m{"sortBy": order, "output": m{
			"group": m{"$denseRank": m{}}, "previous": neighbor(-1), "next": neighbor(1),
			"diffNumber": runningCount("$diff"), "typeNumber": runningCount("$typeChanged"),
			"unmatched": runningCount(m{"$not": bson.A{"$matched"}}), "nulls": runningCount("$nullOn"),
			"arrays": runningCount("$arrayOn"), "invalidNames": runningCount("$invalidFieldName"),
		}}},
		m{"$set": m{"last": m{"$eq": bson.A{"$next", nil}}, "sample": sample, "readIDs": m{"$or": bson.A{sample, raw}}}},
		// Only exceptional IDs, duplicate groups, bounded samples and the final
		// running totals cross the wire. Empty output has zero totals.
		m{"$match": m{"$expr": m{"$or": bson.A{"$last", "$readIDs",
			m{"$eq": bson.A{"$previous", "$identity"}}, m{"$eq": bson.A{"$next", "$identity"}}}}}},
		m{"$sort": order},
		m{"$set": m{"parts": m{"$cond": bson.A{"$readIDs", m{"$cond": bson.A{"$matched",
			bson.A{"metrics", "source", "target"}, bson.A{"metrics", "source"}}}, bson.A{"metrics"}}}}},
		m{"$unwind": "$parts"},
		m{"$replaceWith": m{"$switch": m{"branches": bson.A{
			m{"case": m{"$eq": bson.A{"$parts", "source"}}, "then": source},
			m{"case": m{"$eq": bson.A{"$parts", "target"}}, "then": target},
		}, "default": metrics}}},
	)
}

type idDocumentContext int

const (
	idDocument idDocumentContext = iota
	idDBRefPayload
	idCodeScope
)

// Ordinary DBRef payloads must be portable across servers. BSON code scopes
// inside a DBRef payload and DBRef payloads inside a code scope stay opaque.
func validStoredIDDocument(doc bson.Raw, where idDocumentContext) bool {
	fields, err := doc.Elements()
	if err != nil {
		return false
	}
	if len(fields) >= 2 && fields[0].Key() == "$ref" && fields[0].Value().Type == bson.TypeString && fields[1].Key() == "$id" {
		if where != idCodeScope && !validIDValue(fields[1].Value(), idDBRefPayload) {
			return false
		}
		fields = fields[2:]
		if len(fields) > 0 && fields[0].Key() == "$db" && fields[0].Value().Type == bson.TypeString {
			fields = fields[1:]
		}
	}
	for _, f := range fields {
		if strings.HasPrefix(f.Key(), "$") || !validIDValue(f.Value(), where) {
			return false
		}
	}
	return true
}

func validIDValue(v bson.RawValue, where idDocumentContext) bool {
	switch v.Type {
	case bson.TypeEmbeddedDocument, bson.TypeArray:
		return validStoredIDDocument(v.Value, where)
	case bson.TypeCodeWithScope:
		if where != idDBRefPayload {
			_, scope := v.CodeWithScope()
			return validStoredIDDocument(scope, idCodeScope)
		}
	}
	return true
}
