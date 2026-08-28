package search

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/vidra/vidra-search/internal/experiment"
	"github.com/vidra/vidra-search/internal/paging"
	"github.com/vidra/vidra-search/internal/pgconv"
	"github.com/vidra/vidra-search/internal/ranking"
	"github.com/vidra/vidra-search/internal/rankutil"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// heuristicRanker is the built-in fallback used when no RankerProvider is wired.
type heuristicRanker struct{}

func (heuristicRanker) Rerank(docs []ranking.Doc) []ranking.Ranked {
	return ranking.Rerank(docs, ranking.DefaultAdvancedWeights)
}
func (heuristicRanker) Version() string { return "heuristic-v1" }

// searchAdvanced runs the two-stage advanced funnel (§1.7): SQL stage-1 recall
// (bounded by recallWindow) enriched with per-doc text + engagement features, then a Go
// stage-2 rerank over those features plus the personalization signals
// (neighbour/channel affinity, session intent). The ranker is chosen by
// experiment assignment; personalization is applied only for a signed-in,
// personalized=true request, so an anonymous / personalized=false request reduces
// to the deterministic text+engagement ordering.
func (s *Service) searchAdvanced(ctx context.Context, req Request, normalized string) (Response, error) {
	// Experiment assignment + ranker selection.
	subject := experiment.SubjectOf(req.UserID, req.SessionID)
	var assignment *experiment.Assignment
	wantVersion := ""
	if s.exp != nil {
		if a, ok := s.exp.Assign(ExperimentKey, subject); ok {
			assignment = &a
			wantVersion = a.ModelVersion
		}
	}
	ranker, servedVersion := s.pickRanker(wantVersion)

	resp := Response{Query: req.Query, IDs: []Hit{}, ModelVersion: servedVersion, Experiment: assignment}

	offset := paging.Offset(req.Offset)
	limit := paging.Limit(req.Limit, defaultLimit, maxLimit)
	// The recall window follows the requested page (see recallWindow): paging
	// past the base block widens the recall instead of running off its end, which
	// is what used to turn page ~26 into a permanently empty result set.
	window := recallWindow(offset, limit)

	// Recall one row past the window purely as an overflow probe — it tells us
	// candidates remain beyond the window, and is dropped before ranking so the
	// ranked set is exactly the window.
	rows, err := s.q.SearchAdvancedRecall(ctx, sqlcgen.SearchAdvancedRecallParams{
		Query:         normalized,
		HideSensitive: req.HideSensitive,
		Tag:           pgconv.OptStr(req.Tag),
		Category:      pgconv.OptStr(req.Category),
		Language:      pgconv.OptStr(req.Language),
		License:       pgconv.OptStr(req.License),
		Lim:           int32(window + 1),
	})
	if err != nil {
		return Response{}, err
	}
	truncated := len(rows) > window
	if truncated {
		rows = rows[:window]
	}
	// Total is the recall set the reranker sees. Exact when the recall came back
	// under the window, a lower bound when the probe fired.
	resp.Total = intPtr(len(rows))
	resp.TotalIsLowerBound = truncated
	if len(rows) == 0 {
		return resp, nil
	}

	// Base + engagement features per candidate.
	now := time.Now()
	docs := make([]ranking.Doc, 0, len(rows))
	candIDs := make([]uuid.UUID, 0, len(rows))
	channelOf := make(map[uuid.UUID]uuid.UUID, len(rows))
	for _, r := range rows {
		f := ranking.Features{
			TextRank:          r.TsRank,
			TrgmSim:           r.TrgmSim,
			ExactFlags:        r.ExactFlags,
			Views:             float64(r.Views),
			AgeDays:           rankutil.AgeDays(r.PublishedAt, r.SourceUpdatedAt, now),
			Impressions:       float64(r.Impressions),
			Clicks:            float64(r.Clicks),
			MeaningfulWatches: float64(r.MeaningfulWatches),
			// Language is a hard SQL filter in search, so language_match is neutral
			// here (it is a soft signal only in recommendations).
			LanguageMatch: false,
		}
		chID := ""
		if ch, ok := pgconv.UUIDValue(r.ChannelID); ok {
			chID = ch.String()
			channelOf[r.VideoID] = ch
		}
		docs = append(docs, ranking.Doc{VideoID: r.VideoID.String(), ChannelID: chID, Features: f})
		candIDs = append(candIDs, r.VideoID)
	}

	// Personalization: only for a signed-in, personalized request.
	if req.Personalized {
		if uid, perr := uuid.Parse(req.UserID); perr == nil {
			if err := s.applyPersonalAffinity(ctx, uid, candIDs, channelOf, docs); err != nil {
				return Response{}, err
			}
		}
	}

	// Session intent: co-visitation overlap of candidates with the session's
	// recent videos (best-effort; needs a session + the Redis reader).
	if req.SessionID != "" && s.session != nil {
		if err := s.applySessionIntent(ctx, req.SessionID, candIDs, docs); err != nil {
			return Response{}, err
		}
	}

	ranked := ranker.Rerank(docs)

	// Apply offset/limit in Go over the reranked order.
	hits := make([]Hit, 0, limit)
	for i := offset; i < len(ranked) && len(hits) < limit; i++ {
		hits = append(hits, Hit{VideoID: ranked[i].VideoID, Score: ranked[i].Score})
	}
	resp.IDs = hits
	// A further page is servable when ranked candidates remain inside this
	// window, or when the probe fired AND the window still has room to grow. At
	// the ceiling HasMore goes false — paging stops, and TotalIsLowerBound is
	// what tells the client the corpus was not exhausted.
	resp.HasMore = offset+len(hits) < len(ranked) || (truncated && window < maxRecallLimit)
	return resp, nil
}

