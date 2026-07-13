package worker

import (
	"sort"
	"time"

	"github.com/google/uuid"
)

// QueryEvent is one settled query_log row the sessionizer considers.
type QueryEvent struct {
	ID              int64
	NormalizedQuery string
	UserID          *uuid.UUID
	SessionID       string
	SubmittedAt     time.Time
}

// Signal is one click/play the sessionizer uses to decide abandonment.
type Signal struct {
	SessionID       string
	NormalizedQuery string
	OccurredAt      time.Time
}

// Derived is one synthetic behavioral event the sessionizer emits.
type Derived struct {
	SourceID        int64 // the source query_log id — makes the event id deterministic + unique
	Type            string
	UserID          *uuid.UUID
	SessionID       string
	NormalizedQuery string
	OccurredAt      time.Time
	From            string // reformulated: previous normalized query
	To              string // reformulated: new normalized query
}

// SessionizeConfig holds the two detection windows.
type SessionizeConfig struct {
	ReformulationGap time.Duration // consecutive queries closer than this are a reformulation
	AbandonWindow    time.Duration // a query with no click/play within this window is abandoned
}

// Sessionize derives search.reformulated and search.abandoned events from a batch
// of settled query events and the click/play signals in their window. It is a
// pure function over its inputs so the detection rules are unit-testable.
//
//   - reformulated: two consecutive queries in the same session, submitted within
//     ReformulationGap of each other, with different normalized forms. The later
//     query is the reformulation.
//   - abandoned: a query with no result_clicked/play_started in the same session
//     within AbandonWindow after it (a "bad abandonment" signal).
//
// queries must be grouped by session and time-ordered (the ListQueryLogRange
// query guarantees this); Sessionize also sorts defensively.
func Sessionize(queries []QueryEvent, signals []Signal, cfg SessionizeConfig) []Derived {
	if len(queries) == 0 {
		return nil
	}
	qs := append([]QueryEvent(nil), queries...)
	sort.SliceStable(qs, func(i, j int) bool {
		if qs[i].SessionID != qs[j].SessionID {
			return qs[i].SessionID < qs[j].SessionID
		}
		if !qs[i].SubmittedAt.Equal(qs[j].SubmittedAt) {
			return qs[i].SubmittedAt.Before(qs[j].SubmittedAt)
		}
		return qs[i].ID < qs[j].ID
	})

	// Index signals by session for windowed abandonment lookups.
	bySession := map[string][]Signal{}
	for _, s := range signals {
		bySession[s.SessionID] = append(bySession[s.SessionID], s)
	}

	var out []Derived
	for i, q := range qs {
		// Reformulation: compare with the previous query in the SAME session.
		if i > 0 {
			prev := qs[i-1]
			if prev.SessionID == q.SessionID &&
				q.NormalizedQuery != prev.NormalizedQuery &&
				!q.SubmittedAt.Before(prev.SubmittedAt) &&
				q.SubmittedAt.Sub(prev.SubmittedAt) <= cfg.ReformulationGap {
				out = append(out, Derived{
					SourceID: q.ID, Type: "search.reformulated", UserID: q.UserID,
					SessionID: q.SessionID, NormalizedQuery: q.NormalizedQuery,
					OccurredAt: q.SubmittedAt, From: prev.NormalizedQuery, To: q.NormalizedQuery,
				})
			}
		}
		// Abandonment: no engagement in the window after this query.
		if !hasEngagement(bySession[q.SessionID], q, cfg.AbandonWindow) {
			out = append(out, Derived{
				SourceID: q.ID, Type: "search.abandoned", UserID: q.UserID,
				SessionID: q.SessionID, NormalizedQuery: q.NormalizedQuery, OccurredAt: q.SubmittedAt,
			})
		}
	}
	return out
}

// hasEngagement reports whether any signal for the session falls in
// [q.SubmittedAt, q.SubmittedAt+window]. A click must match the query's normalized
// form; a play (empty normalized query) counts for any query in the window.
func hasEngagement(signals []Signal, q QueryEvent, window time.Duration) bool {
	end := q.SubmittedAt.Add(window)
	for _, s := range signals {
		if s.OccurredAt.Before(q.SubmittedAt) || s.OccurredAt.After(end) {
			continue
		}
		if s.NormalizedQuery == "" || s.NormalizedQuery == q.NormalizedQuery {
			return true
		}
	}
	return false
}

// MeaningfulWatchQualifies is the reference predicate for meaningful-watch
// derivation (§1.5): a watch counts when the position reaches thresholdSeconds OR
// thresholdPct% of the known duration. The DeriveMeaningfulWatch SQL implements
// exactly this rule; the function exists so the thresholds are unit-testable in Go.
func MeaningfulWatchQualifies(positionSeconds, durationSeconds float64, thresholdSeconds, thresholdPct int) bool {
	if positionSeconds >= float64(thresholdSeconds) {
		return true
	}
	if durationSeconds > 0 && positionSeconds >= float64(thresholdPct)/100.0*durationSeconds {
		return true
	}
	return false
}
