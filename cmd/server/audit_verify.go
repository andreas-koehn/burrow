package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/config"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// newAuditVerifyCmd returns the `burrowd audit verify` cobra subcommand
// (mounted under `burrowd audit`).
//
// Usage:
//
//	burrowd audit verify [--db <path>] [--from <id>] [--to <id>]
//
// Exit codes:
//
//	0 — chain valid; stdout: "Chain valid from <first_id> to <last_id>."
//	1 — chain mismatch; stderr: "Chain mismatch at <mismatched_id>."
//	1 — any other error (failed to open DB, missing signing key, etc.)
//
// Output goes to the io.Writer attached to the cobra command (stdout +
// stderr) so tests can inspect both streams.
func newAuditVerifyCmd() *cobra.Command {
	verifyCmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify the audit-events hash chain on disk",
		Long: `Walks the audit_events table in id order, recomputing the SHA-256
hash chain. Exits 0 on full match and 1 on any mismatch (with the
offending row id printed to stderr).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dbPath, _ := cmd.Flags().GetString("db")
			fromID, _ := cmd.Flags().GetString("from")
			toID, _ := cmd.Flags().GetString("to")
			return runAuditVerify(cmd.Context(), dbPath, fromID, toID, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	verifyCmd.Flags().String("db", "", "path to burrow.db (default: $BURROW_DB_PATH or burrow.db)")
	verifyCmd.Flags().String("from", "", "inclusive lower bound on audit id (default: chain start)")
	verifyCmd.Flags().String("to", "", "inclusive upper bound on audit id (default: chain end)")
	return verifyCmd
}

// runAuditVerify is the testable seam: it opens the database at dbPath,
// resolves the audit logger (loading or generating the signing key on the
// fly), runs Verify, and writes the human-readable result.
func runAuditVerify(ctx context.Context, dbPath, fromID, toID string, stdout, stderr io.Writer) error {
	if dbPath == "" {
		// Fall through to the same path-resolution config.LoadServer uses
		// so `burrowd audit verify` works against an existing deployment
		// with zero extra flags.
		cfg, err := config.LoadServer(nil)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return cobraExit1()
		}
		dbPath = cfg.DatabasePath
	}
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(stderr, "error: database %s: %v\n", dbPath, err)
		return cobraExit1()
	}
	database, err := db.Open(dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: open %s: %v\n", dbPath, err)
		return cobraExit1()
	}
	defer database.Close()
	if err := db.Migrate(database); err != nil {
		fmt.Fprintf(stderr, "error: migrate: %v\n", err)
		return cobraExit1()
	}
	st := store.New(database)
	priv, err := audit.LoadOrGenerateSigningKey(ctx, st)
	if err != nil {
		fmt.Fprintf(stderr, "error: signing key: %v\n", err)
		return cobraExit1()
	}
	logger := audit.NewLogger(db.Wrap(database), priv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ok, mismatched, err := logger.Verify(ctx, fromID, toID)
	if err != nil {
		fmt.Fprintf(stderr, "error: verify: %v\n", err)
		return cobraExit1()
	}
	if !ok {
		fmt.Fprintf(stderr, "Chain mismatch at %s.\n", mismatched)
		return cobraExit1()
	}
	// On success: write the "valid from <first> to <last>" line. Pull
	// the bounds straight from the DB so the message matches the spec
	// even when the caller didn't pass --from/--to.
	first, last := boundaryIDs(ctx, db.Wrap(database), fromID, toID)
	fmt.Fprintf(stdout, "Chain valid from %s to %s.\n", first, last)
	return nil
}

// boundaryIDs returns (firstID, lastID) of the (fromID, toID) range.
// Empty strings on either side fall back to whole-chain bounds.
func boundaryIDs(ctx context.Context, x *db.DB, fromID, toID string) (string, string) {
	last, _ := x.ListAuditEvents(ctx, db.AuditQuery{FromID: fromID, ToID: toID, Limit: 1})
	var lastID string
	if len(last) > 0 {
		lastID = last[0].ID
	}
	all, _ := x.ListAuditEvents(ctx, db.AuditQuery{FromID: fromID, ToID: toID, Limit: 10_000})
	var firstID string
	if len(all) > 0 {
		firstID = all[len(all)-1].ID
	}
	return firstID, lastID
}

// cobraExit1 is the sentinel error that root.SilenceErrors=true uses to
// signal exit 1 without printing a duplicate "Error:" line. The error
// itself is the empty string so cobra does not double-log; main() turns
// any non-nil RunE result into an os.Exit(1).
func cobraExit1() error { return errExit1 }

// errExit1 is the singleton sentinel.
var errExit1 = exitError("")

type exitError string

func (e exitError) Error() string { return string(e) }
