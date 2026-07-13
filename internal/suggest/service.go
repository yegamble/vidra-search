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

	"github.com/google/uuid"

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
	SuggestUserHistoryPrefix(ctx context.Context, arg sqlcgen.SuggestUserHistoryPrefixParams) ([]sqlcgen.SuggestUserHistoryPrefixRow, error)
}

// AggregateSuggester is the global-query-popularity stream (§1.6a). W2 wires the
// query_aggregates-backed StoreAggregate; NoopAggregate remains for tests.
type AggregateSuggester interface {
	Suggest(ctx context.Context, normalizedPrefix string, hideSensitive bool, limit int) ([]ranking.Candidate, error)
}

// NoopAggregate yields no aggregate candidates.
type NoopAggregate struct{}

func (NoopAggregate) Suggest(context.Context, string, bool, int) ([]ranking.Candidate, error) {
	return nil, nil
}

// AggQuerier is the store surface the aggregate stream reads.
type AggQuerier interface {
	SuggestAggregatePrefix(ctx context.Context, arg sqlcgen.SuggestAggregatePrefixParams) ([]sqlcgen.SuggestAggregatePrefixRow, error)
}

// StoreAggregate is the real aggregate stream: suggestible, non-banned queries
// matching the prefix, ordered by decayed frequency (§1.6a).
type StoreAggregate struct{ q AggQuerier }

// NewStoreAggregate builds the query_aggregates-backed aggregate stream.
func NewStoreAggregate(q AggQuerier) StoreAggregate { return StoreAggregate{q: q} }

// Suggest returns the aggregate candidates for a normalized prefix. hideSensitive
// is unused (aggregates carry no sensitivity); it satisfies the interface.
func (s StoreAggregate) Suggest(ctx context.Context, normalizedPrefix string, _ bool, limit int) ([]ranking.Candidate, error) {
	rows, err := s.q.SuggestAggregatePrefix(ctx, sqlcgen.SuggestAggregatePrefixParams{
		Prefix: likePrefix(normalizedPrefix), Lim: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	cands := make([]ranking.Candidate, 0, len(rows))
	for _, r := range rows {
		text := r.DisplayQuery
		if text == "" {
			text = r.NormalizedQuery
		}
		cands = append(cands, ranking.Candidate{
			Text: text, Kind: ranking.KindQuery, Source: ranking.SourceQuery,
			ExactPrefix: true, Popularity: r.DecayedFreq,
		})
	}
	return cands, nil
}

// Cache is the optional Redis-backed short-prefix cache.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)
}

// SessionReader supplies the current session's recent normalized queries (§1.6c).
type SessionReader interface {
	SessionQueries(ctx context.Context, sessionID string) []string
}

// TrendReader supplies the current trending-query set for the small blend boost.
type TrendReader interface {
	TrendingQuerySet(ctx context.Context) map[string]float64
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
	q       Querier
	agg     AggregateSuggester
	cache   Cache
	session SessionReader
	trend   TrendReader
	logger  *slog.Logger
}

// NewService builds the service. agg defaults to NoopAggregate; cache, session,
// trend, and logger may be nil.
func NewService(q Querier, agg AggregateSuggester, cache Cache, session SessionReader, trend TrendReader, logger *slog.Logger) *Service {
	if agg == nil {
		agg = NoopAggregate{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{q: q, agg: agg, cache: cache, session: session, trend: trend, logger: logger}
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

	// The shared prefix cache is ONLY consulted for non-personalized requests —
	// serving one user's history/session results to another would be a privacy
	// leak, so a request carrying include_history + a user/session bypasses it.
	personalized := req.IncludeHistory && (req.UserID != "" || req.SessionID != "")
	cacheable := s.cache != nil && !personalized && len([]rune(normalized)) <= cachePrefixMax
	cacheKey := fmt.Sprintf("sugg:%s:%t:%s", mode, req.HideSensitive, normalized)
	if cacheable {
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

	if cacheable {
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

	// Aggregate (global popularity) stream.
	aggCands, err := s.agg.Suggest(ctx, normalized, req.HideSensitive, streamLimit)
	if err != nil {
		return nil, err
	}
	cands = append(cands, aggCands...)

	// Personal history stream (§1.6c) — only when history is included and the
	// request is attributable to a signed-in user. NOT hidden entries only.
	if req.IncludeHistory && req.UserID != "" {
		if uid, perr := uuid.Parse(req.UserID); perr == nil {
			hist, herr := s.q.SuggestUserHistoryPrefix(ctx, sqlcgen.SuggestUserHistoryPrefixParams{
				UserID: uid, Prefix: like, Lim: streamLimit,
			})
			if herr != nil {
				return nil, herr
			}
			for _, r := range hist {
				text := r.DisplayQuery
				if text == "" {
					text = r.NormalizedQuery
				}
				cands = append(cands, ranking.Candidate{
					Text: text, Kind: ranking.KindHistory, Source: ranking.SourceHistory,
					IsPersonal: true, ExactPrefix: true, Popularity: float64(r.UseCount),
				})
			}
		}
	}

	// Session recency stream (§1.6c) — best-effort from Redis; the stored queries
	// are already normalized, so a prefix match is a plain HasPrefix.
	if req.IncludeHistory && req.SessionID != "" && s.session != nil {
		for _, rq := range s.session.SessionQueries(ctx, req.SessionID) {
			if strings.HasPrefix(rq, normalized) {
				cands = append(cands, ranking.Candidate{
					Text: rq, Kind: ranking.KindHistory, Source: ranking.SourceHistory,
					IsPersonal: true, ExactPrefix: true,
				})
			}
		}
	}

	// Trending boost — mark candidates whose normalized form is currently trending
	// (best-effort; a small weight in the blend).
	if s.trend != nil {
		if trendSet := s.trend.TrendingQuerySet(ctx); len(trendSet) > 0 {
			for i := range cands {
				if _, ok := trendSet[normalize.Normalize(cands[i].Text)]; ok {
					cands[i].Trending = true
				}
			}
		}
	}

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
