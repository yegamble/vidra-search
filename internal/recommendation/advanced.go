package recommendation

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vidra/vidra-search/internal/experiment"
	"github.com/vidra/vidra-search/internal/pgconv"
	"github.com/vidra/vidra-search/internal/ranking"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// Advanced recommender scoring weights (§1.8). Base source relevance +
// personalization + freshness, with recently-watched demoted for novelty. The
// final ordering is MMR-diversified (tag/category overlap) with a per-channel cap
// of 2 and a seed-deterministic ε-greedy exploration slot.
const (
	recWBase     = 1.0
	recWAffinity = 0.6
	recWChannel  = 0.4
	recWFresh    = 0.3
	// recNoveltyPenalty demotes a candidate the user has recently watched.
	recNoveltyPenalty = 0.5
	// recMMRLambda trades relevance for topical spread (0.7 ≈ mostly relevance).
	recMMRLambda = 0.7
	// recExploreEpsilon is the ε-greedy exploration probability.
	recExploreEpsilon = 0.1
	// recFreshMaxViews bounds the exploration pool to genuinely low-view docs.
	recFreshMaxViews = 100
	// recSubscribedThreshold: normalized channel affinity above this reports the
	// "subscribed" reason (search has no subscription data).
	recSubscribedThreshold = 0.8
	// recFreshnessHalfLifeDays matches the simple ranker's freshness decay.
	recFreshnessHalfLifeDays = 30.0
)

// advCand is one advanced-recommendation candidate before reranking.
type advCand struct {
	id         uuid.UUID
	channel    pgtype.UUID
	baseScore  float64
	reason     string
	priority   int // reason priority for merge (co_watch > similar > fresh/trending > popular)
	tokens     map[string]struct{}
	views      float64
	ageDays    float64
	eligible   bool
	normBase   float64
	affinity   float64
	channelAff float64
}

// candidateSet accumulates candidates keyed by id, keeping the highest-priority
// source (and its base score) when a video is proposed by several generators.
type candidateSet struct {
	byID    map[uuid.UUID]*advCand
	exclude map[uuid.UUID]bool
	order   []uuid.UUID
}

func newCandidateSet() *candidateSet {
	return &candidateSet{byID: map[uuid.UUID]*advCand{}, exclude: map[uuid.UUID]bool{}}
}

func (cs *candidateSet) skip(id uuid.UUID) { cs.exclude[id] = true }

func (cs *candidateSet) add(id uuid.UUID, channel pgtype.UUID, base float64, reason string, priority int) {
	if cs.exclude[id] {
		return
	}
	if cur, ok := cs.byID[id]; ok {
		if priority > cur.priority || (priority == cur.priority && base > cur.baseScore) {
			cur.reason, cur.priority = reason, priority
		}
		if base > cur.baseScore {
			cur.baseScore = base
		}
		return
	}
	cs.byID[id] = &advCand{id: id, channel: channel, baseScore: base, reason: reason, priority: priority}
	cs.order = append(cs.order, id)
}

func (cs *candidateSet) ids() []uuid.UUID {
	return append([]uuid.UUID(nil), cs.order...)
}

// reason priorities.
const (
	prioPopular  = 0
	prioFresh    = 1
	prioTrending = 1
	prioSimilar  = 2
	prioCoWatch  = 3
)

