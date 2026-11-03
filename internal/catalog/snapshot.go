package catalog

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// SnapshotPage is persisted evidence, not a wire decoder. Its documents must
// first pass the business validator; this helper verifies content and membership.
type SnapshotPage struct {
	PageDigest
	Documents     json.RawMessage
	DocumentCount int32
}

// VerifySnapshot verifies an entire frozen full, including exact membership.
// It does not check epoch/admission or mutate projection/completion authority.
func VerifySnapshot(manifest Manifest, pages []SnapshotPage, canonicalBytes int64) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	if manifest.PageCount > MaxPages || manifest.DocumentCount > MaxDocuments || len(pages) != int(manifest.PageCount) || canonicalBytes < 0 || canonicalBytes > MaxSnapshotBytes {
		return fmt.Errorf("catalog: incomplete or over-budget snapshot")
	}
	var ids []uuid.UUID
	var digests []PageDigest
	var charged int64
	for _, page := range pages {
		canonical, digest, err := DocumentsDigest(page.Documents)
		if err != nil || digest != page.ContentSHA256 {
			return fmt.Errorf("catalog: persisted page content mismatch")
		}
		var members []struct {
			Video struct {
				ID uuid.UUID `json:"id"`
			} `json:"video"`
		}
		if err := json.Unmarshal(canonical, &members); err != nil || len(members) == 0 || int64(len(members)) != int64(page.DocumentCount) {
			return fmt.Errorf("catalog: persisted page count mismatch")
		}
		for _, member := range members {
			ids = append(ids, member.Video.ID)
		}
		charged += int64(len(canonical))
		if charged > MaxSnapshotBytes || len(ids) > MaxDocuments {
			return fmt.Errorf("catalog: persisted snapshot exceeds quota")
		}
		digests = append(digests, page.PageDigest)
	}
	if charged != canonicalBytes || int64(len(ids)) != manifest.DocumentCount {
		return fmt.Errorf("catalog: snapshot count or canonical-byte ledger mismatch")
	}
	membership, err := MembershipDigest(ids)
	if err != nil || membership != manifest.MembershipSHA256 {
		return fmt.Errorf("catalog: snapshot membership mismatch")
	}
	content, err := PagesDigest(digests)
	if err != nil || content != manifest.PagesSHA256 {
		return fmt.Errorf("catalog: snapshot page sequence mismatch")
	}
	return nil
}
