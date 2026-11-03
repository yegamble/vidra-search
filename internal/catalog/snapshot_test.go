package catalog

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestSnapshotProofRequiresExactMembershipContentIndexesAndByteLedger(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	page := func(index int32, id uuid.UUID, title string) SnapshotPage {
		t.Helper()
		raw, _ := json.Marshal([]map[string]any{{"video": map[string]any{"id": id, "kind": "local", "title": title},
			"eligible": true, "suppressed_reason": nil}})
		canonical, digest, err := DocumentsDigest(raw)
		if err != nil {
			t.Fatal(err)
		}
		return SnapshotPage{PageDigest: PageDigest{PageIndex: index, ContentSHA256: digest}, Documents: canonical, DocumentCount: 1}
	}
	p0, p1 := page(0, a, "first"), page(1, b, "second")
	membership, _ := MembershipDigest([]uuid.UUID{a, b})
	content, _ := PagesDigest([]PageDigest{p0.PageDigest, p1.PageDigest})
	manifest := Manifest{PageCount: 2, DocumentCount: 2, MembershipSHA256: membership, PagesSHA256: content}
	charged := int64(len(p0.Documents) + len(p1.Documents))
	if err := VerifySnapshot(manifest, []SnapshotPage{p1, p0}, charged); err != nil {
		t.Fatalf("reordered complete pages: %v", err)
	}
	wrongID := page(1, c, "second")
	duplicate := page(1, a, "first again")
	for name, replacement := range map[string]SnapshotPage{"different ID same count": wrongID, "duplicate ID": duplicate} {
		changed := manifest
		changed.PagesSHA256, _ = PagesDigest([]PageDigest{p0.PageDigest, replacement.PageDigest})
		if err := VerifySnapshot(changed, []SnapshotPage{p0, replacement}, int64(len(p0.Documents)+len(replacement.Documents))); err == nil {
			t.Errorf("accepted %s with correct counts/content but false membership", name)
		}
	}
	tampered := p1
	tampered.Documents = page(1, b, "altered title").Documents
	wrongCount := p1
	wrongCount.DocumentCount = 2
	gap := p1
	gap.PageIndex = 2
	for name, pages := range map[string][]SnapshotPage{
		"missing page": {p0}, "content changed": {p0, tampered},
		"row count mismatch": {p0, wrongCount}, "index gap": {p0, gap},
	} {
		if err := VerifySnapshot(manifest, pages, charged); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
	for _, bytes := range []int64{-1, charged - 1, charged + 1, MaxSnapshotBytes + 1} {
		if err := VerifySnapshot(manifest, []SnapshotPage{p0, p1}, bytes); err == nil {
			t.Errorf("accepted byte ledger %d", bytes)
		}
	}
	changed := manifest
	changed.PagesSHA256 = membership
	if err := VerifySnapshot(changed, []SnapshotPage{p0, p1}, charged); err == nil {
		t.Error("accepted a false aggregate page digest")
	}
	const empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if err := VerifySnapshot(Manifest{MembershipSHA256: empty, PagesSHA256: empty}, nil, 0); err != nil {
		t.Fatalf("valid empty full proof: %v", err)
	}
}
