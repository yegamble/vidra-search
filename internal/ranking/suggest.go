// Package ranking holds the pure, deterministic scoring used by suggestions and
// simple search. It has no I/O dependencies so the blend logic and score
// formulas are exhaustively unit-testable with fixed fixtures.
package ranking

import (
	"sort"
	"strings"

	"github.com/vidra/vidra-search/internal/normalize"
)

// Weights are the suggestion blend coefficients (algorithms report start
// values, mapping to pop, prefix-quality, personal, doc-popularity, and a
// length penalty). Tunable in one place.
type Weights struct {
	Pop  float64 // global query popularity (decayed_freq) — aggregate stream
	Pfx  float64 // prefix quality (exact vs fuzzy)
	Pers float64 // personal-history boost
	Doc  float64 // document popularity (views/count) — doc-derived stream
	Len  float64 // length penalty
}

// DefaultWeights is the W1 blend. The Pop/Pers terms are inert in simple mode
// (no aggregate/history streams yet) and become live in W2.
var DefaultWeights = Weights{Pop: 1.0, Pfx: 0.6, Pers: 0.8, Doc: 0.4, Len: 0.1}

// Source identifies which candidate stream a suggestion came from, so its
// popularity is normalized against the right peer group.
type Source int

const (
	SourceQuery   Source = iota // query_aggregates (global popularity)
	SourceDoc                   // documents (titles/channels/tags)
	SourceHistory               // personal history
)

// Kind is the suggestion type surfaced to the client (matches the API enum).
type Kind string

const (
	KindQuery   Kind = "query"
	KindVideo   Kind = "video"
	KindChannel Kind = "channel"
	KindTag     Kind = "tag"
	KindHistory Kind = "history"
)

// Candidate is one suggestion proposed by a stream, before blending.
type Candidate struct {
	Text          string
	Kind          Kind
	Source        Source
	VideoID       string
	ChannelHandle string
	IsPersonal    bool
	// ExactPrefix is true when the candidate matched the query as an anchored
	// prefix; false for a trigram (typo) fuzzy match.
	ExactPrefix bool
	// Popularity is the raw stream-specific signal: decayed_freq for query
	// candidates, view/use counts for doc/history candidates.
	Popularity float64
}

// Suggestion is a blended, client-facing suggestion.
type Suggestion struct {
	Text          string `json:"text"`
	Type          Kind   `json:"type"`
	VideoID       string `json:"video_id,omitempty"`
	ChannelHandle string `json:"channel_handle,omitempty"`
	IsPersonal    bool   `json:"is_personal"`
}

// scoredCandidate pairs a candidate with its blended score for ranking.
type scoredCandidate struct {
	cand  Candidate
	score float64
}

// Blend scores, dedupes, and orders candidates into at most limit suggestions.
// It rank-normalizes each stream's popularity independently, applies the blend
// weights, dedupes case-insensitively (preferring exact-prefix over fuzzy), and
// reserves at least one slot each for a history and a doc-derived suggestion
// when the streams provide them. The final order is fully deterministic.
func Blend(candidates []Candidate, limit int, w Weights) []Suggestion {
	if limit <= 0 || len(candidates) == 0 {
		return nil
	}

	// Per-source max popularity for normalization to [0,1].
	maxPop := map[Source]float64{}
	for _, c := range candidates {
		if c.Popularity > maxPop[c.Source] {
			maxPop[c.Source] = c.Popularity
		}
	}

	// Dedupe case-insensitively; keep the best-scoring variant, preferring an
	// exact-prefix match over a fuzzy one on ties.
	best := map[string]scoredCandidate{}
	for _, c := range candidates {
		key := normalize.Normalize(c.Text)
		if key == "" {
			continue
		}
		s := blendScore(c, maxPop, w)
		cur, ok := best[key]
		if !ok || better(scoredCandidate{c, s}, cur) {
			// Preserve a personal flag if any duplicate was personal.
			if ok && cur.cand.IsPersonal {
				c.IsPersonal = true
			}
			best[key] = scoredCandidate{c, s}
		} else if c.IsPersonal {
			cur.cand.IsPersonal = true
			best[key] = cur
		}
	}

	ranked := make([]scoredCandidate, 0, len(best))
	for _, s := range best {
		ranked = append(ranked, s)
	}
	sort.SliceStable(ranked, func(i, j int) bool { return better(ranked[i], ranked[j]) })

	picked := reserveSlots(ranked, limit)

	out := make([]Suggestion, 0, len(picked))
	for _, s := range picked {
		out = append(out, Suggestion{
			Text:          s.cand.Text,
			Type:          s.cand.Kind,
			VideoID:       s.cand.VideoID,
			ChannelHandle: s.cand.ChannelHandle,
			IsPersonal:    s.cand.IsPersonal,
		})
	}
	return out
}

