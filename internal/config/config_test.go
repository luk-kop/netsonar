package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfigFile is a test helper that writes YAML content to a temp file
// and returns the file path.
func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestLoadConfig_ValidConfig(t *testing.T) {
	yaml := `
agent:
  listen_addr: ":9275"
  metrics_path: "/metrics"
  default_interval: 30s
  log_level: info

targets:
  - name: "tcp-target"
    address: "example.com:443"
    probe_type: tcp
    interval: 30s
    timeout: 5s
    tags:
      service: "web"
  - name: "http-target"
    address: "https://example.com"
    probe_type: http
    interval: 60s
    timeout: 10s
    tags:
      service: "api"
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Agent.ListenAddr != ":9275" {
		t.Errorf("listen_addr = %q, want %q", cfg.Agent.ListenAddr, ":9275")
	}
	if len(cfg.Targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(cfg.Targets))
	}
	if cfg.Targets[0].Name != "tcp-target" {
		t.Errorf("target[0].Name = %q, want %q", cfg.Targets[0].Name, "tcp-target")
	}
	if cfg.Targets[0].ProbeType != ProbeTypeTCP {
		t.Errorf("target[0].ProbeType = %q, want %q", cfg.Targets[0].ProbeType, ProbeTypeTCP)
	}
	if cfg.Targets[1].Timeout != 10*time.Second {
		t.Errorf("target[1].Timeout = %v, want %v", cfg.Targets[1].Timeout, 10*time.Second)
	}
}

func TestLoadConfig_DefaultAgentListenAndMetricsPathApplied(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  default_timeout: 5s

targets:
  - name: "tcp-target"
    address: "example.com:443"
    probe_type: tcp
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Agent.ListenAddr != ":9275" {
		t.Errorf("listen_addr = %q, want %q", cfg.Agent.ListenAddr, ":9275")
	}
	if cfg.Agent.MetricsPath != "/metrics" {
		t.Errorf("metrics_path = %q, want %q", cfg.Agent.MetricsPath, "/metrics")
	}
}

func TestLoadConfig_InitialProbeJitterAccepted(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  default_timeout: 5s
  initial_probe_jitter: 10s

targets:
  - name: "tcp-target"
    address: "example.com:443"
    probe_type: tcp
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Agent.InitialProbeJitter != 10*time.Second {
		t.Errorf("initial_probe_jitter = %v, want 10s", cfg.Agent.InitialProbeJitter)
	}
}

func TestLoadConfig_NegativeInitialProbeJitterRejected(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  default_timeout: 5s
  initial_probe_jitter: -1s

targets:
  - name: "tcp-target"
    address: "example.com:443"
    probe_type: tcp
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for negative initial_probe_jitter, got nil")
	}
	if !strings.Contains(err.Error(), "initial_probe_jitter") {
		t.Fatalf("error = %q, want it to mention initial_probe_jitter", err.Error())
	}
}

func TestLoadConfig_InitialProbeJitterExceedsShortestIntervalRejected(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  default_timeout: 5s
  initial_probe_jitter: 15s

targets:
  - name: "fast-target"
    address: "example.com:443"
    probe_type: tcp
    interval: 10s
  - name: "slow-target"
    address: "example.org:443"
    probe_type: tcp
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for initial_probe_jitter greater than shortest interval, got nil")
	}
	if !strings.Contains(err.Error(), "shortest target interval") {
		t.Fatalf("error = %q, want it to mention shortest target interval", err.Error())
	}
}

func TestLoadConfig_InvalidMetricsPathRejected(t *testing.T) {
	tests := []struct {
		name        string
		metricsPath string
		want        string
	}{
		{"missing-leading-slash", "metrics", "must start with /"},
		{"health-conflict", "/healthz", "conflicts"},
		{"ready-conflict", "/readyz", "conflicts"},
		{"method-pattern", "GET /metrics", "must start with /"},
		{"wildcard-pattern", "/metrics/{name}", "plain path"},
		{"space", "/met rics", "whitespace"},
		{"control", "/metrics\nextra", "whitespace"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := fmt.Sprintf(`
agent:
  metrics_path: %q
  default_interval: 30s
  default_timeout: 5s

targets:
  - name: "tcp-target"
    address: "example.com:443"
    probe_type: tcp
`, tt.metricsPath)

			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err == nil {
				t.Fatalf("expected error for metrics_path %q, got nil", tt.metricsPath)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestLoadConfig_UnknownYAMLFieldsRejected(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "agent-field",
			yaml: `
agent:
  default_interval: 30s
  default_timeout: 5s
  metrics_pat: "/metrics"

targets:
  - name: "tcp-target"
    address: "example.com:443"
    probe_type: tcp
`,
			want: "metrics_pat",
		},
		{
			name: "target-field",
			yaml: `
agent:
  default_interval: 30s
  default_timeout: 5s

targets:
  - name: "tcp-target"
    adress: "example.com:443"
    address: "example.com:443"
    probe_type: tcp
`,
			want: "adress",
		},
		{
			name: "probe-opts-field",
			yaml: `
agent:
  default_interval: 30s
  default_timeout: 5s

targets:
  - name: "http-target"
    address: "https://example.com"
    probe_type: http
    probe_opts:
      tls_skip_verfy: true
`,
			want: "tls_skip_verfy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfigFile(t, tt.yaml))
			if err == nil {
				t.Fatal("expected error for unknown YAML field, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestLoadConfig_DefaultIntervalApplied(t *testing.T) {
	yaml := `
agent:
  default_interval: 45s

targets:
  - name: "no-interval"
    address: "example.com:80"
    probe_type: tcp
    timeout: 5s
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Targets[0].Interval != 45*time.Second {
		t.Errorf("interval = %v, want %v (default_interval)", cfg.Targets[0].Interval, 45*time.Second)
	}
}

func TestLoadConfig_DefaultTimeoutApplied(t *testing.T) {
	yaml := `
agent:
  default_interval: 45s
  default_timeout: 7s

targets:
  - name: "no-timeout"
    address: "example.com:80"
    probe_type: tcp
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Targets[0].Timeout != 7*time.Second {
		t.Errorf("timeout = %v, want %v (default_timeout)", cfg.Targets[0].Timeout, 7*time.Second)
	}
}

func TestLoadConfig_TargetTimeoutOverridesDefaultTimeout(t *testing.T) {
	yaml := `
agent:
  default_interval: 45s
  default_timeout: 7s

targets:
  - name: "custom-timeout"
    address: "example.com:80"
    probe_type: tcp
    timeout: 3s
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if cfg.Targets[0].Timeout != 3*time.Second {
		t.Errorf("timeout = %v, want target override 3s", cfg.Targets[0].Timeout)
	}
}

func TestLoadConfig_TimeoutMissingNoDefault(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "needs-timeout"
    address: "example.com:80"
    probe_type: tcp
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error when target has no timeout and agent.default_timeout is unset, got nil")
	}
	if !strings.Contains(err.Error(), "timeout must be > 0") {
		t.Errorf("error = %q, want it to mention 'timeout must be > 0'", err.Error())
	}
}

