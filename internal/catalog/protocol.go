// Package catalog defines canonical catalog observations. Observation generations
// order source snapshots, not source mutation commits or transport delivery.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/google/uuid"
)

const (
	ProtocolVersion = 2
	ScopeLocal      = "local"
	// MaxRequestBytes includes the encoded envelope, not only document payloads.
	MaxRequestBytes = 1 << 20
)

// Generation is a positive int64 encoded as a decimal JSON string so a status
// consumer cannot silently round a fence above JavaScript's exact integer range.
type Generation int64

func (g Generation) MarshalJSON() ([]byte, error) {
	if g <= 0 {
		return nil, fmt.Errorf("catalog: generation must be positive")
	}
	return json.Marshal(strconv.FormatInt(int64(g), 10))
}

func (g *Generation) UnmarshalJSON(data []byte) error {
	var wire string
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("catalog: generation must be a decimal string: %w", err)
	}
	n, err := strconv.ParseInt(wire, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != wire {
		return fmt.Errorf("catalog: invalid generation")
	}
	*g = Generation(n)
	return nil
}

// Identity fences one observation to an explicitly initialized ownership scope.
// Remote ownership is intentionally unsupported until it has its own observer.
type Identity struct {
	Scope      string     `json:"scope"`
	Epoch      uuid.UUID  `json:"epoch"`
	Generation Generation `json:"generation"`
}

func (v Identity) Validate() error {
	if v.Scope != ScopeLocal {
		return fmt.Errorf("catalog: unsupported scope")
	}
	if v.Epoch == uuid.Nil || v.Generation <= 0 {
		return fmt.Errorf("catalog: missing observation fence")
	}
	return nil
}

// MembershipDigest hashes sorted canonical UUIDs, each followed by a newline.
// Duplicate IDs are invalid membership, not extra evidence of completeness.
// The caller's page order is preserved for immutable page replay comparisons.
func MembershipDigest(ids []uuid.UUID) (string, error) {
	ordered := make([]string, 0, len(ids))
	seen := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		if id == uuid.Nil || seen[id] {
			return "", fmt.Errorf("catalog: invalid or duplicate membership ID")
		}
		seen[id] = true
		ordered = append(ordered, id.String())
	}
	sort.Strings(ordered)
	digest := sha256.New()
	for _, id := range ordered {
		_, _ = digest.Write([]byte(id + "\n"))
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
