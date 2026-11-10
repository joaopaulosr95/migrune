package migration

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

//go:embed schema.json
var schemaJSON []byte

var schema = mustCompileSchema()

func mustCompileSchema() *jsonschema.Schema {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		panic(fmt.Sprintf("migration: embedded schema: %v", err))
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("schema.json", doc); err != nil {
		panic(fmt.Sprintf("migration: embedded schema: %v", err))
	}
	s, err := c.Compile("schema.json")
	if err != nil {
		panic(fmt.Sprintf("migration: embedded schema: %v", err))
	}
	return s
}

// SchemaError reports a file that doesn't match the migration file schema.
type SchemaError struct {
	File       string
	Violations []Violation
}

// Violation is one schema failure at a JSON pointer into the file.
type Violation struct {
	Pointer string
	Message string
}

func (e *SchemaError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: does not match the migration file schema:", e.File)
	for _, v := range e.Violations {
		ptr := v.Pointer
		if ptr == "" {
			ptr = "(root)"
		}
		fmt.Fprintf(&sb, "\n  %s: %s", ptr, v.Message)
	}
	return sb.String()
}

// stepKinds are the step definitions of the schema's top-level oneOf, in
// order: oneOf/0 is the data step, oneOf/1 the index step, oneOf/2 the merge
// step.
var stepKinds = []Kind{KindData, KindIndex, KindMerge}

// violations flattens a validation error into its leaf causes, deduplicated,
// sorted by pointer, and with clearer messages where the validator's
// would be cryptic, so each problem in the file is reported once.
//
// The schema's top-level oneOf makes the validator report the failures of
// every step kind. The file's kind is the kind of its first step with an
// allowed operation; only that branch's failures are kept. A step whose
// operation is missing, unknown, or of another kind gets one violation on
// its operation instead of every failure of its fields.
func violations(ve *jsonschema.ValidationError, inst any) []Violation {
	doc, _ := inst.(map[string]any)
	steps, _ := doc["steps"].([]any)
	var (
		out       []Violation
		fileKind  Kind
		firstOp   string
		collapsed = map[int]bool{}
	)
	add := func(v Violation) {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	for _, s := range steps {
		step, _ := s.(map[string]any)
		op, _ := step["operation"].(string)
		if k, ok := kinds[op]; ok {
			fileKind, firstOp = k, op
			break
		}
	}
	var stepViolations []Violation
	for i, s := range steps {
		step, isObject := s.(map[string]any)
		if !isObject {
			continue
		}
		ptr := "/steps/" + strconv.Itoa(i)
		raw, present := step["operation"]
		op, _ := raw.(string)
		k, known := kinds[op]
		switch {
		case !present:
			stepViolations = append(stepViolations, Violation{ptr, "missing property 'operation'"})
		case !known:
			stepViolations = append(stepViolations, Violation{ptr + "/operation", unknownOperation(raw)})
		case k != fileKind:
			stepViolations = append(stepViolations, Violation{ptr + "/operation", fmt.Sprintf(
				"%s belongs to %s migrations, but the file's first operation, %s, belongs to %s migrations; a file holds one kind of migration",
				op, k, firstOp, fileKind)})
		default:
			continue
		}
		collapsed[i] = true
	}
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if i, ok := stepIndex(e.InstanceLocation); ok && collapsed[i] {
			return
		}
		ptr := jsonPointer(e.InstanceLocation)
		switch k := e.ErrorKind.(type) {
		case *kind.OneOf:
			if len(e.InstanceLocation) == 0 && fileKind != "" {
				branch := "#/oneOf/" + strconv.Itoa(slices.Index(stepKinds, fileKind)) + "/"
				for _, c := range e.Causes {
					if strings.Contains(c.SchemaURL, branch) {
						walk(c)
					}
				}
				return
			}
			if len(e.Causes) == 0 {
				// More than one branch matched: steps is missing or
				// not an array of steps, which is reported on its own.
				return
			}
		case *kind.Contains, *kind.MinContains:
			// An empty pipeline has no causes here, and fails minItems.
			if len(e.Causes) > 0 {
				add(Violation{ptr, "the pipeline has no $merge stage: a merge step's pipeline ends with exactly one"})
			}
			return
		case *kind.MaxContains:
			stages := make([]string, len(k.Got))
			for j, g := range k.Got {
				stages[j] = strconv.Itoa(g)
			}
			add(Violation{ptr, fmt.Sprintf("the pipeline has %d $merge stages, at indexes %s: a merge step's pipeline ends with exactly one",
				len(k.Got), strings.Join(stages, ", "))})
			return
		}
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		add(Violation{ptr, leafMessage(e, ptr)})
	}
	walk(ve)
	// Then one per step whose operation is missing, unknown or of another
	// kind.
	for _, v := range stepViolations {
		add(v)
	}
	// The validator walks properties in map order, so sort by pointer to
	// report the same file the same way every run.
	slices.SortStableFunc(out, func(a, b Violation) int { return comparePointers(a.Pointer, b.Pointer) })
	return out
}

