package config

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"go.yaml.in/yaml/v4"
)

// **Validates: Requirements 1.1, 2.1**
// Property 1: Configuration round-trip
// For any valid Config struct, serializing to YAML and then parsing back
// SHALL produce an equivalent Config struct.

// genDuration generates a time.Duration as whole seconds (1s–300s) to avoid
// sub-second precision issues with YAML serialization.
func genDuration(minSec, maxSec int) gopter.Gen {
	return gen.IntRange(minSec, maxSec).Map(func(s int) time.Duration {
		return time.Duration(s) * time.Second
	})
}

// genAlphaString generates a non-empty alphanumeric string of length 1–maxLen.
func genAlphaString(maxLen int) gopter.Gen {
	return gen.IntRange(1, maxLen).FlatMap(func(v interface{}) gopter.Gen {
		n := v.(int)
		return gen.SliceOfN(n, gen.AlphaChar()).Map(func(chars []rune) string {
			return string(chars)
		})
	}, reflect.TypeOf(""))
}

// genProbeType picks a random valid ProbeType.
func genProbeType() gopter.Gen {
	probeTypes := []ProbeType{
		ProbeTypeTCP, ProbeTypeHTTP, ProbeTypeICMP, ProbeTypeMTU,
		ProbeTypeDNS, ProbeTypeTLSCert, ProbeTypeHTTPBody, ProbeTypeProxy,
	}
	return gen.IntRange(0, len(probeTypes)-1).Map(func(i int) ProbeType {
		return probeTypes[i]
	})
}

// genLogLevel picks a random valid log level.
func genLogLevel() gopter.Gen {
	levels := []string{"debug", "info", "warn", "error"}
	return gen.IntRange(0, len(levels)-1).Map(func(i int) string {
		return levels[i]
	})
}

// genLogFormat picks a random valid log format.
func genLogFormat() gopter.Gen {
	formats := []string{"text", "json"}
	return gen.IntRange(0, len(formats)-1).Map(func(i int) string {
		return formats[i]
	})
}

// genTags generates a small map of alphanumeric key-value pairs (0–3 entries).
func genTags() gopter.Gen {
	return gen.IntRange(0, 3).FlatMap(func(v interface{}) gopter.Gen {
		n := v.(int)
		if n == 0 {
			return gen.Const(map[string]string{})
		}
		return gen.SliceOfN(n, gen.Struct(reflect.TypeOf(struct {
			K string
			V string
		}{}), map[string]gopter.Gen{
			"K": genAlphaString(8),
			"V": genAlphaString(12),
		})).Map(func(entries []struct {
			K string
			V string
		}) map[string]string {
			m := make(map[string]string, len(entries))
			for _, e := range entries {
				m[e.K] = e.V
			}
			return m
		})
	}, reflect.TypeOf(map[string]string{}))
}

// genHeaders generates a small map of HTTP header key-value pairs (0–2 entries).
func genHeaders() gopter.Gen {
	return gen.IntRange(0, 2).FlatMap(func(v interface{}) gopter.Gen {
		n := v.(int)
		if n == 0 {
			return gen.Const(map[string]string{})
		}
		return gen.SliceOfN(n, gen.Struct(reflect.TypeOf(struct {
			K string
			V string
		}{}), map[string]gopter.Gen{
			"K": genAlphaString(8),
			"V": genAlphaString(12),
		})).Map(func(entries []struct {
			K string
			V string
		}) map[string]string {
			m := make(map[string]string, len(entries))
			for _, e := range entries {
				m[e.K] = e.V
			}
			return m
		})
	}, reflect.TypeOf(map[string]string{}))
}

// genDescendingICMPPayloadSizes generates a descending slice of MTU sizes (2–5 entries).
func genDescendingICMPPayloadSizes() gopter.Gen {
	return gen.IntRange(2, 5).FlatMap(func(v interface{}) gopter.Gen {
		n := v.(int)
		return gen.SliceOfN(n, gen.IntRange(500, 1472)).Map(func(vals []int) []int {
			// Sort descending and ensure strictly descending by deduplicating.
			// Simple approach: generate unique values then sort descending.
			seen := make(map[int]bool)
			unique := make([]int, 0, len(vals))
			for _, v := range vals {
				if !seen[v] {
					seen[v] = true
					unique = append(unique, v)
				}
			}
			// Insertion sort descending.
			for i := 1; i < len(unique); i++ {
				for j := i; j > 0 && unique[j] > unique[j-1]; j-- {
					unique[j], unique[j-1] = unique[j-1], unique[j]
				}
			}
			if len(unique) < 2 {
				// Ensure at least 2 entries for a valid MTU config.
				return []int{1472, 1372}
			}
			return unique
		})
	}, reflect.TypeOf([]int{}))
}