func TestLoadConfig_NegativeDefaultTimeoutUnusedIsOK(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  default_timeout: -5s

targets:
  - name: "self-contained"
    address: "example.com:80"
    probe_type: tcp
    timeout: 2s
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error when negative default_timeout is unused, got: %v", err)
	}
	if cfg.Targets[0].Timeout != 2*time.Second {
		t.Errorf("timeout = %v, want 2s (target value preserved)", cfg.Targets[0].Timeout)
	}
}

func TestLoadConfig_NegativeDefaultTimeoutUsedIsRejected(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  default_timeout: -5s

targets:
  - name: "bad-default-timeout"
    address: "example.com:80"
    probe_type: tcp
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error when negative default_timeout is applied, got nil")
	}
	if !strings.Contains(err.Error(), "timeout must be > 0") {
		t.Errorf("error = %q, want it to mention 'timeout must be > 0'", err.Error())
	}
}

func TestLoadConfig_IntervalMissingNoDefault(t *testing.T) {
	yaml := `
agent: {}

targets:
  - name: "needs-interval"
    address: "example.com:80"
    probe_type: tcp
    timeout: 2s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error when target has no interval and agent.default_interval is unset, got nil")
	}
	if !strings.Contains(err.Error(), "interval must be > 0") {
		t.Errorf("error = %q, want it to mention 'interval must be > 0'", err.Error())
	}
}

func TestLoadConfig_NegativeIntervalRejected(t *testing.T) {
	yaml := `
agent: {}

targets:
  - name: "bad-interval"
    address: "example.com:80"
    probe_type: tcp
    interval: -5s
    timeout: 2s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for negative interval, got nil")
	}
	if !strings.Contains(err.Error(), "interval must be > 0") {
		t.Errorf("error = %q, want it to mention 'interval must be > 0'", err.Error())
	}
}

func TestLoadConfig_NegativeDefaultIntervalUnusedIsOK(t *testing.T) {
	// A negative default_interval is harmless as long as every target sets
	// its own interval, because applyDefaults never touches those targets
	// and the negative default never reaches the scheduler.
	yaml := `
agent:
  default_interval: -5s

targets:
  - name: "self-contained"
    address: "example.com:80"
    probe_type: tcp
    interval: 30s
    timeout: 2s
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error when negative default_interval is unused, got: %v", err)
	}
	if cfg.Targets[0].Interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s (target value preserved)", cfg.Targets[0].Interval)
	}
}

func TestLoadConfig_MissingName(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - address: "example.com:443"
    probe_type: tcp
    timeout: 5s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing name, got nil")
	}
	if !strings.Contains(err.Error(), "missing required field 'name'") {
		t.Errorf("error = %q, want it to mention missing name", err.Error())
	}
}

func TestLoadConfig_MissingAddress(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "no-address"
    probe_type: tcp
    timeout: 5s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for missing address, got nil")
	}
	if !strings.Contains(err.Error(), "missing required field 'address'") {
		t.Errorf("error = %q, want it to mention missing address", err.Error())
	}
}

func TestLoadConfig_DuplicateNames(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "dup"
    address: "a.example.com:443"
    probe_type: tcp
    timeout: 5s
  - name: "dup"
    address: "b.example.com:443"
    probe_type: tcp
    timeout: 5s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for duplicate names, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate target name") {
		t.Errorf("error = %q, want it to mention duplicate", err.Error())
	}
}

func TestLoadConfig_InvalidProbeType(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "bad-probe"
    address: "example.com:443"
    probe_type: ftp
    timeout: 5s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for invalid probe_type, got nil")
	}
	if !strings.Contains(err.Error(), "invalid probe_type") {
		t.Errorf("error = %q, want it to mention invalid probe_type", err.Error())
	}
	if !strings.Contains(err.Error(), "ftp") {
		t.Errorf("error = %q, want it to include the invalid value 'ftp'", err.Error())
	}
}

func TestLoadConfig_TimeoutExceedsInterval(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "slow"
    address: "example.com:443"
    probe_type: tcp
    interval: 10s
    timeout: 20s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for timeout > interval, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "exceeds interval") {
		t.Errorf("error = %q, want it to mention timeout exceeding interval", err.Error())
	}
}

func TestLoadConfig_UnsortedICMPPayloadSizes(t *testing.T) {
	yaml := `
agent:
  default_interval: 300s

targets:
  - name: "mtu-bad"
    address: "example.com"
    probe_type: mtu
    timeout: 30s
    probe_opts:
      icmp_payload_sizes: [1072, 1272, 1472]
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for unsorted icmp_payload_sizes, got nil")
	}
	if !strings.Contains(err.Error(), "icmp_payload_sizes must be sorted in descending order") {
		t.Errorf("error = %q, want it to mention descending order", err.Error())
	}
}

func TestLoadConfig_ICMPPayloadSizesDescendingValid(t *testing.T) {
	yaml := `
agent:
  default_interval: 300s

targets:
  - name: "mtu-ok"
    address: "example.com"
    probe_type: mtu
    timeout: 30s
    probe_opts:
      icmp_payload_sizes: [1472, 1372, 1272, 1172]
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error for valid descending icmp_payload_sizes, got: %v", err)
	}
}

func TestLoadConfig_ICMPRejectsLiteralIPv6Address(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "icmp-ipv6"
    address: "2606:4700:4700::1111"
    probe_type: icmp
    timeout: 5s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for literal IPv6 ICMP target, got nil")
	}
	if !strings.Contains(err.Error(), "supports IPv4 addresses only") {
		t.Errorf("error = %q, want it to mention IPv4-only support", err.Error())
	}
}

func TestLoadConfig_MTURejectsLiteralIPv6Address(t *testing.T) {
	yaml := `
agent:
  default_interval: 300s

targets:
  - name: "mtu-ipv6"
    address: "2606:4700:4700::1111"
    probe_type: mtu
    timeout: 30s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for literal IPv6 MTU target, got nil")
	}
	if !strings.Contains(err.Error(), "supports IPv4 addresses only") {
		t.Errorf("error = %q, want it to mention IPv4-only support", err.Error())
	}
}

func TestLoadConfig_ICMPMTURejectAddressWithPort(t *testing.T) {
	tests := []struct {
		name      string
		probeType string
		address   string
	}{
		{"icmp-hostname-port", "icmp", "example.com:80"},
		{"icmp-ipv4-port", "icmp", "192.0.2.1:80"},
		{"icmp-ipv6-port", "icmp", "[2001:db8::1]:80"},
		{"mtu-hostname-port", "mtu", "example.com:80"},
		{"mtu-ipv4-port", "mtu", "192.0.2.1:80"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := `
agent:
  default_interval: 300s

targets:
  - name: "target"
    address: "` + tt.address + `"
    probe_type: ` + tt.probeType + `
    timeout: 30s
`
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err == nil {
				t.Fatalf("expected error for %s address %q, got nil", tt.probeType, tt.address)
			}
			if !strings.Contains(err.Error(), "address must not include a port") {
				t.Errorf("error = %q, want it to mention port rejection", err.Error())
			}
		})
	}
}

