package lint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// checkFile runs the rules that need more than the schema on one file, raw
// being its content. It reads the file as plain JSON, so it also works on
// files the schema rejects; a file that isn't JSON at all is left to
// migration.LoadFile.
func checkFile(path, stem string, raw []byte) []Finding {
	c := checker{path: path, raw: raw}
	if i := bytes.IndexByte(raw, '\r'); i >= 0 {
		c.add(RuleLineEndings, fmt.Sprintf("line %d ends with a carriage return (CRLF): %s", bytes.Count(raw[:i], []byte("\n"))+1, checksumAdvice))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return c.findings
	}
	if id, ok := doc["id"].(string); ok && id != stem {
		c.add(RuleID, fmt.Sprintf("/id: %q doesn't match the file name; want %q", id, stem))
	}
	steps, _ := doc["steps"].([]any)
	for i, s := range steps {
		step, ok := s.(map[string]any)
		if !ok {
			continue
		}
		ptr := "/steps/" + strconv.Itoa(i)
		op, _ := step["operation"].(string)
		if filter, ok := step["filter"].(map[string]any); ok && len(filter) == 0 && (op == "deleteMany" || op == "updateMany") {
			coll, _ := step["collection"].(string)
			c.warn(RuleEmptyFilter, fmt.Sprintf("%s/filter: {} matches every document in %s: make sure this %s is meant to touch them all, and check the counts plan reports",
				ptr, coll, op))
		}
		if opts, ok := step["options"].(map[string]any); ok && op == "createIndex" {
			if ttl, ok := opts["expireAfterSeconds"]; ok {
				keys, _ := step["keys"].(map[string]any)
				coll, _ := step["collection"].(string)
				c.warn(RuleTTLIndex, fmt.Sprintf("%s/options/expireAfterSeconds: a TTL index deletes the documents of %s whose %s is more than %s seconds old, "+
					"from the moment it is built and from then on: check the wouldExpire count plan reports for it",
					ptr, coll, strings.Join(sortedKeys(keys), ", "), jsonString(ttl)))
			}
		}
		if set, ok := step["set"].(map[string]any); ok && op == "collModIndex" {
			if ttl, ok := set["expireAfterSeconds"]; ok {
				coll, _ := step["collection"].(string)
				name, _ := step["name"].(string)
				c.warn(RuleTTLIndex, fmt.Sprintf("%s/set/expireAfterSeconds: with a TTL, index %s deletes the documents of %s whose indexed date is more than %s seconds old, "+
					"from the moment it is set and from then on: check the wouldExpire count plan reports for it",
					ptr, name, coll, jsonString(ttl)))
			}
		}
		p, _ := step["pipeline"].([]any)
		catalog := op == "aggregate" && insertPipeline(p)
		c.serverJS(ptr, step, op == "aggregate", catalog)
		if op == "aggregate" {
			if p != nil {
				c.mergePipeline(ptr+"/pipeline", p)
				c.volatile(ptr+"/pipeline", p, catalog)
			}
			continue
		}
		for _, field := range []string{"update", "pipeline"} {
			if p, ok := step[field].([]any); ok {
				c.pipeline(ptr+"/"+field, p)
			}
		}
	}
	return c.findings
}

// checksumAdvice explains a line-endings finding.
const checksumAdvice = "the checksum covers the file's bytes, so a checkout or an editor that changes them " +
	"would fail every database where the migration is applied. Save the file as UTF-8 without a BOM, with LF line endings, " +
	"and keep git and formatters from rewriting it (migrations/*.json -text in .gitattributes)"

type checker struct {
	path     string
	raw      []byte
	ordered  bson.Raw
	findings []Finding
}

func (c *checker) add(rule, msg string) {
	c.findings = append(c.findings, Finding{File: c.path, Severity: Error, Rule: rule, Message: msg})
}

func (c *checker) warn(rule, msg string) {
	c.findings = append(c.findings, Finding{File: c.path, Severity: Warning, Rule: rule, Message: msg})
}

// mergePipeline checks the pipeline of a merge step: its $merge is the last
// stage. The schema checks there is one, with the allowed options, and no
// $out.
func (c *checker) mergePipeline(ptr string, stages []any) {
	for i, s := range stages {
		sp := ptr + "/" + strconv.Itoa(i)
		for _, name := range stageNames(s) {
			if name == "$merge" && i != len(stages)-1 {
				c.add(RuleMergeLast, fmt.Sprintf("%s: $merge is stage %d of %d; it must be the last stage", sp, i+1, len(stages)))
			}
			c.subPipelines(sp+"/"+migration.PointerToken(name), name, s.(map[string]any)[name])
		}
	}
}

// pipeline checks the pipelines nested in the stages of a pipeline outside
// a merge step, such as an update pipeline. The schema refuses $out and
// $merge as its own stages.
func (c *checker) pipeline(ptr string, stages []any) {
	for i, s := range stages {
		sp := ptr + "/" + strconv.Itoa(i)
		for _, name := range stageNames(s) {
			c.subPipelines(sp+"/"+migration.PointerToken(name), name, s.(map[string]any)[name])
		}
	}
}

// subPipelines checks the pipelines nested in a stage: $lookup.pipeline,
// $unionWith.pipeline and every $facet output. None may write with $out or
// $merge, at any depth.
func (c *checker) subPipelines(ptr, stage string, v any) {
	arg, _ := v.(map[string]any)
	switch stage {
	case "$lookup", "$unionWith":
		if p, ok := arg["pipeline"].([]any); ok {
			c.nested(ptr+"/pipeline", stage, p)
		}
	case "$facet":
		for _, name := range sortedKeys(arg) {
			if p, ok := arg[name].([]any); ok {
				c.nested(ptr+"/"+migration.PointerToken(name), stage, p)
			}
		}
	}
}