// comparePointers orders JSON pointers segment by segment, array indexes
// numerically, so /steps/2 comes before /steps/10.
func comparePointers(a, b string) int {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	for i := range min(len(as), len(bs)) {
		ai, aerr := strconv.Atoi(as[i])
		bi, berr := strconv.Atoi(bs[i])
		c := strings.Compare(as[i], bs[i])
		if aerr == nil && berr == nil {
			c = ai - bi
		}
		if c != 0 {
			return c
		}
	}
	return len(as) - len(bs)
}

// notMessages explain the schema's "not" keywords, by the location of the
// schema holding them.
var notMessages = map[string]string{
	"#/$defs/collection":                     "the collection is reserved: " + reservedMessage,
	"#/$defs/noOut":                          "$out is not allowed: it replaces the whole target collection",
	"#/$defs/noMerge":                        "$merge is only allowed as the last stage of an aggregate step in a merge migration",
	"#/$defs/dataStep/else":                  "update is not allowed: deleteMany takes no update",
	"#/$defs/indexStep/allOf/0/then/allOf/0": "name is not allowed in createIndex: name the index in options.name",
	"#/$defs/indexStep/allOf/0/then/allOf/1": "set is not allowed in createIndex: give the options in options; to change an existing index, use collModIndex",
	"#/$defs/indexStep/allOf/1/then/allOf/0": "keys and options are not allowed in dropIndex: it takes only the name of the index",
	"#/$defs/indexStep/allOf/1/then/allOf/1": "set is not allowed in dropIndex: it takes only the name of the index",
	"#/$defs/indexStep/allOf/2/then":         "keys and options are not allowed in collModIndex: it takes the name of the index and the options to change in set",
	"#/$defs/indexStep/allOf/3/then":         "keys, options and name are not allowed in collModCollection: it takes the validation options to change in set",
	"#/$defs/indexSet": "prepareUnique can't be combined with another option: MongoDB sets it in a collMod of its own. " +
		"Set prepareUnique in one step, and the other options in another",
}

// mergeHint follows a violation on a $merge stage itself.
const mergeHint = `: $merge takes into, on, whenMatched: "merge" and whenNotMatched: "discard"`

// leafMessage renders a leaf violation at ptr.
func leafMessage(e *jsonschema.ValidationError, ptr string) string {
	switch k := e.ErrorKind.(type) {
	case *kind.Not:
		for suffix, msg := range notMessages {
			if !strings.HasSuffix(e.SchemaURL, suffix) {
				continue
			}
			if suffix == "#/$defs/noOut" && strings.Contains(ptr, "/pipeline/") {
				msg += `. Use $merge with whenMatched: "merge" and whenNotMatched: "discard"`
			}
			return msg
		}
	case *kind.Const:
		return fmt.Sprintf("must be %s, got %s", jsonValue(k.Want), jsonValue(k.Got))
	}
	// A leaf renders as "at '<pointer>': <message>".
	msg := strings.TrimPrefix(e.Error(), "at "+quote(ptr)+": ")
	if strings.HasSuffix(ptr, "/$merge") {
		msg += mergeHint
	}
	return msg
}

// unknownOperation explains a step operation migrune doesn't run.
func unknownOperation(op any) string {
	name, isString := op.(string)
	if !isString {
		name = jsonValue(op)
	}
	msg := name + " is not an operation migrune runs: use updateOne, updateMany or deleteMany (data), " +
		"createIndex, dropIndex, collModIndex or collModCollection (index), or aggregate (merge)"
	if strings.HasPrefix(name, "insert") {
		msg += "; seed documents with updateOne, upsert: true and $setOnInsert"
	}
	return msg
}

// stepIndex returns i when tokens points into /steps/i.
func stepIndex(tokens []string) (int, bool) {
	if len(tokens) < 2 || tokens[0] != "steps" {
		return 0, false
	}
	i, err := strconv.Atoi(tokens[1])
	return i, err == nil
}

func jsonValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func jsonPointer(tokens []string) string {
	var sb strings.Builder
	for _, t := range tokens {
		sb.WriteString("/" + PointerToken(t))
	}
	return sb.String()
}

// quote matches how the validator quotes instance locations in its messages.
func quote(s string) string {
	s = fmt.Sprintf("%q", s)
	s = strings.ReplaceAll(s, `\"`, `"`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s[1:len(s)-1] + "'"
}

// PointerToken escapes a JSON pointer reference token.
func PointerToken(t string) string {
	return strings.ReplaceAll(strings.ReplaceAll(t, "~", "~0"), "/", "~1")
}
