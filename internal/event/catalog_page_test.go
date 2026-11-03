package event

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/catalog"
)

func TestCatalogPageValidatesEachCanonicalDocumentBeforeHashing(t *testing.T) {
	doc := VideoDoc{ID: uuid.New(), Kind: "local", Title: "<title>", Privacy: "public", State: "published"}
	var fields map[string]any
	raw, _ := json.Marshal(doc)
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["created_at"] = "2026-11-03T12:00:00Z"
	fields["views"] = json.Number("9007199254740993")
	validVideo, _ := json.Marshal(fields)
	wrap := func(videos ...string) []byte {
		t.Helper()
		docs := make([]json.RawMessage, len(videos))
		for i, video := range videos {
			docs[i] = json.RawMessage(`{"video":` + video + `,"eligible":true,"suppressed_reason":null}`)
		}
		documents, _ := json.Marshal(docs)
		_, digest, err := catalog.DocumentsDigest(documents)
		if err != nil {
			t.Fatal(err)
		}
		payload := map[string]any{"scope": "local", "epoch": "c9895c40-13af-4aa9-bf07-9ca761b7e172",
			"generation": "1", "page_index": 0, "documents": json.RawMessage(documents), "content_sha256": digest}
		out, _ := json.Marshal(payload)
		return out
	}
	page := wrap(string(validVideo))
	got, err := parseCatalogPage(page)
	if err != nil || len(got.Documents) != 1 || got.Documents[0].Video.Views != 9007199254740993 {
		t.Fatalf("valid canonical page = %+v, %v", got, err)
	}
	for _, mutate := range []func(map[string]any){
		func(v map[string]any) { v["kind"] = "remote" },
		func(v map[string]any) { v["privacy"] = "private" },
		func(v map[string]any) { v["state"] = "draft" },
		func(v map[string]any) { v["views"] = json.Number("1e0") },
		func(v map[string]any) { v["likes"] = -1 },
		func(v map[string]any) { v["created_at"] = nil },
		func(v map[string]any) { v["ID"] = v["id"] },
		func(v map[string]any) { delete(v, "is_sensitive") },
	} {
		var changed map[string]any
		_ = json.Unmarshal(validVideo, &changed)
		mutate(changed)
		v, _ := json.Marshal(changed)
		if _, err := parseCatalogPage(wrap(string(v))); err == nil {
			t.Errorf("accepted invalid document %s", v)
		}
	}
	for _, bad := range [][]byte{
		wrap(), wrap(string(validVideo), string(validVideo)),
		wrap(strings.Replace(string(validVideo), `"title":`, `"title":"duplicate","title":`, 1)),
		[]byte(strings.Replace(string(page), `"eligible":true`, `"eligible":false`, 1)),
		[]byte(strings.Replace(string(page), `"suppressed_reason":null`, `"suppressed_reason":"hidden"`, 1)),
		[]byte(strings.Replace(string(page), `"page_index":0`, `"page_index":-1`, 1)),
		[]byte(strings.Replace(string(page), got.ContentSHA256, strings.Repeat("0", 64), 1)),
	} {
		if _, err := parseCatalogPage(bad); err == nil {
			t.Errorf("accepted invalid page %s", bad)
		}
	}
}
