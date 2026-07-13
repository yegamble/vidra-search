package index

import "testing"

func TestEligible(t *testing.T) {
	cases := []struct {
		privacy, state string
		wantEligible   bool
		wantReason     string
	}{
		{"public", "published", true, ""},
		{"private", "published", false, "privacy_private"},
		{"unlisted", "published", false, "privacy_unlisted"},
		{"password", "published", false, "privacy_password"},
		{"public", "draft", false, "state_draft"},
		{"public", "quarantined", false, "state_quarantined"},
		{"public", "processing", false, "state_processing"},
		{"", "", false, "privacy_unknown"},
	}
	for _, tc := range cases {
		gotEligible, gotReason := Eligible(tc.privacy, tc.state)
		if gotEligible != tc.wantEligible || gotReason != tc.wantReason {
			t.Errorf("Eligible(%q,%q) = (%v,%q), want (%v,%q)",
				tc.privacy, tc.state, gotEligible, gotReason, tc.wantEligible, tc.wantReason)
		}
	}
}
