package dbmigrate

import (
	"io/fs"
	nurl "net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/vidra/vidra-search/migrations"
)

// migrationsDir is this repo's migrations directory, relative to this package.
const migrationsDir = "../../migrations"

// TestLedgerTableName pins the version ledger. The golang-migrate CLI put the
// ledger in this table via `x-migrations-table`; changing the name would restart
// the counter on every existing deployment and re-run every migration (and, in
// the shared-database deployment, collide with vidra-core's schema_migrations).
// The schema is pinned for the same reason: golang-migrate resolves the ledger
// against CURRENT_SCHEMA() when SchemaName is empty, so a DSN carrying
// search_path=search would open a SECOND, empty ledger in `search` and re-apply
// every migration to an already-migrated database.
func TestLedgerTableName(t *testing.T) {
	if Table != "vidra_search_migrations" {
		t.Fatalf("migration ledger table = %q, want vidra_search_migrations (existing deployments track their version there)", Table)
	}
	if Schema != "public" {
		t.Fatalf("migration ledger schema = %q, want public (where the golang-migrate CLI left the ledger)", Schema)
	}
}

// TestEmbeddedMigrationsMatchDisk proves the binary carries the whole migrations
// directory: a new .sql file that is somehow not embedded (wrong directory,
// deleted embed.go) would otherwise silently never be applied by `migrate up`.
func TestEmbeddedMigrationsMatchDisk(t *testing.T) {
	onDisk, err := filepath.Glob(filepath.Join(migrationsDir, "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(onDisk) == 0 {
		t.Fatalf("no .sql files found in %s", migrationsDir)
	}
	want := make([]string, 0, len(onDisk))
	for _, p := range onDisk {
		want = append(want, filepath.Base(p))
	}
	sort.Strings(want)

	entries, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob embedded migrations: %v", err)
	}
	sort.Strings(entries)

	if len(entries) != len(want) {
		t.Fatalf("embedded migrations = %d files, on disk = %d\nembedded: %v\non disk:  %v", len(entries), len(want), entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("embedded migration %d = %q, want %q", i, entries[i], want[i])
		}
	}
}

// TestEmbeddedMigrationsAreReadable guards against an embedded-but-empty file
// (e.g. an .sql created and never written), which golang-migrate would apply as
// a successful no-op and record as done.
func TestEmbeddedMigrationsAreReadable(t *testing.T) {
	entries, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob embedded migrations: %v", err)
	}
	for _, name := range entries {
		b, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		onDisk, err := os.ReadFile(filepath.Join(migrationsDir, name))
		if err != nil {
			t.Fatalf("read %s from disk: %v", name, err)
		}
		if string(b) != string(onDisk) {
			t.Fatalf("embedded %s differs from the file on disk", name)
		}
	}
}

// TestNormalizeDSN covers the DSN operators inherit from the old golang-migrate
// CLI invocation: it carried the ledger name as a query parameter that pgx does
// not understand.
func TestNormalizeDSN(t *testing.T) {
	const base = "postgres://u:p@h:5432/db?sslmode=disable"

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "plain dsn untouched",
			in:   base,
			want: base,
		},
		{
			name: "legacy ledger parameter dropped",
			in:   base + "&x-migrations-table=vidra_search_migrations",
			want: base,
		},
		{
			name:    "parameter naming a different ledger is refused",
			in:      base + "&x-migrations-table=schema_migrations",
			wantErr: true,
		},
		{
			name:    "search_path is refused",
			in:      base + "&search_path=search",
			wantErr: true,
		},
		{
			name:    "options carrying a search_path is refused",
			in:      base + "&options=" + nurl.QueryEscape("-c search_path=search"),
			wantErr: true,
		},
		{
			name: "keyword/value dsn passed through",
			in:   "host=h user=u dbname=db sslmode=disable",
			want: "host=h user=u dbname=db sslmode=disable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeDSN(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeDSN(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeDSN(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("normalizeDSN(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestForceRejectsInvalidVersion pins the guard that runs BEFORE any database
// is opened, so a nonsense version can never reach golang-migrate.
func TestForceRejectsInvalidVersion(t *testing.T) {
	if _, _, err := Force("postgres://u:p@127.0.0.1:1/db", -2); err == nil {
		t.Fatal("Force(-2) = nil error, want a refusal (valid versions are >= -1)")
	}
}

func TestStatusString(t *testing.T) {
	tests := []struct {
		name string
		in   Status
		want string
	}{
		{"empty ledger", Status{}, "version=none dirty=false table=vidra_search_migrations"},
		{"applied", Status{Version: 14, Applied: true}, "version=14 dirty=false table=vidra_search_migrations"},
		{"dirty", Status{Version: 3, Applied: true, Dirty: true}, "version=3 dirty=true table=vidra_search_migrations"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
