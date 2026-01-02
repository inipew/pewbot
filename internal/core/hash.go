package core

import (
	"encoding/json"
	"hash/fnv"
)

// hashBytes returns a stable 64-bit hash of bytes. Empty input returns 0.
func hashBytes(b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}

// canonicalHashJSON hashes JSON after canonicalizing it (whitespace and key order changes won't matter).
// If raw is not valid JSON, it falls back to hashing raw bytes.
func canonicalHashJSON(raw json.RawMessage) uint64 {
	if len(raw) == 0 {
		return 0
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return hashBytes(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return hashBytes(raw)
	}
	return hashBytes(b)
}
