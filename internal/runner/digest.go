package runner

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/joaopaulosr95/migrune/internal/migration"
)

// DigestVersion is the first line of the digest input. A change to what the
// digest covers, or how, gets a new version.
const DigestVersion = "migrune-digest-v1"

// DigestInput is what the plan digest covers: the target, the deployment
// and the pending set. plan writes it, with its digest, and apply refuses to
// run when its own input gives a different digest.
type DigestInput struct {
	// Database is the target database's name.
	Database string
	// ReplicaSet names the deployment: the replica set name hello reports.
	ReplicaSet string
	// Deployment is the UUID of the migrations_changelog collection
	// (ChangelogDeployment), which plan and apply create when it's missing
	// and then read again. It tells apart two deployments whose database and
	// replica set names are the same.
	Deployment string
	// Pending are the pending migrations, sorted by id.
	Pending []DigestMigration
}

// DeploymentNone is the deployment of a database without a
// migrations_changelog collection. plan and apply create it before they
// read the deployment, so their digests don't have it; ReadDigestFile still
// accepts it.
const DeploymentNone = "none"

var deploymentPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// DigestMigration is a pending migration in the digest input.
type DigestMigration struct {
	ID       string
	Checksum string
}

// NewDigestInput returns the digest input for a run against database on the
// replica set replicaSet, whose changelog collection is deployment
// (ChangelogDeployment).
func NewDigestInput(database, replicaSet, deployment string, pending []migration.Migration) *DigestInput {
	d := &DigestInput{Database: database, ReplicaSet: replicaSet, Deployment: deployment}
	for _, m := range pending {
		d.Pending = append(d.Pending, DigestMigration{ID: m.ID, Checksum: m.Checksum})
	}
	slices.SortFunc(d.Pending, func(a, b DigestMigration) int { return strings.Compare(a.ID, b.ID) })
	return d
}

// Text renders the input the digest hashes, one line each, every line ending
// in a newline:
//
//	migrune-digest-v1
//	database <name>
//	replicaSet <name>
//	deployment <uuid>
//	migration <id> <checksum>   one per pending migration, by id
func (d *DigestInput) Text() string {
	var b strings.Builder
	b.WriteString(DigestVersion + "\n")
	b.WriteString("database " + d.Database + "\n")
	b.WriteString("replicaSet " + d.ReplicaSet + "\n")
	b.WriteString("deployment " + d.Deployment + "\n")
	for _, m := range d.Pending {
		b.WriteString("migration " + m.ID + " " + m.Checksum + "\n")
	}
	return b.String()
}

// Sum is the digest: the lowercase hex SHA-256 of Text.
func (d *DigestInput) Sum() string {
	sum := sha256.Sum256([]byte(d.Text()))
	return hex.EncodeToString(sum[:])
}

// parseDigestInput parses what Text renders.
func parseDigestInput(text string) (*DigestInput, error) {
	d := &DigestInput{}
	sc := bufio.NewScanner(strings.NewReader(text))
	n := 0
	for sc.Scan() {
		line := sc.Text()
		n++
		kind, rest, _ := strings.Cut(line, " ")
		switch {
		case n == 1:
			if line != DigestVersion {
				return nil, fmt.Errorf("unknown digest version %q: want %s", line, DigestVersion)
			}
		case n == 2 && kind == "database":
			d.Database = rest
		case n == 3 && kind == "replicaSet":
			d.ReplicaSet = rest
		case n == 4 && kind == "deployment":
			if rest != DeploymentNone && !deploymentPattern.MatchString(rest) {
				return nil, fmt.Errorf("line %d: want deployment <uuid> or deployment none", n)
			}
			d.Deployment = rest
		case n > 4 && kind == "migration":
			id, sum, ok := strings.Cut(rest, " ")
			if !ok {
				return nil, fmt.Errorf("line %d: want migration <id> <checksum>", n)
			}
			d.Pending = append(d.Pending, DigestMigration{ID: id, Checksum: sum})
		default:
			return nil, fmt.Errorf("line %d: unexpected %q", n, line)
		}
	}
	if n < 4 {
		return nil, errors.New("the database, replica set or deployment line is missing")
	}
	return d, nil
}

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// WriteDigestFile writes d to path: its digest on the first line, then the
// input it hashes (see Text). ReadDigestFile rejects a file cut short, since
// its digest no longer matches the input.
func WriteDigestFile(path string, d *DigestInput) error {
	if err := os.WriteFile(path, []byte(d.Sum()+"\n"+d.Text()), 0o644); err != nil {
		return fmt.Errorf("write plan digest: %w", err)
	}
	return nil
}

