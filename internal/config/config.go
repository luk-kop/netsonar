// Package config handles YAML configuration loading and validation.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"

	"go.yaml.in/yaml/v4"
)

// ProbeType enumerates the supported probe types.
type ProbeType string

const (
	ProbeTypeTCP      ProbeType = "tcp"
	ProbeTypeHTTP     ProbeType = "http"
	ProbeTypeICMP     ProbeType = "icmp"
	ProbeTypeMTU      ProbeType = "mtu"
	ProbeTypeDNS      ProbeType = "dns"
	ProbeTypeTLSCert  ProbeType = "tls_cert"
	ProbeTypeHTTPBody ProbeType = "http_body"
	ProbeTypeProxy    ProbeType = "proxy"
)

// ValidProbeTypes is the set of all valid probe type values.
var ValidProbeTypes = map[ProbeType]bool{
	ProbeTypeTCP:      true,
	ProbeTypeHTTP:     true,
	ProbeTypeICMP:     true,
	ProbeTypeMTU:      true,
	ProbeTypeDNS:      true,
	ProbeTypeTLSCert:  true,
	ProbeTypeHTTPBody: true,
	ProbeTypeProxy:    true,
}

// ValidDNSQueryTypes is the set of allowed dns_query_type values.
var ValidDNSQueryTypes = map[string]bool{
	"A":     true,
	"AAAA":  true,
	"CNAME": true,
}

// ValidLogLevels is the case-sensitive set of allowed agent.log_level values.
// The empty string is resolved to "info" in applyDefaults; everything else
// must match exactly.
var ValidLogLevels = map[string]bool{
	"debug": true,
	"info":  true,
	"warn":  true,
	"error": true,
}

var validHTTPMethods = map[string]bool{
	"GET":  true,
	"HEAD": true,
	"POST": true,
}

// MaxTagsPerTarget is the maximum number of tags allowed per target.
// Each unique tag key becomes a Prometheus label, and high label
// cardinality degrades TSDB performance.
const MaxTagsPerTarget = 20

// MaxGlobalTagKeys is the safety-net limit on the total number of unique
// tag keys across all targets in legacy (dynamic) mode. This prevents
// accidentally building a very wide label schema when allowed_tag_keys
// is not configured.
const MaxGlobalTagKeys = 30

// FixedLabels are the label names hardcoded in the agent binary.
// Tag keys must not collide with these.
var FixedLabels = map[string]bool{
	"target":      true,
	"target_name": true,
	"probe_type":  true,
	"proxied":     true,
}

// prometheusLabelRe matches valid Prometheus label names: [a-zA-Z_][a-zA-Z0-9_]*.
var prometheusLabelRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// DefaultICMPPayloadSizes is the default set of ICMP payload sizes used
// for MTU probes when a target does not specify icmp_payload_sizes.
// Sorted descending. Path MTU = payload + 28 (IP + ICMP headers).
var DefaultICMPPayloadSizes = []int{1472, 1392, 1372, 1272, 1172, 1072}

const (
	DefaultMTURetries           = 3
	DefaultMTUPerAttemptTimeout = 2 * time.Second
)

// Config is the top-level configuration structure.
type Config struct {
	Agent   AgentConfig    `yaml:"agent"`
	Targets []TargetConfig `yaml:"targets"`
}

// AgentConfig holds agent-level settings.
type AgentConfig struct {
	ListenAddr              string        `yaml:"listen_addr"`
	MetricsPath             string        `yaml:"metrics_path"`
	DefaultInterval         time.Duration `yaml:"default_interval"`
	DefaultTimeout          time.Duration `yaml:"default_timeout"`
	DefaultICMPPayloadSizes []int         `yaml:"default_icmp_payload_sizes"`
	LogLevel                string        `yaml:"log_level"`
	AllowedTagKeys          []string      `yaml:"allowed_tag_keys"`
}

// TargetConfig defines a single probe target.
type TargetConfig struct {
	Name      string            `yaml:"name"`
	Address   string            `yaml:"address"`
	ProbeType ProbeType         `yaml:"probe_type"`
	Interval  time.Duration     `yaml:"interval"`
	Timeout   time.Duration     `yaml:"timeout"`
	Tags      map[string]string `yaml:"tags"`
	ProbeOpts ProbeOptions      `yaml:"probe_opts"`
}

