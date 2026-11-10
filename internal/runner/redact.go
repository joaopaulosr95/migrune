package runner

import (
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// serverMessageHidden ends errorText's text when it left out a server's
// message.
const serverMessageHidden = "server message hidden; rerun plan with --sample-values to see it"

// errorText is the text migrune shows for err, in the report, the logs and
// changelog StepError messages. With values, it is err's own text. Without,
// it keeps no text a server wrote: each server error in err's chain shows
// only its code name and code, and a write error the failing document's
// _id; migrune's own text around them stays, and serverMessageHidden ends
// it. A server's message can quote document values, and which ones do isn't
// a closed list, so none is shown. Errors that aren't server errors, such as
// network errors, keep their text: they quote no document values.
func errorText(err error, values bool) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if values {
		return text
	}
	rs := redactions(err)
	if len(rs) == 0 {
		return text
	}
	for _, r := range rs {
		if !strings.Contains(text, r.from) {
			// err's text doesn't quote this server error as is, so there's no
			// telling which part of it is the server's: keep only the codes.
			shown := make([]string, len(rs))
			for i, r := range rs {
				shown[i] = r.to
			}
			return strings.Join(shown, ", ") + "; " + serverMessageHidden
		}
	}
	for _, r := range rs {
		text = strings.ReplaceAll(text, r.from, r.to)
	}
	if strings.Contains(text, "\n") {
		return text + "\n" + serverMessageHidden
	}
	return text + "; " + serverMessageHidden
}

// redactedError is an error whose text is errorText's; it unwraps to the
// original error, so errors.Is and errors.As still see the server's error.
type redactedError struct {
	err  error
	text string
}

func (e *redactedError) Error() string { return e.text }

func (e *redactedError) Unwrap() error { return e.err }

// redact returns err with errorText's text, for an error that leaves the
// runner.
func redact(err error, values bool) error {
	if err == nil || values {
		return err
	}
	text := errorText(err, false)
	if text == err.Error() {
		return err
	}
	return &redactedError{err: err, text: text}
}

// RedactError is redact, for an error of the runner's that a command returns
// without going through Execute, such as CheckFormats'.
func RedactError(err error, values bool) error { return redact(err, values) }

// errText is errorText for the run's --sample-values.
func (r *Run) errText(err error) string { return errorText(err, r.sampleValues) }

type redaction struct{ from, to string }

// redactions returns, for each server error in err's chain, its text and the
// text to show instead.
func redactions(err error) []redaction {
	var to string
	switch e := err.(type) {
	case *redactedError:
		// Already redacted.
		return nil
	case mongo.CommandError:
		if e.Code != 0 || e.Name != "" || len(e.Raw) > 0 {
			to = codeText(e.Name, int(e.Code))
		}
	case mongo.WriteException:
		to = writeExceptionText("write exception: ", e.WriteConcernError, e.WriteErrors)
	case mongo.BulkWriteException:
		wes := make([]mongo.WriteError, len(e.WriteErrors))
		for i, we := range e.WriteErrors {
			wes[i] = we.WriteError
		}
		to = writeExceptionText("bulk write exception: ", e.WriteConcernError, wes)
	case mongo.WriteError:
		to = writeErrorText(e)
	case mongo.ServerError:
		codes := make([]string, 0, len(e.ErrorCodes()))
		for _, c := range e.ErrorCodes() {
			codes = append(codes, codeText("", c))
		}
		to = strings.Join(codes, ", ")
		if to == "" {
			to = codeText("", 0)
		}
	}
	if to != "" {
		if from := err.Error(); from != "" {
			return []redaction{{from, to}}
		}
		return nil
	}
	var out []redaction
	switch u := err.(type) {
	case interface{ Unwrap() error }:
		if inner := u.Unwrap(); inner != nil {
			out = redactions(inner)
		}
	case interface{ Unwrap() []error }:
		for _, inner := range u.Unwrap() {
			out = append(out, redactions(inner)...)
		}
	}
	return out
}

// codeNames names the codes of the write errors data migrations most often
// meet: the server sends a write error's code without its name.
var codeNames = map[int]string{
	2:     "BadValue",
	9:     "FailedToParse",
	13:    "Unauthorized",
	14:    "TypeMismatch",
	28:    "PathNotViable",
	52:    "DollarPrefixedFieldName",
	66:    "ImmutableField",
	121:   "DocumentValidationFailure",
	241:   "ConversionFailure",
	11000: "DuplicateKey",
}

// codeText is "(<name>, code <code>)", or as much of it as is known.
func codeText(name string, code int) string {
	if name == "" {
		name = codeNames[code]
	}
	switch {
	case code == 0 && name == "":
		return "(server error)"
	case code == 0:
		return "(" + name + ")"
	case name == "":
		return fmt.Sprintf("(code %d)", code)
	}
	return fmt.Sprintf("(%s, code %d)", name, code)
}

// writeExceptionText is the text of a write exception, as the driver words
// it, with writeErrorText for each write error and only the code of its
// write concern error.
func writeExceptionText(prefix string, wce *mongo.WriteConcernError, wes []mongo.WriteError) string {
	var causes []string
	if wce != nil {
		causes = append(causes, "write concern error: "+codeText(wce.Name, wce.Code))
	}
	if len(wes) > 0 {
		texts := make([]string, len(wes))
		for i, we := range wes {
			texts[i] = writeErrorText(we)
		}
		causes = append(causes, "write errors: ["+strings.Join(texts, ", ")+"]")
	}
	if len(causes) == 0 {
		return prefix + "no causes"
	}
	return prefix + strings.Join(causes, ", ")
}

// writeErrorText is a write error's code, and the failing document's _id
// when its details give it (a document validation failure's do).
func writeErrorText(we mongo.WriteError) string {
	text := codeText("", we.Code)
	if id, err := we.Details.LookupErr("failingDocumentId"); err == nil {
		if b, err := bson.MarshalExtJSON(bson.D{{Key: "failingDocumentId", Value: id}}, false, false); err == nil {
			return text + " " + string(b)
		}
	}
	return text
}
