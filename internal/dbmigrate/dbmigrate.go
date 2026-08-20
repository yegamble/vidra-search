// Package dbmigrate applies the embedded SQL migrations (see the migrations
// package) to the vidra-search database, using golang-migrate as a LIBRARY
// rather than the CLI. That is what makes the published image self-sufficient:
// no migrate/migrate sidecar, no bind-mounted migrations directory, hence no
// runtime dependence on a git checkout.
//
// The version ledger stays in the table this service has always used
// (see Table), so an existing deployment continues on exactly the same counter
// the golang-migrate CLI maintained for it.
package dbmigrate

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	nurl "net/url"
	"strings"

	golangmigrate "github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx/v5" database/sql driver

	"github.com/vidra/vidra-search/migrations"
)

// Table is the golang-migrate version ledger table. It MUST stay
// "vidra_search_migrations" (in the DSN's current schema, i.e. public): the
// golang-migrate CLI put the ledger there via `x-migrations-table`, and
// vidra-search usually shares vidra-core's database, where the default
// `schema_migrations` belongs to core. Renaming it would restart the counter
// from zero on every existing deployment and re-run every migration.
const Table = "vidra_search_migrations"

// Schema is the schema Table lives in. It is pinned rather than inherited from
// the connection: golang-migrate otherwise resolves the ledger against
// CURRENT_SCHEMA(), so a DSN carrying search_path=search would create a SECOND
// ledger in the search schema and re-apply every migration to an already
// migrated database.
const Schema = "public"

// legacyTableParam is the CLI-only DSN parameter that used to carry Table.
// pgx rejects unknown DSN parameters, so a DSN inherited from the old migrator
// invocation is normalized (see normalizeDSN) instead of failing obscurely.
const legacyTableParam = "x-migrations-table"

// schemaParams are the DSN parameters that move schema resolution. The migrator
// refuses them: Schema and Table are compiled in, so a DSN that says otherwise
// is a disagreement about where the ledger lives, exactly like a conflicting
// x-migrations-table. (Migrations themselves schema-qualify everything they
// create, so they never needed a search_path either.)
var schemaParams = []string{"search_path", "options"}

// Status is the state of the migration ledger.
type Status struct {
	// Version is the newest applied migration's numeric prefix. It is
	// meaningless unless Applied is true.
	Version uint
	// Applied reports whether any migration has been recorded at all.
	Applied bool
	// Dirty reports that a migration failed part-way and the schema is in an
	// unknown state. It requires manual repair; no further migration will run.
	Dirty bool
}

// String renders the status the way the `migrate version` subcommand reports it.
func (s Status) String() string {
	version := "none"
	if s.Applied {
		version = fmt.Sprintf("%d", s.Version)
	}
	return fmt.Sprintf("version=%s dirty=%t table=%s", version, s.Dirty, Table)
}

// Up applies every embedded migration the database has not run yet and returns
// the resulting status. It is idempotent: an already-current database is a
// no-op. Per-migration progress is written to progress (nil discards it).
func Up(dsn string, progress io.Writer) (Status, error) {
	m, closeFn, err := open(dsn)
	if err != nil {
		return Status{}, err
	}
	defer closeFn()

	if progress != nil {
		m.Log = migrateLogger{w: progress}
	}
	if err := m.Up(); err != nil && !errors.Is(err, golangmigrate.ErrNoChange) {
		return Status{}, fmt.Errorf("dbmigrate: apply migrations: %w", err)
	}
	return status(m)
}

// Version reports the ledger state without changing the schema. (The ledger
// table itself is created if missing — golang-migrate does that when it opens
// the database, whichever direction it is asked about.)
func Version(dsn string) (Status, error) {
	m, closeFn, err := open(dsn)
	if err != nil {
		return Status{}, err
	}
	defer closeFn()
	return status(m)
}

