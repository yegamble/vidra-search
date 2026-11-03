package catalog

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
)

func TestSharedCanonicalHashVectors(t *testing.T) {
	// This fixture is shared byte-for-byte with vidra-core. It is a hash-format
	// oracle, not a set of complete, business-valid snapshot requests.
	raw, err := os.ReadFile("testdata/catalog_hash_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Documents []struct {
			Name      string `json:"name"`
			Raw       string `json:"raw_json"`
			Canonical string `json:"canonical_json"`
			SHA256    string `json:"sha256"`
		} `json:"document_vectors"`
		Pages struct {
			Items  []PageDigest `json:"pages"`
			SHA256 string       `json:"pages_sha256"`
		} `json:"page_aggregate"`
		Membership struct {
			IDs    []uuid.UUID `json:"sorted_ids"`
			SHA256 string      `json:"membership_sha256"`
		} `json:"membership"`
		Empty struct {
			SHA256 string `json:"pages_sha256"`
		} `json:"empty_manifest"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Documents) != 5 {
		t.Fatal("shared hash fixture is incomplete")
	}
	for _, v := range fixture.Documents {
		t.Run(v.Name, func(t *testing.T) {
			canonical, digest, err := DocumentsDigest([]byte(v.Raw))
			if err != nil || string(canonical) != v.Canonical || digest != v.SHA256 {
				t.Fatalf("canonical/digest mismatch: %s / %s, %v", canonical, digest, err)
			}
		})
	}
	got, err := PagesDigest(fixture.Pages.Items)
	if err != nil || got != fixture.Pages.SHA256 || fixture.Pages.Items[0].PageIndex != 1 {
		t.Fatalf("page digest/order mismatch: %s, %v", got, err)
	}
	got, err = PagesDigest(nil)
	if err != nil || got != fixture.Empty.SHA256 {
		t.Fatalf("empty manifest digest = %s, %v", got, err)
	}
	got, err = MembershipDigest(fixture.Membership.IDs)
	if err != nil || got != fixture.Membership.SHA256 {
		t.Fatalf("membership digest = %s, %v", got, err)
	}
}

func TestCanonicalHashesRejectAmbiguousFramingAndIncompletePages(t *testing.T) {
	for _, raw := range []string{``, `null`, `{}`, `true`, `[] []`, `[] trailing`, `[`} {
		if _, _, err := DocumentsDigest([]byte(raw)); err == nil {
			t.Errorf("accepted invalid document array %q", raw)
		}
	}
	const hash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for _, pages := range [][]PageDigest{
		{{PageIndex: -1, ContentSHA256: hash}},
		{{PageIndex: 1, ContentSHA256: hash}},
		{{PageIndex: 0, ContentSHA256: hash}, {PageIndex: 0, ContentSHA256: hash}},
		{{PageIndex: 0, ContentSHA256: "not-a-hash"}},
		{{PageIndex: 0, ContentSHA256: "E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855"}},
	} {
		if _, err := PagesDigest(pages); err == nil {
			t.Errorf("accepted invalid page manifest %+v", pages)
		}
	}
}
