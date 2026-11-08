package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

func TestCheckServerVersion(t *testing.T) {
	cases := []struct {
		version string
		want    string // the error
		warning string
	}{
		{version: "7.0.0"},
		{version: "7.0.45"},
		{version: "8.0.34"},
		{version: "9.0.4"},
		{version: "9.3.1"},
		{version: "10.0.0-rc1", warning: "MongoDB 10.0.0-rc1 is newer than the newest version migrune is tested on (9.x)"},
		{version: "10.0.0", warning: "MongoDB 10.0.0 is newer than the newest version migrune is tested on (9.x)"},
		{version: "6.0.14", want: "MongoDB 6.0.14 is not supported: migrune needs 7.0 or later"},
		{version: "6.3.2", want: "MongoDB 6.3.2 is not supported: migrune needs 7.0 or later"},
		{version: "4.4.29", want: "MongoDB 4.4.29 is not supported: migrune needs 7.0 or later"},
		{version: "", want: `MongoDB server version "": not a version number`},
		{version: "dev", want: `MongoDB server version "dev": not a version number`},
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			warning, err := checkServerVersion(tc.version)
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != tc.want {
				t.Errorf("checkServerVersion(%q) error = %q, want %q", tc.version, got, tc.want)
			}
			if warning != tc.warning {
				t.Errorf("checkServerVersion(%q) warning = %q, want %q", tc.version, warning, tc.warning)
			}
		})
	}
}

func TestClientOptions(t *testing.T) {
	cases := []struct {
		name    string
		uri     string
		usage   bool   // a *UsageError
		failure string // substring of a connection failure
		appName string // the app name sent to the server
		ignored string // the URI's readPreference mode, ignored
	}{
		{name: "not mongodb", uri: "postgres://ada:s3cret@localhost", usage: true},
		{name: "bad option", uri: "mongodb://ada:s3cret@host/?authMechanismProperties=AWS_SESSION_TOKEN:tok3n&connectTimeoutMS=soon", usage: true},
		{name: "semicolon separated", uri: "mongodb://ada:s3cret@host/?replicaSet=rs0;authMechanismProperties=AWS_SESSION_TOKEN:tok3n;connectTimeoutMS=soon", usage: true},
		{name: "srv with a port", uri: "mongodb+srv://ada:s3cret@cluster.example.com:27017/", usage: true},
		{name: "srv lookup fails", uri: "mongodb+srv://ada:s3cret@cluster.migrune.invalid/", failure: "resolve the --uri hosts: lookup _mongodb._tcp.cluster.migrune.invalid"},
		{name: "default app name", uri: "mongodb://ada:s3cret@host/", appName: "migrune"},
		{name: "app name from the uri", uri: "mongodb://host/?appName=billing-migrations", appName: "billing-migrations"},
		{name: "read preference primary", uri: "mongodb://host/?readPreference=primary", appName: "migrune"},
		{name: "read preference primaryPreferred", uri: "mongodb://host/?readPreference=primaryPreferred", appName: "migrune", ignored: "primaryPreferred"},
		{name: "read preference secondary", uri: "mongodb://host/?readPreference=secondary", appName: "migrune", ignored: "secondary"},
		{name: "read preference secondaryPreferred", uri: "mongodb://host/?readPreference=secondaryPreferred&maxStalenessSeconds=120", appName: "migrune", ignored: "secondaryPreferred"},
		{name: "read preference nearest with tags", uri: "mongodb://host/?readPreference=nearest&readPreferenceTags=dc:east", appName: "migrune", ignored: "nearest"},
		{name: "read concern local", uri: "mongodb://host/?readConcernLevel=local", appName: "migrune"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, ignored, err := clientOptions(tc.uri)
			var usage *UsageError
			switch {
			case tc.usage:
				if !errors.As(err, &usage) {
					t.Fatalf("got %v, want a usage error", err)
				}
			case tc.failure != "":
				if err == nil || errors.As(err, &usage) || !strings.Contains(err.Error(), tc.failure) {
					t.Fatalf("got %v, want a connection failure containing %q", err, tc.failure)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if opts.AppName == nil || *opts.AppName != tc.appName {
					t.Errorf("app name %v, want %q", opts.AppName, tc.appName)
				}
				if ignored != tc.ignored {
					t.Errorf("ignored read preference %q, want %q", ignored, tc.ignored)
				}
				if opts.ReadPreference == nil || opts.ReadPreference.Mode() != readpref.PrimaryMode {
					t.Errorf("read preference %v, want primary", opts.ReadPreference)
				}
				if opts.ReadConcern == nil || opts.ReadConcern.Level != "majority" {
					t.Errorf("read concern %v, want majority", opts.ReadConcern)
				}
			}
			for _, secret := range []string{"s3cret", "tok3n"} {
				if err != nil && strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks %q: %v", secret, err)
				}
			}
		})
	}
}

