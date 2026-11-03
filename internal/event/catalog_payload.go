package event

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/catalog"
)

type catalogEnd struct {
	catalog.Identity
	catalog.Manifest
}

func parseCatalogEnd(raw []byte) (catalogEnd, error) {
	fields, err := catalog.ObjectFields(raw, []string{"scope", "epoch", "generation",
		"page_count", "document_count", "membership_sha256", "pages_sha256"}, nil)
	var end catalogEnd
	if err != nil {
		return end, err
	}
	if err := json.Unmarshal(raw, &end); err != nil {
		return end, err
	}
	if err := validateCatalogIdentity(end.Identity, fields); err != nil {
		return end, err
	}
	return end, end.Manifest.Validate()
}

func validateCatalogIdentity(id catalog.Identity, fields map[string]json.RawMessage) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if !canonicalCatalogUUID(fields["epoch"], id.Epoch) {
		return fmt.Errorf("catalog: noncanonical epoch")
	}
	return nil
}

func canonicalCatalogUUID(raw []byte, id uuid.UUID) bool {
	var value string
	return id != uuid.Nil && json.Unmarshal(raw, &value) == nil && value == id.String()
}
