// Package config handles YAML configuration loading and validation.
package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"regexp"
	"sort"
	"time"

	"go.yaml.in/yaml/v4"
)

// ProbeType enumerates the supported probe types.
type ProbeType string

const (
	ProbeTypeTCP          ProbeType = "tcp"
	ProbeTypeHTTP         ProbeType = "http"
	ProbeTypeICMP         ProbeType = "icmp"
	ProbeTypeMTU          ProbeType = "mtu"
	ProbeTypeDNS          ProbeType = "dns"
	ProbeTypeTLSCert      ProbeType = "tls_cert"
	ProbeTypeHTTPBody     ProbeType = "http_body"
	ProbeTypeProxyConnect ProbeType = "proxy_connect"
)

// ValidProbeTypes is the set of all valid probe type values.
var ValidProbeTypes = map[ProbeType]bool{
	ProbeTypeTCP:          true,
	ProbeTypeHTTP:         true,
	ProbeTypeICMP:         true,
	ProbeTypeMTU:          true,
	ProbeTypeDNS:          true,
	ProbeTypeTLSCert:      true,
	ProbeTypeHTTPBody:     true,
	ProbeTypeProxyConnect: true,
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

// ValidLogFormats is the case-sensitive set of allowed agent.log_format values.
// The empty string is resolved to "text" in applyDefaults.
var ValidLogFormats = map[string]bool{
	"text": true,
	"json": true,
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

// MaxProxies is a safety-net limit against broken config generation creating
// excessive fixed proxy label values.
const MaxProxies = 64

// MaxHTTPRequestBodyBytes caps generated HTTP request bodies. This prevents a
// misconfigured probe from creating excessive background upload traffic.
const MaxHTTPRequestBodyBytes int64 = 16 << 20 // 16 MiB

// FixedLabels are the label names hardcoded in the agent binary.
// Tag keys must not collide with these.
var FixedLabels = map[string]bool{
	"target":         true,
	"target_name":    true,
	"probe_type":     true,
	"proxy_name":     true,
	"proxy_endpoint": true,
}

var proxyNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

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
	Agent   AgentConfig            `yaml:"agent"`
	Proxies map[string]ProxyConfig `yaml:"proxies"`
	Targets []TargetConfig         `yaml:"targets"`
}

// ProxyConfig defines a named proxy registry entry. URL is raw YAML input and
// is intentionally excluded from JSON; Endpoint is the canonical clean identity.
type ProxyConfig struct {
	URL           string `yaml:"url" json:"-"`
	Endpoint      string `yaml:"-" json:"endpoint"`
	TLSSkipVerify bool   `yaml:"tls_skip_verify" json:"tls_skip_verify"`
	Username      string `yaml:"username" json:"username,omitempty"`
	UsernameEnv   string `yaml:"username_env" json:"username_env,omitempty"`
	UsernameFile  string `yaml:"username_file" json:"username_file,omitempty"`
	PasswordEnv   string `yaml:"password_env" json:"password_env,omitempty"`
	PasswordFile  string `yaml:"password_file" json:"password_file,omitempty"`

	Credentials ResolvedProxyCredentials `yaml:"-" json:"-"`
}

// ResolvedProxyConfig is attached to targets that reference a proxy. It carries
// hashable effective proxy fields plus runtime-only credentials.
type ResolvedProxyConfig struct {
	Endpoint      string `json:"endpoint"`
	TLSSkipVerify bool   `json:"tls_skip_verify"`
	Username      string `json:"username,omitempty"`
	UsernameEnv   string `json:"username_env,omitempty"`
	UsernameFile  string `json:"username_file,omitempty"`
	PasswordEnv   string `json:"password_env,omitempty"`
	PasswordFile  string `json:"password_file,omitempty"`

	Credentials ResolvedProxyCredentials `json:"-"`
}

// ResolvedProxyCredentials stores secret runtime values outside JSON hashing.
type ResolvedProxyCredentials struct {
	username string
	password string
}

// NewResolvedProxyCredentials constructs runtime proxy credentials while
// keeping the underlying fields out of JSON/logging-capable paths.
func NewResolvedProxyCredentials(username, password string) ResolvedProxyCredentials {
	return ResolvedProxyCredentials{username: username, password: password}
}

// BasicAuthHeader returns a Proxy-Authorization header value without exposing
// the underlying credential strings to callers.
func (c ResolvedProxyCredentials) BasicAuthHeader() (string, bool) {
	if c.username == "" && c.password == "" {
		return "", false
	}
	auth := c.username + ":" + c.password
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(auth)), true
}