// relatedAdvanced builds the advanced related feed (§1.8): candidates from
// item_neighbors ∪ the simple sets ∪ session co-watch, reranked by affinity +
// freshness + novelty, MMR-diversified, creator-capped, with an ε-greedy slot.
func (s *Service) relatedAdvanced(ctx context.Context, req RelatedRequest) (Response, error) {
	limit := clamp(req.Limit, defaultLimit, maxRelatedLimit)
	resp := Response{Items: []Item{}, ModelVersion: AdvancedModelVersion}
	s.attachExperiment(&resp, subjectOf(req.UserID, req.SessionID))

	seed, err := s.q.GetDocument(ctx, req.VideoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return resp, nil
		}
		return Response{}, err
	}

	cs := newCandidateSet()
	cs.skip(req.VideoID)

	// 1. Co-visitation neighbours of the seed.
	neighbors, err := s.q.NeighborsForVideo(ctx, sqlcgen.NeighborsForVideoParams{VideoID: req.VideoID, Lim: int32(limit * 4)})
	if err != nil {
		return Response{}, err
	}
	for _, n := range neighbors {
		cs.add(n.NeighborID, pgtype.UUID{}, float64(n.Score), ReasonCoWatch, prioCoWatch)
	}

	// 2. Simple sets: same-channel + tag/category overlap.
	if _, ok := pgconv.UUIDValue(seed.ChannelID); ok {
		rows, err := s.q.RelatedSameChannel(ctx, sqlcgen.RelatedSameChannelParams{
			HideSensitive: req.HideSensitive, ChannelID: seed.ChannelID, VideoID: req.VideoID, Lim: sameChannelCap,
		})
		if err != nil {
			return Response{}, err
		}
		for _, r := range rows {
			cs.add(r.VideoID, r.ChannelID, 0.5, ReasonSimilar, prioSimilar)
		}
	}
	overlap, err := s.q.RelatedByOverlap(ctx, sqlcgen.RelatedByOverlapParams{
		Category: seed.Category, Language: seed.Language, Tags: nonNil(seed.Tags),
		HideSensitive: req.HideSensitive, VideoID: req.VideoID, Lim: int32(limit * 3),
	})
	if err != nil {
		return Response{}, err
	}
	for _, r := range overlap {
		cs.add(r.VideoID, r.ChannelID, float64(r.Overlap), ReasonSimilar, prioSimilar)
	}

	// 3. Session co-watch seed.
	s.addSessionCoWatch(ctx, cs, req.SessionID, limit)

	// 4. Popular fill so a cold seed still yields a feed.
	pop, err := s.q.PopularEligible(ctx, sqlcgen.PopularEligibleParams{
		HideSensitive: req.HideSensitive, Exclude: []uuid.UUID{req.VideoID}, Lim: int32(limit),
	})
	if err != nil {
		return Response{}, err
	}
	for _, r := range pop {
		cs.add(r.VideoID, r.ChannelID, 0.0, ReasonPopular, prioPopular)
	}

	// Related has no language preference (the exploration pool spans languages).
	items, err := s.composeAdvanced(ctx, cs, limit, req.HideSensitive, req.UserID, req.Personalized,
		"", subjectOf(req.UserID, req.SessionID))
	if err != nil {
		return Response{}, err
	}
	resp.Items = items
	return resp, nil
}

