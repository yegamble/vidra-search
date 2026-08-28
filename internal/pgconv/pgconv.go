// Package pgconv converts between plain Go values and the shapes sqlc emits for
// nullable columns — the pgtype wrappers (nullable uuid → pgtype.UUID, nullable
// timestamptz → pgtype.Timestamptz) and the plain pointers it uses for nullable
// text. Keeping the conversions in one place keeps the event applier and query
// callers readable.
package pgconv

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// UUID wraps a non-optional uuid.UUID as a valid pgtype.UUID.
func UUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// UUIDPtr wraps an optional uuid as a pgtype.UUID (invalid when nil → SQL NULL).
func UUIDPtr(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

// TimePtr wraps an optional time as a pgtype.Timestamptz (invalid when nil).
func TimePtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// UUIDValue returns the plain uuid and whether it was non-NULL.
func UUIDValue(v pgtype.UUID) (uuid.UUID, bool) {
	if !v.Valid {
		return uuid.Nil, false
	}
	return v.Bytes, true
}

// OptStr maps an empty string to a nil (SQL NULL) optional parameter, which is
// how every optional text filter reaches sqlc: "" means "no filter", not "match
// the empty string".
func OptStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// DerefStr reads an optional text column back as a plain string, NULL becoming
// "". The inverse of OptStr.
func DerefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
