package cmd

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/joaopaulosr95/migrune/internal/runner"
)

// lockHeldError is an unlock refusal that exits 3.
type lockHeldError struct{ msg string }

func (e *lockHeldError) Error() string { return e.msg }

func (e *lockHeldError) Is(target error) bool { return target == runner.ErrLockHeld }

type unlockOptions struct {
	conn      connFlags
	namespace string
	token     string
}

func (a *app) newUnlockCmd() *cobra.Command {
	var o unlockOptions
	c := &cobra.Command{
		Use:   "unlock",
		Short: "Remove a dead holder's migration lock by token",
		Long: `Remove one namespace's migration lock, only if its token matches --token
and its last committed heartbeat is older than the server's transaction
lifetime plus two heartbeat intervals (80s with defaults), by the server's
clock. Inspect the lock with status first. --uri, --db and --token are required.

If the server's transactionLifetimeLimitSeconds can't be read, unlock assumes
60s and warns: a raised limit makes removing a live holder's lock possible.
The delete may wait behind an open transaction, for at most the transaction
lifetime plus 10s; after 5s it logs that it is waiting.

A matching token with a recent heartbeat is refused with exit 3: its holder
may be inside a transaction; retry after the time shown. A token belonging to
another namespace exits 1 and shows which --namespace to use. A lock now held
by another acquisition is refused with exit 3: run status again. If no lock
has the token and the namespace has none, unlock exits 0: it was already
released or expired and removed. A removed lock is logged at warn; usage
errors exit 2 and other failures exit 1.

The namespace is then as a killed run leaves it: run plan and apply again.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, _ []string) error { return a.unlock(cmd, o) }),
	}
	fs := c.Flags()
	fs.StringVar(&o.conn.uri, "uri", "", "MongoDB connection string; prefer MIGRUNE_URI to keep credentials out of process arguments")
	fs.StringVar(&o.conn.db, "db", "", "target database name")
	namespaceFlag(fs, &o.namespace)
	fs.StringVar(&o.token, "token", "", "token of the lock to remove, as status shows it")
	return c
}

func (a *app) unlock(cmd *cobra.Command, o unlockOptions) error {
	for _, f := range []struct{ name, value string }{{"uri", o.conn.uri}, {"db", o.conn.db}, {"token", o.token}} {
		if f.value == "" {
			return &UsageError{Err: fmt.Errorf("--%s is required", f.name)}
		}
	}
	if err := checkNamespace(o.namespace); err != nil {
		return err
	}
	ctx := cmd.Context()
	db, _, disconnect, err := a.connect(ctx, o.conn, false)
	if err != nil {
		return err
	}
	defer disconnect()
	threshold, lifetime := a.unlockThreshold(ctx, db, slog.LevelWarn)
	waiting := time.AfterFunc(5*time.Second, func() {
		a.log.Info("waiting for an open transaction on the lock to end")
	})
	res, err := runner.Unlock(ctx, db, o.namespace, o.token, threshold, lifetime+10*time.Second)
	waiting.Stop()
	if err != nil {
		return runner.RedactError(err, false)
	}
	switch res.Outcome {
	case runner.UnlockFresh:
		return &lockHeldError{fmt.Sprintf("runner %q heartbeated %s ago and may be inside a transaction; retry after %s",
			res.Lock.Owner, res.Lock.HeartbeatAge.Round(time.Second), res.Lock.UnlockAfter.UTC().Format(time.RFC3339))}
	case runner.UnlockOtherNamespace:
		return fmt.Errorf("this token is the lock of namespace %q; rerun with --namespace %q", res.Lock.Namespace, res.Lock.Namespace)
	case runner.UnlockOtherToken:
		return &lockHeldError{fmt.Sprintf("the lock is now held by another acquisition: runner %q, token %s; run status again",
			res.Lock.Owner, res.Lock.Token)}
	case runner.UnlockGone:
		a.log.Info("no lock has this token: it was released or expired and removed")
	case runner.UnlockDeleted:
		a.log.Warn("migration lock removed", "runner", res.Lock.Owner, "token", res.Lock.Token,
			"current", res.Lock.Current, "heartbeatAge", res.Lock.HeartbeatAge.Round(time.Second), "expired", res.Lock.Expired)
	}
	return nil
}