// AgentConfig holds agent-level settings.
type AgentConfig struct {
	ListenAddr              string        `yaml:"listen_addr"`
	MetricsPath             string        `yaml:"metrics_path"`
	DefaultInterval         time.Duration `yaml:"default_interval"`
	DefaultTimeout          time.Duration `yaml:"default_timeout"`
	InitialProbeJitter      time.Duration `yaml:"initial_probe_jitter"`
	DefaultICMPPayloadSizes []int         `yaml:"default_icmp_payload_sizes"`
	LogLevel                string        `yaml:"log_level"`
	LogFormat               string        `yaml:"log_format"`
	EnableRuntimeMetrics    bool          `yaml:"enable_runtime_metrics"`
	AllowedTagKeys          []string      `yaml:"allowed_tag_keys"`

	// DNSResolver overrides the system DNS resolver for all hostname → IP
	// lookups performed by NetSonar across all probe types. The empty
	// string means "use the host's system resolver path" (NSS,
	// systemd-resolved, /etc/resolv.conf). A non-empty value must be a
	// validated IP literal with port (e.g. "8.8.8.8:53",
	// "[2001:db8::1]:53"); hostnames are rejected at load time.
	//
	// Per-target overrides via TargetConfig.DNSResolver take precedence.
	DNSResolver string `yaml:"dns_resolver"`
}

// TargetConfig defines a single probe target.
type TargetConfig struct {
	Name          string               `yaml:"name"`
	Address       string               `yaml:"address"`
	ProbeType     ProbeType            `yaml:"probe_type"`
	Interval      time.Duration        `yaml:"interval"`
	Timeout       time.Duration        `yaml:"timeout"`
	Tags          map[string]string    `yaml:"tags"`
	ProbeOpts     ProbeOptions         `yaml:"probe_opts"`
	ProxyName     string               `yaml:"proxy_name"`
	ResolvedProxy *ResolvedProxyConfig `yaml:"-" json:"resolved_proxy,omitempty"`

	// DNSResolver overrides the agent-level DNS resolver for this target.
	//
	// YAML semantics (three-state pointer):
	//   field omitted          → inherit agent.dns_resolver (most common)
	//   dns_resolver: ""       → force system resolver (opt out of agent default)
	//   dns_resolver: "x:y"    → use this specific resolver
	//
	// After applyDefaults() this field is guaranteed non-nil. Hashing
	// (hash.go) and Equal() rely on this post-applyDefaults invariant.
	DNSResolver *string `yaml:"dns_resolver"`
}

// ProbeOptions holds probe-type-specific settings.
type ProbeOptions struct {
	// HTTP/HTTPS options
	Method                          string            `yaml:"method"`
	Headers                         map[string]string `yaml:"headers"`
	ExpectedStatusCodes             []int             `yaml:"expected_status_codes"`
	ExpectedProxyConnectStatusCodes []int             `yaml:"expected_proxy_connect_status_codes"`
	FollowRedirects                 bool              `yaml:"follow_redirects"`
	TLSSkipVerify                   bool              `yaml:"tls_skip_verify"`
	TLSEmitCertMetrics              bool              `yaml:"tls_emit_cert_metrics"`
	ResponseBodyLimitBytes          int64             `yaml:"response_body_limit_bytes"`
	RequestBodyBytes                int64             `yaml:"request_body_bytes"`

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
}

// LoadConfig reads a YAML configuration file, applies defaults, and validates
// the result. It returns a fully populated Config or a descriptive error.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := yaml.Load(data, &cfg, yaml.WithKnownFields()); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}

	applyDefaults(&cfg)

	if err := validate(&cfg, envLookupFunc(os.LookupEnv), fileReadFunc(os.ReadFile)); err != nil {
		return nil, err
	}

	return &cfg, nil
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
