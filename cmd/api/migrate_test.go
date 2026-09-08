package main

import (
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/vidra/vidra-search/internal/dbmigrate"
)

// TestParseMigrateArgs pins the argv contract of the `migrate` subcommand — in
// particular the force gate: `migrate force <version>` must be REFUSED without
// --yes-i-know, because forcing records a version without running any migration
// and the next `migrate up` then trusts it.
func TestParseMigrateArgs(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		want        migrateCmd
		wantErr     bool
		errContains string
	}{
		{name: "up", args: []string{"up"}, want: migrateCmd{name: "up"}},
		{name: "version", args: []string{"version"}, want: migrateCmd{name: "version"}},
		{name: "no command", args: nil, wantErr: true},
		{name: "embedded-max", args: []string{"embedded-max"}, want: migrateCmd{name: "embedded-max"}},
		{name: "unknown command", args: []string{"sideways"}, wantErr: true, errContains: "up|version|embedded-max|force"},
		{name: "embedded-max takes no arguments", args: []string{"embedded-max", "3"}, wantErr: true},
		{name: "up takes no arguments", args: []string{"up", "3"}, wantErr: true},
		{
			name: "force gated",
			args: []string{"force", "12", "--yes-i-know"},
			want: migrateCmd{name: "force", version: 12},
		},
		{
			name: "force gate flag may come first",
			args: []string{"force", "--yes-i-know", "12"},
			want: migrateCmd{name: "force", version: 12},
		},
		{
			name: "force to the empty ledger",
			args: []string{"force", "-1", "--yes-i-know"},
			want: migrateCmd{name: "force", version: -1},
		},
		{
			name:        "force refused without the gate",
			args:        []string{"force", "12"},
			wantErr:     true,
			errContains: forceGateFlag,
		},
		{
			name:        "force refused with a typo'd gate",
			args:        []string{"force", "12", "--yes"},
			wantErr:     true,
			errContains: "neither a version number",
		},
		{name: "force wants a version", args: []string{"force", "--yes-i-know"}, wantErr: true},
		{name: "force wants exactly one version", args: []string{"force", "1", "2", "--yes-i-know"}, wantErr: true},
		{name: "force rejects versions below -1", args: []string{"force", "-2", "--yes-i-know"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMigrateArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseMigrateArgs(%q) = %+v, want an error", tc.args, got)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("parseMigrateArgs(%q) error = %q, want it to mention %q", tc.args, err, tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMigrateArgs(%q): %v", tc.args, err)
			}
			if got != tc.want {
				t.Fatalf("parseMigrateArgs(%q) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// TestRunMigrateRefusesForceBeforeTouchingTheDatabase proves the gate is checked
// before the DSN is even read: an ungated force must fail identically whether or
// not a database is reachable (here DATABASE_URL is unset, which would otherwise
// be the first error reported).
func TestRunMigrateRefusesForceBeforeTouchingTheDatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	err := runMigrate([]string{"force", "7"})
	if err == nil {
		t.Fatal("runMigrate(force 7) = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), forceGateFlag) {
		t.Fatalf("runMigrate(force 7) error = %q, want the %s refusal (not a config error)", err, forceGateFlag)
	}
}

// `migrate embedded-max` is deploy/restore.sh's preflight question and it is
// asked of an image that may have NO database in front of it. LoadDatabaseURL
// REFUSES an unset DATABASE_URL, so this proves the answer is produced before
// that call — and that it is a BARE integer, which restore.sh reads with $(...).
func TestRunMigrateEmbeddedMaxNeedsNoDatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	runErr := runMigrate([]string{"embedded-max"})
	_ = w.Close()
	os.Stdout = stdout
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if runErr != nil {
		t.Fatalf("runMigrate([embedded-max]) = %v, want it to answer from the embedded FS alone", runErr)
	}

	got := strings.TrimSpace(string(out))
	n, err := strconv.ParseUint(got, 10, 64)
	if err != nil {
		t.Fatalf("migrate embedded-max printed %q, want a bare integer: %v", got, err)
	}
	want, err := dbmigrate.EmbeddedMax()
	if err != nil {
		t.Fatalf("EmbeddedMax: %v", err)
	}
	if uint(n) != want {
		t.Fatalf("migrate embedded-max printed %d, want %d", n, want)
	}
}
