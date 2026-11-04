package catalog

import (
	"time"

	"github.com/google/uuid"
)

type Limits struct {
	MaxRequestBytes  int64 `json:"max_request_bytes"`
	MaxSnapshotBytes int64 `json:"max_snapshot_bytes"`
	MaxDocuments     int64 `json:"max_documents"`
	MaxPages         int64 `json:"max_pages"`
}

// Status is receiver evidence, not proof of a retained producer manifest match.
// Counter strings deliberately support zero separately from positive Generation.
type Status struct {
	ProtocolVersion           int        `json:"protocol_version"`
	SupportedScopes           []string   `json:"supported_scopes"`
	SupportedEventTypes       []string   `json:"supported_event_types"`
	Limits                    Limits     `json:"limits"`
	Scope                     string     `json:"scope"`
	Epoch                     *uuid.UUID `json:"epoch"`
	Ready                     bool       `json:"ready"`
	HighestObservedGeneration string     `json:"highest_observed_generation"`
	CompletedGeneration       string     `json:"completed_generation"`
	CompletedAt               *time.Time `json:"completed_at"`
	CompletedManifest         *Manifest  `json:"completed_manifest"`
	StagedSnapshots           int32      `json:"staged_snapshots"`
	OldestStagedAt            *time.Time `json:"oldest_staged_at"`
}
