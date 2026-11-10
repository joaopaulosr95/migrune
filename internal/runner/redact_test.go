package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// serverErrorFixture is an error captured from a server, in
// testdata/servererrors/<version>/: the driver's error type and text, and
// the server's response.
type serverErrorFixture struct {
	Type  string          `json:"type"`
	Error string          `json:"error"`
	Raw   json.RawMessage `json:"raw"`
}

// serverError rebuilds the driver's error from the fixture
// testdata/servererrors/<name>.json, and checks its text is the one
// captured, which it returns.
func serverError(t *testing.T, name string) (captured string, err error) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "servererrors", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var f serverErrorFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	var raw bson.Raw
	if err := bson.UnmarshalExtJSON(f.Raw, true, &raw); err != nil {
		t.Fatal(err)
	}
	var e error
	switch f.Type {
	case "mongo.WriteException":
		var res struct {
			WriteErrors []struct {
				Index   int      `bson:"index"`
				Code    int      `bson:"code"`
				Message string   `bson:"errmsg"`
				Info    bson.Raw `bson:"errInfo"`
			} `bson:"writeErrors"`
		}
		if err := bson.Unmarshal(raw, &res); err != nil {
			t.Fatal(err)
		}
		we := mongo.WriteException{Raw: raw}
		for _, w := range res.WriteErrors {
			we.WriteErrors = append(we.WriteErrors, mongo.WriteError{Index: w.Index, Code: w.Code, Message: w.Message, Details: w.Info})
		}
		e = we
	case "mongo.CommandError":
		var res struct {
			Code    int32  `bson:"code"`
			Name    string `bson:"codeName"`
			Message string `bson:"errmsg"`
		}
		if err := bson.Unmarshal(raw, &res); err != nil {
			t.Fatal(err)
		}
		e = mongo.CommandError{Code: res.Code, Name: res.Name, Message: res.Message, Raw: raw}
	default:
		t.Fatalf("%s: unknown error type %s", name, f.Type)
	}
	if e.Error() != f.Error {
		t.Fatalf("%s: rebuilt error %q, captured %q", name, e.Error(), f.Error)
	}
	return f.Error, e
}

// valuesInFixtures are the document values the captured errors quote.
var valuesInFixtures = []string{"ada@example.com", "bob@example.com", "ada.example.com", `a} "x"`, "consideredValue", `"-3"`,
	"ada-secret-42", "ada-secret-name", "40734f8e"}

// serverMessages returns the messages the server wrote in err.
func serverMessages(err error) []string {
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		return []string{ce.Message}
	}
	var we mongo.WriteException
	var msgs []string
	if errors.As(err, &we) {
		for _, w := range we.WriteErrors {
			msgs = append(msgs, w.Message)
		}
	}
	return msgs
}