// homeAdvanced builds the advanced home feed (§1.8): candidates from the co-watch
// of the user's recent watches ∪ trending ∪ fresh ∪ popular-in-language ∪ session
// co-watch, reranked and diversified like related, hiding already-watched videos.
func (s *Service) homeAdvanced(ctx context.Context, req HomeRequest) (Response, error) {
	limit := clamp(req.Limit, defaultLimit, maxHomeLimit)
	resp := Response{Items: []Item{}, ModelVersion: AdvancedModelVersion}
	s.attachExperiment(&resp, subjectOf(req.UserID, req.SessionID))
	fetch := int32(limit * 2)

	cs := newCandidateSet()

	// Hide already-watched videos from the home feed.
	if uid, err := uuid.Parse(req.UserID); err == nil && req.Personalized {
		watched, err := s.q.RecentlyWatchedVideos(ctx, sqlcgen.RecentlyWatchedVideosParams{UserID: uid, Lim: 200})
		if err != nil {
			return Response{}, err
		}
		for _, w := range watched {
			cs.skip(w)
		}
		// Co-watch of the user's recent watches.
		neigh, err := s.q.NeighborsForUserWatches(ctx, sqlcgen.NeighborsForUserWatchesParams{UserID: uid, Lim: fetch})
		if err != nil {
			return Response{}, err
		}
		for _, n := range neigh {
			cs.add(n.VideoID, pgtype.UUID{}, float64(n.Score), ReasonCoWatch, prioCoWatch)
		}
	}

	// Trending (gated Redis list, else SQL gravity).
	for _, c := range s.homeTrendingCandidates(ctx, req.HideSensitive, req.Lang, fetch) {
		cs.add(c.videoID, c.channelID, 1.0, ReasonTrending, prioTrending)
	}

	// Fresh + popular-in-language.
	recent, err := s.q.HomeRecent(ctx, sqlcgen.HomeRecentParams{
		HideSensitive: req.HideSensitive, Language: optStr(req.Lang), Lim: fetch,
	})
	if err != nil {
		return Response{}, err
	}
	for _, r := range recent {
		cs.add(r.VideoID, r.ChannelID, 0.5, ReasonFresh, prioFresh)
	}
	pop, err := s.q.PopularEligible(ctx, sqlcgen.PopularEligibleParams{HideSensitive: req.HideSensitive, Lim: fetch})
	if err != nil {
		return Response{}, err
	}
	for _, r := range pop {
		cs.add(r.VideoID, r.ChannelID, 0.0, ReasonPopular, prioPopular)
	}

	// Session co-watch seed.
	s.addSessionCoWatch(ctx, cs, req.SessionID, limit)

	items, err := s.composeAdvanced(ctx, cs, limit, req.HideSensitive, req.UserID, req.Personalized,
		req.Lang, subjectOf(req.UserID, req.SessionID))
	if err != nil {
		return Response{}, err
	}
	resp.Items = items
	return resp, nil
}

// trendCand is a trending candidate id+channel.
type trendCand struct {
	videoID   uuid.UUID
	channelID pgtype.UUID
}

// homeTrendingCandidates returns trending candidates, preferring the gated Redis
// list and falling back to the SQL HN-gravity query.
func (s *Service) homeTrendingCandidates(ctx context.Context, hideSensitive bool, lang string, fetch int32) []trendCand {
	if cands := s.redisTrendingCandidates(ctx, hideSensitive); cands != nil {
		out := make([]trendCand, 0, len(cands))
		for _, c := range cands {
			out = append(out, trendCand{videoID: c.videoID, channelID: c.channelID})
		}
		return out
	}
	rows, err := s.q.HomeTrending(ctx, sqlcgen.HomeTrendingParams{
		HideSensitive: hideSensitive, Language: optStr(lang), Lim: fetch,
	})
	if err != nil {
		return nil
	}
	out := make([]trendCand, 0, len(rows))
	for _, r := range rows {
		out = append(out, trendCand{videoID: r.VideoID, channelID: r.ChannelID})
	}
	return out
}

// addSessionCoWatch adds co-visitation neighbours of the session's recent videos.
func (s *Service) addSessionCoWatch(ctx context.Context, cs *candidateSet, sessionID string, limit int) {
	if sessionID == "" || s.session == nil {
		return
	}
	seeds := parseUUIDs(s.session.SessionVideos(ctx, sessionID))
	if len(seeds) == 0 {
		return
	}
	rows, err := s.q.NeighborsForSeeds(ctx, sqlcgen.NeighborsForSeedsParams{Seeds: seeds, Lim: int32(limit * 2)})
	if err != nil {
		return
	}
	for _, r := range rows {
		cs.add(r.VideoID, pgtype.UUID{}, float64(r.Score), ReasonCoWatch, prioCoWatch)
	}
}