// errNotDigest is ReadDigestFile's error for a file plan didn't write, or
// one cut short.
var errNotDigest = errors.New("not a plan digest file: want the file migrune plan --digest-out wrote, unchanged")

// ReadDigestFile reads a digest file written by WriteDigestFile, and checks
// its digest matches the input it holds.
func ReadDigestFile(path string) (*DigestInput, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum, text, ok := strings.Cut(string(raw), "\n")
	if !ok || !digestPattern.MatchString(sum) {
		return nil, errNotDigest
	}
	d, err := parseDigestInput(text)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNotDigest, err)
	}
	if d.Text() != text || d.Sum() != sum {
		return nil, fmt.Errorf("%w: its digest doesn't match its contents", errNotDigest)
	}
	return d, nil
}

// DigestMismatchError reports that the run apply is about to do isn't the
// one the plan was made for.
type DigestMismatchError struct {
	Plan, Run *DigestInput
}

func (e *DigestMismatchError) Error() string {
	return fmt.Sprintf("this run doesn't match the plan: %s (plan digest %s, this run's digest %s); "+
		"run plan again against this database and get it approved",
		strings.Join(e.Differences(), "; "), e.Plan.Sum(), e.Run.Sum())
}

// Differences names what differs between the plan and the run, one item per
// part of the digest input.
func (e *DigestMismatchError) Differences() []string {
	var out []string
	if e.Plan.Deployment != e.Run.Deployment {
		out = append(out, fmt.Sprintf("the plan was made against another deployment "+
			"(this run's %s collection is %s, the plan's was %s)", ChangelogCollection, e.Run.Deployment, e.Plan.Deployment))
	}
	if e.Plan.Database != e.Run.Database {
		out = append(out, fmt.Sprintf("the database is %q, the plan's was %q", e.Run.Database, e.Plan.Database))
	}
	if e.Plan.ReplicaSet != e.Run.ReplicaSet {
		out = append(out, fmt.Sprintf("the replica set is %q, the plan's was %q", e.Run.ReplicaSet, e.Plan.ReplicaSet))
	}
	plan := make(map[string]string, len(e.Plan.Pending))
	for _, m := range e.Plan.Pending {
		plan[m.ID] = m.Checksum
	}
	run := make(map[string]string, len(e.Run.Pending))
	var added, changed, gone []string
	for _, m := range e.Run.Pending {
		run[m.ID] = m.Checksum
		sum, ok := plan[m.ID]
		switch {
		case !ok:
			added = append(added, m.ID)
		case sum != m.Checksum:
			changed = append(changed, m.ID)
		}
	}
	for _, m := range e.Plan.Pending {
		if _, ok := run[m.ID]; !ok {
			gone = append(gone, m.ID)
		}
	}
	for _, d := range []struct {
		what string
		ids  []string
	}{
		{"pending since the plan", added},
		{"edited since the plan", changed},
		{"no longer pending (applied, or removed, since the plan)", gone},
	} {
		if len(d.ids) > 0 {
			out = append(out, d.what+": "+strings.Join(d.ids, ", "))
		}
	}
	if len(out) == 0 {
		// Only a digest written by another version of the input could get
		// here.
		out = append(out, "the digest input differs")
	}
	return out
}
