package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// ConfigHashLength is the number of hex characters from the SHA256 digest
// used as the short operational config hash.
const ConfigHashLength = 12

// ComputeHash returns a short hex prefix of SHA256 computed over a canonical
// form of the effective configuration. The canonical form is produced by
// sorting Targets by Name and serializing the struct to JSON (which emits
// map keys in alphabetical order). The caller must pass a config that has
// already been through applyDefaults and validate, so the hash reflects the
// configuration the agent actually runs on, not the raw YAML bytes.
func ComputeHash(cfg *Config) (string, error) {
	canonical := canonicalize(cfg)

	data, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("marshaling canonical config: %w", err)
	}

	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:ConfigHashLength], nil
}

// canonicalize returns a deep copy of cfg with Targets sorted by Name so the
// hash is invariant to target ordering in the YAML file. The JSON round-trip
// produces an independent deep copy of all map/slice fields (Tags, Headers,
// etc.) so the caller can safely mutate the result without affecting the
// original config.
func canonicalize(cfg *Config) Config {
	b, err := json.Marshal(cfg)
	if err != nil {
		// Should never happen for a validated Config; return a shallow
		// copy as a best-effort fallback.
		return Config{Agent: cfg.Agent}
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		return Config{Agent: cfg.Agent}
	}

	sort.Slice(out.Targets, func(i, j int) bool {
		return out.Targets[i].Name < out.Targets[j].Name
	})

	return out
}

// HashTarget returns a short hex prefix of SHA256 computed over a canonical
// JSON representation of a single TargetConfig. This is used for diff-based
// reload comparisons and is robust to unexported field additions.
func HashTarget(t *TargetConfig) (string, error) {
	data, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("marshaling target config: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:ConfigHashLength], nil
}

// Equal reports whether two TargetConfig values are semantically equal by
// comparing their canonical JSON hashes. This avoids reflect.DeepEqual and
// is safe against unexported field additions.
func (t TargetConfig) Equal(other TargetConfig) bool {
	h1, err1 := HashTarget(&t)
	h2, err2 := HashTarget(&other)
	if err1 != nil || err2 != nil {
		return false
	}
	return h1 == h2
}
