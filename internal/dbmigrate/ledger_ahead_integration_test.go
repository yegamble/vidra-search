//go:build integration

// The rollback floor (A38, 2026-09-07), proved against a REAL Postgres ledger:
// a CLEAN ledger ahead of this binary's newest embedded migration is the state
// deploy/rollback.sh puts the previous release's search migrator in, and it must
// be a logged no-op with exit 0 — not the "no migration found for version 18"
// failure that used to take every service depending on the one-shot down.
//
// TWIN: vidra-core internal/dbmigrate/ledger_ahead_integration_test.go.
//
//	DATABASE_URL=postgres://…/vidra_search?sslmode=disable \
//	go test -tags=integration ./internal/dbmigrate/ -run TestUpWhenTheLedgerIsAhead
package dbmigrate

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
)

// stampAhead moves the ledger past the newest embedded migration the way a newer
// release's migrator leaves it, and restores the real version when the test ends.
func stampAhead(t *testing.T, dsn string) (ahead, embeddedMax uint) {
	t.Helper()
	current, err := Version(dsn)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if !current.Applied {
		t.Skip("integration test: the database has never been migrated; run `migrate up` first")
	}
	embeddedMax, err = EmbeddedMax()
	if err != nil {
		t.Fatalf("EmbeddedMax: %v", err)
	}
	ahead = embeddedMax + 10
	t.Cleanup(func() {
		if _, _, err := Force(dsn, int(current.Version)); err != nil {
			t.Fatalf("restore ledger to %d: %v", current.Version, err)
		}
	})
	if _, _, err := Force(dsn, int(ahead)); err != nil {
		t.Fatalf("force ledger to %d: %v", ahead, err)
	}
	return ahead, embeddedMax
}

func TestUpWhenTheLedgerIsAheadAndCleanIsALoggedNoOp(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("integration test: DATABASE_URL must be set")
	}
	if _, err := Up(dsn, nil); err != nil {
		t.Fatalf("initial Up: %v", err)
	}
	ahead, embeddedMax := stampAhead(t, dsn)

	var buf bytes.Buffer
	st, err := Up(dsn, &buf)
	if err != nil {
		t.Fatalf("Up against a ledger at %d with embedded max %d: %v (want a no-op)", ahead, embeddedMax, err)
	}

	// One line, and it says which two numbers disagree — an operator reading a
	// rollback log has to be able to tell this apart from a silent success.
	out := buf.String()
	if want := LedgerAheadMessage(ahead, embeddedMax); !strings.Contains(out, want) {
		t.Errorf("output = %q, want it to contain %q", out, want)
	}
	for _, field := range []string{
		fmt.Sprintf("ledger_version=%d", ahead),
		fmt.Sprintf("embedded_max=%d", embeddedMax),
		"dirty=false",
	} {
		if !strings.Contains(out, field) {
			t.Errorf("output = %q, want the structured field %q", out, field)
		}
	}

	// Nothing was applied and nothing was rewritten.
	if st.Version != ahead || st.Dirty || !st.Applied {
		t.Fatalf("Up reported %s, want a clean version %d", st, ahead)
	}
	after, err := Version(dsn)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if after != st {
		t.Fatalf("ledger = %s, Up reported %s — the no-op must not touch it", after, st)
	}
}

// A ledger ahead but DIRTY still fails: dirty means the schema state is unknown,
// which no compatibility policy makes safe to serve.
func TestUpWhenTheLedgerIsAheadAndDirtyStillFails(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("integration test: DATABASE_URL must be set")
	}
	if _, err := Up(dsn, nil); err != nil {
		t.Fatalf("initial Up: %v", err)
	}
	ahead, _ := stampAhead(t, dsn)

	db, err := sql.Open("pgx/v5", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE ` + Schema + `.` + Table + ` SET dirty = true`); err != nil {
		t.Fatalf("dirty the ledger: %v", err)
	}

	if _, err := Up(dsn, nil); err == nil {
		t.Fatalf("Up against a DIRTY ledger at %d returned nil, want a refusal", ahead)
	}
}
