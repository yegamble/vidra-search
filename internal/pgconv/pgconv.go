// Package pgconv converts between plain Go values and the pgtype wrappers that
// sqlc emits for nullable columns (nullable uuid → pgtype.UUID, nullable
// timestamptz → pgtype.Timestamptz). Keeping the conversions in one place keeps
// the event applier and query callers readable.
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
