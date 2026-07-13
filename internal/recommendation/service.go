// Package recommendation implements simple-mode related and home feeds (§1.8)
// for GET /internal/v1/recommendations/*. Both return ranked video IDs with a
// reason, composed deterministically from indexed candidate queries with a
// per-channel cap so no single creator dominates a feed.
package recommendation

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vidra/vidra-search/internal/pgconv"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// ModelVersion identifies the W1 simple recommender.
const ModelVersion = "simple-v1"

const (
	defaultLimit    = 20
	maxRelatedLimit = 50
	maxHomeLimit    = 100
	// perChannelCap bounds how many results one channel may contribute.
	perChannelCap = 2
	// sameChannelCap bounds the same-channel "similar" seed for related.
	sameChannelCap = 2
)

// Reason values (subset of the §1.4 enum used by simple mode).
const (
	ReasonSimilar  = "similar"
	ReasonTrending = "trending"
	ReasonFresh    = "fresh"
	ReasonPopular  = "popular"
)

// Querier is the store surface the recommenders read.
type Querier interface {
	GetDocument(ctx context.Context, videoID uuid.UUID) (sqlcgen.GetDocumentRow, error)
	RelatedSameChannel(ctx context.Context, arg sqlcgen.RelatedSameChannelParams) ([]sqlcgen.RelatedSameChannelRow, error)
	RelatedByOverlap(ctx context.Context, arg sqlcgen.RelatedByOverlapParams) ([]sqlcgen.RelatedByOverlapRow, error)
	PopularEligible(ctx context.Context, arg sqlcgen.PopularEligibleParams) ([]sqlcgen.PopularEligibleRow, error)
	HomeTrending(ctx context.Context, arg sqlcgen.HomeTrendingParams) ([]sqlcgen.HomeTrendingRow, error)
	HomeRecent(ctx context.Context, arg sqlcgen.HomeRecentParams) ([]sqlcgen.HomeRecentRow, error)
}

// Item is one recommended video with its provenance.
type Item struct {
	VideoID string  `json:"video_id"`
	Score   float64 `json:"score"`
	Reason  string  `json:"reason"`
}

// Response is the recommendations payload (§1.4).
type Response struct {
	Items        []Item `json:"items"`
	ModelVersion string `json:"model_version"`
}

// Service composes related and home feeds.
type Service struct {
	q Querier
}

// NewService builds the recommendation service.
func NewService(q Querier) *Service {
	return &Service{q: q}
}

// candidate is an internal, pre-scored recommendation with its channel (for the
// per-channel cap) and reason.
type candidate struct {
	videoID   uuid.UUID
	channelID pgtype.UUID
	reason    string
}

// Related builds the related feed for a seed video (§1.8 simple): up to two
// recent same-channel videos, then tag/category/language overlap, then a popular
// fill. Excludes the seed; respects eligibility + hide_sensitive; caps each
// channel. A missing seed yields an empty feed.
func (s *Service) Related(ctx context.Context, videoID uuid.UUID, limit int, hideSensitive bool) (Response, error) {
	limit = clamp(limit, defaultLimit, maxRelatedLimit)
	resp := Response{Items: []Item{}, ModelVersion: ModelVersion}

	seed, err := s.q.GetDocument(ctx, videoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return resp, nil
		}
		return Response{}, err
	}

	comp := newComposer(limit)
	comp.exclude(videoID)

	// 1. Same-channel recent (cap 2).
	if _, ok := pgconv.UUIDValue(seed.ChannelID); ok {
		rows, err := s.q.RelatedSameChannel(ctx, sqlcgen.RelatedSameChannelParams{
			HideSensitive: hideSensitive, ChannelID: seed.ChannelID, VideoID: videoID, Lim: sameChannelCap,
		})
		if err != nil {
			return Response{}, err
		}
		for _, r := range rows {
			comp.add(candidate{videoID: r.VideoID, channelID: r.ChannelID, reason: ReasonSimilar})
		}
	}

	// 2. Tag/category/language overlap.
	if !comp.full() {
		rows, err := s.q.RelatedByOverlap(ctx, sqlcgen.RelatedByOverlapParams{
			Category: seed.Category, Language: seed.Language, Tags: nonNil(seed.Tags),
			HideSensitive: hideSensitive, VideoID: videoID, Lim: int32(limit * 3),
		})
		if err != nil {
			return Response{}, err
		}
		for _, r := range rows {
			comp.add(candidate{videoID: r.VideoID, channelID: r.ChannelID, reason: ReasonSimilar})
		}
	}

	// 3. Popular fill.
	if !comp.full() {
		rows, err := s.q.PopularEligible(ctx, sqlcgen.PopularEligibleParams{
			HideSensitive: hideSensitive, Exclude: comp.excluded(), Lim: int32(limit),
		})
		if err != nil {
			return Response{}, err
		}
		for _, r := range rows {
			comp.add(candidate{videoID: r.VideoID, channelID: r.ChannelID, reason: ReasonPopular})
		}
	}

	resp.Items = comp.items()
	return resp, nil
}