// TestErrorText checks errorText on errors captured from MongoDB 7.0 and
// 9.0: without values, each shows its code name and code (and the failing
// document's _id) but no text the server wrote; with values, it is the
// driver's text.
func TestErrorText(t *testing.T) {
	const (
		dup        = "write exception: write errors: [(DuplicateKey, code 11000)]"
		validation = `write exception: write errors: [(DocumentValidationFailure, code 121) {"failingDocumentId":1}]`
		convert    = "(CannotConvertIndexToUnique, code 359)"
	)
	cases := []struct{ name, want string }{
		{"9.0/update-dupkey", dup},
		{"7.0/update-dupkey", dup},
		{"9.0/update-dupkey-compound", dup},
		{"7.0/update-dupkey-compound", dup},
		{"9.0/upsert-dupkey", dup},
		{"7.0/upsert-dupkey", dup},
		{"9.0/insert-prepareunique", dup},
		{"7.0/insert-prepareunique", dup},
		{"9.0/merge-dupkey", "(DuplicateKey, code 11000)"},
		{"7.0/merge-dupkey", "(DuplicateKey, code 11000)"},
		{"9.0/createindex-dupkey", "(DuplicateKey, code 11000)"},
		{"7.0/createindex-dupkey", "(DuplicateKey, code 11000)"},
		{"9.0/update-validation", validation},
		{"7.0/update-validation", validation},
		{"9.0/update-validation-query", validation},
		{"7.0/update-validation-query", validation},
		{"9.0/merge-validation", "(DocumentValidationFailure, code 121)"},
		{"7.0/merge-validation", "(DocumentValidationFailure, code 121)"},
		{"9.0/collmod-unique", convert},
		{"7.0/collmod-unique", convert},
		{"9.0/collmod-unique-dryrun", convert},
		{"7.0/collmod-unique-dryrun", convert},
		// The server quotes the value $convert failed on, or the document a
		// path ran into.
		{"9.0/update-convert", "write exception: write errors: [(ConversionFailure, code 241)]"},
		{"9.0/merge-convert", "(ConversionFailure, code 241)"},
		{"9.0/update-inc-type", "write exception: write errors: [(TypeMismatch, code 14)]"},
		{"9.0/update-path-type", "write exception: write errors: [(PathNotViable, code 28)]"},
		// It quotes the command, session id included.
		{"9.0/update-unauthorized", "(Unauthorized, code 13)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captured, err := serverError(t, tc.name)
			want := tc.want + "; " + serverMessageHidden
			if got := errorText(err, false); got != want {
				t.Errorf("errorText without values:\n got %s\nwant %s", got, want)
			}
			if got := errorText(err, true); got != captured {
				t.Errorf("errorText with values = %q, want the driver's %q", got, captured)
			}

			// Wrapped as a failed step of a migration, with migrune's hint
			// after it, and joined with another error.
			wrapped := errors.Join(&MigrationError{ID: "m1", Err: &StepFailure{Step: 0, Operation: "updateOne", Collection: "users",
				Err: fmt.Errorf("merge into users: %w; a hint of migrune's", err)}}, errors.New("release the lock"))
			want = "migration m1 failed: /steps/0 (updateOne on users): merge into users: " + tc.want +
				"; a hint of migrune's\nrelease the lock\n" + serverMessageHidden
			if got := errorText(wrapped, false); got != want {
				t.Errorf("errorText of the wrapped error without values:\n got %s\nwant %s", got, want)
			}
			if got := errorText(wrapped, true); got != wrapped.Error() {
				t.Errorf("errorText of the wrapped error with values = %q, want %q", got, wrapped.Error())
			}
			text := errorText(wrapped, false)
			for _, v := range valuesInFixtures {
				if strings.Contains(text, v) {
					t.Errorf("errorText without values quotes %s", v)
				}
			}
			for _, m := range serverMessages(err) {
				if m == "" || strings.Contains(text, m) {
					t.Errorf("errorText without values has the server's message %q", m)
				}
			}

			red := redact(wrapped, false)
			var ae interface{ ErrorCodes() []int }
			if red.Error() != want || !errors.As(red, &ae) || len(ae.ErrorCodes()) == 0 {
				t.Errorf("redact = %q, unwrapping to %v; want %q, unwrapping to the server error", red, ae, want)
			}
			if redact(wrapped, true) != wrapped {
				t.Errorf("redact with values changed the error")
			}
		})
	}
}

// TestRedactErrorOutsideExecute checks the errors and logs that leave the
// runner outside Execute's own redaction: CheckFormats' error, which plan and
// apply redact with RedactError, and the lock release logged by withRelease.
func TestRedactErrorOutsideExecute(t *testing.T) {
	_, server := serverError(t, "9.0/update-unauthorized")
	// The server's message, up to its first quote, which the log escapes.
	message, _, _ := strings.Cut(serverMessages(server)[0], `"`)

	format := fmt.Errorf("check the changelog format version: %w", server)
	want := "check the changelog format version: (Unauthorized, code 13); " + serverMessageHidden
	if got := RedactError(format, false); got.Error() != want || !errors.As(got, new(mongo.CommandError)) {
		t.Errorf("RedactError = %q, want %q, unwrapping to the server error", got, want)
	}
	if got := RedactError(format, true); got != format {
		t.Errorf("RedactError with values = %q, want %q", got, format)
	}
	other := &FormatError{What: "changelog", Found: 9, Known: 1}
	if got := RedactError(other, false); got != other {
		t.Errorf("RedactError of a FormatError = %q, want it unchanged", got)
	}

	for _, values := range []bool{false, true} {
		var logs bytes.Buffer
		if err := withRelease(nil, server, slog.New(slog.NewTextHandler(&logs, nil)), values); err != nil {
			t.Fatalf("withRelease = %v, want nil", err)
		}
		if got := strings.Contains(logs.String(), message); got != values {
			t.Errorf("values %t: the release warning has the server's message: %t\n%s", values, got, logs.String())
		}
		if !values && !strings.Contains(logs.String(), "(Unauthorized, code 13)") {
			t.Errorf("the release warning lacks the code:\n%s", logs.String())
		}
	}
}

// opaqueError hides the text of the error it wraps.
type opaqueError struct{ err error }

func (e opaqueError) Error() string { return "the step failed" }

func (e opaqueError) Unwrap() error { return e.err }