// genDNSQueryType picks a random valid DNS query type.
func genDNSQueryType() gopter.Gen {
	types := []string{"A", "AAAA", "CNAME"}
	return gen.IntRange(0, len(types)-1).Map(func(i int) string {
		return types[i]
	})
}

// genProbeOptions generates ProbeOptions appropriate for the given probe type.
func genProbeOptions(pt ProbeType) gopter.Gen {
	switch pt {
	case ProbeTypeMTU:
		return genDescendingICMPPayloadSizes().Map(func(sizes []int) ProbeOptions {
			return ProbeOptions{
				ICMPPayloadSizes:     sizes,
				ExpectedMinMTU:       sizes[0] + 28,
				MTURetries:           DefaultMTURetries,
				MTUPerAttemptTimeout: DefaultMTUPerAttemptTimeout,
			}
		})
	case ProbeTypeDNS:
		return genDNSQueryType().FlatMap(func(v interface{}) gopter.Gen {
			qt := v.(string)
			return genAlphaString(10).Map(func(name string) ProbeOptions {
				return ProbeOptions{
					DNSQueryName: name + ".example.com",
					DNSQueryType: qt,
				}
			})
		}, reflect.TypeOf(ProbeOptions{}))
	case ProbeTypeProxy:
		return genAlphaString(8).Map(func(host string) ProbeOptions {
			return ProbeOptions{
				ProxyURL: "http://" + host + ":8888",
			}
		})
	case ProbeTypeHTTP, ProbeTypeHTTPBody:
		return genHeaders().Map(func(h map[string]string) ProbeOptions {
			return ProbeOptions{
				Method:  "GET",
				Headers: h,
			}
		})
	case ProbeTypeICMP:
		return gen.IntRange(1, 5).Map(func(count int) ProbeOptions {
			return ProbeOptions{
				PingCount:       count,
				PingIntervalSec: 1.0,
			}
		})
	default:
		// TCP, TLS cert: no special options needed.
		return gen.Const(ProbeOptions{})
	}
}

// genAgentConfig generates a valid AgentConfig.
func genAgentConfig() gopter.Gen {
	return gopter.CombineGens(
		genAlphaString(4),   // port suffix
		genAlphaString(6),   // metrics path suffix
		genDuration(10, 60), // default interval
		genDuration(1, 9),   // default timeout
		genLogLevel(),
		genLogFormat(),
		gen.Bool(), // whether to include default_icmp_payload_sizes
	).Map(func(vals []interface{}) AgentConfig {
		cfg := AgentConfig{
			ListenAddr:      ":" + vals[0].(string),
			MetricsPath:     "/" + vals[1].(string),
			DefaultInterval: vals[2].(time.Duration),
			DefaultTimeout:  vals[3].(time.Duration),
			LogLevel:        vals[4].(string),
			LogFormat:       vals[5].(string),
		}
		if vals[6].(bool) {
			cfg.DefaultICMPPayloadSizes = []int{1472, 1392, 1372, 1272}
		}
		return cfg
	})
}

// genTargetConfig generates a valid TargetConfig with the given index for unique naming.
func genTargetConfig(idx int) gopter.Gen {
	return genProbeType().FlatMap(func(v interface{}) gopter.Gen {
		pt := v.(ProbeType)
		return gopter.CombineGens(
			genAlphaString(8),    // address host
			genDuration(10, 120), // interval
			genDuration(1, 9),    // timeout (always < interval since max 9 < min interval 10)
			genTags(),            // tags
			genProbeOptions(pt),  // probe options
		).Map(func(vals []interface{}) TargetConfig {
			return TargetConfig{
				Name:      fmt.Sprintf("target%d", idx),
				Address:   vals[0].(string) + ".example.com:443",
				ProbeType: pt,
				Interval:  vals[1].(time.Duration),
				Timeout:   vals[2].(time.Duration),
				Tags:      vals[3].(map[string]string),
				ProbeOpts: vals[4].(ProbeOptions),
			}
		})
	}, reflect.TypeOf(TargetConfig{}))
}