// Home builds the home feed (§1.8 simple): a language-aware mix of HN-gravity
// trending, fresh, and popular, interleaved so all three contribute, with a
// per-channel cap.
func (s *Service) Home(ctx context.Context, limit int, hideSensitive bool, lang string) (Response, error) {
	limit = clamp(limit, defaultLimit, maxHomeLimit)
	resp := Response{Items: []Item{}, ModelVersion: ModelVersion}
	language := optStr(lang)
	fetch := int32(limit * 2)

	trending, err := s.q.HomeTrending(ctx, sqlcgen.HomeTrendingParams{
		HideSensitive: hideSensitive, Exclude: nil, Language: language, Lim: fetch,
	})
	if err != nil {
		return Response{}, err
	}
	recent, err := s.q.HomeRecent(ctx, sqlcgen.HomeRecentParams{
		HideSensitive: hideSensitive, Exclude: nil, Language: language, Lim: fetch,
	})
	if err != nil {
		return Response{}, err
	}
	popular, err := s.q.PopularEligible(ctx, sqlcgen.PopularEligibleParams{
		HideSensitive: hideSensitive, Exclude: nil, Lim: fetch,
	})
	if err != nil {
		return Response{}, err
	}

	trendingCands := make([]candidate, 0, len(trending))
	for _, r := range trending {
		trendingCands = append(trendingCands, candidate{videoID: r.VideoID, channelID: r.ChannelID, reason: ReasonTrending})
	}
	recentCands := make([]candidate, 0, len(recent))
	for _, r := range recent {
		recentCands = append(recentCands, candidate{videoID: r.VideoID, channelID: r.ChannelID, reason: ReasonFresh})
	}
	popularCands := make([]candidate, 0, len(popular))
	for _, r := range popular {
		popularCands = append(popularCands, candidate{videoID: r.VideoID, channelID: r.ChannelID, reason: ReasonPopular})
	}
	streams := [][]candidate{trendingCands, recentCands, popularCands}

	comp := newComposer(limit)
	// Round-robin interleave so trending/fresh/popular all contribute.
	for i := 0; !comp.full(); i++ {
		progressed := false
		for _, stream := range streams {
			if i < len(stream) {
				comp.add(stream[i])
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	resp.Items = comp.items()
	return resp, nil
}

// --- composer: dedupe + per-channel cap + deterministic scoring ---

type composer struct {
	limit    int
	seen     map[uuid.UUID]bool
	perChan  map[uuid.UUID]int
	excludes []uuid.UUID
	picks    []candidate
}

func newComposer(limit int) *composer {
	return &composer{limit: limit, seen: map[uuid.UUID]bool{}, perChan: map[uuid.UUID]int{}}
}

func (c *composer) exclude(id uuid.UUID) {
	c.seen[id] = true
	c.excludes = append(c.excludes, id)
}

func (c *composer) excluded() []uuid.UUID {
	// Exclude already-picked ids as well, so a fill query never re-proposes them.
	out := append([]uuid.UUID(nil), c.excludes...)
	for _, p := range c.picks {
		out = append(out, p.videoID)
	}
	return out
}

func (c *composer) full() bool { return len(c.picks) >= c.limit }

func (c *composer) add(cand candidate) {
	if c.full() || c.seen[cand.videoID] {
		return
	}
	if ch, ok := pgconv.UUIDValue(cand.channelID); ok {
		if c.perChan[ch] >= perChannelCap {
			return
		}
		c.perChan[ch]++
	}
	c.seen[cand.videoID] = true
	c.picks = append(c.picks, cand)
}

// items assigns descending synthetic scores so the emitted order equals score
// order (vidra-core preserves id order; the score is an informational hint).
func (c *composer) items() []Item {
	n := len(c.picks)
	out := make([]Item, 0, n)
	for i, p := range c.picks {
		out = append(out, Item{
			VideoID: p.videoID.String(),
			Score:   float64(n-i) / float64(n),
			Reason:  p.reason,
		})
	}
	return out
}

func clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