func TestCheckTopology(t *testing.T) {
	const standalone = "migrune needs a replica set; " +
		"for local development, start mongod with --replSet and run rs.initiate() once"
	cases := []struct {
		name    string
		setName string
		msg     string
		want    server
		err     string
	}{
		{name: "replica set member", setName: "rs0", want: server{SetName: "rs0"}},
		{name: "mongos", msg: "isdbgrid", err: "connected to a mongos: sharded clusters aren't supported; migrune needs a replica set"},
		{name: "standalone", err: standalone},
		{name: "other msg", msg: "something", err: standalone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := checkTopology(tc.setName, tc.msg)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("got %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("checkTopology(%q, %q) = %+v, %v; want %+v", tc.setName, tc.msg, got, err, tc.want)
			}
		})
	}
}

func TestScrubSecrets(t *testing.T) {
	cases := []struct{ name, uri, in, want string }{
		{name: "no secret quoted", uri: "mongodb://ada:s3cret@host/",
			in: "auth error: sasl conversation error", want: "auth error: sasl conversation error"},
		{name: "password", uri: "mongodb://ada:s3cret@host/",
			in: "dial mongodb://ada:s3cret@host: refused", want: "dial mongodb://ada:REDACTED@host: refused"},
		{name: "authMechanismProperties", uri: "mongodb://key:s3cret@host/?authMechanism=MONGODB-AWS&authMechanismProperties=AWS_SESSION_TOKEN:t0ken%2Bx",
			in: "aws: session token t0ken+x and secret s3cret refused", want: "aws: session token REDACTED and secret REDACTED refused"},
		{name: "other authMechanismProperties kept", uri: "mongodb://ada@host/?authMechanism=GSSAPI&authMechanismProperties=SERVICE_NAME:mongodb,CANONICALIZE_HOST_NAME:true",
			in: "kerberos: service mongodb, canonicalize true: refused", want: "kerberos: service mongodb, canonicalize true: refused"},
		{name: "percent-encoded password", uri: "mongodb://ada:p%40ss%21@host/",
			in: "dial mongodb://ada:p%40ss%21@host as p@ss!: refused", want: "dial mongodb://ada:REDACTED@host as REDACTED: refused"},
		{name: "tlsCertificateKeyFilePassword", uri: "mongodb://host/?tls=true&tlsCertificateKeyFilePassword=k3y%21pass",
			in: "decrypt key with k3y!pass, given as k3y%21pass", want: "decrypt key with REDACTED, given as REDACTED"},
		{name: "sslClientCertificateKeyPassword", uri: "mongodb://host/?ssl=true&sslClientCertificateKeyPassword=k3ypass",
			in: "decrypt key with k3ypass", want: "decrypt key with REDACTED"},
		{name: "option name in another case", uri: "mongodb://host/?tls=true&TLSCertificateKeyFilePassword=k3ypass",
			in: "decrypt key with k3ypass", want: "decrypt key with REDACTED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, _, err := clientOptions(tc.uri)
			if err != nil {
				t.Fatal(err)
			}
			if got := scrubSecrets(errors.New(tc.in), uriSecrets(tc.uri, opts)).Error(); got != tc.want {
				t.Errorf("scrubSecrets(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestConnectFailure checks a server that can't be reached, or refuses the
// credentials, is a failure (exit 1), not a usage error, with the driver's
// cause and without the password.
func TestConnectFailure(t *testing.T) {
	uri, db := testTarget(t)
	dir := migrationsDir(t, setFlag)
	authURI := strings.Replace(uri, "mongodb://", "mongodb://ada:s3cret-pw@", 1)
	cases := []struct {
		name  string
		uri   string
		cause string
	}{
		{name: "unreachable", uri: "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=300", cause: "server selection"},
		{name: "authentication", uri: authURI + "&serverSelectionTimeoutMS=3000", cause: "auth"},
	}
	for _, tc := range cases {
		for _, command := range []string{"plan", "lint"} {
			t.Run(tc.name+" "+command, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				_, err := Execute(context.Background(), []string{command, "--dir", dir, "--uri", tc.uri, "--db", db.Name()}, &stdout, &stderr)
				t.Logf("error: %v\nstderr:\n%s", err, stderr.String())
				if err == nil || errors.As(err, new(*UsageError)) || !strings.Contains(err.Error(), tc.cause) {
					t.Fatalf("got %v, want a failure containing %q", err, tc.cause)
				}
				if strings.Contains(err.Error()+stderr.String()+stdout.String(), "s3cret-pw") {
					t.Errorf("output leaks the password")
				}
				if strings.Contains(stderr.String(), tc.uri) {
					t.Errorf("logs echo the URI:\n%s", stderr.String())
				}
			})
		}
	}
}
