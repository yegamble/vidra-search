package store

import "testing"

// The bug this pins: PostgreSQL 14+ writes reltuples = -1 on a relation that has
// never been analyzed — "unknown", deliberately distinct from 0 — and the
// previous gauge source clamped that to 0 with GREATEST(reltuples, 0). On a
// young instance every search table therefore reported EMPTY. Measured in the
// A35 lab: 0 rows for all fifteen tables while events_inbox held 87.
func TestRowEstimatePrefersLiveTuplesAndRefusesTheUnknownSentinel(t *testing.T) {
	ptr := func(v int64) *int64 { return &v }
	cases := []struct {
		name      string
		nLiveTup  *int64
		reltuples float64
		want      *int64
	}{
		{"never analyzed but written to", ptr(87), -1, ptr(87)},
		{"never analyzed and no stats at all", nil, -1, nil},
		{"genuinely empty", ptr(0), -1, ptr(0)},
		{"analyzed, stats view missing", nil, 1234, ptr(1234)},
		{"both known — the live count wins", ptr(87), 40, ptr(87)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rowEstimate(tc.nLiveTup, tc.reltuples)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %d, want no estimate at all", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("got no estimate, want %d", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("got %d, want %d", *got, *tc.want)
			}
		})
	}
}
