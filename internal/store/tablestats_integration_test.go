//go:build integration

// The capacity gauge must not report a table with rows as empty.
//
// vidra_search_table_rows was sourced from GREATEST(pg_class.reltuples, 0). On
// PostgreSQL 14+ reltuples is -1 until VACUUM/ANALYZE first touches the
// relation, so on a young instance the clamp turned every table into "0 rows" —
// indistinguishable from an empty index, and the only capacity signal the
// service exports. Measured in the A35 lab: 0 for all fifteen tables while
// events_inbox held 87 rows, query_log 63 and documents 3.
package store_test

import (
	"context"
	"testing"
)

func TestIntegrationTableRowEstimatesSeeRowsBeforeAnyAnalyze(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Real writes through the real ingest path: documents + events_inbox both
	// gain rows, and nothing ANALYZEs afterwards — which is precisely the state
	// a freshly deployed instance is in.
	res := ingest(t, env, upsertEnvelope(t, video("A35 table-stats probe")))
	if res.Accepted != 1 {
		t.Fatalf("ingest did not land: %+v", res)
	}

	rows, err := env.store.TableRowEstimates(ctx)
	if err != nil {
		t.Fatalf("table row estimates: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no tables reported for the search schema")
	}
	byTable := map[string]*int64{}
	for _, r := range rows {
		byTable[r.Table] = r.Rows
	}
	for _, table := range []string{"documents", "events_inbox"} {
		got, ok := byTable[table]
		if !ok {
			t.Fatalf("%s missing from the estimates", table)
		}
		if got == nil {
			t.Fatalf("%s reported no estimate at all", table)
		}
		if *got < 1 {
			t.Errorf("%s estimated at %d rows after a successful ingest — the "+
				"never-analyzed reltuples sentinel is being read as empty", table, *got)
		}
	}
}
