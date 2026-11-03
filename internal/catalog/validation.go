package catalog

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

const (
	TypeSnapshotPage = "catalog.snapshot.page"
	TypeSnapshotEnd  = "catalog.snapshot.end"
)

type Manifest struct {
	PageCount        int32  `json:"page_count"`
	DocumentCount    int64  `json:"document_count"`
	MembershipSHA256 string `json:"membership_sha256"`
	PagesSHA256      string `json:"pages_sha256"`
}

// Validate checks shape only. Completion must also verify exact indexes,
// distinct document IDs and both digests against the frozen pages.
func (m Manifest) Validate() error {
	if m.PageCount < 0 || m.DocumentCount < 0 || int64(m.PageCount) > m.DocumentCount || (m.PageCount == 0) != (m.DocumentCount == 0) {
		return fmt.Errorf("catalog: inconsistent manifest counts")
	}
	for _, digest := range []string{m.MembershipSHA256, m.PagesSHA256} {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != 32 || strings.ToLower(digest) != digest {
			return fmt.Errorf("catalog: manifest requires lowercase SHA-256 digests")
		}
	}
	const empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if m.PageCount == 0 && (m.MembershipSHA256 != empty || m.PagesSHA256 != empty) {
		return fmt.Errorf("catalog: empty manifest requires empty digests")
	}
	return nil
}

// ObjectFields requires exact, unique field names and explicit required values.
// Struct decoding alone accepts case aliases and silently takes the last duplicate.
// This validates one object level; callers must validate each nested object too.
func ObjectFields(raw []byte, keys, nullable []string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("catalog: expected object")
	}
	fields := make(map[string]json.RawMessage, len(keys))
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok || !slices.Contains(keys, key) || fields[key] != nil {
			return nil, fmt.Errorf("catalog: unknown or duplicate field")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return nil, fmt.Errorf("catalog: incomplete object")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("catalog: trailing JSON content")
	}
	for _, key := range keys {
		value, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) && !slices.Contains(nullable, key) {
			return nil, fmt.Errorf("catalog: missing or null field %s", key)
		}
	}
	return fields, nil
}
