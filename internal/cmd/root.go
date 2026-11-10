// Package cmd implements the migrune command line: the cobra commands, the
// environment variable binding, logging setup and error classification.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

// UsageError reports a usage or configuration problem: an unknown command or
// flag, a bad argument, or an invalid flag or environment variable value.
type UsageError struct {
	Err error
}

func (e *UsageError) Error() string { return e.Err.Error() }

func (e *UsageError) Unwrap() error { return e.Err }

// app holds the state shared by the commands of one invocation.
type app struct {
	root   *cobra.Command
	stderr io.Writer
	log    *slog.Logger

	logFormat string
	logLevel  string

	// running is set once a command body starts. Any error returned before
	// that point comes from cobra or from the pre-run setup, so it is a usage
	// or configuration error.
	running bool

	// settled is set once the command's outcome is decided; interrupted
	// records whether the context was cancelled by then. See settle.
	settled, interrupted bool
}

// Outcome is what the caller of Execute needs besides the error: how to set
// the exit code, and how to print the error.
type Outcome struct {
	// Interrupted reports whether ctx was cancelled before the command's
	// outcome was decided: for plan and apply, before the run returned, so a
	// commit already sent still counts as interrupted; for the other
	// commands, before Execute returned. A cancellation after that, while
	// the report is written or the client disconnects, leaves it false: the
	// outcome stands.
	Interrupted bool
	// JSONLogs reports whether the log format is json, so the final error
	// is printed as a JSON log record too.
	JSONLogs bool
}

// Execute runs migrune with the given arguments. It returns nil on success,
// a *UsageError for usage and configuration problems, and any other error when
// a command fails.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) (Outcome, error) {
	a := newApp(stdout, stderr)
	err := a.execute(ctx, args)
	return Outcome{Interrupted: a.interrupted, JSONLogs: a.jsonLogs()}, err
}

// jsonLogs reports whether the log format is json: --log-format, or else
// MIGRUNE_LOG_FORMAT. It reads them itself rather than trusting preRun,
// which an unknown flag or an invalid value stops before the logger is set
// up; an invalid --log-format is not json.
func (a *app) jsonLogs() bool {
	if f := a.root.PersistentFlags().Lookup("log-format"); f != nil && f.Changed {
		return a.logFormat == "json"
	}
	v, _ := os.LookupEnv(envName("log-format"))
	return v == "json"
}

// NewRootCmd returns the migrune command tree. It is used to generate the CLI
// reference documentation.
func NewRootCmd() *cobra.Command {
	return newApp(os.Stdout, os.Stderr).root
}

func newApp(stdout, stderr io.Writer) *app {
	a := &app{stderr: stderr, log: slog.New(slog.DiscardHandler)}

	root := &cobra.Command{
		Use:   "migrune",
		Short: "Run MongoDB migrations",
		Long: `migrune runs MongoDB migrations written as Extended JSON files.

Every flag can also be set through an environment variable named
MIGRUNE_<FLAG>, in upper case with dashes replaced by underscores
(for example MIGRUNE_URI or MIGRUNE_LOG_LEVEL). A flag given on the
command line wins over the environment variable, which wins over the default.`,
		Version:           Version(),
		SilenceErrors:     true,
		SilenceUsage:      true,
		PersistentPreRunE: a.preRun,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)

	pf := root.PersistentFlags()
	pf.StringVar(&a.logFormat, "log-format", "text", "log format: text or json")
	pf.StringVar(&a.logLevel, "log-level", "info", "log level: debug, info, warn or error")

	root.AddCommand(a.newLintCmd(), a.newPlanCmd(), a.newApplyCmd(), a.newDriftCmd())
	// cobra adds these when it executes; adding them now puts them in the
	// generated CLI reference too.
	root.InitDefaultCompletionCmd()
	root.InitDefaultVersionFlag()
	a.root = root
	return a
}

func (a *app) execute(ctx context.Context, args []string) error {
	a.root.SetArgs(args)
	err := a.root.ExecuteContext(ctx)
	a.settle(ctx)
	if err == nil {
		return nil
	}
	var usage *UsageError
	if !a.running && !errors.As(err, &usage) {
		return &UsageError{Err: err}
	}
	return err
}

// settle decides whether the command was interrupted: whether ctx is
// cancelled now. Only the first call counts, so a signal after the outcome
// was decided doesn't change it.
func (a *app) settle(ctx context.Context) {
	if !a.settled {
		a.settled, a.interrupted = true, ctx.Err() != nil
	}
}

// uriFlagWarning is the warning logged when --uri is given on the command
// line rather than as MIGRUNE_URI.
const uriFlagWarning = "--uri is set on the command line: process arguments are visible to other users " +
	"of this machine and in CI logs; set MIGRUNE_URI instead"

// preRun fills unset flags from the environment, then sets up logging. It
// warns when --uri was given on the command line, never naming its value.
func (a *app) preRun(cmd *cobra.Command, _ []string) error {
	// Read before bindEnv, which marks a flag set from the environment as
	// changed too.
	uriFlag := cmd.Flags().Changed("uri")
	if err := bindEnv(cmd.Flags(), os.LookupEnv); err != nil {
		return &UsageError{Err: err}
	}
	log, err := newLogger(a.stderr, a.logFormat, a.logLevel)
	if err != nil {
		return &UsageError{Err: err}
	}
	a.log = log
	if uriFlag {
		a.log.Warn(uriFlagWarning)
	}
	return nil
}

// run wraps a command body so that errors it returns are classified as
// command failures rather than usage errors.
func (a *app) run(fn func(cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		a.running = true
		return fn(cmd, args)
	}
}

func newLogger(w io.Writer, format, level string) (*slog.Logger, error) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid --log-level %q: want debug, info, warn or error", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("invalid --log-format %q: want text or json", format)
	}
}