func TestLoadConfig_ICMPMTURejectBracketedIPv6Literal(t *testing.T) {
	tests := []struct {
		name      string
		probeType string
		address   string
		wantMsg   string
	}{
		{"icmp-bracketed-ipv6", "icmp", "[2001:db8::1]", "supports IPv4 addresses only"},
		{"icmp-bracketed-loopback", "icmp", "[::1]", "supports IPv4 addresses only"},
		{"mtu-bracketed-ipv6", "mtu", "[2001:db8::1]", "supports IPv4 addresses only"},
		{"icmp-bracketed-malformed", "icmp", "[not-an-ip]", "is malformed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := `
agent:
  default_interval: 300s

targets:
  - name: "target"
    address: "` + tt.address + `"
    probe_type: ` + tt.probeType + `
    timeout: 30s
`
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err == nil {
				t.Fatalf("expected error for %s address %q, got nil", tt.probeType, tt.address)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestLoadConfig_ICMPAllowsIPv4Address(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "icmp-ipv4"
    address: "192.0.2.1"
    probe_type: icmp
    timeout: 5s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error for IPv4 ICMP target, got: %v", err)
	}
}

func TestLoadConfig_MTUAllowsHostnameAddress(t *testing.T) {
	yaml := `
agent:
  default_interval: 300s

targets:
  - name: "mtu-hostname"
    address: "example.com"
    probe_type: mtu
    timeout: 30s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error for hostname MTU target, got: %v", err)
	}
}

func TestLoadConfig_ICMPAllowsIPv4MappedIPv6Address(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "icmp-ipv4-mapped"
    address: "::ffff:192.0.2.1"
    probe_type: icmp
    timeout: 5s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error for IPv4-mapped IPv6 ICMP target, got: %v", err)
	}
}

func TestLoadConfig_MTUDefaultPayloadSizesApplied(t *testing.T) {
	yaml := `
agent:
  default_interval: 300s

targets:
  - name: "mtu-no-sizes"
    address: "example.com"
    probe_type: mtu
    timeout: 30s
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	sizes := cfg.Targets[0].ProbeOpts.ICMPPayloadSizes
	if len(sizes) == 0 {
		t.Fatal("expected default icmp_payload_sizes to be applied, got empty")
	}
	want := []int{1472, 1392, 1372, 1272, 1172, 1072}
	if len(sizes) != len(want) {
		t.Fatalf("expected builtin defaults %v, got %v", want, sizes)
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Errorf("expected builtin defaults %v, got %v", want, sizes)
			break
		}
	}
	opts := cfg.Targets[0].ProbeOpts
	if opts.ExpectedMinMTU != 1500 {
		t.Errorf("expected_min_mtu = %d, want 1500", opts.ExpectedMinMTU)
	}
	if opts.MTURetries != DefaultMTURetries {
		t.Errorf("mtu_retries = %d, want %d", opts.MTURetries, DefaultMTURetries)
	}
	if opts.MTUPerAttemptTimeout != DefaultMTUPerAttemptTimeout {
		t.Errorf("mtu_per_attempt_timeout = %v, want %v", opts.MTUPerAttemptTimeout, DefaultMTUPerAttemptTimeout)
	}
}

func TestLoadConfig_MTUAgentDefaultOverridesBuiltin(t *testing.T) {
	yaml := `
agent:
  default_interval: 300s
  default_icmp_payload_sizes: [1400, 1300, 1200]

targets:
  - name: "mtu-agent-default"
    address: "example.com"
    probe_type: mtu
    timeout: 30s
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	sizes := cfg.Targets[0].ProbeOpts.ICMPPayloadSizes
	if len(sizes) != 3 || sizes[0] != 1400 || sizes[1] != 1300 || sizes[2] != 1200 {
		t.Errorf("expected agent defaults [1400, 1300, 1200], got %v", sizes)
	}
}

func TestLoadConfig_MTUTargetOverridesAgentDefault(t *testing.T) {
	yaml := `
agent:
  default_interval: 300s
  default_icmp_payload_sizes: [1400, 1300, 1200]

targets:
  - name: "mtu-custom"
    address: "example.com"
    probe_type: mtu
    timeout: 30s
    probe_opts:
      icmp_payload_sizes: [1600, 1472, 1372]
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	sizes := cfg.Targets[0].ProbeOpts.ICMPPayloadSizes
	if len(sizes) != 3 || sizes[0] != 1600 {
		t.Errorf("expected target override [1600, 1472, 1372], got %v", sizes)
	}
}

func TestLoadConfig_MTUOptionsValid(t *testing.T) {
	yaml := `
agent:
  default_interval: 300s

targets:
  - name: "mtu-options"
    address: "example.com"
    probe_type: mtu
    timeout: 30s
    probe_opts:
      icmp_payload_sizes: [1472, 1392, 1372]
      expected_min_mtu: 1420
      mtu_retries: 5
      mtu_per_attempt_timeout: 3s
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error for valid MTU options, got: %v", err)
	}
	opts := cfg.Targets[0].ProbeOpts
	if opts.ExpectedMinMTU != 1420 {
		t.Errorf("expected_min_mtu = %d, want 1420", opts.ExpectedMinMTU)
	}
	if opts.MTURetries != 5 {
		t.Errorf("mtu_retries = %d, want 5", opts.MTURetries)
	}
	if opts.MTUPerAttemptTimeout != 3*time.Second {
		t.Errorf("mtu_per_attempt_timeout = %v, want 3s", opts.MTUPerAttemptTimeout)
	}
}

func TestLoadConfig_MTUExpectedMinExceedsLargestTested(t *testing.T) {
	yaml := `
agent:
  default_interval: 300s

targets:
  - name: "mtu-bad-expected"
    address: "example.com"
    probe_type: mtu
    timeout: 30s
    probe_opts:
      icmp_payload_sizes: [1392, 1372]
      expected_min_mtu: 1500
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for expected_min_mtu larger than largest tested MTU, got nil")
	}
	if !strings.Contains(err.Error(), "expected_min_mtu") || !strings.Contains(err.Error(), "largest tested MTU") {
		t.Errorf("error = %q, want it to mention expected_min_mtu and largest tested MTU", err.Error())
	}
}

func TestLoadConfig_MTURetriesMustBePositive(t *testing.T) {
	yaml := `
agent:
  default_interval: 300s

targets:
  - name: "mtu-bad-retries"
    address: "example.com"
    probe_type: mtu
    timeout: 30s
    probe_opts:
      icmp_payload_sizes: [1472, 1392]
      mtu_retries: -1
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for invalid mtu_retries, got nil")
	}
	if !strings.Contains(err.Error(), "mtu_retries must be >= 1") {
		t.Errorf("error = %q, want it to mention mtu_retries", err.Error())
	}
}