// ProbeOptions holds probe-type-specific settings.
type ProbeOptions struct {
	// HTTP/HTTPS options
	Method              string            `yaml:"method"`
	Headers             map[string]string `yaml:"headers"`
	ExpectedStatusCodes []int             `yaml:"expected_status_codes"`
	FollowRedirects     bool              `yaml:"follow_redirects"`
	TLSSkipVerify       bool              `yaml:"tls_skip_verify"`

	// HTTP body validation
	BodyMatchRegex  string `yaml:"body_match_regex"`
	BodyMatchString string `yaml:"body_match_string"`

	// ICMP options
	PingCount       int     `yaml:"ping_count"`
	PingIntervalSec float64 `yaml:"ping_interval"`

	// MTU/PMTUD options — ICMP payload sizes in bytes (descending order).
	// Path MTU is calculated as: largest successful payload + 28 (20 IP + 8 ICMP headers).
	ICMPPayloadSizes     []int         `yaml:"icmp_payload_sizes"`
	ExpectedMinMTU       int           `yaml:"expected_min_mtu"`
	MTURetries           int           `yaml:"mtu_retries"`
	MTUPerAttemptTimeout time.Duration `yaml:"mtu_per_attempt_timeout"`

	// DNS options
	DNSQueryName       string   `yaml:"dns_query_name"`
	DNSQueryType       string   `yaml:"dns_query_type"`
	DNSServer          string   `yaml:"dns_server"`
	DNSExpectedResults []string `yaml:"dns_expected"`

	// Proxy options
	ProxyURL string `yaml:"proxy_url"`
}

// LoadConfig reads a YAML configuration file, applies defaults, and validates
// the result. It returns a fully populated Config or a descriptive error.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// applyDefaults fills in missing target-level fields from agent-level defaults.
func applyDefaults(cfg *Config) {
	if cfg.Agent.ListenAddr == "" {
		cfg.Agent.ListenAddr = ":9275"
	}
	if cfg.Agent.MetricsPath == "" {
		cfg.Agent.MetricsPath = "/metrics"
	}
	if cfg.Agent.LogLevel == "" {
		cfg.Agent.LogLevel = "info"
	}

	for i := range cfg.Targets {
		if cfg.Targets[i].Interval == 0 {
			cfg.Targets[i].Interval = cfg.Agent.DefaultInterval
		}
		if cfg.Targets[i].Timeout == 0 {
			cfg.Targets[i].Timeout = cfg.Agent.DefaultTimeout
		}

		// Apply default ICMP payload sizes for MTU probes that don't specify their own.
		if cfg.Targets[i].ProbeType == ProbeTypeMTU && len(cfg.Targets[i].ProbeOpts.ICMPPayloadSizes) == 0 {
			if len(cfg.Agent.DefaultICMPPayloadSizes) > 0 {
				cfg.Targets[i].ProbeOpts.ICMPPayloadSizes = cfg.Agent.DefaultICMPPayloadSizes
			} else {
				cfg.Targets[i].ProbeOpts.ICMPPayloadSizes = DefaultICMPPayloadSizes
			}
		}
		if cfg.Targets[i].ProbeType == ProbeTypeMTU {
			opts := &cfg.Targets[i].ProbeOpts
			if opts.MTURetries == 0 {
				opts.MTURetries = DefaultMTURetries
			}
			if opts.MTUPerAttemptTimeout == 0 {
				opts.MTUPerAttemptTimeout = DefaultMTUPerAttemptTimeout
			}
			if opts.ExpectedMinMTU == 0 && len(opts.ICMPPayloadSizes) > 0 {
				opts.ExpectedMinMTU = opts.ICMPPayloadSizes[0] + 28
			}
		}
	}
}

