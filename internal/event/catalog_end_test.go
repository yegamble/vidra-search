package event

import (
	"encoding/json"
	"strings"
	"testing"
)

const validCatalogEnd = `{"scope":"local","epoch":"c9895c40-13af-4aa9-bf07-9ca761b7e172","generation":"9007199254740993","page_count":0,"document_count":0,"membership_sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","pages_sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}`

func TestCatalogEndRequiresExactFenceAndExplicitManifest(t *testing.T) {
	got, err := parseCatalogEnd([]byte(validCatalogEnd))
	if err != nil || int64(got.Generation) != 9007199254740993 || got.PageCount != 0 {
		t.Fatalf("valid empty full = %+v, %v", got, err)
	}
	bounded := strings.Replace(strings.Replace(validCatalogEnd, `"page_count":0`, `"page_count":1024`, 1), `"document_count":0`, `"document_count":100000`, 1)
	if _, err := parseCatalogEnd([]byte(bounded)); err != nil {
		t.Fatal(err)
	}
	var valid map[string]json.RawMessage
	if err := json.Unmarshal([]byte(validCatalogEnd), &valid); err != nil {
		t.Fatal(err)
	}
	for key := range valid {
		fields := make(map[string]json.RawMessage, len(valid))
		for k, v := range valid {
			fields[k] = v
		}
		delete(fields, key)
		raw, _ := json.Marshal(fields)
		if _, err := parseCatalogEnd(raw); err == nil {
			t.Errorf("accepted missing field %s", key)
		}
		fields[key] = json.RawMessage("null")
		raw, _ = json.Marshal(fields)
		if _, err := parseCatalogEnd(raw); err == nil {
			t.Errorf("accepted null field %s", key)
		}
	}
	for _, raw := range []string{
		strings.Replace(validCatalogEnd, `"local"`, `"remote"`, 1),
		strings.Replace(validCatalogEnd, `c9895c40-13af-4aa9-bf07-9ca761b7e172`, `C9895C40-13AF-4AA9-BF07-9CA761B7E172`, 1),
		strings.Replace(validCatalogEnd, `c9895c40-13af-4aa9-bf07-9ca761b7e172`, `00000000-0000-0000-0000-000000000000`, 1),
		strings.Replace(validCatalogEnd, `"9007199254740993"`, `9007199254740993`, 1),
		strings.Replace(validCatalogEnd, `"page_count":0`, `"page_count":1.0`, 1),
		strings.Replace(validCatalogEnd, `"page_count":0`, `"page_count":1e0`, 1),
		strings.Replace(validCatalogEnd, `"page_count":0`, `"page_count":1`, 1),
		strings.Replace(validCatalogEnd, `"page_count":0`, `"page_count":-1`, 1),
		strings.Replace(bounded, `"page_count":1024`, `"page_count":1025`, 1),
		strings.Replace(bounded, `"document_count":100000`, `"document_count":100001`, 1),
		strings.Replace(validCatalogEnd, `"scope":"local"`, `"scope":"local","Scope":"local"`, 1),
		strings.Replace(validCatalogEnd, `"scope":"local"`, `"scope":"local","scope":"local"`, 1),
		validCatalogEnd + `{}`,
	} {
		if _, err := parseCatalogEnd([]byte(raw)); err == nil {
			t.Errorf("accepted malformed end %s", raw)
		}
	}
}