func (c *checker) nested(ptr, parent string, stages []any) {
	for i, s := range stages {
		sp := ptr + "/" + strconv.Itoa(i)
		for _, name := range stageNames(s) {
			switch name {
			case "$out", "$merge":
				c.add(RuleNested, fmt.Sprintf("%s: %s is not allowed inside a %s pipeline", sp, name, parent))
			default:
				c.subPipelines(sp+"/"+migration.PointerToken(name), name, s.(map[string]any)[name])
			}
		}
	}
}

// serverJSOperators run JavaScript on the server.
var serverJSOperators = []string{"$where", "$function", "$accumulator"}

// walkOperators skips literal operands. Query comparisons take BSON data,
// while comparisons below $expr take aggregation expressions.
func (c *checker) walkOperators(ptr string, v any, parent string, query, catalog bool, visit func(ptr, operator, variable string)) {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(x) {
			if query && parent == "$all" && k != "$elemMatch" {
				continue
			}
			kp := ptr + "/" + migration.PointerToken(k)
			visit(kp, k, "")
			if catalog && (k == "$literal" || k == "$const") {
				continue
			}
			if query {
				// A field compared directly to an array or a document with
				// ordinary field names is an implicit equality predicate.
				if !strings.HasPrefix(k, "$") && queryLiteral(x[k]) {
					continue
				}
				switch k {
				case "$eq", "$ne", "$lt", "$lte", "$gt", "$gte", "$in", "$nin":
					continue
				}
			}
			isQuery := catalog && (query && k != "$expr" || k == "$match" ||
				parent == "$geoNear" && k == "query" || parent == "$graphLookup" && k == "restrictSearchWithMatch")
			c.walkOperators(kp, x[k], k, isQuery, catalog, visit)
		}
	case []any:
		if query && parent == "$all" && !c.allUsesElemMatch(ptr, x) {
			return
		}
		for i, e := range x {
			c.walkOperators(ptr+"/"+strconv.Itoa(i), e, parent, query, catalog, visit)
		}
	case string:
		if !query {
			visit(ptr, "", x)
		}
	}
}

// MongoDB selects the predicate form of $all only when the first operand's
// first key is $elemMatch. Only a multi-key operand needs the file's BSON order.
func (c *checker) allUsesElemMatch(ptr string, operands []any) bool {
	if len(operands) == 0 {
		return false
	}
	doc, _ := operands[0].(map[string]any)
	if _, ok := doc["$elemMatch"]; !ok {
		return false
	}
	if len(doc) == 1 {
		return true
	}
	if c.raw != nil {
		// Files with invalid Extended JSON still receive the other lint checks.
		_ = bson.UnmarshalExtJSON(c.raw, false, &c.ordered)
		c.raw = nil
	}
	tokens := strings.Split(strings.TrimPrefix(ptr, "/")+"/0", "/")
	for i, token := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
	}
	operand, _ := c.ordered.Lookup(tokens...).DocumentOK()
	first, err := operand.IndexErr(0)
	return err == nil && first.Key() == "$elemMatch"
}

func queryLiteral(value any) bool {
	doc, ok := value.(map[string]any)
	if !ok {
		return true
	}
	for k := range doc {
		if !strings.HasPrefix(k, "$") {
			return true
		}
	}
	return false
}

// In a merge step, volatile also checks $function and $accumulator.
func (c *checker) serverJS(ptr string, v any, merge, catalog bool) {
	c.walkOperators(ptr, v, "", false, catalog, func(kp, k, _ string) {
		if slices.Contains(serverJSOperators, k) && (k == "$where" || !merge) {
			c.add(RuleServerJS, kp+": "+k+" runs JavaScript on the server: migrune can't check what it does, "+
				"it is slow, often disabled, and deprecated from MongoDB 8.0; use aggregation operators instead")
		}
	})
}

func insertPipeline(p []any) bool {
	for _, stage := range p {
		doc, _ := stage.(map[string]any)
		opts, _ := doc["$merge"].(map[string]any)
		if opts["whenNotMatched"] == "insert" {
			return true
		}
	}
	return false
}

// volatileOperators give a different result on each run.
var volatileOperators = map[string]string{
	"$rand":        "returns a new random number on each run",
	"$function":    "runs JavaScript migrune can't check",
	"$accumulator": "runs JavaScript migrune can't check",
}

// volatileVariables give a different value on each run.
var volatileVariables = []string{"$$NOW", "$$CLUSTER_TIME"}

// volatile reports, anywhere in a merge pipeline v, the operators and
// variables whose value changes from one run to the next: a merge computed
// from them never settles, and plan can't tell.
func (c *checker) volatile(ptr string, v any, insert bool) {
	c.walkOperators(ptr, v, "", false, insert, func(kp, k, value string) {
		reason := volatileOperators[k]
		if insert && (k == "$sample" || k == "$sampleRate" || k == "$createObjectId") {
			reason = "can change the output identities on each run"
		}
		if reason != "" {
			c.add(RuleVolatileMerge, kp+": "+k+" "+reason+volatileAdvice)
		}
		for _, name := range volatileVariables {
			if value == name || strings.HasPrefix(value, name+".") {
				c.add(RuleVolatileMerge, kp+": "+name+" changes on each run"+volatileAdvice)
			}
		}
	})
}

const volatileAdvice = ": a merge must give the same output when run again, computed from fields it doesn't write, " +
	"or it never settles; plan can't detect that, and apply only after writing"

// stageNames returns the keys of a pipeline stage, sorted. A valid stage has
// exactly one.
func stageNames(stage any) []string {
	m, _ := stage.(map[string]any)
	return sortedKeys(m)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
