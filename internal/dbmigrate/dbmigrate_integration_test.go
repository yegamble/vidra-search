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
	if err := db.QueryRow(`SELECT version, dirty FROM `+Table).Scan(&version, &dirty); err != nil {
		t.Fatalf("read %s: %v", Table, err)
	}
	if uint(version) != st.Version || dirty != st.Dirty {
		t.Fatalf("%s holds (version=%d dirty=%t), Up reported %s", Table, version, dirty, st)
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
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	if _, err := Up(dsn+sep+legacyTableParam+"="+Table, io.Discard); err != nil {
		t.Fatalf("Up with %s in the DSN: %v", legacyTableParam, err)
	}
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
