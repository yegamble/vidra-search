//go:build integration

// Integration coverage for the embedded migrator against a REAL Postgres. It
// self-skips when DATABASE_URL is unset, so `make test-integration` stays safe
// on a bare host.
//
// Run locally:
//
//	docker compose up -d postgres
//	export DATABASE_URL=postgres://vidra_search:vidra_search@localhost:5433/vidra_search?sslmode=disable
//	go test -tags integration ./internal/dbmigrate/
package dbmigrate

import (
	"database/sql"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/vidra/vidra-search/migrations"
)

// TestUpAppliesEmbeddedMigrationsToTheLedgerTable is the live proof of the two
// properties deployments depend on: migrations apply from the binary alone, and
// the version ledger stays in Table (where the golang-migrate CLI left it — in
// CI this very database was migrated by the CLI first, so a mismatch would show
// up as a re-run of every migration).
func TestUpAppliesEmbeddedMigrationsToTheLedgerTable(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("integration test: DATABASE_URL must be set")
	}

	st, err := Up(dsn, io.Discard)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !st.Applied {
		t.Fatalf("Up reported no applied migrations: %s", st)
	}
	if st.Dirty {
		t.Fatalf("Up left a dirty ledger: %s", st)
	}
	if want := newestEmbeddedVersion(t); st.Version != want {
		t.Fatalf("Up left version %d, want the newest embedded migration %d", st.Version, want)
	}

	// The ledger row lives in Table, and it is the row golang-migrate reads back.
	db, err := sql.Open("pgx/v5", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	var version int64
	var dirty bool
	if err := db.QueryRow(`SELECT version, dirty FROM `+Schema+`.`+Table).Scan(&version, &dirty); err != nil {
		t.Fatalf("read %s.%s: %v", Schema, Table, err)
	}
	if uint(version) != st.Version || dirty != st.Dirty {
		t.Fatalf("%s.%s holds (version=%d dirty=%t), Up reported %s", Schema, Table, version, dirty, st)
	}

	// …and it is the ONLY ledger in the database. Before the schema was pinned,
	// a DSN carrying search_path=search made golang-migrate create a second,
	// empty ledger in `search` and re-apply every migration.
	var ledgers int
	if err := db.QueryRow(`SELECT count(*) FROM pg_tables WHERE tablename = $1`, Table).Scan(&ledgers); err != nil {
		t.Fatalf("count %s tables: %v", Table, err)
	}
	if ledgers != 1 {
		t.Fatalf("found %d %q tables, want exactly 1 (in %s)", ledgers, Table, Schema)
	}

	// Re-running is a no-op, not an error and not a re-apply.
	again, err := Up(dsn, io.Discard)
	if err != nil {
		t.Fatalf("second Up: %v", err)
	}
	if again != st {
		t.Fatalf("second Up changed the ledger: %s -> %s", st, again)
	}

	// Version agrees with Up without touching the schema.
	reported, err := Version(dsn)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if reported != st {
		t.Fatalf("Version = %s, want %s", reported, st)
	}
}

// TestUpAcceptsTheLegacyLedgerParameter covers the DSN shape the compose
// migrator used to be given: pgx would reject the unknown parameter outright.
func TestUpAcceptsTheLegacyLedgerParameter(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("integration test: DATABASE_URL must be set")
	}
	if _, err := Up(dsn+dsnSep(dsn)+legacyTableParam+"="+Table, io.Discard); err != nil {
		t.Fatalf("Up with %s in the DSN: %v", legacyTableParam, err)
	}
}

// TestUpRefusesASearchPathDSN is the live half of the ledger-schema fix: such a
// DSN used to succeed, silently creating a second ledger in the `search` schema.
func TestUpRefusesASearchPathDSN(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("integration test: DATABASE_URL must be set")
	}
	if _, err := Up(dsn+dsnSep(dsn)+"search_path=search", io.Discard); err == nil {
		t.Fatal("Up with search_path in the DSN succeeded, want a refusal")
	}

	db, err := sql.Open("pgx/v5", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = db.Close() }()
	var stray int
	if err := db.QueryRow(`SELECT count(*) FROM pg_tables WHERE tablename = $1 AND schemaname <> $2`, Table, Schema).Scan(&stray); err != nil {
		t.Fatalf("count stray ledgers: %v", err)
	}
	if stray != 0 {
		t.Fatalf("found %d %q tables outside %s", stray, Table, Schema)
	}
}

// TestForceRepairsADirtyLedger exercises the repair path end to end: dirty the
// ledger the way a half-applied migration would, force it back, and confirm the
// recorded state — because a force is exactly the operation nobody gets to
// rehearse during an incident.
func TestForceRepairsADirtyLedger(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("integration test: DATABASE_URL must be set")
	}
	current, err := Up(dsn, io.Discard)
	if err != nil {
		t.Fatalf("Up: %v", err)
	}

	db, err := sql.Open("pgx/v5", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Restore whatever the ledger said, whichever assertion below fails.
	t.Cleanup(func() {
		if _, _, err := Force(dsn, int(current.Version)); err != nil {
			t.Fatalf("restore ledger to %d: %v", current.Version, err)
		}
	})

	if _, err := db.Exec(`UPDATE ` + Schema + `.` + Table + ` SET dirty = true`); err != nil {
		t.Fatalf("dirty the ledger: %v", err)
	}
	dirty, err := Version(dsn)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if !dirty.Dirty {
		t.Fatalf("ledger did not go dirty: %s", dirty)
	}

	before, after, err := Force(dsn, int(current.Version))
	if err != nil {
		t.Fatalf("Force: %v", err)
	}
	if !before.Dirty {
		t.Fatalf("Force reported a clean ledger before the force: %s", before)
	}
	if after.Dirty || !after.Applied || after.Version != current.Version {
		t.Fatalf("Force left %s, want version=%d dirty=false", after, current.Version)
	}
	if reported, err := Version(dsn); err != nil || reported != after {
		t.Fatalf("Version = %s (err %v), want %s", reported, err, after)
	}

	// -1 empties the ledger; Up then re-applies from scratch against the schema
	// that is still there, which is a no-op for every idempotent migration —
	// so only assert the ledger state, and let Cleanup put the version back.
	if _, emptied, err := Force(dsn, -1); err != nil {
		t.Fatalf("Force(-1): %v", err)
	} else if emptied.Applied {
		t.Fatalf("Force(-1) left %s, want an empty ledger", emptied)
	}
}

// dsnSep is "?" or "&", whichever appends a parameter to dsn.
func dsnSep(dsn string) string {
	if strings.Contains(dsn, "?") {
		return "&"
	}
	return "?"
}

// newestEmbeddedVersion is the highest numeric prefix among the embedded up
// migrations — the version a fully migrated database must report.
func newestEmbeddedVersion(t *testing.T) uint {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.up.sql")
	if err != nil {
		t.Fatalf("glob embedded migrations: %v", err)
	}
	var newest uint
	for _, name := range names {
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			t.Fatalf("embedded migration %q has no version prefix", name)
		}
		n, err := strconv.ParseUint(prefix, 10, 64)
		if err != nil {
			t.Fatalf("embedded migration %q has a non-numeric version prefix: %v", name, err)
		}
		if uint(n) > newest {
			newest = uint(n)
		}
	}
	if newest == 0 {
		t.Fatal("no embedded up migrations found")
	}
	return newest
}
