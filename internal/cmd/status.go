package cmd

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/joaopaulosr95/migrune/internal/migration"
	"github.com/joaopaulosr95/migrune/internal/runner"
)

type statusOptions struct {
	conn         connFlags
	namespace    string
	output       string
	sampleValues bool
}

func (a *app) newStatusCmd() *cobra.Command {
	var o statusOptions
	c := &cobra.Command{
		Use:   "status",
		Short: "Show a namespace's migrations and lock",
		Long: `Show one namespace's migrations and lock, read-only: it takes no lock and
writes nothing. --uri and --db are required; --dir is optional.

STATE    MEANING
applied  Finished and the checksum matches, or no --dir
failed   Errored with no later Finished entry
pending  File with no Finished entry
changed  Finished, but the file's checksum differs
missing  Finished, but no file

Without --dir, only applied and failed migrations are shown. A failed
migration's stored error message is hidden unless --sample-values is set:
it may contain document values. -o json prints one JSON object.

The lock's heartbeat age uses the server's clock. A holder may be inside a
transaction until the server's transaction lifetime plus two heartbeat
intervals have passed without a committed heartbeat. status exits 0 whatever
the state, 1 on a read failure, and 2 on a usage or configuration error.

Status fields: docs/reference/status.md, in
https://github.com/joaopaulosr95/migrune.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error { return a.status(cmd, o) }),
	}
	fs := c.Flags()
	o.conn.register(fs)
	namespaceFlag(fs, &o.namespace)
	outputFlag(fs, &o.output)
	sampleValuesFlag(fs, &o.sampleValues)
	return c
}

func (a *app) status(cmd *cobra.Command, o statusOptions) error {
	for _, f := range []struct{ name, value string }{{"uri", o.conn.uri}, {"db", o.conn.db}} {
		if f.value == "" {
			return &UsageError{Err: fmt.Errorf("--%s is required", f.name)}
		}
	}
	if err := checkOutput(o.output); err != nil {
		return err
	}
	if err := checkNamespace(o.namespace); err != nil {
		return err
	}
	files, err := a.loadStatusFiles(o.conn.dir)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	db, _, disconnect, err := a.connect(ctx, o.conn, o.sampleValues)
	if err != nil {
		return err
	}
	defer disconnect()
	finished, err := runner.Finished(ctx, db, o.namespace)
	if err != nil {
		return runner.RedactError(err, o.sampleValues)
	}
	errored, err := runner.Errored(ctx, db, o.namespace)
	if err != nil {
		return runner.RedactError(err, o.sampleValues)
	}
	threshold, _ := a.unlockThreshold(ctx, db)
	lock, err := runner.ReadLock(ctx, db, o.namespace, threshold)
	if err != nil {
		return runner.RedactError(err, o.sampleValues)
	}
	rep := statusReport{Version: Version(), Database: o.conn.db, Namespace: o.namespace, Dir: o.conn.dir,
		Migrations: migrationStates(files, finished, errored, o.sampleValues)}
	if err := writeStatus(cmd.OutOrStdout(), rep, lock, o.output); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	return nil
}

func (a *app) loadStatusFiles(dir string) ([]migration.Migration, error) {
	if dir == "" {
		return nil, nil
	}
	loaded, err := migration.LoadDir(dir, a.log)
	if err != nil {
		return nil, err
	}
	// nil means no --dir to migrationStates, even when LoadDir found no files.
	if loaded.Migrations == nil {
		loaded.Migrations = make([]migration.Migration, 0)
	}
	return loaded.Migrations, nil
}

type statusReport struct {
	Version    string           `json:"version"`
	Database   string           `json:"database"`
	Namespace  string           `json:"namespace"`
	Dir        string           `json:"dir,omitempty"`
	Migrations []migrationState `json:"migrations"`
	Lock       *statusLock      `json:"lock"`
}

type statusLock struct {
	Owner          string    `json:"owner"`
	Current        string    `json:"current"`
	Token          string    `json:"token"`
	AcquiredAt     time.Time `json:"acquiredAt"`
	LastHeartbeat  time.Time `json:"lastHeartbeat"`
	ExpiresAt      time.Time `json:"expiresAt"`
	HeartbeatAgeMs int64     `json:"heartbeatAgeMs"`
	Expired        bool      `json:"expired"`
	UnlockAfter    time.Time `json:"unlockAfter"`
}

func writeStatus(w io.Writer, rep statusReport, lock *runner.LockInfo, output string) error {
	if output == "json" {
		rep.Lock = nil
		if lock != nil {
			rep.Lock = &statusLock{Owner: lock.Owner, Current: lock.Current, Token: lock.Token,
				AcquiredAt: lock.AcquiredAt.UTC(), LastHeartbeat: lock.LastHeartbeat.UTC(), ExpiresAt: lock.ExpiresAt.UTC(),
				HeartbeatAgeMs: lock.HeartbeatAge.Milliseconds(), Expired: lock.Expired, UnlockAfter: lock.UnlockAfter.UTC()}
		}
		rep.Migrations = slices.Clone(rep.Migrations)
		for i, row := range rep.Migrations {
			rep.Migrations[i].AppliedAt = utc(row.AppliedAt)
			rep.Migrations[i].FailedAt = utc(row.FailedAt)
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	ew := &errWriter{w: w}
	fmt.Fprintf(ew, "Database: %s\n", rep.Database)
	if rep.Namespace != "" {
		fmt.Fprintf(ew, "Namespace: %s\n", rep.Namespace)
	}
	fmt.Fprintln(ew)
	if rep.Dir == "" {
		fmt.Fprintln(ew, "No --dir: pending, changed and missing migrations aren't shown.")
	}
	var table bytes.Buffer
	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MIGRATION\tSTATUS\tAPPLIED AT\tRUNNER")
	for _, row := range rep.Migrations {
		at := ""
		if t := cmp.Or(row.AppliedAt, row.FailedAt); t != nil {
			at = statusTime(*t)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", row.ID, row.Status, at, row.Runner)
	}
	_ = tw.Flush()
	i := -1
	for line := range strings.Lines(table.String()) {
		fmt.Fprintln(ew, strings.TrimRight(line, " \n"))
		if i >= 0 && rep.Migrations[i].Status == "failed" {
			row := rep.Migrations[i]
			failures := row.errors
			if len(failures) == 0 {
				failures = []runner.StepError{{Step: row.FailedStep, Message: row.Error}}
			}
			for _, failure := range failures {
				message := failure.Message
				if message == "" {
					message = "error recorded (rerun with --sample-values to show it)"
				}
				if failure.Step != nil {
					fmt.Fprintf(ew, "  step %d: %s\n", *failure.Step, message)
				} else {
					fmt.Fprintf(ew, "  %s\n", message)
				}
			}
		}
		i++
	}
	fmt.Fprintln(ew)
	if lock == nil {
		fmt.Fprintln(ew, "Lock: not held")
		return ew.err
	}
	fmt.Fprintf(ew, "Lock: held by runner %q", lock.Owner)
	if lock.Current != "" {
		fmt.Fprintf(ew, " (applying %s)", lock.Current)
	}
	fmt.Fprintln(ew)
	fmt.Fprintf(ew, "  token        %s\n", lock.Token)
	fmt.Fprintf(ew, "  acquired     %s\n", statusTime(lock.AcquiredAt))
	label := "may be inside a transaction"
	if lock.Stale {
		label = "holder stopped: unlock allowed"
	}
	fmt.Fprintf(ew, "  heartbeat    %s (%ds ago; %s)\n", statusTime(lock.LastHeartbeat), lock.HeartbeatAge.Round(time.Second)/time.Second, label)
	fmt.Fprintf(ew, "  unlock after %s\n", statusTime(lock.UnlockAfter))
	remaining := lock.ExpiresAt.Sub(lock.LastHeartbeat.Add(lock.HeartbeatAge)).Round(time.Second) / time.Second
	if lock.Expired {
		fmt.Fprintf(ew, "  expires      %s (%ds ago, expired)\n", statusTime(lock.ExpiresAt), -remaining)
	} else {
		fmt.Fprintf(ew, "  expires      %s (in %ds)\n", statusTime(lock.ExpiresAt), remaining)
	}
	return ew.err
}

// utc returns at in UTC, or nil.
func utc(at *time.Time) *time.Time {
	if at == nil {
		return nil
	}
	t := at.UTC()
	return &t
}

func statusTime(at time.Time) string {
	if at.IsZero() {
		return "-"
	}
	return at.UTC().Format(time.RFC3339)
}

type migrationState struct {
	ID         string             `json:"id"`
	Status     string             `json:"status"` // applied|failed|pending|changed|missing
	Checksum   string             `json:"checksum,omitempty"`
	AppliedAt  *time.Time         `json:"appliedAt,omitempty"` // the latest Finished entry's createdAt
	FailedAt   *time.Time         `json:"failedAt,omitempty"`  // the latest Errored entry's createdAt, for failed
	Runner     string             `json:"runner,omitempty"`
	FailedStep *int               `json:"failedStep,omitempty"`
	Error      string             `json:"error,omitempty"` // set only with --sample-values
	errors     []runner.StepError // for the table's detail lines
}

// migrationStates classifies, sorted by id. files is nil without --dir.
func migrationStates(files []migration.Migration, finished map[string]migration.Applied,
	errored map[string]runner.ErroredEntry, sampleValues bool) []migrationState {
	states := make(map[string]migrationState, len(files)+len(finished)+len(errored))
	for _, m := range files {
		states[m.ID] = migrationState{ID: m.ID, Status: "pending", Checksum: m.Checksum}
	}
	for id, a := range finished {
		status := "applied"
		if files != nil {
			if file, ok := states[id]; !ok {
				status = "missing"
			} else if file.Checksum != a.Checksum {
				status = "changed"
			}
		}
		states[id] = migrationState{
			ID: id, Status: status, Checksum: a.Checksum, AppliedAt: &a.AppliedAt, Runner: a.Runner,
		}
	}
	for id, e := range errored {
		if a, ok := finished[id]; ok && !e.CreatedAt.After(a.AppliedAt) {
			continue
		}
		state := migrationState{
			ID: id, Status: "failed", Checksum: e.Checksum, FailedAt: &e.CreatedAt, Runner: e.Runner,
		}
		for i, failure := range e.Errors {
			if state.FailedStep == nil && failure.Step != nil {
				state.FailedStep = failure.Step
			}
			if !sampleValues {
				failure.Message = ""
			} else if i == 0 {
				state.Error = failure.Message
			}
			state.errors = append(state.errors, failure)
		}
		states[id] = state
	}
	out := make([]migrationState, 0, len(states))
	for _, state := range states {
		out = append(out, state)
	}
	slices.SortFunc(out, func(a, b migrationState) int { return strings.Compare(a.ID, b.ID) })
	return out
}
