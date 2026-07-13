// Package suggest implements the autosuggest pipeline (§1.6) for
// GET /internal/v1/suggestions. It normalizes the prefix, gathers doc-derived
// candidate streams (title/channel/tag), consults an aggregate-query stream
// (a no-op seam in W1, swapped for query_aggregates in W2), falls back to a
// trigram typo match when exact prefixes are scarce, then blends and dedupes via
// internal/ranking. It NEVER returns a 5xx: any internal trouble degrades to an
// empty suggestion list.
package suggest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vidra/vidra-search/internal/normalize"
	"github.com/vidra/vidra-search/internal/ranking"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// ModelVersion identifies the W1 heuristic blend.
const ModelVersion = "heuristic-v1"

const (
	defaultLimit = 10
	maxLimit     = 20
	streamLimit  = 15
	cacheTTL     = 60 * time.Second
	// cachePrefixMax is the longest prefix cached in Redis (hot, high-fanout
	// short prefixes only).
	cachePrefixMax = 3
)

// Querier is the store surface the suggestion pipeline reads.
type Querier interface {
	SuggestTitlePrefix(ctx context.Context, arg sqlcgen.SuggestTitlePrefixParams) ([]sqlcgen.SuggestTitlePrefixRow, error)
	SuggestChannelPrefix(ctx context.Context, arg sqlcgen.SuggestChannelPrefixParams) ([]sqlcgen.SuggestChannelPrefixRow, error)
	SuggestTagPrefix(ctx context.Context, arg sqlcgen.SuggestTagPrefixParams) ([]sqlcgen.SuggestTagPrefixRow, error)
	SuggestTitleFuzzy(ctx context.Context, arg sqlcgen.SuggestTitleFuzzyParams) ([]sqlcgen.SuggestTitleFuzzyRow, error)
}

// AggregateSuggester is the global-query-popularity stream. W1 ships a no-op
// implementation (NoopAggregate); W2 replaces it with a query_aggregates-backed
// reader without touching the pipeline.
type AggregateSuggester interface {
	Suggest(ctx context.Context, normalizedPrefix string, hideSensitive bool, limit int) ([]ranking.Candidate, error)
}

// NoopAggregate is the W1 aggregate stream: it always yields no candidates.
type NoopAggregate struct{}

func (NoopAggregate) Suggest(context.Context, string, bool, int) ([]ranking.Candidate, error) {
	return nil, nil
}

// Cache is the optional Redis-backed short-prefix cache.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)
}

// Request is a suggestion request (already parsed from the HTTP query).
type Request struct {
	Query          string
	Limit          int
	HideSensitive  bool
	Personalized   bool
	IncludeHistory bool
	Lang           string
	UserID         string
	SessionID      string
	Mode           string
}

// Response is the suggestions payload (§1.4).
type Response struct {
	Query           string               `json:"query"`
	NormalizedQuery string               `json:"normalized_query"`
	Suggestions     []ranking.Suggestion `json:"suggestions"`
	ModelVersion    string               `json:"model_version"`
}

// Service runs the suggestion pipeline.
type Service struct {
	q      Querier
	agg    AggregateSuggester
	cache  Cache
	logger *slog.Logger
}

