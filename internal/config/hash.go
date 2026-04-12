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
// hash is invariant to target ordering in the YAML file. Tags and Headers
// maps are hashed deterministically by encoding/json, which sorts map keys.
func canonicalize(cfg *Config) Config {
	out := Config{
		Agent:   cfg.Agent,
		Targets: make([]TargetConfig, len(cfg.Targets)),
	}
	copy(out.Targets, cfg.Targets)

	sort.Slice(out.Targets, func(i, j int) bool {
		return out.Targets[i].Name < out.Targets[j].Name
	})

	return out
}