// genConfig generates a valid Config with 1–5 targets, each with a unique name.
func genConfig() gopter.Gen {
	return gen.IntRange(1, 5).FlatMap(func(v interface{}) gopter.Gen {
		n := v.(int)
		gens := make([]gopter.Gen, n+1)
		gens[0] = genAgentConfig()
		for i := 0; i < n; i++ {
			gens[i+1] = genTargetConfig(i)
		}
		return gopter.CombineGens(gens...).Map(func(vals []interface{}) Config {
			agent := vals[0].(AgentConfig)
			targets := make([]TargetConfig, n)
			for i := 0; i < n; i++ {
				targets[i] = vals[i+1].(TargetConfig)
			}
			return Config{
				Agent:   agent,
				Targets: targets,
			}
		})
	}, reflect.TypeOf(Config{}))
}

// normalizeProbeOptions converts nil slices/maps to empty equivalents (and vice
// versa) so that reflect.DeepEqual is not tripped by the nil-vs-empty
// distinction that YAML round-tripping introduces.
func normalizeProbeOptions(opts *ProbeOptions) {
	if opts.Headers == nil {
		opts.Headers = map[string]string{}
	}
	if len(opts.Headers) == 0 {
		opts.Headers = map[string]string{}
	}
	if opts.ExpectedStatusCodes == nil {
		opts.ExpectedStatusCodes = []int{}
	}
	if opts.ICMPPayloadSizes == nil {
		opts.ICMPPayloadSizes = []int{}
	}
	if opts.DNSExpectedResults == nil {
		opts.DNSExpectedResults = []string{}
	}
}

// normalizeConfig ensures nil slices/maps are replaced with empty equivalents
// so that reflect.DeepEqual works correctly after a YAML round-trip.
func normalizeConfig(cfg *Config) {
	if cfg.Agent.DefaultICMPPayloadSizes == nil {
		cfg.Agent.DefaultICMPPayloadSizes = []int{}
	}
	if cfg.Agent.AllowedTagKeys == nil {
		cfg.Agent.AllowedTagKeys = []string{}
	}
	for i := range cfg.Targets {
		if cfg.Targets[i].Tags == nil {
			cfg.Targets[i].Tags = map[string]string{}
		}
		normalizeProbeOptions(&cfg.Targets[i].ProbeOpts)
	}
}

// TestPropertyConfigRoundTrip verifies Property 1: for all valid Config structs,
// serializing to YAML and then parsing back produces an equivalent Config struct.
func TestPropertyConfigRoundTrip(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 200
	properties := gopter.NewProperties(parameters)

	properties.Property("yaml.Marshal → yaml.Unmarshal round-trip preserves Config", prop.ForAll(
		func(cfg Config) bool {
			// Normalize the original before serialization to establish a
			// canonical form (nil slices/maps → empty).
			normalizeConfig(&cfg)

			// Serialize to YAML.
			data, err := yaml.Marshal(&cfg)
			if err != nil {
				t.Logf("Marshal error: %v", err)
				return false
			}

			// Parse back from YAML.
			var parsed Config
			if err := yaml.Unmarshal(data, &parsed); err != nil {
				t.Logf("Unmarshal error: %v\nYAML:\n%s", err, string(data))
				return false
			}

			// Normalize the parsed config the same way.
			normalizeConfig(&parsed)

			// Compare: the round-tripped config must equal the original.
			if !reflect.DeepEqual(cfg, parsed) {
				t.Logf("Round-trip mismatch.\nOriginal: %+v\nParsed:   %+v\nYAML:\n%s", cfg, parsed, string(data))
				return false
			}

			return true
		},
		genConfig(),
	))

	properties.TestingRun(t)
}

// **Validates: Requirements 1.2–1.9**
// Property 2: Configuration validation rejects invalid configs
// For all configs that contain at least one validation violation,
// the validate() function returns a non-nil error.

