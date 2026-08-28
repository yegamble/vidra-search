// Package rankutil holds the small feature-prep helpers search and
// recommendation both need to turn a stored document (or a session's video
// list) into ranking input. They were byte-identical copies in each package,
// which meant a fix to one — the future-timestamp clamp in AgeDays, say —
// silently applied to only half the ranked surface.
package rankutil

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// AgeDays returns a document's age in days from published_at, falling back to
// source_updated_at when the source gave no publication date. A future
// timestamp — which imported catalogues do contain — clamps to 0 rather than
// scoring as negative age, which every freshness decay would read as
// impossibly fresh.
func AgeDays(publishedAt pgtype.Timestamptz, sourceUpdatedAt, now time.Time) float64 {
	t := sourceUpdatedAt
	if publishedAt.Valid {
		t = publishedAt.Time
	}
	d := now.Sub(t).Hours() / 24
	if d < 0 {
		return 0
	}
	return d
}

// ParseUUIDs parses a slice of id strings, dropping any that do not parse. The
// input is session state read back from Redis, so a malformed entry is a
// reason to ignore that seed, never to fail the request.
func ParseUUIDs(ss []string) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ss))
	for _, s := range ss {
		if id, err := uuid.Parse(s); err == nil {
			out = append(out, id)
		}
	}
	return out
}
