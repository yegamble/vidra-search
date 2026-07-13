// Package normalize provides the single query/text normalization function used
// EVERYWHERE in vidra-search — at event ingest (document titles/tags are matched
// case-insensitively), at suggestion time, and at search time. Normalizing in
// exactly one place guarantees a query and the corpus it is matched against are
// folded identically, so "CAFÉ", "café", and the full-width "ｃａｆé" all collapse
// to the same key.
//
// The transform is, in order:
//  1. Unicode NFKC — compatibility composition folds full-width forms, ligatures
//     (ﬁ→fi), and other compatibility variants to a canonical shape.
//  2. Full Unicode case folding (x/text/cases.Fold) — a locale-independent,
//     more thorough lowercase (e.g. ß→ss, İ handled) suited to matching.
//  3. Whitespace collapse — runs of any Unicode whitespace become a single
//     ASCII space, with leading/trailing space trimmed.
package normalize

import (
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// folder is stateless and safe for concurrent use across goroutines.
var folder = cases.Fold()

// Normalize returns the canonical, case-folded, whitespace-collapsed form of s.
// It is idempotent: Normalize(Normalize(x)) == Normalize(x).
func Normalize(s string) string {
	s = norm.NFKC.String(s)
	s = folder.String(s)
	// strings.Fields splits on runs of Unicode whitespace and drops empty
	// tokens, giving both collapse and trim in one pass.
	return strings.Join(strings.Fields(s), " ")
}
