package normalize

import "testing"

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain lowercase unchanged", "hello world", "hello world"},
		{"uppercase folds", "HeLLo WORLD", "hello world"},
		{"leading/trailing whitespace trimmed", "  hello  ", "hello"},
		{"internal whitespace collapsed", "a\t\t b\n\nc", "a b c"},
		{"full-width latin to ascii", "ＡＢＣ", "abc"},
		{"full-width digits and space", "Ｈｅｌｌｏ　Ｗｏｒｌｄ", "hello world"},
		{"fi ligature decomposes", "ﬁle", "file"},
		{"german sharp s folds to ss", "Straße", "strasse"},
		{"cjk preserved", "日本語 の 動画", "日本語 の 動画"},
		{"cjk case-neutral preserved with fold", "東京タワー", "東京タワー"},
		{"mixed case with accents", "CaFÉ Del Mar", "café del mar"},
		{"tab and newline runs collapse", "one\t\ntwo   three", "one two three"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.in); got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeIdempotent(t *testing.T) {
	inputs := []string{"ＨＥＬＬＯ  World", "Straße  ﬁ", "  日本語 ", "CaFÉ"}
	for _, in := range inputs {
		once := Normalize(in)
		twice := Normalize(once)
		if once != twice {
			t.Fatalf("not idempotent for %q: once=%q twice=%q", in, once, twice)
		}
	}
}
