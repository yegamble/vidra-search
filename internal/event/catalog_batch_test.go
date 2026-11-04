package event

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/catalog"
)

func TestCatalogBatchRequiresExactUnambiguousEnvelopeAndWholeWireBudget(t *testing.T) {
	event := Envelope{EventID: uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"), Type: catalog.TypeSnapshotEnd, SchemaVersion: 2,
		OccurredAt: time.Now().UTC(), Payload: json.RawMessage(`{"scope":"local"}`)}
	item, _ := json.Marshal(event)
	valid := []byte(`{"events":[` + string(item) + `]}`)
	exact := append(append([]byte(nil), valid...), bytes.Repeat([]byte(" "), catalog.MaxRequestBytes-len(valid))...)
	if got, err := DecodeCatalogBatch(exact); err != nil || len(got) != 1 || got[0].EventID != event.EventID {
		t.Fatalf("exact full wire budget refused: %+v/%v", got, err)
	}
	for _, raw := range []string{
		string(append(exact, ' ')), string(valid) + `{}`, `{"events":[]}`, `{"events":null}`,
		`{"events":[null]}`, `{"Events":[` + string(item) + `]}`,
		`{"events":[` + string(item) + `],"events":[]}`,
		`{"events":[` + strings.Replace(string(item), `"schema_version":2`, `"schema_version":2,"Schema_version":2`, 1) + `]}`,
		`{"events":[` + strings.Replace(string(item), `"type":`, `"ty\u0070e":"other","type":`, 1) + `]}`,
		`{"events":[` + strings.Replace(string(item), event.EventID.String(), strings.ToUpper(event.EventID.String()), 1) + `]}`,
		`{"events":[` + strings.Replace(string(item), `"payload":{"scope":"local"}`, `"payload":null`, 1) + `]}`,
		`{"events":[` + strings.Replace(string(item), `"schema_version":2`, `"schema_version":2.0`, 1) + `]}`,
	} {
		if _, err := DecodeCatalogBatch([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid batch prefix: %.200s", raw)
		}
	}
	tooMany := []byte(`{"events":[` + strings.TrimSuffix(strings.Repeat(string(item)+",", MaxBatch+1), ",") + `]}`)
	if _, err := DecodeCatalogBatch(tooMany); err == nil {
		t.Fatal("accepted oversized event count")
	}
	// Unknown type and schema remain explicit per-event failures in the service,
	// rather than an ambiguous decoder or a legacy accepted/ignored fallback.
	event.Type, event.SchemaVersion = "catalog.target", 1
	item, _ = json.Marshal(event)
	if _, err := DecodeCatalogBatch([]byte(`{"events":[` + string(item) + `]}`)); err != nil {
		t.Fatalf("typed envelope could not reach explicit service failure: %v", err)
	}
}
