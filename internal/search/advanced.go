package search

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vidra/vidra-search/internal/experiment"
	"github.com/vidra/vidra-search/internal/pgconv"
	"github.com/vidra/vidra-search/internal/ranking"
	"github.com/vidra/vidra-search/internal/store/sqlcgen"
)

// heuristicRanker is the built-in fallback used when no RankerProvider is wired.
type heuristicRanker struct{}

func (heuristicRanker) Rerank(docs []ranking.Doc) []ranking.Ranked {
	return ranking.Rerank(docs, ranking.DefaultAdvancedWeights)
}
func (heuristicRanker) Version() string { return "heuristic-v1" }

// searchAdvanced runs the two-stage advanced funnel (§1.7): SQL stage-1 recall
// (≤ recallLimit) enriched with per-doc text + engagement features, then a Go
// stage-2 rerank over those features plus the personalization signals
// (neighbour/channel affinity, session intent). The ranker is chosen by
// experiment assignment; personalization is applied only for a signed-in,
// personalized=true request, so an anonymous / personalized=false request reduces
// to the deterministic text+engagement ordering.
func (s *Service) searchAdvanced(ctx context.Context, req Request, normalized string) (Response, error) {
	// Experiment assignment + ranker selection.
	subject := subjectOf(req.UserID, req.SessionID)
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

	rows, err := s.q.SearchAdvancedRecall(ctx, sqlcgen.SearchAdvancedRecallParams{
		Query:         normalized,
		HideSensitive: req.HideSensitive,
		Tag:           optStr(req.Tag),
		Category:      optStr(req.Category),
		Language:      optStr(req.Language),
		Lim:           recallLimit,
	})
	if err != nil {
		return Response{}, err
	}
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
			AgeDays:           ageDays(r.PublishedAt, r.SourceUpdatedAt, now),
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
	offset := req.Offset
	if offset < 0 {
		offset = 0
	}
	limit := clampLimit(req.Limit)
	hits := make([]Hit, 0, limit)
	for i := offset; i < len(ranked) && len(hits) < limit; i++ {
		hits = append(hits, Hit{VideoID: ranked[i].VideoID, Score: ranked[i].Score})
	}
	resp.IDs = hits
	return resp, nil
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
	seeds := parseUUIDs(s.session.SessionVideos(ctx, sessionID))
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

// ageDays returns the document's age in days from published_at (falling back to
// source_updated_at), clamped at 0 for a future timestamp.
func ageDays(publishedAt pgtype.Timestamptz, sourceUpdatedAt, now time.Time) float64 {
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

// subjectOf returns the experiment subject: user id, else session id, else "".
func subjectOf(userID, sessionID string) string {
	if userID != "" {
		return userID
	}
	return sessionID
}

// parseUUIDs parses a slice of id strings, dropping any that do not parse.
func parseUUIDs(ss []string) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ss))
	for _, s := range ss {
		if id, err := uuid.Parse(s); err == nil {
			out = append(out, id)
		}
	}
	return out
}
