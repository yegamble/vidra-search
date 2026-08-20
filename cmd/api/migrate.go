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
	"strconv"

	"github.com/vidra/vidra-search/internal/config"
	"github.com/vidra/vidra-search/internal/dbmigrate"
)

// forceGateFlag must accompany `migrate force`. Forcing rewrites the recorded
// version WITHOUT running any migration, so a wrong version silently makes the
// next `migrate up` skip real migrations — too destructive to be one typo away.
const forceGateFlag = "--yes-i-know"

// migrateCmd is a parsed `migrate …` invocation. Parsing is separated from
// execution so the argument rules (notably the force gate) are testable without
// a database — and so a rejected invocation never opens one.
type migrateCmd struct {
	// name is up | version | force.
	name string
	// version is the target ledger version, force only.
	version int
}

// runMigrate dispatches `migrate <up|version|force>`:
//
//	up                            apply every embedded migration not yet applied (idempotent)
//	version                       print the ledger version and dirty flag
//	force <version> --yes-i-know  rewrite the ledger to <version>, clearing dirty
//
// The DSN comes from DATABASE_URL, which is required (no dev fallback: a
// migrator that quietly migrated the wrong database would be worse than one
// that refuses to run). up and version exit non-zero when the ledger is dirty (a
// half-applied migration needing manual repair, which force then records). There
// is deliberately no `down`: rolling a production schema back from the service
// binary is not an operation we want one typo away.
func runMigrate(args []string) error {
	cmd, err := parseMigrateArgs(args)
	if err != nil {
		return err
	}
	dsn, err := config.LoadDatabaseURL()
	if err != nil {
		return err
	}

	switch cmd.name {
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
	default: // force; parseMigrateArgs admits nothing else
		before, after, err := dbmigrate.Force(dsn, cmd.version)
		if err != nil {
			return err
		}
		fmt.Printf("migrate force: before %s\n", before)
		fmt.Printf("migrate force: after  %s\n", after)
		// A force always clears the dirty flag, so there is nothing left to
		// fail on — but the schema is only as repaired as the operator made it.
		fmt.Printf("migrate force: the ledger now claims version %d; verify the schema matches it\n", cmd.version)
		return nil
	}
}

// parseMigrateArgs validates an argv tail for runMigrate.
func parseMigrateArgs(args []string) (migrateCmd, error) {
	if len(args) == 0 {
		return migrateCmd{}, fmt.Errorf("migrate: want a command (up|version|force), got none")
	}
	switch args[0] {
	case "up", "version":
		if len(args) != 1 {
			return migrateCmd{}, fmt.Errorf("migrate %s: takes no arguments, got %d", args[0], len(args)-1)
		}
		return migrateCmd{name: args[0]}, nil
	case "force":
		return parseForceArgs(args[1:])
	default:
		return migrateCmd{}, fmt.Errorf("migrate: unknown command %q (want: up|version|force)", args[0])
	}
}

// parseForceArgs reads `<version> --yes-i-know` in either order and refuses the
// whole invocation when the gate flag is absent.
func parseForceArgs(args []string) (migrateCmd, error) {
	version, gated, haveVersion := 0, false, false
	for _, arg := range args {
		if arg == forceGateFlag {
			gated = true
			continue
		}
		n, err := strconv.Atoi(arg)
		if err != nil {
			return migrateCmd{}, fmt.Errorf("migrate force: %q is neither a version number nor %s", arg, forceGateFlag)
		}
		if haveVersion {
			return migrateCmd{}, fmt.Errorf("migrate force: want exactly one version, got %d and %d", version, n)
		}
		version, haveVersion = n, true
	}
	if !haveVersion {
		return migrateCmd{}, fmt.Errorf("migrate force: want a version, e.g. `migrate force 12 %s` (-1 empties the ledger)", forceGateFlag)
	}
	if version < -1 {
		return migrateCmd{}, fmt.Errorf("migrate force: version %d is invalid (want >= -1; -1 empties the ledger)", version)
	}
	if !gated {
		return migrateCmd{}, fmt.Errorf("migrate force: refusing to rewrite the %s ledger to version %d without %s — forcing records a version WITHOUT running any migration, so the next `migrate up` will trust it; only use it after repairing the schema by hand", dbmigrate.Table, version, forceGateFlag)
	}
	return migrateCmd{name: "force", version: version}, nil
}

// dirtyLedgerErr turns a dirty ledger into a non-zero exit: the schema is in an
// unknown state, so no caller (deploy script, healthcheck, operator) may read
// success from this process.
func dirtyLedgerErr(st dbmigrate.Status) error {
	if !st.Dirty {
		return nil
	}
	return fmt.Errorf("migrate: %s is DIRTY at version %d — a migration failed part-way; repair the schema, then record it with `migrate force <version> %s`", dbmigrate.Table, st.Version, forceGateFlag)
}
