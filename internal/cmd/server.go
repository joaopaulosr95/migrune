package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

// minServerMajor is the oldest MongoDB major version migrune supports.
const minServerMajor = 7

// maxTestedMajor is the newest MongoDB major version migrune is tested on.
// A newer server is let through with a warning.
const maxTestedMajor = 9

// appName is the application name migrune sends to the server, so its
// operations show in currentOp, the logs and the profiler, unless the URI
// sets appName.
const appName = "migrune"

// errInvalidURI is the usage error for a --uri the driver can't parse. The
// driver's message is left out: it may quote parts of the URI, credentials
// included.
var errInvalidURI = &UsageError{Err: errors.New("--uri is not a valid MongoDB connection string")}

// clientOptions parses uri. A URI that doesn't parse is a usage error. A
// mongodb+srv:// URI is resolved here, through DNS: a failed lookup is a
// connection failure, with its cause, which names the host looked up.
//
// Every read goes to the primary with read concern majority, whatever the
// URI says: a secondary can lag behind the writes migrune just made, and a
// read that isn't majority-committed can be rolled back by a failover. A
// transaction still sets its own read concern. ignoredMode is the URI's
// readPreference mode when it isn't primary, for the caller to log.
func clientOptions(uri string) (opts *options.ClientOptions, ignoredMode string, err error) {
	opts = options.Client().ApplyURI(uri)
	if err := opts.Validate(); err != nil {
		return nil, "", uriError(err)
	}
	if opts.AppName == nil {
		opts.SetAppName(appName)
	}
	if opts.ReadPreference != nil && opts.ReadPreference.Mode() != readpref.PrimaryMode {
		ignoredMode = opts.ReadPreference.Mode().String()
	}
	opts.SetReadPreference(readpref.Primary())
	opts.SetReadConcern(readconcern.Majority())
	return opts, ignoredMode, nil
}

// uriError classifies an error from parsing a URI: a DNS lookup that failed
// is a connection failure, with its cause; anything else is a usage error.
func uriError(err error) error {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Errorf("resolve the --uri hosts: %w", dnsErr)
	}
	return errInvalidURI
}

// connect returns the target database, once it has checked the server runs a
// supported MongoDB version as a replica set, and that no
// changelog entry or lock document is newer than this migrune understands
// (runner.CheckFormats): every command that connects checks the formats
// before it reads or writes anything else. The
// returned function disconnects, on its own short context so it works after
// an interruption. It logs the hosts and the database, never the URI.
// values is plan and apply's --sample-values: without it, the format check's
// error leaves out the server's message (runner.RedactError); drift and lint
// have no such flag and pass true.
func (a *app) connect(ctx context.Context, c connFlags, values bool) (*mongo.Database, server, func(), error) {
	opts, ignoredMode, err := clientOptions(c.uri)
	if err != nil {
		return nil, server{}, nil, err
	}
	if ignoredMode != "" {
		a.log.Warn("migrune reads from the primary; the URI's readPreference " + ignoredMode + " is ignored")
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, server{}, nil, errInvalidURI
	}
	a.log.Info("connecting", "hosts", strings.Join(opts.Hosts, ","), "db", c.db)
	disconnect := func() {
		ctx, cancel := context.WithTimeout(context.Background(), runner.CleanupTimeout)
		defer cancel()
		_ = client.Disconnect(ctx)
	}
	srv, warnings, err := checkServer(ctx, client)
	if err != nil {
		disconnect()
		return nil, server{}, nil, scrubSecrets(err, uriSecrets(c.uri, opts))
	}
	for _, w := range warnings {
		a.log.Warn(w)
	}
	db := client.Database(c.db)
	if err := runner.CheckFormats(ctx, db); err != nil {
		disconnect()
		return nil, server{}, nil, runner.RedactError(err, values)
	}
	return db, srv, disconnect, nil
}

// scrubSecrets replaces each of secrets in err's message with REDACTED,
// should the driver ever quote one. err's chain is lost then, which only the
// exit code reads, and a connection failure exits 1 anyway.
func scrubSecrets(err error, secrets []string) error {
	msg := err.Error()
	// The longest first, so a secret that holds another is replaced whole.
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "REDACTED")
		}
	}
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

