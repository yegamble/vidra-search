package event

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestCollectsHistoryRequiresAllowFlagAndUser is the CRITICAL privacy guarantee:
// an event may write the durable personal projections ONLY when it carries
// allow_history=true AND is attributable to a signed-in user.
func TestCollectsHistoryRequiresAllowFlagAndUser(t *testing.T) {
	uid := uuid.New()
	mk := func(m map[string]any) json.RawMessage {
		b, _ := json.Marshal(m)
		return b
	}

	cases := []struct {
		name    string
		typ     string
		payload map[string]any
		want    bool
	}{
		{"submitted with flag + user", TypeSearchSubmitted, map[string]any{"query": "q", "user_id": uid.String(), "allow_history": true}, true},
		{"submitted without flag", TypeSearchSubmitted, map[string]any{"query": "q", "user_id": uid.String()}, false},
		{"submitted flag but anonymous", TypeSearchSubmitted, map[string]any{"query": "q", "allow_history": true}, false},
		{"play with flag + user", TypeVideoPlayStarted, map[string]any{"video_id": uuid.New().String(), "user_id": uid.String(), "allow_history": true}, true},
		{"play without flag", TypeVideoPlayStarted, map[string]any{"video_id": uuid.New().String(), "user_id": uid.String()}, false},
		{"completed with flag + user", TypeVideoCompleted, map[string]any{"video_id": uuid.New().String(), "user_id": uid.String(), "allow_history": true}, true},
		{"result_clicked never collects", TypeSearchResultClicked, map[string]any{"query": "q", "video_id": uuid.New().String(), "user_id": uid.String(), "allow_history": true}, false},
		{"watch_progress never collects at intake", TypeVideoWatchProgress, map[string]any{"video_id": uuid.New().String(), "user_id": uid.String(), "allow_history": true}, false},
	}
	for _, c := range cases {
		if got := CollectsHistory(c.typ, mk(c.payload)); got != c.want {
			t.Errorf("%s: CollectsHistory=%v want %v", c.name, got, c.want)
		}
	}
}

// TestResultFailedIsAlwaysArray guards the contract shape: the failed field must
// serialize as [] (never null) so vidra-core's contract can rely on an array.
func TestResultFailedIsAlwaysArray(t *testing.T) {
	res := Result{Failed: []Failure{}}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"failed":[]`) {
		t.Fatalf("all-success result must render failed as an empty array, got %s", b)
	}
}

// TestFeedsWatchProjectionFollowsPersonalization is the A13 opt-out ruling's
// search half: user_watch_projection is the durable per-user vector every
// PERSONALIZED generator keys off, and it was gated by allow_history — the
// HISTORY control. A user who kept their search history and switched
// personalization off therefore had a projection built for them that no feature
// they had enabled could read. vidra-core now sends allow_personalization for
// exactly this store, and each control feeds only its own.
func TestFeedsWatchProjectionFollowsPersonalization(t *testing.T) {
	uid := uuid.New()
	vid := uuid.New().String()
	mk := func(m map[string]any) json.RawMessage {
		b, _ := json.Marshal(m)
		return b
	}

	cases := []struct {
		name    string
		typ     string
		payload map[string]any
		want    bool
	}{
		{"play: personalization on, history off", TypeVideoPlayStarted,
			map[string]any{"video_id": vid, "user_id": uid.String(), "allow_history": false, "allow_personalization": true}, true},
		{"play: personalization off, history on — history must not feed the projection", TypeVideoPlayStarted,
			map[string]any{"video_id": vid, "user_id": uid.String(), "allow_history": true, "allow_personalization": false}, false},
		{"completed: personalization off, history on", TypeVideoCompleted,
			map[string]any{"video_id": vid, "user_id": uid.String(), "allow_history": true, "allow_personalization": false}, false},
		{"play: personalization on but unattributed — no user, no projection", TypeVideoPlayStarted,
			map[string]any{"video_id": vid, "allow_personalization": true}, false},
		// Compatibility: a vidra-core that predates the field sends neither value,
		// and the projection must keep behaving exactly as it did rather than
		// silently stopping for every user mid-rolling-upgrade.
		{"play: field absent falls back to allow_history=true", TypeVideoPlayStarted,
			map[string]any{"video_id": vid, "user_id": uid.String(), "allow_history": true}, true},
		{"play: field absent falls back to allow_history=false", TypeVideoPlayStarted,
			map[string]any{"video_id": vid, "user_id": uid.String(), "allow_history": false}, false},
		{"submitted never feeds the projection", TypeSearchSubmitted,
			map[string]any{"query": "q", "user_id": uid.String(), "allow_personalization": true}, false},
	}
	for _, c := range cases {
		if got := FeedsWatchProjection(c.typ, mk(c.payload)); got != c.want {
			t.Errorf("%s: FeedsWatchProjection=%v want %v", c.name, got, c.want)
		}
	}
}

// TestCollectsSearchHistoryIsTheHistoryControlAlone: the search-history store
// stays on allow_history, and allow_personalization can neither grant nor
// withhold it.
func TestCollectsSearchHistoryIsTheHistoryControlAlone(t *testing.T) {
	uid := uuid.New()
	mk := func(m map[string]any) json.RawMessage {
		b, _ := json.Marshal(m)
		return b
	}
	if !CollectsSearchHistory(TypeSearchSubmitted, mk(map[string]any{"query": "q", "user_id": uid.String(), "allow_history": true, "allow_personalization": false})) {
		t.Error("history on + personalization off must still keep the user's own search history")
	}
	if CollectsSearchHistory(TypeSearchSubmitted, mk(map[string]any{"query": "q", "user_id": uid.String(), "allow_history": false, "allow_personalization": true})) {
		t.Error("personalization must not grant search-history collection")
	}
	if CollectsSearchHistory(TypeVideoPlayStarted, mk(map[string]any{"video_id": uuid.New().String(), "user_id": uid.String(), "allow_history": true})) {
		t.Error("a play event never writes search history")
	}
}