func TestLoadConfig_MTUPerAttemptTimeoutMustNotExceedTimeout(t *testing.T) {
	yaml := `
agent:
  default_interval: 300s

targets:
  - name: "mtu-bad-attempt-timeout"
    address: "example.com"
    probe_type: mtu
    timeout: 2s
    probe_opts:
      icmp_payload_sizes: [1472, 1392]
      mtu_per_attempt_timeout: 3s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for mtu_per_attempt_timeout > timeout, got nil")
	}
	if !strings.Contains(err.Error(), "mtu_per_attempt_timeout") || !strings.Contains(err.Error(), "exceeds timeout") {
		t.Errorf("error = %q, want it to mention mtu_per_attempt_timeout exceeding timeout", err.Error())
	}
}

func TestLoadConfig_MTUAgentDefaultUnsorted(t *testing.T) {
	yaml := `
agent:
  default_interval: 300s
  default_icmp_payload_sizes: [1200, 1400, 1300]

targets:
  - name: "mtu-t"
    address: "example.com"
    probe_type: mtu
    timeout: 30s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for unsorted agent default_icmp_payload_sizes, got nil")
	}
	if !strings.Contains(err.Error(), "default_icmp_payload_sizes must be sorted in descending order") {
		t.Errorf("error = %q, want it to mention descending order", err.Error())
	}
}

func TestLoadConfig_MTUDefaultNotAppliedToNonMTU(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  default_icmp_payload_sizes: [1472, 1372]

targets:
  - name: "tcp-target"
    address: "example.com:443"
    probe_type: tcp
    timeout: 5s
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	sizes := cfg.Targets[0].ProbeOpts.ICMPPayloadSizes
	if len(sizes) != 0 {
		t.Errorf("expected no icmp_payload_sizes on TCP target, got %v", sizes)
	}
}

func TestLoadConfig_InvalidDNSQueryType(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "dns-bad"
    address: "example.com"
    probe_type: dns
    timeout: 5s
    probe_opts:
      dns_query_name: "example.com"
      dns_query_type: "MX"
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for invalid dns_query_type, got nil")
	}
	if !strings.Contains(err.Error(), "invalid dns_query_type") {
		t.Errorf("error = %q, want it to mention invalid dns_query_type", err.Error())
	}
	if !strings.Contains(err.Error(), "MX") {
		t.Errorf("error = %q, want it to include the invalid value 'MX'", err.Error())
	}
}

func TestLoadConfig_ValidDNSQueryTypes(t *testing.T) {
	for _, qt := range []string{"A", "AAAA", "CNAME"} {
		t.Run(qt, func(t *testing.T) {
			yaml := `
agent:
  default_interval: 30s

targets:
  - name: "dns-` + strings.ToLower(qt) + `"
    address: "example.com"
    probe_type: dns
    timeout: 5s
    probe_opts:
      dns_query_name: "example.com"
      dns_query_type: "` + qt + `"
`
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err != nil {
				t.Fatalf("expected no error for dns_query_type %q, got: %v", qt, err)
			}
		})
	}
}

func TestLoadConfig_ProxyWithoutProxyURL(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "proxy-bad"
    address: "https://example.com"
    probe_type: proxy
    timeout: 5s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for proxy without proxy_url, got nil")
	}
	if !strings.Contains(err.Error(), "proxy_url") {
		t.Errorf("error = %q, want it to mention proxy_url", err.Error())
	}
}

func TestLoadConfig_ProxyWithProxyURL(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "proxy-ok"
    address: "https://example.com"
    probe_type: proxy
    timeout: 5s
    probe_opts:
      proxy_url: "http://proxy.internal:8888"
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error for proxy with proxy_url, got: %v", err)
	}
}

// httpBodyMatchOpt returns a YAML probe_opts snippet that satisfies the
// http_body body-matcher requirement. Returns empty string for other types.
func httpBodyMatchOpt(probeType string) string {
	if probeType == "http_body" {
		return "      body_match_string: \"ok\"\n"
	}
	return ""
}