// uriSecrets returns the secrets uri gives, which opts was parsed from: the
// password, the AWS_SESSION_TOKEN authMechanismProperty, and
// tlsCertificateKeyFilePassword (or its older name,
// sslClientCertificateKeyPassword), each as given and decoded. Other
// authMechanismProperties, such as SERVICE_NAME or CANONICALIZE_HOST_NAME,
// aren't secret, and replacing a short value like true would mangle the
// message.
func uriSecrets(uri string, opts *options.ClientOptions) []string {
	var secrets []string
	if opts.Auth != nil {
		secrets = append(secrets, opts.Auth.Password)
		for k, v := range opts.Auth.AuthMechanismProperties {
			if strings.EqualFold(k, "AWS_SESSION_TOKEN") {
				secrets = append(secrets, v)
			}
		}
	}
	// The password as written in the URI, percent-encoded or not.
	if _, rest, ok := strings.Cut(uri, "://"); ok {
		host, _, _ := strings.Cut(rest, "/")
		host, _, _ = strings.Cut(host, "?")
		if i := strings.LastIndex(host, "@"); i >= 0 {
			if _, pw, ok := strings.Cut(host[:i], ":"); ok {
				secrets = append(secrets, pw)
			}
		}
	}
	_, query, _ := strings.Cut(uri, "?")
	for _, kv := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(kv, "=")
		if !strings.EqualFold(k, "tlsCertificateKeyFilePassword") && !strings.EqualFold(k, "sslClientCertificateKeyPassword") {
			continue
		}
		secrets = append(secrets, v)
		if d, err := url.QueryUnescape(v); err == nil {
			secrets = append(secrets, d)
		}
	}
	return secrets
}

// server is what checkServer learned about the deployment.
type server struct {
	// SetName is the replica set name.
	SetName string
}

// checkServer fails unless the server runs a supported MongoDB version, as a
// replica set. Its errors, connection and authentication
// failures included, keep the driver's cause. The warnings, to log, are
// checkServerVersion's.
func checkServer(ctx context.Context, client *mongo.Client) (server, []string, error) {
	var hello struct {
		SetName string `bson:"setName"`
		Msg     string `bson:"msg"`
	}
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		return server{}, nil, fmt.Errorf("connect to MongoDB: %w", err)
	}
	srv, err := checkTopology(hello.SetName, hello.Msg)
	if err != nil {
		return server{}, nil, err
	}
	var warnings []string
	var info struct {
		Version string `bson:"version"`
	}
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil {
		return server{}, nil, fmt.Errorf("read the MongoDB server version: %w", err)
	}
	warning, err := checkServerVersion(info.Version)
	if warning != "" {
		warnings = append(warnings, warning)
	}
	return srv, warnings, err
}

// checkTopology fails unless hello's setName shows a replica set member:
// migrune's transactions need one. A mongos (msg "isdbgrid") is refused:
// sharded clusters aren't supported.
func checkTopology(setName, msg string) (server, error) {
	switch {
	case setName != "":
		return server{SetName: setName}, nil
	case msg == "isdbgrid":
		return server{}, errors.New("connected to a mongos: sharded clusters aren't supported; migrune needs a replica set")
	default:
		return server{}, errors.New("migrune needs a replica set; " +
			"for local development, start mongod with --replSet and run rs.initiate() once")
	}
}

// checkServerVersion fails unless version, as buildInfo reports it, is
// minServerMajor.0 or later. On a major newer than maxTestedMajor it returns
// a warning to log instead: the run continues.
func checkServerVersion(version string) (warning string, err error) {
	var major int
	if _, err := fmt.Sscanf(version, "%d.", &major); err != nil {
		return "", fmt.Errorf("MongoDB server version %q: not a version number", version)
	}
	if major < minServerMajor {
		return "", fmt.Errorf("MongoDB %s is not supported: migrune needs %d.0 or later", version, minServerMajor)
	}
	if major > maxTestedMajor {
		return fmt.Sprintf("MongoDB %s is newer than the newest version migrune is tested on (%d.x)", version, maxTestedMajor), nil
	}
	return "", nil
}
