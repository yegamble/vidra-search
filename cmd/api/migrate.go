// The `migrate` subcommand of the vidra-search binary: it applies the SQL
// migrations embedded in the binary (see the migrations + internal/dbmigrate
// packages), replacing the golang-migrate CLI container that used to bind-mount
// ./migrations from a git checkout. Output goes to stdout in plain lines (this
// path runs before — and independently of — the service logger, because a
// migrator must not require the rest of the service configuration).

package main

import (
	"fmt"
	"os"

	"github.com/vidra/vidra-search/internal/config"
	"github.com/vidra/vidra-search/internal/dbmigrate"
)

// runMigrate dispatches `migrate <up|version>`:
//
//	up       apply every embedded migration not yet applied (idempotent)
//	version  print the ledger version and dirty flag
//
// The DSN comes from DATABASE_URL. Both commands exit non-zero when the ledger
// is dirty (a half-applied migration needing manual repair). There is
// deliberately no `down`: rolling a production schema back from the service
// binary is not an operation we want one typo away.
func runMigrate(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("migrate: want exactly one command (up|version), got %d arguments", len(args))
	}
	dsn, err := config.LoadDatabaseURL()
	if err != nil {
		return err
	}

	switch args[0] {
	case "up":
		st, err := dbmigrate.Up(dsn, os.Stdout)
		if err != nil {
			return err
		}
		fmt.Printf("migrate up: %s\n", st)
		return dirtyLedgerErr(st)
	case "version":
		st, err := dbmigrate.Version(dsn)
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", st)
		return dirtyLedgerErr(st)
	default:
		return fmt.Errorf("migrate: unknown command %q (want: up|version)", args[0])
	}
}

// dirtyLedgerErr turns a dirty ledger into a non-zero exit: the schema is in an
// unknown state, so no caller (deploy script, healthcheck, operator) may read
// success from this process.
func dirtyLedgerErr(st dbmigrate.Status) error {
	if !st.Dirty {
		return nil
	}
	return fmt.Errorf("migrate: %s is DIRTY at version %d — a migration failed part-way; repair the schema and force the version before deploying", dbmigrate.Table, st.Version)
}
