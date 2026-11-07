// Command migrune runs MongoDB migrations.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joaopaulosr95/migrune/internal/cmd"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run())
}

func run() int {
	// sigs sees the same signals as the context. The first one is recorded
	// for the exit code; a second one exits at once, with the same code.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	first := watchSignals(sigs, os.Exit)

	out, err := cmd.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)

	// A signal counts only when it interrupted the command: one that lands
	// after its outcome was decided leaves the exit code to the outcome.
	var sig os.Signal
	if out.Interrupted {
		// The context is only cancelled by a signal, which was also sent
		// to sigs.
		sig = <-first
	}
	code := exitCode(err, sig)
	if err != nil {
		printError(os.Stderr, err, code, out.JSONLogs)
	}
	return code
}

// printError writes the error a run ended with to w: as an "Error:" line,
// followed by a pointer to --help for a usage error, or, with JSON logs, as
// one JSON log record at level ERROR with the error as msg and the exit code
// as exitCode.
func printError(w io.Writer, err error, code int, jsonLogs bool) {
	if jsonLogs {
		slog.New(slog.NewJSONHandler(w, nil)).Error(err.Error(), "exitCode", code)
		return
	}
	fmt.Fprintln(w, "Error:", err)
	var usage *cmd.UsageError
	if errors.As(err, &usage) {
		fmt.Fprintln(w, "Run 'migrune --help' for usage.")
	}
}

// watchSignals hands the first signal from sigs to the returned channel, for
// the exit code once the run has stopped, and calls exit at the second with
// the first one's exit code: a second Ctrl+C after a SIGTERM still exits 143.
func watchSignals(sigs <-chan os.Signal, exit func(int)) <-chan os.Signal {
	first := make(chan os.Signal, 1)
	go func() {
		sig := <-sigs
		first <- sig
		<-sigs
		exit(exitCode(nil, sig))
	}()
	return first
}

// exitCode maps the outcome of a run to the process exit code: 128 + the
// signal number when sig interrupted the run (sig is nil otherwise), 2 for
// usage and configuration errors, 1 for any other failure and 0 on success.
func exitCode(err error, sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	var usage *cmd.UsageError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &usage):
		return exitUsage
	default:
		return exitFailure
	}
}
