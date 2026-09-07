package store

import (
	"context"
	"fmt"
)

// TableRowEstimate is one search-schema table's estimated live row count.
//
// Rows is a POINTER because "PostgreSQL does not know" is a real answer and is
// not the same fact as "the table is empty". A gauge that renders the first as
// the second tells an operator their index is empty while it holds a hundred
// thousand documents.
type TableRowEstimate struct {
	Table string
	Rows  *int64
}

// tableRowEstimateQuery samples the search schema's tables from the statistics
// PostgreSQL already keeps, so a scrape costs no counting.
//
// It reads BOTH estimates because they fail in opposite directions:
//
//   - pg_stat_user_tables.n_live_tup is maintained by the cumulative statistics
//     system on every insert/update/delete, so it is live on a running instance
//     that has never been vacuumed or analyzed. It is the one to prefer.
//   - pg_class.reltuples is only written by VACUUM/ANALYZE, and since
//     PostgreSQL 14 it is **-1** on a relation that has never been analyzed —
//     the documented "unknown" sentinel, deliberately distinct from 0 so the
//     planner can tell a never-analyzed table from an empty one.
//
// Both remain ESTIMATES, and n_live_tup is the looser of the two after bulk
// work: measured in the same lab, repeated TRUNCATE+insert cycles left it
// reading 6 for a table holding 1, and a plain ANALYZE reconciled both
// statistics to 1. That is drift in a number the metric's own help text calls
// approximate. Reading a sentinel as a count is not drift — it is a different
// answer to a different question, and it is what this fixes.
//
// Reading reltuples alone and clamping it at zero (the shape this replaced)
// therefore reported EVERY table as empty on a young instance: measured in the
// A35 lab, `vidra_search_table_rows` was 0 for all fifteen tables while
// events_inbox held 87 rows, query_log 63 and documents 3, with reltuples = -1
// and last_analyze/last_autoanalyze both null.
const tableRowEstimateQuery = `SELECT c.relname, s.n_live_tup, c.reltuples
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
WHERE n.nspname = 'search' AND c.relkind = 'r'`

// TableRowEstimates returns one estimate per table in the search schema. It uses
// the raw pool because the system catalogs are outside sqlc's analyzed schema.
func (s *Store) TableRowEstimates(ctx context.Context) ([]TableRowEstimate, error) {
	rows, err := s.Pool.Query(ctx, tableRowEstimateQuery)
	if err != nil {
		return nil, fmt.Errorf("store: table row estimates: %w", err)
	}
	defer rows.Close()
	var out []TableRowEstimate
	for rows.Next() {
		var (
			name      string
			nLiveTup  *int64
			reltuples float64
		)
		if err := rows.Scan(&name, &nLiveTup, &reltuples); err != nil {
			return nil, fmt.Errorf("store: table row estimates: %w", err)
		}
		out = append(out, TableRowEstimate{Table: name, Rows: rowEstimate(nLiveTup, reltuples)})
	}
	return out, rows.Err()
}

// rowEstimate picks the trustworthy of the two estimates, or nil when neither
// can answer. Split out from the query so the choice is provable without a
// database.
func rowEstimate(nLiveTup *int64, reltuples float64) *int64 {
	if nLiveTup != nil {
		v := *nLiveTup
		return &v
	}
	if reltuples >= 0 {
		v := int64(reltuples)
		return &v
	}
	return nil
}