// validate checks all configuration invariants and returns the first violation found.
func validate(cfg *Config) error {
	// Validate agent.log_level against a case-sensitive allowlist so that
	// typos (e.g. "DEBUG", "warning", "debgu") fail loudly at load time
	// instead of silently falling back to "info" in the logger setup.
	if !ValidLogLevels[cfg.Agent.LogLevel] {
		return fmt.Errorf("agent: invalid log_level %q (valid: debug, info, warn, error)", cfg.Agent.LogLevel)
	}

	// Validate agent-level default_icmp_payload_sizes if provided.
	if len(cfg.Agent.DefaultICMPPayloadSizes) > 0 {
		for i := 1; i < len(cfg.Agent.DefaultICMPPayloadSizes); i++ {
			if cfg.Agent.DefaultICMPPayloadSizes[i] >= cfg.Agent.DefaultICMPPayloadSizes[i-1] {
				return fmt.Errorf("agent: default_icmp_payload_sizes must be sorted in descending order (found %d at index %d after %d)",
					cfg.Agent.DefaultICMPPayloadSizes[i], i, cfg.Agent.DefaultICMPPayloadSizes[i-1])
			}
		}
	}

	// Validate allowed_tag_keys if configured (allowlist mode).
	if err := validateAllowedTagKeys(cfg.Agent.AllowedTagKeys); err != nil {
		return err
	}

	allowlist := make(map[string]bool, len(cfg.Agent.AllowedTagKeys))
	for _, k := range cfg.Agent.AllowedTagKeys {
		allowlist[k] = true
	}
	allowlistMode := len(allowlist) > 0

	seen := make(map[string]bool, len(cfg.Targets))

	for i, t := range cfg.Targets {
		// Required fields.
		if t.Name == "" {
			return fmt.Errorf("target[%d]: missing required field 'name'", i)
		}
		if t.Address == "" {
			return fmt.Errorf("target %q: missing required field 'address'", t.Name)
		}

		// Unique names.
		if seen[t.Name] {
			return fmt.Errorf("target %q: duplicate target name", t.Name)
		}
		seen[t.Name] = true

		// Valid probe type.
		if !ValidProbeTypes[t.ProbeType] {
			return fmt.Errorf("target %q: invalid probe_type %q (valid: tcp, http, icmp, mtu, dns, tls_cert, http_body, proxy)", t.Name, t.ProbeType)
		}

		// Interval must be positive after defaults have been applied. Zero
		// or negative values would panic time.NewTicker in the scheduler.
		if t.Interval <= 0 {
			return fmt.Errorf("target %q: interval must be > 0 (set target.interval or agent.default_interval)", t.Name)
		}

		// Timeout must be positive and no greater than interval.
		if t.Timeout <= 0 {
			return fmt.Errorf("target %q: timeout must be > 0 (set target.timeout or agent.default_timeout)", t.Name)
		}
		if t.Timeout > t.Interval {
			return fmt.Errorf("target %q: timeout (%s) exceeds interval (%s)", t.Name, t.Timeout, t.Interval)
		}

		// Tag count limit.
		if len(t.Tags) > MaxTagsPerTarget {
			return fmt.Errorf("target %q: too many tags (%d), maximum is %d", t.Name, len(t.Tags), MaxTagsPerTarget)
		}

		// Validate tag keys against allowlist or Prometheus naming rules.
		if err := validateTagKeys(t, allowlistMode, allowlist); err != nil {
			return err
		}

		// Probe-type-specific validation.
		if err := validateProbeOpts(t); err != nil {
			return err
		}
	}

	// Dynamic mode: enforce MaxGlobalTagKeys safety net.
	if !allowlistMode {
		globalKeys := collectDynamicTagKeys(cfg)
		if len(globalKeys) > MaxGlobalTagKeys {
			return fmt.Errorf("too many unique tag keys across all targets (%d), maximum is %d; consider using agent.allowed_tag_keys",
				len(globalKeys), MaxGlobalTagKeys)
		}
	}

	return nil
}

// validateAllowedTagKeys checks the agent.allowed_tag_keys list for
// duplicates, invalid Prometheus label names, and collisions with fixed labels.
func validateAllowedTagKeys(keys []string) error {
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		if seen[k] {
			return fmt.Errorf("agent: duplicate entry %q in allowed_tag_keys", k)
		}
		seen[k] = true
		if err := validateLabelName(k); err != nil {
			return fmt.Errorf("agent: allowed_tag_keys: %w", err)
		}
	}
	return nil
}

// validateTagKeys checks every tag key on a target. In allowlist mode it
// rejects keys not in the allowlist. In dynamic mode it validates Prometheus
// naming rules and fixed-label collisions.
func validateTagKeys(t TargetConfig, allowlistMode bool, allowlist map[string]bool) error {
	for k := range t.Tags {
		if allowlistMode {
			if !allowlist[k] {
				return fmt.Errorf("target %q: tag key %q is not in agent.allowed_tag_keys", t.Name, k)
			}
		} else {
			if err := validateLabelName(k); err != nil {
				return fmt.Errorf("target %q: %w", t.Name, err)
			}
		}
	}
	return nil
}