// blendScore applies the weighted blend to one candidate.
func blendScore(c Candidate, maxPop map[Source]float64, w Weights) float64 {
	norm := func(src Source) float64 {
		if m := maxPop[src]; m > 0 {
			return c.Popularity / m
		}
		return 0
	}
	prefixQuality := 0.5 // fuzzy
	if c.ExactPrefix {
		prefixQuality = 1.0
	}
	personal := 0.0
	if c.IsPersonal {
		personal = 1.0
	}
	var pop, doc float64
	switch c.Source {
	case SourceQuery:
		pop = norm(SourceQuery)
	case SourceDoc:
		doc = norm(SourceDoc)
	case SourceHistory:
		// History popularity contributes via the personal term; its recency is
		// carried by the query-side ordering upstream.
	}
	lenPenalty := float64(len([]rune(c.Text))) / 50.0
	if lenPenalty > 1 {
		lenPenalty = 1
	}
	return w.Pop*pop + w.Pfx*prefixQuality + w.Pers*personal + w.Doc*doc - w.Len*lenPenalty
}

// better is the total order used both for dedupe and final ranking. Exact-prefix
// matches form a hard tier ABOVE fuzzy (typo-fallback) matches — a fuzzy result
// never outranks an exact-prefix one however popular it is — then blended score,
// then lexical text order for a fully deterministic result.
func better(a, b scoredCandidate) bool {
	if a.cand.ExactPrefix != b.cand.ExactPrefix {
		return a.cand.ExactPrefix
	}
	if a.score != b.score {
		return a.score > b.score
	}
	return strings.ToLower(a.cand.Text) < strings.ToLower(b.cand.Text)
}

// reserveSlots takes the top `limit` but guarantees at least one history and one
// doc-derived suggestion when available, by promoting the best of a missing
// class over the weakest already-selected item of another class.
func reserveSlots(ranked []scoredCandidate, limit int) []scoredCandidate {
	if len(ranked) <= limit {
		return ranked
	}
	picked := append([]scoredCandidate(nil), ranked[:limit]...)

	ensure := func(match func(Candidate) bool) {
		for _, p := range picked {
			if match(p.cand) {
				return // class already represented
			}
		}
		// Find the best candidate of the class outside the picked window
		// (ranked is sorted, so the first match is the best).
		var promote *scoredCandidate
		for i := limit; i < len(ranked); i++ {
			if match(ranked[i].cand) {
				promote = &ranked[i]
				break
			}
		}
		if promote == nil {
			return // class not available at all
		}
		// Evict the weakest picked item NOT of the class we are promoting.
		for i := len(picked) - 1; i >= 0; i-- {
			if !match(picked[i].cand) {
				picked[i] = *promote
				break
			}
		}
	}
	ensure(func(c Candidate) bool { return c.Source == SourceHistory })
	ensure(func(c Candidate) bool { return c.Source == SourceDoc })

	// Re-sort so promoted items land in their scored position.
	sort.SliceStable(picked, func(i, j int) bool { return better(picked[i], picked[j]) })
	return picked
}
