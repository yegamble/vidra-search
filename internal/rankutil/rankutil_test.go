package rankutil

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func TestAgeDays(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	updated := now.Add(-100 * 24 * time.Hour)

	cases := []struct {
		name      string
		published pgtype.Timestamptz
		want      float64
	}{
		// published_at wins whenever it is present…
		{"published wins", ts(now.Add(-10 * 24 * time.Hour)), 10},
		{"published today", ts(now), 0},
		// …and source_updated_at is the fallback, not a tiebreak.
		{"falls back to source_updated_at", pgtype.Timestamptz{}, 100},
		// A future publication date is a real thing in imported catalogues; it
		// must not score as negative age.
		{"future clamps to zero", ts(now.Add(48 * time.Hour)), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgeDays(tc.published, updated, now); got != tc.want {
				t.Errorf("AgeDays = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseUUIDs(t *testing.T) {
	a := uuid.New()
	b := uuid.New()

	got := ParseUUIDs([]string{a.String(), "not-a-uuid", "", b.String()})
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("ParseUUIDs = %v, want [%v %v] — bad entries are dropped, order kept", got, a, b)
	}

	// Never nil: the result goes straight into a SQL array parameter.
	if got := ParseUUIDs(nil); got == nil || len(got) != 0 {
		t.Errorf("ParseUUIDs(nil) = %v, want an empty non-nil slice", got)
	}
}
