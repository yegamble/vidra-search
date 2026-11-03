package catalog

import (
	"strings"
	"testing"
)

func TestManifestRequiresConsistentCountsAndCanonicalDigests(t *testing.T) {
	const empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for _, m := range []Manifest{
		{MembershipSHA256: empty, PagesSHA256: empty},
		{PageCount: 1, DocumentCount: 2, MembershipSHA256: strings.Repeat("a", 64), PagesSHA256: empty},
	} {
		if err := m.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []Manifest{
		{}, {PageCount: -1}, {DocumentCount: -1},
		{PageCount: 2, DocumentCount: 1, MembershipSHA256: empty, PagesSHA256: empty},
		{PageCount: 1, MembershipSHA256: empty, PagesSHA256: empty},
		{DocumentCount: 1, MembershipSHA256: empty, PagesSHA256: empty},
		{MembershipSHA256: strings.Repeat("a", 64), PagesSHA256: empty},
		{PageCount: 1, DocumentCount: 1, MembershipSHA256: strings.ToUpper(empty), PagesSHA256: empty},
		{PageCount: 1, DocumentCount: 1, MembershipSHA256: empty, PagesSHA256: "invalid"},
	} {
		if err := m.Validate(); err == nil {
			t.Errorf("accepted invalid manifest %+v", m)
		}
	}
}

func TestObjectFieldsRejectsMissingUnknownDuplicateAndCaseAliases(t *testing.T) {
	keys := []string{"count", "optional"}
	for _, raw := range []string{`{"count":0,"optional":null}`, `{"optional":"value","count":0}`} {
		if _, err := ObjectFields([]byte(raw), keys, []string{"optional"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{`{"count":0,"optional":null}`, `{"count":0,"optional":null,"omitted":null}`} {
		if _, err := ObjectFields([]byte(raw), keys, []string{"optional"}, "omitted"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ObjectFields([]byte(`{"count":0,"optional":null,"omitted":null,"omitted":null}`), keys, []string{"optional"}, "omitted"); err == nil {
		t.Error("accepted duplicate optional field")
	}
	for _, raw := range []string{
		`null`, `[]`, `{}`, `{"count":null,"optional":null}`, `{"count":0}`,
		`{"count":0,"optional":null,"extra":1}`, `{"count":0,"Count":0,"optional":null}`,
		`{"count":0,"count":0,"optional":null}`, `{"count":0,"c\u006funt":0,"optional":null}`,
		`{"count":0,"optional":null} {}`, `{"count":0,"optional":null`,
	} {
		if _, err := ObjectFields([]byte(raw), keys, []string{"optional"}); err == nil {
			t.Errorf("accepted ambiguous object %s", raw)
		}
	}
}