func TestLoadConfig_ValidProxyURLs(t *testing.T) {
	tests := []struct {
		name      string
		probeType string
		proxyURL  string
	}{
		{"http no port", "http", "http://proxy.internal"},
		{"http root path", "http", "http://proxy.internal/"},
		{"http with port", "http", "http://proxy.internal:8888"},
		{"https with port", "http_body", "https://proxy.internal:443"},
		{"userinfo", "http", "http://user:pass@proxy.internal:8080"},
		{"tls cert", "tls_cert", "http://proxy.internal:8888"},
		{"ipv6 bracketed", "proxy", "http://[::1]:8080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := fmt.Sprintf(`
agent:
  default_interval: 30s

targets:
  - name: "proxy-url-ok"
    address: "https://example.com"
    probe_type: %s
    timeout: 5s
    probe_opts:
      proxy_url: %q
%s`, tt.probeType, tt.proxyURL, httpBodyMatchOpt(tt.probeType))
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err != nil {
				t.Fatalf("expected no error for proxy_url %q, got: %v", tt.proxyURL, err)
			}
		})
	}
}

func TestLoadConfig_InvalidProxyURLs(t *testing.T) {
	tests := []struct {
		name      string
		probeType string
		proxyURL  string
		want      string
	}{
		{"relative host port", "http", "proxy.internal:8888", "must use scheme://host form"},
		{"opaque uri", "http", "http:proxy:8080", "must use scheme://host form"},
		{"unsupported scheme", "http", "ftp://proxy.internal:21", "scheme must be http or https"},
		{"empty host", "http", "http://", "host is required"},
		{"invalid port", "http", "http://proxy.internal:abc", "not a valid absolute URL"},
		{"port out of range", "http", "http://proxy.internal:99999", "port must be in range 1-65535"},
		{"zero port", "http_body", "http://proxy.internal:0", "port must be in range 1-65535"},
		{"path", "http", "http://proxy.internal:8080/foo", "path is not allowed"},
		{"query", "http", "http://proxy.internal:8080?x=y", "query is not allowed"},
		{"fragment", "proxy", "http://proxy.internal:8080#frag", "not a valid absolute URL"},
		{"ipv6 without brackets", "http", "http://::1:8080", "not a valid absolute URL"},
		{"tls cert unsupported scheme", "tls_cert", "socks5://proxy.internal:1080", "scheme must be http or https"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := fmt.Sprintf(`
agent:
  default_interval: 30s

targets:
  - name: "proxy-url-bad"
    address: "https://example.com"
    probe_type: %s
    timeout: 5s
    probe_opts:
      proxy_url: %q
%s`, tt.probeType, tt.proxyURL, httpBodyMatchOpt(tt.probeType))
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err == nil {
				t.Fatalf("expected error for proxy_url %q, got nil", tt.proxyURL)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestLoadConfig_ProxyURLEmptyAllowedForUnsupportedProbeTypes(t *testing.T) {
	tests := []struct {
		name      string
		probeType string
	}{
		{"tcp", "tcp"},
		{"icmp", "icmp"},
		{"mtu", "mtu"},
		{"dns", "dns"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := fmt.Sprintf(`
agent:
  default_interval: 30s

targets:
  - name: "%s-empty-proxy-url"
    address: "example.com"
    probe_type: %s
    timeout: 5s
    probe_opts:
      proxy_url: ""
`, tt.probeType, tt.probeType)
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err != nil {
				t.Fatalf("expected no error for empty proxy_url on %s, got: %v", tt.probeType, err)
			}
		})
	}
}

func TestLoadConfig_ProxyURLRejectedForUnsupportedProbeTypes(t *testing.T) {
	tests := []struct {
		name      string
		probeType string
	}{
		{"tcp", "tcp"},
		{"icmp", "icmp"},
		{"mtu", "mtu"},
		{"dns", "dns"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := fmt.Sprintf(`
agent:
  default_interval: 30s

targets:
  - name: "%s-proxy-url"
    address: "example.com"
    probe_type: %s
    timeout: 5s
    probe_opts:
      proxy_url: "http://proxy.internal:8888"
`, tt.probeType, tt.probeType)
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err == nil {
				t.Fatalf("expected error for proxy_url on %s, got nil", tt.probeType)
			}
			if !strings.Contains(err.Error(), "does not support 'proxy_url'") {
				t.Fatalf("error = %q, want unsupported proxy_url error", err.Error())
			}
		})
	}
}

func TestLoadConfig_InvalidProxyURLDoesNotLeakCredentials(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "proxy-secret"
    address: "https://example.com"
    probe_type: http
    timeout: 5s
    probe_opts:
      proxy_url: "ftp://user:secret-password@proxy.internal:21"
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for invalid proxy_url scheme, got nil")
	}
	if strings.Contains(err.Error(), "secret-password") {
		t.Fatalf("error leaked proxy credentials: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "xxxxx") {
		t.Fatalf("error = %q, want redacted credentials marker", err.Error())
	}
}

func TestLoadConfig_ValidHTTPMethods(t *testing.T) {
	tests := []struct {
		name      string
		probeType string
		method    string
	}{
		{"http default", "http", ""},
		{"http get", "http", "GET"},
		{"http head", "http", "HEAD"},
		{"http post", "http", "POST"},
		{"http body default", "http_body", ""},
		{"http body get", "http_body", "GET"},
		{"http body head", "http_body", "HEAD"},
		{"http body post", "http_body", "POST"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probeOpts := ""
			if tt.method != "" || tt.probeType == "http_body" {
				probeOpts = "    probe_opts:\n"
				if tt.method != "" {
					probeOpts += fmt.Sprintf("      method: %q\n", tt.method)
				}
				probeOpts += httpBodyMatchOpt(tt.probeType)
			}
			yaml := fmt.Sprintf(`
agent:
  default_interval: 30s

targets:
  - name: "method-ok"
    address: "https://example.com"
    probe_type: %s
    timeout: 5s
%s`, tt.probeType, probeOpts)
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err != nil {
				t.Fatalf("expected no error for method %q on %s, got: %v", tt.method, tt.probeType, err)
			}
		})
	}
}

func TestLoadConfig_InvalidHTTPMethods(t *testing.T) {
	tests := []struct {
		name      string
		probeType string
		method    string
	}{
		{"http lowercase", "http", "get"},
		{"http typo", "http", "GTE"},
		{"http put", "http", "PUT"},
		{"http delete", "http", "DELETE"},
		{"http patch", "http", "PATCH"},
		{"http body lowercase", "http_body", "get"},
		{"http body typo", "http_body", "GTE"},
		{"http body put", "http_body", "PUT"},
		{"http body delete", "http_body", "DELETE"},
		{"http body patch", "http_body", "PATCH"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := fmt.Sprintf(`
agent:
  default_interval: 30s

targets:
  - name: "method-bad"
    address: "https://example.com"
    probe_type: %s
    timeout: 5s
    probe_opts:
      method: %q
%s`, tt.probeType, tt.method, httpBodyMatchOpt(tt.probeType))
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err == nil {
				t.Fatalf("expected error for method %q on %s, got nil", tt.method, tt.probeType)
			}
			if !strings.Contains(err.Error(), "invalid method") {
				t.Fatalf("error = %q, want it to mention invalid method", err.Error())
			}
		})
	}
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "reading config file") {
		t.Errorf("error = %q, want it to mention reading config file", err.Error())
	}
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	yaml := `
agent:
  listen_addr: ":9275"
targets:
  - name: [invalid yaml structure
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	if !strings.Contains(err.Error(), "parsing YAML") {
		t.Errorf("error = %q, want it to mention parsing YAML", err.Error())
	}
}

func TestLoadConfig_AllProbeTypes(t *testing.T) {
	probeTypes := []struct {
		name      string
		probeType string
		address   string
		extra     string
	}{
		{"tcp-t", "tcp", "example.com:443", ""},
		{"http-t", "http", "example.com:443", ""},
		{"icmp-t", "icmp", "example.com", ""},
		{"mtu-t", "mtu", "example.com", ""},
		{"dns-t", "dns", "example.com:443", "\n    probe_opts:\n      dns_query_name: example.com\n      dns_query_type: A"},
		{"tls-t", "tls_cert", "example.com:443", ""},
		{"body-t", "http_body", "example.com:443", "\n    probe_opts:\n      body_match_string: \"ok\""},
		{"proxy-t", "proxy", "example.com:443", "\n    probe_opts:\n      proxy_url: http://proxy:8888"},
	}

	var targets strings.Builder
	for _, pt := range probeTypes {
		targets.WriteString("  - name: \"" + pt.name + "\"\n")
		targets.WriteString("    address: \"" + pt.address + "\"\n")
		targets.WriteString("    probe_type: " + pt.probeType + "\n")
		targets.WriteString("    timeout: 5s\n")
		if pt.extra != "" {
			targets.WriteString(pt.extra + "\n")
		}
	}

	yaml := "agent:\n  default_interval: 30s\n\ntargets:\n" + targets.String()

	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error for all valid probe types, got: %v", err)
	}
	if len(cfg.Targets) != len(probeTypes) {
		t.Errorf("expected %d targets, got %d", len(probeTypes), len(cfg.Targets))
	}
}

func TestLoadConfig_ICMPPayloadSizesDuplicateValues(t *testing.T) {
	yaml := `
agent:
  default_interval: 300s

targets:
  - name: "mtu-dup"
    address: "example.com"
    probe_type: mtu
    timeout: 30s
    probe_opts:
      icmp_payload_sizes: [1472, 1472, 1272]
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for equal adjacent icmp_payload_sizes, got nil")
	}
	if !strings.Contains(err.Error(), "descending order") {
		t.Errorf("error = %q, want it to mention descending order", err.Error())
	}
}

// --- allowed_tag_keys tests ---

func TestLoadConfig_AllowedTagKeysAllowlistMode(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  allowed_tag_keys:
    - service
    - scope

targets:
  - name: "t1"
    address: "example.com:443"
    probe_type: tcp
    timeout: 5s
    tags:
      service: "web"
      scope: "same-region"
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	keys := CollectTagKeys(cfg)
	if len(keys) != 2 || keys[0] != "scope" || keys[1] != "service" {
		t.Errorf("CollectTagKeys = %v, want [scope service]", keys)
	}
}

func TestLoadConfig_AllowedTagKeysRejectsUnlisted(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  allowed_tag_keys:
    - service

targets:
  - name: "t1"
    address: "example.com:443"
    probe_type: tcp
    timeout: 5s
    tags:
      service: "web"
      scope: "same-region"
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for tag key not in allowlist, got nil")
	}
	if !strings.Contains(err.Error(), "not in agent.allowed_tag_keys") {
		t.Errorf("error = %q, want it to mention allowed_tag_keys", err.Error())
	}
}

func TestLoadConfig_AllowedTagKeysDuplicateRejected(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  allowed_tag_keys:
    - service
    - service

targets:
  - name: "t1"
    address: "example.com:443"
    probe_type: tcp
    timeout: 5s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for duplicate allowed_tag_keys, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate entry") {
		t.Errorf("error = %q, want it to mention duplicate", err.Error())
	}
}

func TestLoadConfig_AllowedTagKeysInvalidLabelName(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  allowed_tag_keys:
    - "123invalid"

targets:
  - name: "t1"
    address: "example.com:443"
    probe_type: tcp
    timeout: 5s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for invalid Prometheus label name, got nil")
	}
	if !strings.Contains(err.Error(), "not a valid Prometheus label name") {
		t.Errorf("error = %q, want it to mention invalid label name", err.Error())
	}
}

func TestLoadConfig_AllowedTagKeysExceedsMaxGlobalTagKeys(t *testing.T) {
	var keys strings.Builder
	keys.WriteString("  allowed_tag_keys:\n")
	for i := 0; i <= MaxGlobalTagKeys; i++ {
		fmt.Fprintf(&keys, "    - key_%d\n", i)
	}
	yaml := fmt.Sprintf(`
agent:
  default_interval: 30s
%s
targets:
  - name: "t1"
    address: "example.com:443"
    probe_type: tcp
    timeout: 5s
`, keys.String())
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for exceeding MaxGlobalTagKeys in allowed_tag_keys, got nil")
	}
	if !strings.Contains(err.Error(), "allowed_tag_keys has") {
		t.Errorf("error = %q, want it to mention allowed_tag_keys limit", err.Error())
	}
}

func TestLoadConfig_AllowedTagKeysFixedLabelCollision(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  allowed_tag_keys:
    - target

targets:
  - name: "t1"
    address: "example.com:443"
    probe_type: tcp
    timeout: 5s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for fixed label collision, got nil")
	}
	if !strings.Contains(err.Error(), "collides with a fixed label") {
		t.Errorf("error = %q, want it to mention fixed label collision", err.Error())
	}
}

func TestLoadConfig_DynamicModeValidatesTagKeys(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "t1"
    address: "example.com:443"
    probe_type: tcp
    timeout: 5s
    tags:
      "123bad": "val"
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for invalid tag key in dynamic mode, got nil")
	}
	if !strings.Contains(err.Error(), "not a valid Prometheus label name") {
		t.Errorf("error = %q, want it to mention invalid label name", err.Error())
	}
}

func TestLoadConfig_DynamicModeFixedLabelCollision(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "t1"
    address: "example.com:443"
    probe_type: tcp
    timeout: 5s
    tags:
      network_path: "proxy"
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for fixed label collision in dynamic mode, got nil")
	}
	if !strings.Contains(err.Error(), "collides with a fixed label") {
		t.Errorf("error = %q, want it to mention fixed label collision", err.Error())
	}
}

func TestLoadConfig_DynamicModeMaxGlobalTagKeys(t *testing.T) {
	// Build a config with 31 unique tag keys across targets to exceed MaxGlobalTagKeys.
	var targets strings.Builder
	for i := 0; i <= MaxGlobalTagKeys; i++ {
		fmt.Fprintf(&targets, `
  - name: "t%d"
    address: "example.com:%d"
    probe_type: tcp
    timeout: 5s
    tags:
      key_%d: "val"
`, i, 8000+i, i)
	}
	yaml := "agent:\n  default_interval: 30s\n\ntargets:\n" + targets.String()

	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for exceeding MaxGlobalTagKeys, got nil")
	}
	if !strings.Contains(err.Error(), "too many unique tag keys") {
		t.Errorf("error = %q, want it to mention too many unique tag keys", err.Error())
	}
}

func TestLoadConfig_EmptyAllowedTagKeysIsDynamicMode(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s
  allowed_tag_keys: []

targets:
  - name: "t1"
    address: "example.com:443"
    probe_type: tcp
    timeout: 5s
    tags:
      service: "web"
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error (dynamic mode), got: %v", err)
	}
	keys := CollectTagKeys(cfg)
	if len(keys) != 1 || keys[0] != "service" {
		t.Errorf("CollectTagKeys = %v, want [service]", keys)
	}
}

func TestLoadConfig_HTTPBodyMissingMatcher(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "body-no-matcher"
    address: "https://example.com"
    probe_type: http_body
    timeout: 5s
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for http_body without body_match_regex or body_match_string, got nil")
	}
	if !strings.Contains(err.Error(), "body_match_regex") || !strings.Contains(err.Error(), "body_match_string") {
		t.Errorf("error = %q, want it to mention body_match_regex and body_match_string", err.Error())
	}
}

func TestLoadConfig_InvalidHTTPBodyRegex(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "body-bad-regex"
    address: "https://example.com"
    probe_type: http_body
    timeout: 5s
    probe_opts:
      body_match_regex: "[invalid(regex"
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatal("expected error for invalid body_match_regex, got nil")
	}
	if !strings.Contains(err.Error(), "invalid body_match_regex") {
		t.Fatalf("error = %q, want it to mention invalid body_match_regex", err.Error())
	}
}

func TestLoadConfig_ValidHTTPBodyRegex(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "body-ok-regex"
    address: "https://example.com"
    probe_type: http_body
    timeout: 5s
    probe_opts:
      body_match_regex: "status.*ok"
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error for valid body_match_regex, got: %v", err)
	}
}

func TestLoadConfig_HTTPRegexValidationDoesNotApplyToHTTPProbe(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "http-ignores-body-regex"
    address: "https://example.com"
    probe_type: http
    timeout: 5s
    probe_opts:
      body_match_regex: "[invalid(regex"
`
	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error because body_match_regex is only validated for http_body, got: %v", err)
	}
}

func TestCollectTagKeys_AllowlistReturnsCopyNotMutatingConfig(t *testing.T) {
	cfg := &Config{
		Agent: AgentConfig{
			AllowedTagKeys: []string{"zebra", "alpha", "middle"},
		},
	}
	keys := CollectTagKeys(cfg)
	// Returned keys should be sorted.
	if keys[0] != "alpha" || keys[1] != "middle" || keys[2] != "zebra" {
		t.Errorf("CollectTagKeys = %v, want sorted [alpha middle zebra]", keys)
	}
	// Original config slice must not be mutated.
	if cfg.Agent.AllowedTagKeys[0] != "zebra" {
		t.Errorf("AllowedTagKeys[0] = %q, want %q (original order preserved)", cfg.Agent.AllowedTagKeys[0], "zebra")
	}
}

func TestLoadConfig_InvalidExpectedStatusCode(t *testing.T) {
	tests := []struct {
		name      string
		probeType string
		codes     string
		wantCode  string
	}{
		{"http below range", "http", "[99]", "99"},
		{"http above range", "http", "[200, 700]", "700"},
		{"http body below range", "http_body", "[99]", "99"},
		{"http body above range", "http_body", "[200, 700]", "700"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := fmt.Sprintf(`
agent:
  default_interval: 30s

targets:
  - name: "http-bad-code"
    address: "https://example.com"
    probe_type: %s
    timeout: 5s
    probe_opts:
      expected_status_codes: %s
%s`, tt.probeType, tt.codes, httpBodyMatchOpt(tt.probeType))
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err == nil {
				t.Fatal("expected error for invalid expected_status_codes, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantCode) || !strings.Contains(err.Error(), "expected_status_codes") {
				t.Errorf("error = %q, want it to mention %s and expected_status_codes", err.Error(), tt.wantCode)
			}
		})
	}
}

func TestLoadConfig_HTTPResponseBodyLimitBytes(t *testing.T) {
	t.Run("negative rejected", func(t *testing.T) {
		yaml := `
agent:
  default_interval: 30s

targets:
  - name: "http-negative-response-body-limit"
    address: "https://example.com"
    probe_type: http
    timeout: 5s
    probe_opts:
      response_body_limit_bytes: -1
`
		_, err := LoadConfig(writeConfigFile(t, yaml))
		if err == nil {
			t.Fatal("expected error for negative response_body_limit_bytes, got nil")
		}
		if !strings.Contains(err.Error(), "response_body_limit_bytes") {
			t.Errorf("error = %q, want it to mention response_body_limit_bytes", err.Error())
		}
	})

	t.Run("zero means default", func(t *testing.T) {
		yaml := `
agent:
  default_interval: 30s

targets:
  - name: "http-zero-response-body-limit"
    address: "https://example.com"
    probe_type: http
    timeout: 5s
    probe_opts:
      response_body_limit_bytes: 0
`
		cfg, err := LoadConfig(writeConfigFile(t, yaml))
		if err != nil {
			t.Fatalf("expected no error for zero response_body_limit_bytes, got: %v", err)
		}
		if got := cfg.Targets[0].ProbeOpts.ResponseBodyLimitBytes; got != 0 {
			t.Errorf("ResponseBodyLimitBytes = %d, want 0", got)
		}
	})

	t.Run("positive preserved", func(t *testing.T) {
		yaml := `
agent:
  default_interval: 30s

targets:
  - name: "http-positive-response-body-limit"
    address: "https://example.com"
    probe_type: http
    timeout: 5s
    probe_opts:
      response_body_limit_bytes: 2097152
`
		cfg, err := LoadConfig(writeConfigFile(t, yaml))
		if err != nil {
			t.Fatalf("expected no error for positive response_body_limit_bytes, got: %v", err)
		}
		if got := cfg.Targets[0].ProbeOpts.ResponseBodyLimitBytes; got != 2097152 {
			t.Errorf("ResponseBodyLimitBytes = %d, want 2097152", got)
		}
	})
}

func TestLoadConfig_HTTPRequestBodyBytes(t *testing.T) {
	t.Run("negative rejected", func(t *testing.T) {
		yaml := `
agent:
  default_interval: 30s

targets:
  - name: "http-negative-request-body"
    address: "https://example.com"
    probe_type: http
    timeout: 5s
    probe_opts:
      method: POST
      request_body_bytes: -1
`
		_, err := LoadConfig(writeConfigFile(t, yaml))
		if err == nil {
			t.Fatal("expected error for negative request_body_bytes, got nil")
		}
		if !strings.Contains(err.Error(), "request_body_bytes") {
			t.Errorf("error = %q, want it to mention request_body_bytes", err.Error())
		}
	})

	t.Run("over cap rejected", func(t *testing.T) {
		yaml := fmt.Sprintf(`
agent:
  default_interval: 30s

targets:
  - name: "http-over-cap-request-body"
    address: "https://example.com"
    probe_type: http
    timeout: 5s
    probe_opts:
      method: POST
      request_body_bytes: %d
`, MaxHTTPRequestBodyBytes+1)
		_, err := LoadConfig(writeConfigFile(t, yaml))
		if err == nil {
			t.Fatal("expected error for over-cap request_body_bytes, got nil")
		}
		if !strings.Contains(err.Error(), "request_body_bytes") {
			t.Errorf("error = %q, want it to mention request_body_bytes", err.Error())
		}
	})

	t.Run("requires explicit POST", func(t *testing.T) {
		yaml := `
agent:
  default_interval: 30s

targets:
  - name: "http-request-body-default-method"
    address: "https://example.com"
    probe_type: http
    timeout: 5s
    probe_opts:
      request_body_bytes: 1024
`
		_, err := LoadConfig(writeConfigFile(t, yaml))
		if err == nil {
			t.Fatal("expected error for request_body_bytes without explicit POST, got nil")
		}
		if !strings.Contains(err.Error(), "method: POST") {
			t.Errorf("error = %q, want it to mention method: POST", err.Error())
		}
	})

	t.Run("http body rejected", func(t *testing.T) {
		yaml := `
agent:
  default_interval: 30s

targets:
  - name: "http-body-request-body"
    address: "https://example.com"
    probe_type: http_body
    timeout: 5s
    probe_opts:
      method: POST
      request_body_bytes: 1024
      body_match_string: ok
`
		_, err := LoadConfig(writeConfigFile(t, yaml))
		if err == nil {
			t.Fatal("expected error for request_body_bytes on http_body, got nil")
		}
		if !strings.Contains(err.Error(), "probe_type 'http'") {
			t.Errorf("error = %q, want it to mention probe_type 'http'", err.Error())
		}
	})

	t.Run("non-http rejected", func(t *testing.T) {
		yaml := `
agent:
  default_interval: 30s

targets:
  - name: "tcp-request-body"
    address: "example.com:443"
    probe_type: tcp
    timeout: 5s
    probe_opts:
      request_body_bytes: 1024
`
		_, err := LoadConfig(writeConfigFile(t, yaml))
		if err == nil {
			t.Fatal("expected error for request_body_bytes on tcp, got nil")
		}
		if !strings.Contains(err.Error(), "probe_type 'http'") {
			t.Errorf("error = %q, want it to mention probe_type 'http'", err.Error())
		}
	})

	t.Run("positive preserved", func(t *testing.T) {
		yaml := `
agent:
  default_interval: 30s

targets:
  - name: "http-request-body"
    address: "https://example.com"
    probe_type: http
    timeout: 5s
    probe_opts:
      method: POST
      request_body_bytes: 65536
`
		cfg, err := LoadConfig(writeConfigFile(t, yaml))
		if err != nil {
			t.Fatalf("expected no error for positive request_body_bytes, got: %v", err)
		}
		if got := cfg.Targets[0].ProbeOpts.RequestBodyBytes; got != 65536 {
			t.Errorf("RequestBodyBytes = %d, want 65536", got)
		}
	})
}

func TestLoadConfig_ValidLogLevels(t *testing.T) {
	levels := []string{"debug", "info", "warn", "error"}
	for _, lvl := range levels {
		t.Run(lvl, func(t *testing.T) {
			yaml := fmt.Sprintf(`
agent:
  default_interval: 30s
  log_level: %s

targets:
  - name: "t"
    address: "example.com:80"
    probe_type: tcp
    timeout: 5s
`, lvl)
			cfg, err := LoadConfig(writeConfigFile(t, yaml))
			if err != nil {
				t.Fatalf("expected no error for log_level=%q, got: %v", lvl, err)
			}
			if cfg.Agent.LogLevel != lvl {
				t.Errorf("LogLevel = %q, want %q", cfg.Agent.LogLevel, lvl)
			}
		})
	}
}

func TestLoadConfig_EmptyLogLevelDefaultsToInfo(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "t"
    address: "example.com:80"
    probe_type: tcp
    timeout: 5s
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error for missing log_level, got: %v", err)
	}
	if cfg.Agent.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q (default)", cfg.Agent.LogLevel, "info")
	}
}

