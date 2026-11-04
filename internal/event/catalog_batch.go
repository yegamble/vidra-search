package event

import (
	"encoding/json"
	"fmt"

	"github.com/vidra/vidra-search/internal/catalog"
)

// DecodeCatalogBatch validates exact framing before typed decoding can erase
// duplicate/case-aliased keys. The byte budget includes every envelope and space.
// Per-event schema/type/business failures remain isolated by IngestCatalog.
func DecodeCatalogBatch(raw []byte) ([]Envelope, error) {
	if len(raw) > catalog.MaxRequestBytes {
		return nil, fmt.Errorf("catalog: batch exceeds request byte budget")
	}
	fields, err := catalog.ObjectFields(raw, []string{"events"}, nil)
	if err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if err := json.Unmarshal(fields["events"], &items); err != nil || len(items) == 0 || len(items) > MaxBatch {
		return nil, fmt.Errorf("catalog: batch requires 1..%d events", MaxBatch)
	}
	events := make([]Envelope, len(items))
	for i, item := range items {
		fields, err := catalog.ObjectFields(item,
			[]string{"event_id", "type", "occurred_at", "schema_version", "payload"}, nil)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(item, &events[i]); err != nil {
			return nil, err
		}
		if !canonicalCatalogUUID(fields["event_id"], events[i].EventID) {
			return nil, fmt.Errorf("catalog: noncanonical event identity")
		}
	}
	return events, nil
}
