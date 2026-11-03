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
	if err := end.Manifest.Validate(); err != nil {
		return end, err
	}
	if end.PageCount > catalog.MaxPages || end.DocumentCount > catalog.MaxDocuments {
		return end, fmt.Errorf("catalog: manifest exceeds pending quota")
	}
	return end, nil
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

type catalogDocument struct {
	Video            VideoDoc `json:"video"`
	Eligible         bool     `json:"eligible"`
	SuppressedReason *string  `json:"suppressed_reason"`
}

type catalogPage struct {
	catalog.Identity
	catalog.PageDigest
	Documents          []catalogDocument
	CanonicalDocuments []byte
}

func parseCatalogPage(raw []byte) (catalogPage, error) {
	var page catalogPage
	if len(raw) > catalog.MaxRequestBytes {
		return page, fmt.Errorf("catalog: page exceeds request byte budget")
	}
	fields, err := catalog.ObjectFields(raw, []string{"scope", "epoch", "generation",
		"page_index", "documents", "content_sha256"}, nil)
	if err != nil {
		return page, err
	}
	var wire struct {
		catalog.Identity
		catalog.PageDigest
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return page, err
	}
	if err := validateCatalogIdentity(wire.Identity, fields); err != nil {
		return page, err
	}
	if wire.PageIndex < 0 || wire.PageIndex >= catalog.MaxPages {
		return page, fmt.Errorf("catalog: page index outside pending quota")
	}
	var documents []json.RawMessage
	if err := json.Unmarshal(fields["documents"], &documents); err != nil || len(documents) == 0 || len(documents) > catalog.MaxDocuments {
		return page, fmt.Errorf("catalog: page requires nonempty documents")
	}
	seen := make(map[uuid.UUID]bool, len(documents))
	for _, rawDocument := range documents {
		documentFields, err := catalog.ObjectFields(rawDocument,
			[]string{"video", "eligible", "suppressed_reason"}, []string{"suppressed_reason"})
		if err != nil {
			return page, err
		}
		videoFields, err := catalog.ObjectFields(documentFields["video"], []string{
			"id", "kind", "channel_id", "channel_handle", "channel_name", "owner_id", "title", "description",
			"tags", "is_sensitive", "privacy", "state", "created_at", "updated_at", "views", "likes",
		}, []string{"channel_id", "channel_handle", "channel_name", "owner_id", "tags", "created_at", "updated_at"},
			"category", "language", "license", "duration_seconds", "published_at")
		if err != nil {
			return page, err
		}
		var doc catalogDocument
		if err := json.Unmarshal(rawDocument, &doc); err != nil {
			return page, err // Typed integral fields reject exponent/fraction forms.
		}
		v := doc.Video
		if !doc.Eligible || doc.SuppressedReason != nil || v.Kind != wire.Scope || v.Privacy != "public" || v.State != "published" || !canonicalCatalogUUID(videoFields["id"], v.ID) || seen[v.ID] {
			return page, fmt.Errorf("catalog: invalid canonical eligibility or membership")
		}
		if v.Views < 0 || v.Likes < 0 || v.DurationSeconds != nil && *v.DurationSeconds < 0 || v.CreatedAt == nil && v.UpdatedAt == nil || v.CreatedAt != nil && v.CreatedAt.IsZero() || v.UpdatedAt != nil && v.UpdatedAt.IsZero() {
			return page, fmt.Errorf("catalog: invalid counters or source timestamp")
		}
		for key, id := range map[string]*uuid.UUID{"channel_id": v.ChannelID, "owner_id": v.OwnerID} {
			if id != nil && !canonicalCatalogUUID(videoFields[key], *id) {
				return page, fmt.Errorf("catalog: invalid ownership identifier")
			}
		}
		seen[v.ID] = true
		page.Documents = append(page.Documents, doc)
	}
	canonical, digest, err := catalog.DocumentsDigest(fields["documents"])
	if err != nil || digest != wire.ContentSHA256 {
		return page, fmt.Errorf("catalog: document content digest mismatch")
	}
	page.Identity, page.PageDigest, page.CanonicalDocuments = wire.Identity, wire.PageDigest, canonical
	return page, nil
}
