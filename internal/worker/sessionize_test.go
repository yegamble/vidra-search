package worker

import (
	"testing"
	"time"
)

var base = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

func at(seconds int) time.Time { return base.Add(time.Duration(seconds) * time.Second) }

func cfg() SessionizeConfig {
	return SessionizeConfig{ReformulationGap: 60 * time.Second, AbandonWindow: 5 * time.Minute}
}

// has reports whether a derived event of the given type + normalized query exists.
func has(derived []Derived, typ, nq string) bool {
	for _, d := range derived {
		if d.Type == typ && d.NormalizedQuery == nq {
			return true
		}
	}
	return false
}

func TestSessionizeReformulation(t *testing.T) {
	// Two queries in one session, 30s apart, different forms → the later is a
	// reformulation of the earlier.
	queries := []QueryEvent{
		{ID: 1, NormalizedQuery: "golang tutorial", SessionID: "s1", SubmittedAt: at(0)},
		{ID: 2, NormalizedQuery: "golang guide", SessionID: "s1", SubmittedAt: at(30)},
	}
	got := Sessionize(queries, nil, cfg())
	if !has(got, "search.reformulated", "golang guide") {
		t.Errorf("expected reformulated for the second query, got %+v", got)
	}
	// The reformulated event carries the from/to pair.
	for _, d := range got {
		if d.Type == "search.reformulated" {
			if d.From != "golang tutorial" || d.To != "golang guide" {
				t.Errorf("reformulation from/to wrong: %+v", d)
			}
		}
	}
}

func TestSessionizeNoReformulationBeyondGap(t *testing.T) {
	queries := []QueryEvent{
		{ID: 1, NormalizedQuery: "a", SessionID: "s1", SubmittedAt: at(0)},
		{ID: 2, NormalizedQuery: "b", SessionID: "s1", SubmittedAt: at(90)}, // 90s > 60s gap
	}
	got := Sessionize(queries, nil, cfg())
	if has(got, "search.reformulated", "b") {
		t.Errorf("queries beyond the reformulation gap must not pair, got %+v", got)
	}
}

func TestSessionizeAbandonment(t *testing.T) {
	queries := []QueryEvent{
		{ID: 1, NormalizedQuery: "lonely query", SessionID: "s1", SubmittedAt: at(0)},
	}
	// No signals → abandoned.
	if got := Sessionize(queries, nil, cfg()); !has(got, "search.abandoned", "lonely query") {
		t.Fatalf("a query with no engagement must be abandoned, got %+v", got)
	}
	// A click on the same query within the window → NOT abandoned.
	signals := []Signal{{SessionID: "s1", NormalizedQuery: "lonely query", OccurredAt: at(20)}}
	if got := Sessionize(queries, signals, cfg()); has(got, "search.abandoned", "lonely query") {
		t.Errorf("an engaged query must not be abandoned, got %+v", got)
	}
	// A click outside the window still counts as abandoned.
	late := []Signal{{SessionID: "s1", NormalizedQuery: "lonely query", OccurredAt: at(600)}}
	if got := Sessionize(queries, late, cfg()); !has(got, "search.abandoned", "lonely query") {
		t.Errorf("engagement outside the window must not rescue the query, got %+v", got)
	}
}

func TestSessionizePlaySignalCountsForAnyQuery(t *testing.T) {
	queries := []QueryEvent{{ID: 1, NormalizedQuery: "watch me", SessionID: "s1", SubmittedAt: at(0)}}
	// A play_started carries no normalized query (empty) but still counts.
	signals := []Signal{{SessionID: "s1", NormalizedQuery: "", OccurredAt: at(10)}}
	if got := Sessionize(queries, signals, cfg()); has(got, "search.abandoned", "watch me") {
		t.Errorf("a play signal in the window must prevent abandonment, got %+v", got)
	}
}

func TestMeaningfulWatchQualifies(t *testing.T) {
	const thrSec, thrPct = 30, 30
	cases := []struct {
		pos, dur float64
		want     bool
	}{
		{35, 0, true},    // >= 30s absolute
		{30, 100, true},  // exactly 30s
		{25, 60, true},   // 25 >= 30% of 60 (=18)
		{10, 100, false}, // 10 < 30s and 10 < 30 (=30% of 100)
		{5, 60, false},   // 5 < 30s and 5 < 18
		{29, 0, false},   // just under, unknown duration
	}
	for _, c := range cases {
		if got := MeaningfulWatchQualifies(c.pos, c.dur, thrSec, thrPct); got != c.want {
			t.Errorf("MeaningfulWatchQualifies(%v,%v)=%v want %v", c.pos, c.dur, got, c.want)
		}
	}
}