// TestErrorTextOthers checks errorText on errors not captured from a
// server: driver and network errors keep their text, and server errors keep
// only their code names and codes.
func TestErrorTextOthers(t *testing.T) {
	secret := bson.Raw(bsonDoc(t, bson.D{{Key: "value", Value: "secret"}}))
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{name: "not a server error", err: errors.New("index email_1 isn't prepareUnique"), want: "index email_1 isn't prepareUnique"},
		{name: "network error", err: fmt.Errorf("update: %w", mongo.CommandError{Labels: []string{"NetworkError"},
			Wrapped: errors.New("connection reset by peer")}), want: "update: : connection reset by peer"},
		{name: "command error without a name", err: mongo.CommandError{Code: 8000, Message: "secret"},
			want: "(code 8000); " + serverMessageHidden},
		{name: "command error without a code", err: mongo.CommandError{Name: "Secretive", Message: "secret", Raw: secret},
			want: "(Secretive); " + serverMessageHidden},
		{name: "write error without details", err: mongo.WriteException{WriteErrors: mongo.WriteErrors{{Code: 8000, Message: "secret"}}},
			want: "write exception: write errors: [(code 8000)]; " + serverMessageHidden},
		{name: "write error with details but no _id", err: mongo.WriteException{WriteErrors: mongo.WriteErrors{
			{Code: 2, Message: "secret", Details: secret}}},
			want: "write exception: write errors: [(BadValue, code 2)]; " + serverMessageHidden},
		{name: "write concern error", err: mongo.WriteException{WriteConcernError: &mongo.WriteConcernError{
			Name: "WriteConcernTimeout", Code: 64, Message: "secret", Details: secret},
			WriteErrors: mongo.WriteErrors{{Code: 11000, Message: "secret"}, {Code: 121, Message: "secret"}}},
			want: "write exception: write concern error: (WriteConcernTimeout, code 64), " +
				"write errors: [(DuplicateKey, code 11000), (DocumentValidationFailure, code 121)]; " + serverMessageHidden},
		{name: "bulk write exception", err: mongo.BulkWriteException{WriteErrors: []mongo.BulkWriteError{
			{WriteError: mongo.WriteError{Code: 11000, Message: "secret"}}}},
			want: "bulk write exception: write errors: [(DuplicateKey, code 11000)]; " + serverMessageHidden},
		{name: "a write error alone", err: fmt.Errorf("insert: %w", mongo.WriteError{Code: 121, Message: "secret",
			Details: bson.Raw(bsonDoc(t, bson.D{{Key: "failingDocumentId", Value: "u1"}, {Key: "details", Value: "secret"}}))}),
			want: `insert: (DocumentValidationFailure, code 121) {"failingDocumentId":"u1"}; ` + serverMessageHidden},
		// The server error's text isn't in err's, so only the codes show.
		{name: "server error text not quoted", err: fmt.Errorf("plan: %w", opaqueError{mongo.CommandError{Code: 8000, Message: "secret"}}),
			want: "(code 8000); " + serverMessageHidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorText(tc.err, false); got != tc.want {
				t.Errorf("errorText = %q, want %q", got, tc.want)
			}
			if tc.err == nil {
				return
			}
			if got := errorText(tc.err, true); got != tc.err.Error() {
				t.Errorf("errorText with values = %q, want %q", got, tc.err.Error())
			}
		})
	}
}

