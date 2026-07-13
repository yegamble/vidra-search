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