// invalidMutation is a named mutation that introduces exactly one validation
// violation into an otherwise valid Config.
type invalidMutation struct {
	name  string
	apply func(cfg *Config, rng *gopter.GenParameters)
}

// invalidMutations defines all the ways we can break a valid config.
var invalidMutations = []invalidMutation{
	{
		name: "missing name",
		apply: func(cfg *Config, _ *gopter.GenParameters) {
			if len(cfg.Targets) > 0 {
				cfg.Targets[0].Name = ""
			}
		},
	},
	{
		name: "missing address",
		apply: func(cfg *Config, _ *gopter.GenParameters) {
			if len(cfg.Targets) > 0 {
				cfg.Targets[0].Address = ""
			}
		},
	},
	{
		name: "duplicate target names",
		apply: func(cfg *Config, _ *gopter.GenParameters) {
			if len(cfg.Targets) >= 2 {
				cfg.Targets[1].Name = cfg.Targets[0].Name
			} else if len(cfg.Targets) == 1 {
				// Add a second target with the same name.
				dup := cfg.Targets[0]
				cfg.Targets = append(cfg.Targets, dup)
			}
		},
	},
	{
		name: "invalid probe type",
		apply: func(cfg *Config, _ *gopter.GenParameters) {
			if len(cfg.Targets) > 0 {
				cfg.Targets[0].ProbeType = ProbeType("ftp")
			}
		},
	},
	{
		name: "timeout exceeds interval",
		apply: func(cfg *Config, _ *gopter.GenParameters) {
			if len(cfg.Targets) > 0 {
				// Set timeout to interval + 10s so it always exceeds.
				cfg.Targets[0].Interval = 10 * time.Second
				cfg.Targets[0].Timeout = 20 * time.Second
			}
		},
	},
	{
		name: "mtu sizes not descending",
		apply: func(cfg *Config, _ *gopter.GenParameters) {
			if len(cfg.Targets) > 0 {
				cfg.Targets[0].ProbeType = ProbeTypeMTU
				// Ascending order violates the descending requirement.
				cfg.Targets[0].ProbeOpts = ProbeOptions{
					ICMPPayloadSizes: []int{1072, 1272, 1472},
				}
			}
		},
	},
	{
		name: "invalid dns query type",
		apply: func(cfg *Config, _ *gopter.GenParameters) {
			if len(cfg.Targets) > 0 {
				cfg.Targets[0].ProbeType = ProbeTypeDNS
				cfg.Targets[0].ProbeOpts = ProbeOptions{
					DNSQueryName: "example.com",
					DNSQueryType: "MX",
				}
			}
		},
	},
	{
		name: "proxy missing proxy_url",
		apply: func(cfg *Config, _ *gopter.GenParameters) {
			if len(cfg.Targets) > 0 {
				cfg.Targets[0].ProbeType = ProbeTypeProxy
				cfg.Targets[0].ProbeOpts = ProbeOptions{
					ProxyURL: "",
				}
			}
		},
	},
}

// genInvalidConfig generates a Config that starts valid and then has exactly
// one randomly chosen mutation applied to make it invalid.
func genInvalidConfig() gopter.Gen {
	return genConfig().FlatMap(func(v interface{}) gopter.Gen {
		cfg := v.(Config)
		return gen.IntRange(0, len(invalidMutations)-1).Map(func(idx int) Config {
			mut := invalidMutations[idx]
			mut.apply(&cfg, nil)
			return cfg
		})
	}, reflect.TypeOf(Config{}))
}

// TestPropertyConfigValidationRejectsInvalid verifies Property 2: for all
// configs that contain at least one validation violation, validate() returns
// a non-nil error.
func TestPropertyConfigValidationRejectsInvalid(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.MinSuccessfulTests = 200
	properties := gopter.NewProperties(parameters)

	properties.Property("validate rejects configs with a validation violation", prop.ForAll(
		func(cfg Config) bool {
			err := validate(&cfg)
			if err == nil {
				t.Logf("Expected validation error but got nil for config: %+v", cfg)
				return false
			}
			return true
		},
		genInvalidConfig(),
	))

	properties.TestingRun(t)
}