func TestLoadConfig_ValidLogFormats(t *testing.T) {
	formats := []string{"text", "json"}
	for _, format := range formats {
		t.Run(format, func(t *testing.T) {
			yaml := fmt.Sprintf(`
agent:
  default_interval: 30s
  log_format: %s

targets:
  - name: "t"
    address: "example.com:80"
    probe_type: tcp
    timeout: 5s
`, format)
			cfg, err := LoadConfig(writeConfigFile(t, yaml))
			if err != nil {
				t.Fatalf("expected no error for log_format=%q, got: %v", format, err)
			}
			if cfg.Agent.LogFormat != format {
				t.Errorf("LogFormat = %q, want %q", cfg.Agent.LogFormat, format)
			}
		})
	}
}

func TestLoadConfig_EmptyLogFormatDefaultsToText(t *testing.T) {
	yaml := `
agent:
  default_interval: 30s

targets:
  - name: "t"
    address: "example.com:80"
    probe_type: tcp
    timeout: 5s
`
	cfg, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error for missing log_format, got: %v", err)
	}
	if cfg.Agent.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want %q (default)", cfg.Agent.LogFormat, "text")
	}
}

func TestLoadConfig_InvalidLogFormats(t *testing.T) {
	cases := []string{
		"TEXT",
		"Json",
		"jsonl",
		"console",
		"pretty",
		"structured",
		" text",
	}
	for _, format := range cases {
		t.Run(format, func(t *testing.T) {
			yaml := fmt.Sprintf(`
agent:
  default_interval: 30s
  log_format: %q

targets:
  - name: "t"
    address: "example.com:80"
    probe_type: tcp
    timeout: 5s
`, format)
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err == nil {
				t.Fatalf("expected error for log_format=%q, got nil", format)
			}
			if !strings.Contains(err.Error(), "log_format") {
				t.Errorf("error %q does not mention log_format", err.Error())
			}
		})
	}
}

