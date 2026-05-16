package config

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

// ptr is a test helper for constructing *string values inline. Used heavily
// for DNSResolver three-state assertions where (nil / *"" / *"x") all carry
// distinct meaning.
func ptr[T any](v T) *T { return &v }

// unmarshalRaw parses YAML into a Config using the same options as
// LoadConfig but skips applyDefaults and validate, so callers can observe
// the raw post-unmarshal state of three-state pointer fields.
func unmarshalRaw(t *testing.T, content string) Config {
	t.Helper()
	var cfg Config
	if err := yaml.Load([]byte(content), &cfg, yaml.WithKnownFields()); err != nil {
		t.Fatalf("yaml.Load: %v", err)
	}
	return cfg
}

// TestValidateDNSResolver covers the standalone validator: empty is OK,
// non-empty must be IP literal with port. Hostnames and bare IPs without
// port are rejected with operator-friendly messages.
func TestValidateDNSResolver(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantErr   bool
		errSubstr string
	}{
		{"empty", "", false, ""},
		{"ipv4_with_port", "8.8.8.8:53", false, ""},
		{"ipv4_high_port", "8.8.8.8:5353", false, ""},
		{"ipv6_bracketed", "[2001:db8::1]:53", false, ""},
		{"hostname", "dns.google:53", true, "must be an IP literal"},
		{"ipv4_no_port", "8.8.8.8", true, "must be an IP literal"},
		{"port_out_of_range_high", "8.8.8.8:99999", true, "port must be in range 1-65535"},
		{"port_zero", "8.8.8.8:0", true, "port must be in range 1-65535"},
		{"port_non_numeric", "8.8.8.8:abc", true, "port must be in range 1-65535"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDNSResolver(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateDNSResolver(%q) = nil, want error", tc.input)
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("validateDNSResolver(%q) error %q does not contain %q", tc.input, err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateDNSResolver(%q) = %v, want nil", tc.input, err)
			}
		})
	}
}

// TestValidateDNSServer covers the strict IP-only rule for probe_opts.dns_server.
// This is the breaking change vs earlier behavior: hostnames are no longer
// accepted because pre-resolution would contaminate probe_dns_resolve_seconds.
func TestValidateDNSServer(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantErr   bool
		errSubstr string
	}{
		{"empty", "", false, ""},
		{"ipv4_with_port", "8.8.8.8:53", false, ""},
		{"ipv4_no_port_auto53", "8.8.8.8", false, ""},
		{"ipv6_bracketed", "[2001:db8::1]:53", false, ""},
		{"hostname_with_port", "dns.google:53", true, "must be an IP literal"},
		{"hostname_no_port", "dns.google", true, "must be an IP literal"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDNSServer(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateDNSServer(%q) = nil, want error", tc.input)
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("validateDNSServer(%q) error %q does not contain %q", tc.input, err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateDNSServer(%q) = %v, want nil", tc.input, err)
			}
		})
	}
}

