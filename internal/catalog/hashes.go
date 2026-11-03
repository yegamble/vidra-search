package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// DocumentsDigest canonicalizes the raw array, not a typed struct whose field
// order or omitted optional fields could differ between producer and receiver.
// Business validation is separate: [] has a hash but is not a valid page.
func DocumentsDigest(raw []byte) ([]byte, string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber() // Catalog counters must not round through float64.
	var documents []any
	if err := decoder.Decode(&documents); err != nil || documents == nil {
		return nil, "", fmt.Errorf("catalog: documents must be a JSON array")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, "", fmt.Errorf("catalog: unexpected JSON after documents")
	}
	canonical, err := json.Marshal(documents)
	if err != nil {
		return nil, "", fmt.Errorf("catalog: encode canonical documents: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return canonical, hex.EncodeToString(digest[:]), nil
}

// PageDigest identifies one immutable page's canonical document content.
type PageDigest struct {
	PageIndex     int32  `json:"page_index"`
	ContentSHA256 string `json:"content_sha256"`
}

// PagesDigest hashes the exact contiguous page sequence while preserving the
// caller's arrival order. A count alone cannot prove which content completed.
func PagesDigest(pages []PageDigest) (string, error) {
	ordered := append([]PageDigest(nil), pages...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PageIndex < ordered[j].PageIndex })
	digest := sha256.New()
	for i, page := range ordered {
		if page.PageIndex != int32(i) || len(page.ContentSHA256) != sha256.Size*2 {
			return "", fmt.Errorf("catalog: incomplete or invalid page sequence")
		}
		if _, err := hex.DecodeString(page.ContentSHA256); err != nil || strings.ToLower(page.ContentSHA256) != page.ContentSHA256 {
			return "", fmt.Errorf("catalog: invalid canonical page digest")
		}
		_, _ = fmt.Fprintf(digest, "%d:%s\n", page.PageIndex, page.ContentSHA256)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