// validateLabelName checks that a tag key is a valid Prometheus label name
// and does not collide with the agent's fixed labels.
func validateLabelName(k string) error {
	if !prometheusLabelRe.MatchString(k) {
		return fmt.Errorf("tag key %q is not a valid Prometheus label name", k)
	}
	if FixedLabels[k] {
		return fmt.Errorf("tag key %q collides with a fixed label", k)
	}
	return nil
}

// validateProbeOpts checks probe-type-specific option constraints.
func validateProbeOpts(t TargetConfig) error {
	switch t.ProbeType {
	case ProbeTypeHTTP, ProbeTypeHTTPBody:
		if err := validateHTTPMethod(t); err != nil {
			return err
		}
		if t.ProbeType == ProbeTypeHTTPBody {
			if t.ProbeOpts.BodyMatchRegex == "" && t.ProbeOpts.BodyMatchString == "" {
				return fmt.Errorf("target %q: probe_type 'http_body' requires 'body_match_regex' or 'body_match_string' in probe_opts", t.Name)
			}
			if err := validateBodyMatchRegex(t); err != nil {
				return err
			}
		}
		if err := validateExpectedStatusCodes(t); err != nil {
			return err
		}
		if err := validateProxyURL(t.Name, t.ProbeOpts.ProxyURL); err != nil {
			return err
		}
	case ProbeTypeICMP:
		if err := validateIPv4OnlyAddress(t); err != nil {
			return err
		}
	case ProbeTypeMTU:
		if err := validateIPv4OnlyAddress(t); err != nil {
			return err
		}
		if err := validateICMPPayloadSizes(t); err != nil {
			return err
		}
	case ProbeTypeDNS:
		if err := validateDNSQueryType(t); err != nil {
			return err
		}
	case ProbeTypeProxy:
		if t.ProbeOpts.ProxyURL == "" {
			return fmt.Errorf("target %q: probe_type 'proxy' requires 'proxy_url' in probe_opts", t.Name)
		}
		if err := validateProxyURL(t.Name, t.ProbeOpts.ProxyURL); err != nil {
			return err
		}
	}
	return nil
}

func validateBodyMatchRegex(t TargetConfig) error {
	if t.ProbeOpts.BodyMatchRegex == "" {
		return nil
	}
	if _, err := regexp.Compile(t.ProbeOpts.BodyMatchRegex); err != nil {
		return fmt.Errorf("target %q: invalid body_match_regex: %w", t.Name, err)
	}
	return nil
}

// validateIPv4OnlyAddress rejects literal IPv6 addresses for probe types that
// currently use IPv4-only ICMP sockets. Hostnames are allowed and resolved at
// runtime so config loading never depends on DNS availability.
func validateIPv4OnlyAddress(t TargetConfig) error {
	ip := net.ParseIP(t.Address)
	if ip == nil {
		return nil
	}
	if ip.To4() == nil {
		return fmt.Errorf("target %q: probe_type %q supports IPv4 addresses only", t.Name, t.ProbeType)
	}
	return nil
}

func validateHTTPMethod(t TargetConfig) error {
	method := t.ProbeOpts.Method
	if method == "" {
		return nil
	}
	if !validHTTPMethods[method] {
		return fmt.Errorf("target %q: invalid method %q (valid: GET, HEAD, POST)", t.Name, method)
	}
	return nil
}

// validateExpectedStatusCodes checks that all expected status codes are
// valid HTTP status codes (100-599). An empty or nil list is valid and
// means "accept any fully received response".
func validateExpectedStatusCodes(t TargetConfig) error {
	for _, code := range t.ProbeOpts.ExpectedStatusCodes {
		if code < 100 || code > 599 {
			return fmt.Errorf("target %q: invalid expected_status_codes value %d (valid: 100-599)", t.Name, code)
		}
	}
	return nil
}