// TestApplyDefaults_DNSResolverThreeState exercises the inheritance and
// opt-out matrix end to end through LoadConfig. Each case is a valid YAML
// snippet so we exercise the YAML/v4 parser too.
func TestApplyDefaults_DNSResolverThreeState(t *testing.T) {
	cases := []struct {
		name             string
		agentResolver    string // value (or "") — empty produces no agent line
		targetField      string // raw YAML fragment for the target's dns_resolver line, or "" to omit
		wantEffectivePtr *string
	}{
		{
			name:             "agent_unset_target_omitted",
			agentResolver:    "",
			targetField:      "",
			wantEffectivePtr: ptr(""),
		},
		{
			name:             "agent_set_target_omitted_inherits",
			agentResolver:    "1.1.1.1:53",
			targetField:      "",
			wantEffectivePtr: ptr("1.1.1.1:53"),
		},
		{
			name:             "agent_set_target_explicit_empty_optout",
			agentResolver:    "1.1.1.1:53",
			targetField:      `    dns_resolver: ""`,
			wantEffectivePtr: ptr(""),
		},
		{
			name:             "agent_set_target_override",
			agentResolver:    "1.1.1.1:53",
			targetField:      `    dns_resolver: "8.8.8.8:53"`,
			wantEffectivePtr: ptr("8.8.8.8:53"),
		},
		{
			name:             "agent_unset_target_override",
			agentResolver:    "",
			targetField:      `    dns_resolver: "8.8.8.8:53"`,
			wantEffectivePtr: ptr("8.8.8.8:53"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agentLine := ""
			if tc.agentResolver != "" {
				agentLine = "  dns_resolver: \"" + tc.agentResolver + "\""
			}

			yaml := "agent:\n" +
				"  default_interval: 30s\n" +
				"  default_timeout: 5s\n" +
				agentLine + "\n" +
				"targets:\n" +
				"  - name: t\n" +
				"    address: \"example.com:443\"\n" +
				"    probe_type: tcp\n"
			if tc.targetField != "" {
				yaml += tc.targetField + "\n"
			}

			cfg, err := LoadConfig(writeConfigFile(t, yaml))
			if err != nil {
				t.Fatalf("LoadConfig error: %v", err)
			}
			if len(cfg.Targets) != 1 {
				t.Fatalf("expected 1 target, got %d", len(cfg.Targets))
			}

			got := cfg.Targets[0].DNSResolver
			if got == nil {
				t.Fatal("target.DNSResolver is nil after applyDefaults; expected non-nil pointer")
			}
			if *got != *tc.wantEffectivePtr {
				t.Fatalf("target.DNSResolver = %q, want %q", *got, *tc.wantEffectivePtr)
			}
		})
	}
}

// TestUnmarshal_DNSResolverYAMLEdgeCases pins down exactly how go.yaml.in
// /yaml/v4 maps each YAML form to *string before applyDefaults runs. The
// post-applyDefaults effective values are covered separately.
//
// The four "absent-equivalent" forms (omitted, ~, null, empty value) all
// produce a nil pointer so they can inherit from agent.dns_resolver. Only
// `dns_resolver: ""` with explicit quotes produces a non-nil empty pointer
// that signals "opt out of agent default".
func TestUnmarshal_DNSResolverYAMLEdgeCases(t *testing.T) {
	cases := []struct {
		name       string
		yamlExtra  string
		wantPreApp *string
	}{
		{"omitted", "", nil},
		{"explicit_null", `    dns_resolver: null`, nil},
		{"tilde_null", `    dns_resolver: ~`, nil},
		{"empty_value", `    dns_resolver:`, nil},
		{"explicit_empty", `    dns_resolver: ""`, ptr("")},
		{"value", `    dns_resolver: "8.8.8.8:53"`, ptr("8.8.8.8:53")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "agent:\n" +
				"  default_interval: 30s\n" +
				"  default_timeout: 5s\n" +
				"targets:\n" +
				"  - name: t\n" +
				"    address: \"example.com:443\"\n" +
				"    probe_type: tcp\n"
			if tc.yamlExtra != "" {
				yaml += tc.yamlExtra + "\n"
			}

			// Use unmarshalRaw so we observe the value *before* applyDefaults
			// runs. LoadConfig would have already replaced nil with a
			// non-nil pointer to the agent default.
			cfg := unmarshalRaw(t, yaml)
			if len(cfg.Targets) != 1 {
				t.Fatalf("expected 1 target, got %d", len(cfg.Targets))
			}

			got := cfg.Targets[0].DNSResolver
			switch {
			case tc.wantPreApp == nil && got != nil:
				t.Fatalf("DNSResolver = *%q, want nil (case %q)", *got, tc.name)
			case tc.wantPreApp != nil && got == nil:
				t.Fatalf("DNSResolver = nil, want *%q (case %q)", *tc.wantPreApp, tc.name)
			case tc.wantPreApp != nil && got != nil && *tc.wantPreApp != *got:
				t.Fatalf("DNSResolver = *%q, want *%q (case %q)", *got, *tc.wantPreApp, tc.name)
			}
		})
	}
}