// recallWindow returns the stage-1 recall size for a page: the smallest whole
// number of recallLimit-sized blocks that covers offset+limit, capped at
// maxRecallLimit.
//
// Quantising to blocks rather than growing per request keeps the window — and
// therefore the reranked order — identical for every page inside a block, so a
// client paging forward only risks a reshuffle at the three block boundaries
// instead of on every request. That reshuffle is inherent to reranking in Go
// after a capped SQL recall: widening the window admits new candidates that the
// ranker may interleave with ones already shown. Widening is still strictly
// better than the alternative it replaces, which was returning nothing at all
// past the first block.
func recallWindow(offset, limit int) int {
	want := offset + limit
	// A negative `want` means offset+limit overflowed; fall back to the base block.
	if want <= recallLimit {
		return recallLimit
	}
	if want >= maxRecallLimit {
		return maxRecallLimit
	}
	blocks := (want + recallLimit - 1) / recallLimit
	return blocks * recallLimit
}

// pickRanker returns the reranker + reported version for the routed model version.
func (s *Service) pickRanker(wantVersion string) (ranking.Ranker, string) {
	if s.ranker != nil {
		return s.ranker.RankerFor(wantVersion)
	}
	h := heuristicRanker{}
	return h, h.Version()
}

// applyPersonalAffinity fills the PersonalAffinity + ChannelAffinity features from
// the user's watch projection (neighbour affinity) and channel affinity.
func (s *Service) applyPersonalAffinity(ctx context.Context, uid uuid.UUID, candIDs []uuid.UUID, channelOf map[uuid.UUID]uuid.UUID, docs []ranking.Doc) error {
	affRows, err := s.q.NeighborAffinity(ctx, sqlcgen.NeighborAffinityParams{UserID: uid, Candidates: candIDs})
	if err != nil {
		return err
	}
	aff := make(map[string]float64, len(affRows))
	for _, r := range affRows {
		aff[r.VideoID.String()] = r.Affinity
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
	for i := range docs {
		docs[i].Features.PersonalAffinity = aff[docs[i].VideoID]
		if ch, ok := channelOf[candIDs[i]]; ok {
			docs[i].Features.ChannelAffinity = chanAff[ch]
		}
	}
	return nil
}

// applySessionIntent fills the SessionIntent feature from the co-visitation
// overlap of candidates with the session's recent videos.
func (s *Service) applySessionIntent(ctx context.Context, sessionID string, candIDs []uuid.UUID, docs []ranking.Doc) error {
	seeds := rankutil.ParseUUIDs(s.session.SessionVideos(ctx, sessionID))
	if len(seeds) == 0 {
		return nil
	}
	rows, err := s.q.NeighborScoresFromSeeds(ctx, sqlcgen.NeighborScoresFromSeedsParams{Seeds: seeds, Candidates: candIDs})
	if err != nil {
		return err
	}
	intent := make(map[string]float64, len(rows))
	for _, r := range rows {
		intent[r.VideoID.String()] = r.Score
	}
	for i := range docs {
		docs[i].Features.SessionIntent = intent[docs[i].VideoID]
	}
	return nil
}