// NewService builds the service. agg defaults to NoopAggregate; cache/logger may
// be nil.
func NewService(q Querier, agg AggregateSuggester, cache Cache, logger *slog.Logger) *Service {
	if agg == nil {
		agg = NoopAggregate{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{q: q, agg: agg, cache: cache, logger: logger}
}

// Suggest runs the pipeline and always returns a Response — on any internal
// error it logs and returns an empty suggestion list rather than failing.
func (s *Service) Suggest(ctx context.Context, req Request) Response {
	normalized := normalize.Normalize(req.Query)
	resp := Response{
		Query:           req.Query,
		NormalizedQuery: normalized,
		Suggestions:     []ranking.Suggestion{},
		ModelVersion:    ModelVersion,
	}
	if normalized == "" {
		return resp
	}
	limit := clamp(req.Limit, defaultLimit, 1, maxLimit)
	mode := req.Mode
	if mode == "" {
		mode = "simple"
	}

	cacheKey := fmt.Sprintf("sugg:%s:%t:%s", mode, req.HideSensitive, normalized)
	if s.cache != nil && len([]rune(normalized)) <= cachePrefixMax {
		if raw, ok := s.cache.Get(ctx, cacheKey); ok {
			var cached []ranking.Suggestion
			if err := json.Unmarshal(raw, &cached); err == nil {
				resp.Suggestions = cached
				return resp
			}
		}
	}

	cands, err := s.candidates(ctx, normalized, req, limit)
	if err != nil {
		s.logger.WarnContext(ctx, "suggest: degraded to empty list", "error", err, "prefix_len", len([]rune(normalized)))
		return resp // empty, never 5xx
	}

	resp.Suggestions = ranking.Blend(cands, limit, ranking.DefaultWeights)
	if resp.Suggestions == nil {
		resp.Suggestions = []ranking.Suggestion{}
	}

	if s.cache != nil && len([]rune(normalized)) <= cachePrefixMax {
		if raw, err := json.Marshal(resp.Suggestions); err == nil {
			s.cache.Set(ctx, cacheKey, raw, cacheTTL)
		}
	}
	return resp
}

// candidates gathers the doc-derived + aggregate streams, adding a trigram
// fuzzy fallback only when exact-prefix candidates fall short of the limit.
func (s *Service) candidates(ctx context.Context, normalized string, req Request, limit int) ([]ranking.Candidate, error) {
	like := likePrefix(normalized)
	var cands []ranking.Candidate

	titles, err := s.q.SuggestTitlePrefix(ctx, sqlcgen.SuggestTitlePrefixParams{
		HideSensitive: req.HideSensitive, Prefix: like, Lim: streamLimit,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range titles {
		cands = append(cands, ranking.Candidate{
			Text: r.Title, Kind: ranking.KindQuery, Source: ranking.SourceDoc,
			ExactPrefix: true, Popularity: float64(r.Views),
		})
	}

	channels, err := s.q.SuggestChannelPrefix(ctx, sqlcgen.SuggestChannelPrefixParams{
		HideSensitive: req.HideSensitive, Prefix: like, Lim: streamLimit,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range channels {
		name := ""
		if r.ChannelName != nil {
			name = *r.ChannelName
		}
		cands = append(cands, ranking.Candidate{
			Text: name, Kind: ranking.KindChannel, ChannelHandle: r.ChannelHandle,
			Source: ranking.SourceDoc, ExactPrefix: true, Popularity: float64(r.Views),
		})
	}

	tags, err := s.q.SuggestTagPrefix(ctx, sqlcgen.SuggestTagPrefixParams{
		HideSensitive: req.HideSensitive, Prefix: like, Lim: streamLimit,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range tags {
		cands = append(cands, ranking.Candidate{
			Text: r.Tag, Kind: ranking.KindTag, Source: ranking.SourceDoc,
			ExactPrefix: true, Popularity: float64(r.Cnt),
		})
	}

	// Aggregate (global popularity) stream — no-op in W1.
	aggCands, err := s.agg.Suggest(ctx, normalized, req.HideSensitive, streamLimit)
	if err != nil {
		return nil, err
	}
	cands = append(cands, aggCands...)

	// Typo fallback only when exact-prefix candidates are short of the limit.
	exact := 0
	for _, c := range cands {
		if c.ExactPrefix {
			exact++
		}
	}
	if exact < limit {
		fuzzy, err := s.q.SuggestTitleFuzzy(ctx, sqlcgen.SuggestTitleFuzzyParams{
			Q: normalized, HideSensitive: req.HideSensitive, Lim: streamLimit,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range fuzzy {
			cands = append(cands, ranking.Candidate{
				Text: r.Title, Kind: ranking.KindQuery, Source: ranking.SourceDoc,
				ExactPrefix: false, Popularity: float64(r.Views),
			})
		}
	}
	return cands, nil
}

// likePrefix escapes LIKE metacharacters in the normalized prefix and appends
// the wildcard, so a query containing % or _ is matched literally.
func likePrefix(normalized string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(normalized) + "%"
}

func clamp(v, def, lo, hi int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
