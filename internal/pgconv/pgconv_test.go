package pgconv

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestUUIDRoundTrip(t *testing.T) {
	id := uuid.New()
	got, ok := UUIDValue(UUID(id))
	if !ok || got != id {
		t.Errorf("UUIDValue(UUID(%v)) = %v, %v", id, got, ok)
	}

	if got, ok := UUIDValue(pgtype.UUID{}); ok || got != uuid.Nil {
		t.Errorf("UUIDValue(NULL) = %v, %v, want uuid.Nil, false", got, ok)
	}

	if v := UUIDPtr(nil); v.Valid {
		t.Error("UUIDPtr(nil) is valid; a nil id must become SQL NULL")
	}
	if v := UUIDPtr(&id); !v.Valid || uuid.UUID(v.Bytes) != id {
		t.Errorf("UUIDPtr(&id) = %+v, want the id, valid", v)
	}
}

func TestTimePtr(t *testing.T) {
	if v := TimePtr(nil); v.Valid {
		t.Error("TimePtr(nil) is valid; a nil time must become SQL NULL")
	}
	now := time.Now()
	if v := TimePtr(&now); !v.Valid || !v.Time.Equal(now) {
		t.Errorf("TimePtr(&now) = %+v, want now, valid", v)
	}
}

// "" means "no filter", not "match the empty string" — which is the whole
// reason optional text filters travel as a pointer.
func TestOptStrAndDerefStr(t *testing.T) {
	if OptStr("") != nil {
		t.Error(`OptStr("") is non-nil; an absent filter must be SQL NULL`)
	}
	got := OptStr("music")
	if got == nil || *got != "music" {
		t.Errorf("OptStr(\"music\") = %v, want a pointer to music", got)
	}

	if s := DerefStr(nil); s != "" {
		t.Errorf("DerefStr(nil) = %q, want the empty string", s)
	}
	if s := DerefStr(got); s != "music" {
		t.Errorf("DerefStr = %q, want music", s)
	}
}