// Force overwrites the ledger with version and clears the dirty flag WITHOUT
// running any SQL, and reports the ledger state before and after. It is the
// repair tool for a migration that died part-way: an operator finishes (or
// undoes) that migration's SQL by hand, then tells the ledger where the schema
// actually is.
//
// It is destructive in the way that matters most quietly: a wrong version makes
// the next `migrate up` skip migrations that were never applied, or re-apply
// ones that were. cmd/api therefore gates it behind an explicit flag. Version
// -1 empties the ledger (back to "nothing applied").
func Force(dsn string, version int) (before, after Status, err error) {
	if version < -1 {
		return Status{}, Status{}, fmt.Errorf("dbmigrate: force version %d is invalid (want >= -1; -1 empties the ledger)", version)
	}
	m, closeFn, err := open(dsn)
	if err != nil {
		return Status{}, Status{}, err
	}
	defer closeFn()

	before, err = status(m)
	if err != nil {
		return Status{}, Status{}, err
	}
	if err := m.Force(version); err != nil {
		return Status{}, Status{}, fmt.Errorf("dbmigrate: force ledger %s to version %d: %w", Table, version, err)
	}
	after, err = status(m)
	if err != nil {
		return Status{}, Status{}, err
	}
	return before, after, nil
}

func status(m *golangmigrate.Migrate) (Status, error) {
	version, dirty, err := m.Version()
	switch {
	case errors.Is(err, golangmigrate.ErrNilVersion):
		return Status{Dirty: dirty}, nil
	case err != nil:
		return Status{}, fmt.Errorf("dbmigrate: read ledger %s: %w", Table, err)
	}
	return Status{Version: version, Applied: true, Dirty: dirty}, nil
}

// open wires the embedded migrations (iofs source) to the database (pgx/v5
// driver) with the ledger table pinned to Table. The returned func releases the
// source and the database handle.
func open(dsn string) (*golangmigrate.Migrate, func(), error) {
	dsn, err := normalizeDSN(dsn)
	if err != nil {
		return nil, nil, err
	}
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("dbmigrate: read embedded migrations: %w", err)
	}
	db, err := sql.Open("pgx/v5", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("dbmigrate: open database: %w", err)
	}
	// WithInstance is the seam that replaces `x-migrations-table`: the ledger
	// name AND its schema are configured in code, so neither can be lost from —
	// or moved by — a DSN.
	drv, err := pgxdriver.WithInstance(db, &pgxdriver.Config{MigrationsTable: Table, SchemaName: Schema})
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("dbmigrate: connect: %w", err)
	}
	m, err := golangmigrate.NewWithInstance("iofs", src, "pgx5", drv)
	if err != nil {
		_ = drv.Close()
		return nil, nil, fmt.Errorf("dbmigrate: init migrator: %w", err)
	}
	// m.Close closes both the source and the driver (which closes the *sql.DB).
	return m, func() { _, _ = m.Close() }, nil
}

// normalizeDSN drops golang-migrate's CLI-only x-migrations-table parameter from
// a URL-form DSN: pgx refuses unknown parameters, and operators inherit DSNs
// that carry it from the previous migrator invocation. A parameter naming a
// DIFFERENT table — or moving the schema (schemaParams) — is a real
// disagreement with the compiled-in ledger and is refused rather than silently
// ignored. Keyword/value DSNs ("host=… user=…") are passed through untouched:
// they cannot carry x-migrations-table, and a search_path they do carry is
// already neutralized by the pinned Schema.
func normalizeDSN(dsn string) (string, error) {
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return dsn, nil
	}
	u, err := nurl.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("dbmigrate: invalid database URL: %w", err)
	}
	q := u.Query()
	for _, p := range schemaParams {
		if q.Has(p) {
			return "", fmt.Errorf("dbmigrate: database URL sets %s=%q but this binary owns the %q ledger in schema %q; drop the parameter (migrations schema-qualify everything they create)", p, q.Get(p), Table, Schema)
		}
	}
	if !q.Has(legacyTableParam) {
		return dsn, nil
	}
	if got := q.Get(legacyTableParam); got != Table {
		return "", fmt.Errorf("dbmigrate: database URL sets %s=%q but this binary owns the %q ledger; drop the parameter", legacyTableParam, got, Table)
	}
	q.Del(legacyTableParam)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// migrateLogger adapts an io.Writer to golang-migrate's logger interface so
// `migrate up` reports each applied migration the way the CLI did.
type migrateLogger struct{ w io.Writer }

func (l migrateLogger) Printf(format string, v ...any) { fmt.Fprintf(l.w, format, v...) }

// Verbose false keeps the output to one line per applied migration.
func (l migrateLogger) Verbose() bool { return false }