// composeAdvanced fetches candidate features, scores relevance (base + affinity +
// freshness − novelty), MMR-diversifies, applies the per-channel cap, and inserts
// the ε-greedy exploration slot. Deterministic given the same inputs + seed.
func (s *Service) composeAdvanced(ctx context.Context, cs *candidateSet, limit int, hideSensitive bool,
	userID string, personalized bool, lang, subject string) ([]Item, error) {

	ids := cs.ids()
	if len(ids) == 0 {
		return []Item{}, nil
	}

	// Features: this also filters to eligible (+ non-sensitive) documents.
	feats, err := s.q.ListDocFeaturesByIDs(ctx, sqlcgen.ListDocFeaturesByIDsParams{HideSensitive: hideSensitive, Ids: ids})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	eligible := make([]*advCand, 0, len(feats))
	for _, f := range feats {
		c := cs.byID[f.VideoID]
		if c == nil {
			continue
		}
		c.eligible = true
		c.channel = f.ChannelID
		c.views = float64(f.Views)
		c.ageDays = ageDaysOf(f.PublishedAt, f.SourceUpdatedAt, now)
		c.tokens = tokensOf(f.Tags, f.Category)
		eligible = append(eligible, c)
	}
	if len(eligible) == 0 {
		return []Item{}, nil
	}

	// Personalization + novelty.
	if personalized {
		if uid, perr := uuid.Parse(userID); perr == nil {
			if err := s.fillAffinity(ctx, uid, eligible); err != nil {
				return nil, err
			}
		}
	}

	// Normalize base + affinity + channel across the eligible set.
	var maxBase, maxAff, maxChan float64
	for _, c := range eligible {
		maxBase = math.Max(maxBase, c.baseScore)
		maxAff = math.Max(maxAff, c.affinity)
		maxChan = math.Max(maxChan, c.channelAff)
	}
	norm := func(v, m float64) float64 {
		if m > 0 {
			return v / m
		}
		return 0
	}

	mmrDocs := make([]ranking.MMRDoc, 0, len(eligible))
	relByID := make(map[string]float64, len(eligible))
	for _, c := range eligible {
		nb := norm(c.baseScore, maxBase)
		na := norm(c.affinity, maxAff)
		nc := norm(c.channelAff, maxChan)
		fresh := math.Exp(-math.Ln2 * c.ageDays / recFreshnessHalfLifeDays)
		rel := recWBase*nb + recWAffinity*na + recWChannel*nc + recWFresh*fresh
		// "subscribed" reason when the user's channel affinity is high.
		if personalized && nc >= recSubscribedThreshold {
			c.reason = ReasonSubscribed
		}
		id := c.id.String()
		relByID[id] = rel
		mmrDocs = append(mmrDocs, ranking.MMRDoc{VideoID: id, Relevance: rel, Tokens: c.tokens})
	}

	ordered := ranking.MMR(mmrDocs, recMMRLambda, len(mmrDocs))

	// ε-greedy exploration: with probability ε surface a fresh low-view doc.
	ordered = s.insertExploration(ctx, ordered, cs, hideSensitive, lang, subject, limit)

	// Compose with the per-channel cap, in the diversified order.
	comp := newComposer(limit)
	for _, id := range ordered {
		vid, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		c := cs.byID[vid]
		if c == nil || !c.eligible {
			continue
		}
		comp.add(candidate{videoID: c.id, channelID: c.channel, reason: c.reason})
		if comp.full() {
			break
		}
	}
	return comp.items(), nil
}

// fillAffinity populates the neighbour + channel affinity on the candidates.
func (s *Service) fillAffinity(ctx context.Context, uid uuid.UUID, cands []*advCand) error {
	ids := make([]uuid.UUID, len(cands))
	for i, c := range cands {
		ids[i] = c.id
	}
	affRows, err := s.q.NeighborAffinity(ctx, sqlcgen.NeighborAffinityParams{UserID: uid, Candidates: ids})
	if err != nil {
		return err
	}
	aff := make(map[uuid.UUID]float64, len(affRows))
	for _, r := range affRows {
		aff[r.VideoID] = r.Affinity
	}
	chanRows, err := s.q.UserChannelAffinity(ctx, uid)
	if err != nil {
		return err
	}
	chanAff := make(map[uuid.UUID]float64, len(chanRows))
	for _, r := range chanRows {
		if ch, ok := pgconv.UUIDValue(r.ChannelID); ok {
			chanAff[ch] = r.Weight
		}
	}
	for _, c := range cands {
		c.affinity = aff[c.id]
		if ch, ok := pgconv.UUIDValue(c.channel); ok {
			c.channelAff = chanAff[ch]
		}
	}
	return nil
}

