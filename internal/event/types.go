// Package event ingests the domain and behavioral events vidra-core delivers to
// POST /internal/v1/events and applies their side effects to the search corpus.
// Every event is deduped through the events_inbox ledger (idempotent intake);
// domain events mutate documents/config synchronously in a transaction, while
// behavioral events are counted and dropped in W1 (persisted in W2).
package event

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Envelope is one event as delivered in the batch. Payload is decoded per type.
type Envelope struct {
	EventID       uuid.UUID       `json:"event_id"`
	Type          string          `json:"type"`
	OccurredAt    time.Time       `json:"occurred_at"`
	SchemaVersion int             `json:"schema_version"`
	Payload       json.RawMessage `json:"payload"`
}

// Failure describes one event that could not be applied.
type Failure struct {
	EventID string `json:"event_id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Result is the batch outcome returned to vidra-core.
type Result struct {
	Accepted   int       `json:"accepted"`
	Duplicates int       `json:"duplicates"`
	Failed     []Failure `json:"failed"`
}

// Event type identifiers (schema_version 1).
const (
	TypeVideoUpsert    = "video.upsert"
	TypeVideoSuppress  = "video.suppress"
	TypeVideoStats     = "video.stats"
	TypeChannelUpsert  = "channel.upsert"
	TypeChannelDelete  = "channel.delete"
	TypeUserSuppress   = "user.suppress"
	TypeUserHistoryDel = "user.history_deleted"
	TypeConfigUpdated  = "search.config_updated"
	TypeReconcileBegin = "reconcile.begin"
	TypeReconcilePage  = "reconcile.page"
	TypeReconcileEnd   = "reconcile.end"

	// Behavioral types (W1: counted + dropped; W2 persists them).
	TypeSearchSubmitted     = "search.submitted"
	TypeSearchSuggShown     = "search.suggestions_shown"
	TypeSearchSuggSelected  = "search.suggestion_selected"
	TypeSearchResultClicked = "search.result_clicked"
	TypeVideoPlayStarted    = "video.play_started"
	TypeVideoWatchProgress  = "video.watch_progress"
	TypeVideoCompleted      = "video.completed"
)

// knownTypes bounds the metric `type` label to a fixed set (unknown → "unknown").
var knownTypes = map[string]bool{
	TypeVideoUpsert: true, TypeVideoSuppress: true, TypeVideoStats: true,
	TypeChannelUpsert: true, TypeChannelDelete: true, TypeUserSuppress: true,
	TypeUserHistoryDel: true, TypeConfigUpdated: true,
	TypeReconcileBegin: true, TypeReconcilePage: true, TypeReconcileEnd: true,
	TypeSearchSubmitted: true, TypeSearchSuggShown: true, TypeSearchSuggSelected: true,
	TypeSearchResultClicked: true, TypeVideoPlayStarted: true,
	TypeVideoWatchProgress: true, TypeVideoCompleted: true,
}

// metricType maps an event type to a bounded metric label.
func metricType(t string) string {
	if knownTypes[t] {
		return t
	}
	return "unknown"
}

// isBehavioral reports whether a type is a behavioral (analytics) event.
func isBehavioral(t string) bool {
	switch t {
	case TypeSearchSubmitted, TypeSearchSuggShown, TypeSearchSuggSelected,
		TypeSearchResultClicked, TypeVideoPlayStarted, TypeVideoWatchProgress,
		TypeVideoCompleted:
		return true
	}
	return false
}

// --- payload shapes (schema_version 1) ---

// VideoDoc is the video document carried by video.upsert and reconcile.page.
// Eligibility is derived here from privacy+state; the source privacy/state are
// not stored (only the derived eligible + suppressed_reason).
type VideoDoc struct {
	ID              uuid.UUID  `json:"id"`
	Kind            string     `json:"kind"`
	ChannelID       *uuid.UUID `json:"channel_id"`
	ChannelHandle   *string    `json:"channel_handle"`
	ChannelName     *string    `json:"channel_name"`
	OwnerID         *uuid.UUID `json:"owner_id"`
	Title           string     `json:"title"`
	Description     string     `json:"description"`
	Tags            []string   `json:"tags"`
	Category        *string    `json:"category"`
	Language        *string    `json:"language"`
	DurationSeconds *int32     `json:"duration_seconds"`
	IsSensitive     bool       `json:"is_sensitive"`
	Privacy         string     `json:"privacy"`
	State           string     `json:"state"`
	PublishedAt     *time.Time `json:"published_at"`
	CreatedAt       *time.Time `json:"created_at"`
	UpdatedAt       *time.Time `json:"updated_at"`
	Views           int64      `json:"views"`
	Likes           int32      `json:"likes"`
}

type videoUpsertPayload struct {
	Video VideoDoc `json:"video"`
}

type videoSuppressPayload struct {
	VideoID uuid.UUID `json:"video_id"`
	Reason  string    `json:"reason"`
}

type videoStatsPayload struct {
	VideoID uuid.UUID `json:"video_id"`
	Views   int64     `json:"views"`
	Likes   int32     `json:"likes"`
}

type channelUpsertPayload struct {
	Channel struct {
		ID          uuid.UUID  `json:"id"`
		Handle      string     `json:"handle"`
		DisplayName string     `json:"display_name"`
		OwnerID     *uuid.UUID `json:"owner_id"`
	} `json:"channel"`
}

type channelDeletePayload struct {
	ChannelID uuid.UUID `json:"channel_id"`
}

type userSuppressPayload struct {
	UserID   uuid.UUID `json:"user_id"`
	Unlisted bool      `json:"unlisted"`
}

type configUpdatedPayload struct {
	Settings map[string]any `json:"settings"`
}

type reconcilePagePayload struct {
	RunID  uuid.UUID  `json:"run_id"`
	Videos []VideoDoc `json:"videos"`
}

type reconcileEndPayload struct {
	RunID uuid.UUID `json:"run_id"`
	Total int       `json:"total"`
}
