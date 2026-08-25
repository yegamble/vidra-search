package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSearchCountSharesSimpleWhereClause is the drift guard for the simple-mode
// total. SearchSimpleCount only means anything if it counts EXACTLY the rows
// SearchSimple pages over — a count whose predicates have drifted from the page
// query's is worse than no count at all, because the client cannot tell it is
// wrong. Both queries use the same named parameters, so their FROM + WHERE text
// must be byte-identical; this test fails the build the moment one is edited
// without the other.
//
// It reads the hand-written .sql (not the generated Go) because that is the file
// a human edits, and because sqlc renumbers positional placeholders per query,
// which would make the generated text differ for reasons that are not drift.
func TestSearchCountSharesSimpleWhereClause(t *testing.T) {
	path := filepath.Join("queries", "search.sql")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sql := string(data)

	page := statement(t, sql, "-- name: SearchSimple :many")
	count := statement(t, sql, "-- name: SearchSimpleCount :one")

	pagePredicates := predicates(t, page, "SearchSimple")
	countPredicates := predicates(t, count, "SearchSimpleCount")

	if pagePredicates != countPredicates {
		t.Errorf("SearchSimpleCount no longer counts what SearchSimple pages over.\n"+
			"Edit the two FROM+WHERE clauses together, or the reported total silently lies.\n"+
			"--- SearchSimple ---\n%s\n--- SearchSimpleCount ---\n%s",
			pagePredicates, countPredicates)
	}
}

// statement returns the text of the query introduced by the given `-- name:`
// header, up to the next header or end of file.
func statement(t *testing.T, sql, header string) string {
	t.Helper()
	i := strings.Index(sql, header)
	if i < 0 {
		t.Fatalf("query %q not found in queries/search.sql", header)
	}
	rest := sql[i+len(header):]
	if j := strings.Index(rest, "-- name:"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// predicates extracts the FROM + WHERE region a query filters on: everything
// from "FROM search.documents" up to the ORDER BY (or the statement terminator).
// The SELECT list is deliberately excluded — the page query's scoring expression
// has no bearing on which rows match.
func predicates(t *testing.T, stmt, name string) string {
	t.Helper()
	i := strings.Index(stmt, "FROM search.documents")
	if i < 0 {
		t.Fatalf("%s: no `FROM search.documents` clause found", name)
	}
	body := stmt[i:]
	if j := strings.Index(body, "ORDER BY"); j >= 0 {
		body = body[:j]
	}
	body = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), ";"))
	if !strings.Contains(body, "WHERE") {
		t.Fatalf("%s: no WHERE clause found in the extracted region", name)
	}
	return body
}