func TestLoadConfig_InvalidLogLevels(t *testing.T) {
	cases := []string{
		"DEBUG",
		"Info",
		"Warn",
		"ERROR",
		"warning",
		"debgu",
		"trace",
		"fatal",
		"verbose",
		" info",
	}
	for _, lvl := range cases {
		t.Run(lvl, func(t *testing.T) {
			yaml := fmt.Sprintf(`
agent:
  default_interval: 30s
  log_level: %q

targets:
  - name: "t"
    address: "example.com:80"
    probe_type: tcp
    timeout: 5s
`, lvl)
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err == nil {
				t.Fatalf("expected error for log_level=%q, got nil", lvl)
			}
			if !strings.Contains(err.Error(), "log_level") {
				t.Errorf("error %q does not mention log_level", err.Error())
			}
		})
	}
}

func TestLoadConfig_ValidExpectedStatusCodes(t *testing.T) {
	tests := []struct {
		name      string
		probeType string
		codes     string
	}{
		{"http empty", "http", "[]"},
		{"http valid", "http", "[100, 200, 301, 404, 503, 599]"},
		{"http body empty", "http_body", "[]"},
		{"http body valid", "http_body", "[100, 200, 301, 404, 503, 599]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := fmt.Sprintf(`
agent:
  default_interval: 30s

targets:
  - name: "http-ok-codes"
    address: "https://example.com"
    probe_type: %s
    timeout: 5s
    probe_opts:
      expected_status_codes: %s
%s`, tt.probeType, tt.codes, httpBodyMatchOpt(tt.probeType))
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err != nil {
				t.Fatalf("expected no error for valid status codes, got: %v", err)
			}
		})
	}
}

func TestLoadConfig_MaxTagsPerTargetAccepted(t *testing.T) {
	// Build a target with exactly MaxTagsPerTarget (20) tags.
	tags := make([]string, 0, MaxTagsPerTarget)
	for i := 0; i < MaxTagsPerTarget; i++ {
		tags = append(tags, fmt.Sprintf("      tag_%02d: \"val%d\"", i, i))
	}
	yaml := fmt.Sprintf(`
agent:
  default_interval: 30s
  allowed_tag_keys: [%s]

targets:
  - name: "max-tags"
    address: "example.com:443"
    probe_type: tcp
    timeout: 5s
    tags:
%s
`, func() string {
		keys := make([]string, MaxTagsPerTarget)
		for i := 0; i < MaxTagsPerTarget; i++ {
			keys[i] = fmt.Sprintf("tag_%02d", i)
		}
		return strings.Join(keys, ", ")
	}(), strings.Join(tags, "\n"))

	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err != nil {
		t.Fatalf("expected no error for %d tags (MaxTagsPerTarget), got: %v", MaxTagsPerTarget, err)
	}
}

func TestLoadConfig_ExceedMaxTagsPerTargetRejected(t *testing.T) {
	// Build a target with MaxTagsPerTarget+1 (21) tags.
	count := MaxTagsPerTarget + 1
	tags := make([]string, 0, count)
	for i := 0; i < count; i++ {
		tags = append(tags, fmt.Sprintf("      tag_%02d: \"val%d\"", i, i))
	}
	yaml := fmt.Sprintf(`
agent:
  default_interval: 30s
  allowed_tag_keys: [%s]

targets:
  - name: "too-many-tags"
    address: "example.com:443"
    probe_type: tcp
    timeout: 5s
    tags:
%s
`, func() string {
		keys := make([]string, count)
		for i := 0; i < count; i++ {
			keys[i] = fmt.Sprintf("tag_%02d", i)
		}
		return strings.Join(keys, ", ")
	}(), strings.Join(tags, "\n"))

	_, err := LoadConfig(writeConfigFile(t, yaml))
	if err == nil {
		t.Fatalf("expected error for %d tags (exceeds MaxTagsPerTarget=%d), got nil", count, MaxTagsPerTarget)
	}
	if !strings.Contains(err.Error(), "too many tags") {
		t.Fatalf("error = %q, want it to contain 'too many tags'", err.Error())
	}
}

func TestLoadConfig_AgentDefaultICMPPayloadSizesNonPositiveRejected(t *testing.T) {
	tests := []struct {
		name  string
		sizes string
	}{
		{"zero value", "[1472, 0]"},
		{"negative value", "[1472, -1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := fmt.Sprintf(`
agent:
  default_interval: 300s
  default_icmp_payload_sizes: %s

targets:
  - name: "mtu-t"
    address: "example.com"
    probe_type: mtu
    timeout: 30s
`, tt.sizes)
			_, err := LoadConfig(writeConfigFile(t, yaml))
			if err == nil {
				t.Fatal("expected error for non-positive default_icmp_payload_sizes value, got nil")
			}
			if !strings.Contains(err.Error(), "must be > 0") {
				t.Fatalf("error = %q, want it to contain 'must be > 0'", err.Error())
			}
		})
	}
}
