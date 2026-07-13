// Package index holds the document eligibility rules — the ONE place that
// decides whether a video is statically visible in search results. vidra-core
// owns per-viewer visibility (mutes/blocks); this service only bakes in the
// static gate: a video is eligible when it is public AND published. Everything
// else is suppressed with a machine-readable reason so operators can tell why a
// document is hidden.
package index

// Eligible reports whether a video with the given privacy and lifecycle state is
// statically visible, and — when it is not — the suppression reason to record.
// The reason names the failing dimension (privacy first, then state) so a
// later change that flips only one dimension is diagnosable.
func Eligible(privacy, state string) (eligible bool, suppressedReason string) {
	if privacy != "public" {
		return false, "privacy_" + safe(privacy)
	}
	if state != "published" {
		return false, "state_" + safe(state)
	}
	return true, ""
}

// safe substitutes a placeholder for an empty dimension so the reason is never a
// dangling "privacy_" / "state_".
func safe(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
