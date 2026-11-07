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

// violations flattens a validation error into its leaf causes, deduplicated,
// sorted by pointer, and with clearer messages where the validator's
// would be cryptic, so each problem in the file is reported once.
//
// A step whose operation is missing or unknown gets one violation on its
// operation instead of every failure of its fields.
func violations(ve *jsonschema.ValidationError, inst any) []Violation {
	doc, _ := inst.(map[string]any)
	steps, _ := doc["steps"].([]any)
	var (
		out       []Violation
		collapsed = map[int]bool{}
	)
	add := func(v Violation) {
		if !slices.Contains(out, v) {
			out = append(out, v)
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
		_, known := kinds[op]
		switch {
		case !present:
			stepViolations = append(stepViolations, Violation{ptr, "missing property 'operation'"})
		case !known:
			stepViolations = append(stepViolations, Violation{ptr + "/operation", unknownOperation(raw)})
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
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		add(Violation{ptr, leafMessage(e, ptr)})
	}
	walk(ve)
	// Then one per step whose operation is missing or unknown.
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
	"#/$defs/collection":    "the collection is reserved: " + reservedMessage,
	"#/$defs/noOut":         "$out is not allowed: it replaces the whole target collection",
	"#/$defs/noMerge":       "$merge is not allowed: it writes the pipeline's output into a collection",
	"#/$defs/dataStep/else": "update is not allowed: deleteMany takes no update",
}

// leafMessage renders a leaf violation at ptr.
func leafMessage(e *jsonschema.ValidationError, ptr string) string {
	switch e.ErrorKind.(type) {
	case *kind.Not:
		for suffix, msg := range notMessages {
			if !strings.HasSuffix(e.SchemaURL, suffix) {
				continue
			}
			return msg
		}
	}
	// A leaf renders as "at '<pointer>': <message>".
	msg := strings.TrimPrefix(e.Error(), "at "+quote(ptr)+": ")
	return msg
}

// unknownOperation explains a step operation migrune doesn't run.
func unknownOperation(op any) string {
	name, isString := op.(string)
	if !isString {
		name = jsonValue(op)
	}
	msg := name + " is not an operation migrune runs: use updateOne, updateMany or deleteMany"
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