func bsonDoc(t *testing.T, d bson.D) []byte {
	t.Helper()
	b, err := bson.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestApplyKeepsValuesOut runs migrations that MongoDB refuses, quoting the
// values of the documents involved: a duplicate key from an update and from
// a unique index build, a document validation failure, and a $convert
// failure. Without SampleValues, the error, the report, the logs and the
// Errored changelog entry show the server error's code name and code but
// not its message, and say how to see it; with it, they quote the server.
func TestApplyKeepsValuesOut(t *testing.T) {
	cases := []struct {
		name, id string
		// setup creates the users collection.
		setup func(t *testing.T, ctx context.Context, db *mongo.Database)
		// value and message are in the server's error, hidden is in
		// migrune's without SampleValues.
		value, message, hidden string
	}{
		{name: "duplicate key from an update", id: "2026-11-07-001-copy-email",
			setup: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				users := db.Collection("users")
				if _, err := users.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "email", Value: 1}},
					Options: options.Index().SetUnique(true)}); err != nil {
					t.Fatal(err)
				}
				insert(t, ctx, users, bson.D{{Key: "_id", Value: 1}, {Key: "email", Value: "ada@example.com"}},
					bson.D{{Key: "_id", Value: 2}, {Key: "email", Value: "bob@example.com"}})
			},
			value: "ada@example.com", message: "duplicate key error", hidden: "write errors: [(DuplicateKey, code 11000)]"},
		{name: "duplicate key from an index build", id: "2026-11-07-003-email-index",
			setup: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				insert(t, ctx, db.Collection("users"), bson.D{{Key: "_id", Value: 1}, {Key: "email", Value: "ada@example.com"}},
					bson.D{{Key: "_id", Value: 2}, {Key: "email", Value: "ada@example.com"}})
			},
			value: "ada@example.com", message: "duplicate key error", hidden: "(DuplicateKey, code 11000); "},
		{name: "document validation failure", id: "2026-11-07-002-negative-age",
			setup: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				validator := bson.D{{Key: "$jsonSchema", Value: bson.D{{Key: "properties", Value: bson.D{
					{Key: "age", Value: bson.D{{Key: "bsonType", Value: "int"}, {Key: "minimum", Value: 0}}}}}}}}
				if err := db.CreateCollection(ctx, "users", options.CreateCollection().SetValidator(validator)); err != nil {
					t.Fatal(err)
				}
				insert(t, ctx, db.Collection("users"), bson.D{{Key: "_id", Value: 1}, {Key: "age", Value: 36}})
			},
			value: "consideredValue", message: "Document failed validation",
			hidden: `(DocumentValidationFailure, code 121) {`},
		{name: "$convert failure", id: "2026-11-07-004-parse-code",
			setup: func(t *testing.T, ctx context.Context, db *mongo.Database) {
				insert(t, ctx, db.Collection("users"), bson.D{{Key: "_id", Value: 1}, {Key: "code", Value: "ada-secret-42"}})
			},
			value: "ada-secret-42", message: "$convert", hidden: "write errors: [(ConversionFailure, code 241)]"},
	}
	for _, tc := range cases {
		for _, values := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/SampleValues=%t", tc.name, values), func(t *testing.T) {
				ctx := testCtx(t)
				db := testDB(t)
				tc.setup(t, ctx, db)
				var logs bytes.Buffer
				rep, err := execute(t, ctx, db, load(t, fixtureDir(t, "redact", tc.id)),
					Options{Logger: slog.New(slog.NewTextHandler(io.MultiWriter(&logs, tWriter{t}), nil))},
					Config{Mode: ModeApply, SampleValues: values})
				var me *MigrationError
				if !errors.As(err, &me) || me.ID != tc.id {
					t.Fatalf("apply: %v, want the migration to fail", err)
				}
				var se mongo.ServerError
				if !errors.As(err, &se) {
					t.Errorf("apply: %v, want it to unwrap to the server's error", err)
				}
				entries := changelog(t, ctx, db)
				if len(entries) != 1 || entries[0].Status != StatusErrored || len(entries[0].Errors) != 1 {
					t.Fatalf("changelog %+v, want one Errored entry", entries)
				}
				for _, out := range []struct{ name, text string }{
					{"the error", err.Error()},
					{"the report's error", rep.Error},
					{"the row's error", rep.row(tc.id).Error},
					{"the changelog's error", entries[0].Errors[0].Message},
					{"the logs", logs.String()},
				} {
					for _, quoted := range []string{tc.value, tc.message} {
						if got := strings.Contains(out.text, quoted); got != values {
							t.Errorf("SampleValues %t, but %s quotes %s: %t\n%s", values, out.name, quoted, got, out.text)
						}
					}
					for _, hidden := range []string{tc.hidden, serverMessageHidden} {
						if got := strings.Contains(out.text, hidden); got == values {
							t.Errorf("SampleValues %t, but %s has %q: %t\n%s", values, out.name, hidden, got, out.text)
						}
					}
				}
			})
		}
	}
}

func TestIDsText(t *testing.T) {
	oid, err := bson.ObjectIDFromHex("6730c0ffee0000000000002a")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		ids  bson.A
		want string
	}{
		{bson.A{int32(1), int32(2)}, "[1,2]"},
		{bson.A{oid, "b"}, `[{"$oid":"6730c0ffee0000000000002a"},"b"]`},
		{bson.A{1, 2, 3, 4, 5}, "[1,2,3,4,5]"},
		{bson.A{1, 2, 3, 4, 5, 6, 7}, "[1,2,3,4,5] and 2 more"},
	}
	for _, tc := range cases {
		if got := idsText(tc.ids); got != tc.want {
			t.Errorf("idsText(%v) = %s, want %s", tc.ids, got, tc.want)
		}
	}
}