// TestLoadConfig_RejectsHostnameDNSResolverAtAgent verifies that the
// agent-level validator fails fast with the operator-friendly error.
func TestLoadConfig_RejectsHostnameDNSResolverAtAgent(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  default_timeout: 5s
  dns_resolver: "dns.google:53"

targets:
  - name: t
    address: "example.com:443"
    probe_type: tcp
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("LoadConfig accepted hostname dns_resolver at agent level; expected error")
	}
	if !strings.Contains(err.Error(), "must be an IP literal") {
		t.Fatalf("expected error to mention 'must be an IP literal', got %q", err.Error())
	}
}

// TestLoadConfig_RejectsHostnameDNSResolverAtTarget verifies that per-target
// overrides are subject to the same strict validation.
func TestLoadConfig_RejectsHostnameDNSResolverAtTarget(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  default_timeout: 5s

targets:
  - name: t
    address: "example.com:443"
    probe_type: tcp
    dns_resolver: "dns.google:53"
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("LoadConfig accepted hostname dns_resolver at target level; expected error")
	}
	if !strings.Contains(err.Error(), "must be an IP literal") {
		t.Fatalf("expected error to mention 'must be an IP literal', got %q", err.Error())
	}
}

// TestLoadConfig_RejectsHostnameDNSServer verifies the breaking change for
// probe_opts.dns_server: hostnames now fail at config load time with a
// migration-friendly message.
func TestLoadConfig_RejectsHostnameDNSServer(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  default_timeout: 5s

targets:
  - name: t
    address: "example.com"
    probe_type: dns
    probe_opts:
      dns_server: "dns.google:53"
      dns_query_name: "example.com"
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("LoadConfig accepted hostname dns_server; expected error")
	}
	if !strings.Contains(err.Error(), "must be an IP literal") {
		t.Fatalf("expected error to mention 'must be an IP literal', got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "dig +short") {
		t.Fatalf("expected migration hint about dig +short, got %q", err.Error())
	}
}

// TestHash_DNSResolverDistinguishesInheritFromOptOut verifies that the
// short config hash treats "inherit agent default" and "explicit opt-out
// to system" as distinct configurations even when the agent default is the
// empty string. JSON serialization of *string preserves this difference
// because applyDefaults gives both forms different post-state pointers when
// agent.dns_resolver is non-empty.
func TestHash_DNSResolverDistinguishesInheritFromOptOut(t *testing.T) {
	yamlInherit := `
agent:
  default_interval: 30s
  default_timeout: 5s
  dns_resolver: "1.1.1.1:53"

targets:
  - name: t
    address: "example.com:443"
    probe_type: tcp
`
	yamlOptOut := `
agent:
  default_interval: 30s
  default_timeout: 5s
  dns_resolver: "1.1.1.1:53"

targets:
  - name: t
    address: "example.com:443"
    probe_type: tcp
    dns_resolver: ""
`

	cfg1, err := LoadConfig(writeConfigFile(t, yamlInherit))
	if err != nil {
		t.Fatalf("LoadConfig inherit: %v", err)
	}
	cfg2, err := LoadConfig(writeConfigFile(t, yamlOptOut))
	if err != nil {
		t.Fatalf("LoadConfig optout: %v", err)
	}

	h1, err := ComputeHash(cfg1)
	if err != nil {
		t.Fatalf("ComputeHash inherit: %v", err)
	}
	h2, err := ComputeHash(cfg2)
	if err != nil {
		t.Fatalf("ComputeHash optout: %v", err)
	}
	if h1 == h2 {
		t.Fatalf("hash identical for inherit vs explicit opt-out (both = %q); they have different runtime semantics", h1)
	}

	// Sanity: change the override value and HashTarget detects it.
	tBase := cfg1.Targets[0]
	tBase.DNSResolver = ptr("8.8.8.8:53")
	hA, _ := HashTarget(&tBase)
	tBase.DNSResolver = ptr("1.1.1.1:53")
	hB, _ := HashTarget(&tBase)
	if hA == hB {
		t.Fatalf("HashTarget identical for *\"8.8.8.8:53\" vs *\"1.1.1.1:53\"")
	}
}
