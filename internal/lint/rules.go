package lint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// checkFile runs the rules that need more than the schema on one file, raw
// being its content. It reads the file as plain JSON, so it also works on
// files the schema rejects; a file that isn't JSON at all is left to
// migration.LoadFile.
func checkFile(path, stem string, raw []byte) []Finding {
	c := checker{path: path}
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
		c.serverJS(ptr, step)
		if p, ok := step["update"].([]any); ok {
			c.pipeline(ptr+"/update", p)
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
	findings []Finding
}

func (c *checker) add(rule, msg string) {
	c.findings = append(c.findings, Finding{File: c.path, Severity: Error, Rule: rule, Message: msg})
}

func (c *checker) warn(rule, msg string) {
	c.findings = append(c.findings, Finding{File: c.path, Severity: Warning, Rule: rule, Message: msg})
}

// pipeline checks the pipelines nested in the stages of an update pipeline.
// The schema refuses $out and $merge as its own stages.
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

// serverJS reports, anywhere in v, the operators that run JavaScript on the
// server.
func (c *checker) serverJS(ptr string, v any) {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(x) {
			kp := ptr + "/" + migration.PointerToken(k)
			if slices.Contains(serverJSOperators, k) {
				c.add(RuleServerJS, kp+": "+k+" runs JavaScript on the server: migrune can't check what it does, "+
					"it is slow, often disabled, and deprecated from MongoDB 8.0; use aggregation operators instead")
			}
			c.serverJS(kp, x[k])
		}
	case []any:
		for i, e := range x {
			c.serverJS(ptr+"/"+strconv.Itoa(i), e)
		}
	}
}

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
