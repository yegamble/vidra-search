package event

import "encoding/json"

// CollectsHistory is the single decision point for the §1.5 history-collection
// rule: an event may write the DURABLE personal projections (user_search_history,
// user_watch_projection) only when it is a behavioral type that carries
// allow_history=true AND is attributable to a signed-in user_id. Everything else
// — the raw query_log/behavior_events ledgers, ephemeral session context, and
// global trending — is populated regardless and is anonymized/pruned separately.
//
// The intake path uses exactly this predicate; exposing it lets the rule be
// unit-tested without a database (the CRITICAL guarantee: an event without
// allow_history NEVER writes a history/projection row).
func CollectsHistory(eventType string, payload json.RawMessage) bool {
	switch eventType {
	case TypeSearchSubmitted:
		var p searchSubmittedPayload
		if json.Unmarshal(payload, &p) != nil {
			return false
		}
		return p.AllowHistory && p.UserID != nil
	case TypeVideoPlayStarted:
		var p playStartedPayload
		if json.Unmarshal(payload, &p) != nil {
			return false
		}
		return p.AllowHistory && p.UserID != nil
	case TypeVideoCompleted:
		var p videoCompletedPayload
		if json.Unmarshal(payload, &p) != nil {
			return false
		}
		return p.AllowHistory && p.UserID != nil
	default:
		return false
	}
}