// validateProxyURL validates optional HTTP proxy URLs without leaking
// credentials from userinfo in returned errors.
func validateProxyURL(targetName, raw string) error {
	if raw == "" {
		return nil
	}

	u, err := url.ParseRequestURI(raw)
	if err != nil {
		// Do not wrap err: *url.Error includes the full input URL, including
		// userinfo credentials if present.
		return fmt.Errorf("target %q: proxy_url is not a valid absolute URL", targetName)
	}

	redacted := u.Redacted()

	if u.Opaque != "" {
		return fmt.Errorf("target %q: invalid proxy_url %q: must use scheme://host form", targetName, redacted)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("target %q: invalid proxy_url %q: scheme must be http or https", targetName, redacted)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("target %q: invalid proxy_url %q: host is required", targetName, redacted)
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("target %q: invalid proxy_url %q: path is not allowed", targetName, redacted)
	}
	if u.RawQuery != "" {
		return fmt.Errorf("target %q: invalid proxy_url %q: query is not allowed", targetName, redacted)
	}
	if u.Fragment != "" {
		return fmt.Errorf("target %q: invalid proxy_url %q: fragment is not allowed", targetName, redacted)
	}

	if port := u.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return fmt.Errorf("target %q: invalid proxy_url %q: port must be in range 1-65535", targetName, redacted)
		}
	}

	return nil
}

// validateICMPPayloadSizes checks that icmp_payload_sizes is sorted in descending order.
func validateICMPPayloadSizes(t TargetConfig) error {
	sizes := t.ProbeOpts.ICMPPayloadSizes
	if len(sizes) == 0 {
		return fmt.Errorf("target %q: icmp_payload_sizes must be non-empty", t.Name)
	}
	for i := 1; i < len(sizes); i++ {
		if sizes[i] >= sizes[i-1] {
			return fmt.Errorf("target %q: icmp_payload_sizes must be sorted in descending order (found %d at index %d after %d)", t.Name, sizes[i], i, sizes[i-1])
		}
	}
	for i, size := range sizes {
		if size <= 0 {
			return fmt.Errorf("target %q: icmp_payload_sizes values must be > 0 (found %d at index %d)", t.Name, size, i)
		}
	}
	if t.ProbeOpts.ExpectedMinMTU <= 0 {
		return fmt.Errorf("target %q: expected_min_mtu must be > 0", t.Name)
	}
	maxTestedMTU := sizes[0] + 28
	if t.ProbeOpts.ExpectedMinMTU > maxTestedMTU {
		return fmt.Errorf("target %q: expected_min_mtu (%d) exceeds largest tested MTU (%d)", t.Name, t.ProbeOpts.ExpectedMinMTU, maxTestedMTU)
	}
	if t.ProbeOpts.MTURetries < 1 {
		return fmt.Errorf("target %q: mtu_retries must be >= 1", t.Name)
	}
	if t.ProbeOpts.MTUPerAttemptTimeout <= 0 {
		return fmt.Errorf("target %q: mtu_per_attempt_timeout must be > 0", t.Name)
	}
	if t.ProbeOpts.MTUPerAttemptTimeout > t.Timeout {
		return fmt.Errorf("target %q: mtu_per_attempt_timeout (%s) exceeds timeout (%s)", t.Name, t.ProbeOpts.MTUPerAttemptTimeout, t.Timeout)
	}
	return nil
}

// validateDNSQueryType checks that dns_query_type is one of A, AAAA, CNAME.
func validateDNSQueryType(t TargetConfig) error {
	qt := t.ProbeOpts.DNSQueryType
	if qt != "" && !ValidDNSQueryTypes[qt] {
		return fmt.Errorf("target %q: invalid dns_query_type %q (valid: A, AAAA, CNAME)", t.Name, qt)
	}
	return nil
}

// collectDynamicTagKeys gathers unique tag keys from all targets.
func collectDynamicTagKeys(cfg *Config) []string {
	seen := make(map[string]bool)
	for _, t := range cfg.Targets {
		for k := range t.Tags {
			seen[k] = true
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// CollectTagKeys returns the effective set of tag keys for metric registration.
//
// Allowlist mode (allowed_tag_keys has elements): returns a sorted copy of
// the allowlist. The original config slice is never mutated.
//
// Dynamic mode (allowed_tag_keys absent or empty): collects unique tag keys
// from all targets and returns them sorted.
func CollectTagKeys(cfg *Config) []string {
	if len(cfg.Agent.AllowedTagKeys) > 0 {
		keys := make([]string, len(cfg.Agent.AllowedTagKeys))
		copy(keys, cfg.Agent.AllowedTagKeys)
		sort.Strings(keys)
		return keys
	}
	return collectDynamicTagKeys(cfg)
}
