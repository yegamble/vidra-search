package event

import "encoding/json"

// CollectsHistory is the single decision point for the §1.5 history-collection
// rule: an event may write EITHER durable personal projection
// (user_search_history, user_watch_projection) only when it is attributable to
// a signed-in user_id AND that store's own consent flag is set. Everything else
// — the raw query_log/behavior_events ledgers, ephemeral session context, and
// global trending — is populated regardless and is anonymized/pruned separately.
//
// The intake path uses exactly these predicates; exposing them lets the rule be
// unit-tested without a database (the CRITICAL guarantee: an event without its
// flag NEVER writes a history/projection row).
//
// It is a union of the two stores' rules, which since the A13 opt-out ruling are
// no longer the same rule:
//
//   - user_search_history follows allow_history — the user's own search-history
//     page, and nothing else.
//   - user_watch_projection follows allow_personalization — the durable per-user
//     watch vector every PERSONALIZED generator keys off. Gating it on
//     allow_history crossed the streams: a user with history on and both
//     personalization controls off had a projection built for them that no
//     feature they had enabled could ever read.
func CollectsHistory(eventType string, payload json.RawMessage) bool {
	return CollectsSearchHistory(eventType, payload) || FeedsWatchProjection(eventType, payload)
}

// CollectsSearchHistory reports whether this event may write
// search.user_search_history: a search.submitted carrying allow_history=true and
// a user_id.
func CollectsSearchHistory(eventType string, payload json.RawMessage) bool {
	if eventType != TypeSearchSubmitted {
		return false
	}
	var p searchSubmittedPayload
	if json.Unmarshal(payload, &p) != nil {
		return false
	}
	return p.AllowHistory && p.UserID != nil
}

// FeedsWatchProjection reports whether this event may write
// search.user_watch_projection: a play/completed carrying a user_id and
// personalization consent.
func FeedsWatchProjection(eventType string, payload json.RawMessage) bool {
	switch eventType {
	case TypeVideoPlayStarted:
		var p playStartedPayload
		if json.Unmarshal(payload, &p) != nil {
			return false
		}
		return p.personalizationAllowed() && p.UserID != nil
	case TypeVideoCompleted:
		var p videoCompletedPayload
		if json.Unmarshal(payload, &p) != nil {
			return false
		}
		return p.personalizationAllowed() && p.UserID != nil
	default:
		return false
	}
}

// projectionConsent resolves allow_personalization with the compatibility
// fallback that keeps a rolling upgrade honest: a vidra-core older than the A13
// ruling sends no such field, and ABSENT must mean "behave exactly as before"
// (follow allow_history) rather than "false", which would silently stop building
// every user's projection for as long as the two versions ran side by side.
// Once both sides are current the field is always present and the fallback is
// dead code — deliberately, because the failure it prevents is invisible.
func projectionConsent(allowPersonalization *bool, allowHistory bool) bool {
	if allowPersonalization != nil {
		return *allowPersonalization
	}
	return allowHistory
}

func (p playStartedPayload) personalizationAllowed() bool {
	return projectionConsent(p.AllowPersonalization, p.AllowHistory)
}

func (p videoCompletedPayload) personalizationAllowed() bool {
	return projectionConsent(p.AllowPersonalization, p.AllowHistory)
}