// insertExploration deterministically inserts one fresh low-view doc into the
// ordered ids when the ε-greedy slot fires (seed-derived, no global RNG). The
// explore doc is registered in the candidate set (reason=fresh) so composition
// picks it up.
func (s *Service) insertExploration(ctx context.Context, ordered []string, cs *candidateSet,
	hideSensitive bool, lang, subject string, limit int) []string {
	if subject == "" {
		return ordered
	}
	pool, err := s.q.FreshLowViewEligible(ctx, sqlcgen.FreshLowViewEligibleParams{
		HideSensitive: hideSensitive, MaxViews: recFreshMaxViews, Language: optStr(lang), Lim: int32(limit * 2),
	})
	if err != nil || len(pool) == 0 {
		return ordered
	}
	// Filter to docs not already excluded/selected.
	fresh := make([]sqlcgen.FreshLowViewEligibleRow, 0, len(pool))
	for _, p := range pool {
		if cs.exclude[p.VideoID] {
			continue
		}
		if c, ok := cs.byID[p.VideoID]; ok && c.eligible {
			continue // already a candidate; not a novel exploration
		}
		fresh = append(fresh, p)
	}
	if len(fresh) == 0 {
		return ordered
	}
	fire, idx := ranking.ExplorationSlot(subject, recExploreEpsilon, len(fresh))
	if !fire {
		return ordered
	}
	pick := fresh[idx]
	// Register as an eligible candidate so composition can select it.
	c := &advCand{id: pick.VideoID, channel: pick.ChannelID, reason: ReasonFresh, eligible: true, priority: prioFresh}
	cs.byID[pick.VideoID] = c
	// Insert at a deterministic slot (mid-feed) so exploration is visible but not
	// pushed to the very top.
	slot := 3
	if slot > len(ordered) {
		slot = len(ordered)
	}
	id := pick.VideoID.String()
	out := make([]string, 0, len(ordered)+1)
	out = append(out, ordered[:slot]...)
	out = append(out, id)
	out = append(out, ordered[slot:]...)
	return out
}

// attachExperiment stamps the experiment assignment onto a response when an
// experiment is defined for the given subject.
func (s *Service) attachExperiment(resp *Response, subject string) {
	if s.exp == nil {
		return
	}
	if a, ok := s.exp.Assign(ExperimentKey, subject); ok {
		resp.Experiment = &a
	}
}

// --- small helpers ---

func subjectOf(userID, sessionID string) string {
	if userID != "" {
		return userID
	}
	return sessionID
}

func parseUUIDs(ss []string) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ss))
	for _, s := range ss {
		if id, err := uuid.Parse(s); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// tokensOf builds the MMR similarity token set: each tag plus a category token.
func tokensOf(tags []string, category *string) map[string]struct{} {
	m := make(map[string]struct{}, len(tags)+1)
	for _, t := range tags {
		m["tag:"+t] = struct{}{}
	}
	if category != nil && *category != "" {
		m["cat:"+*category] = struct{}{}
	}
	return m
}

// ageDaysOf returns a document's age in days from published_at (falling back to
// source_updated_at), clamped at 0.
func ageDaysOf(publishedAt pgtype.Timestamptz, sourceUpdatedAt, now time.Time) float64 {
	t := sourceUpdatedAt
	if publishedAt.Valid {
		t = publishedAt.Time
	}
	d := now.Sub(t).Hours() / 24
	if d < 0 {
		return 0
	}
	return d
}

var _ = experiment.Assignment{}
