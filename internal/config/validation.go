package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"netsonar/internal/proxyurl"
)

type envLookupFunc func(string) (string, bool)
type fileReadFunc func(string) ([]byte, error)

// validate checks all configuration invariants and returns the first violation found.
func validate(cfg *Config, resolvers ...any) error {
	lookupEnv := envLookupFunc(os.LookupEnv)
	readFile := fileReadFunc(os.ReadFile)
	if len(resolvers) >= 1 {
		lookupEnv = resolvers[0].(envLookupFunc)
	}
	if len(resolvers) >= 2 {
		readFile = resolvers[1].(fileReadFunc)
	}
	// Validate agent.log_level against a case-sensitive allowlist so that
	// typos (e.g. "DEBUG", "warning", "debgu") fail loudly at load time
	// instead of silently falling back to "info" in the logger setup.
	if !ValidLogLevels[cfg.Agent.LogLevel] {
		return fmt.Errorf("agent: invalid log_level %q (valid: debug, info, warn, error)", cfg.Agent.LogLevel)
	}
	if !ValidLogFormats[cfg.Agent.LogFormat] {
		return fmt.Errorf("agent: invalid log_format %q (valid: text, json)", cfg.Agent.LogFormat)
	}

	if err := validateMetricsPath(cfg.Agent.MetricsPath); err != nil {
		return err
	}
	if cfg.Agent.InitialProbeJitter < 0 {
		return fmt.Errorf("agent: initial_probe_jitter must be >= 0")
	}

	if err := validateDNSResolver(cfg.Agent.DNSResolver); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if err := validateProxies(cfg, lookupEnv, readFile); err != nil {
		return err
	}

	// Validate agent-level default_icmp_payload_sizes if provided.
	if len(cfg.Agent.DefaultICMPPayloadSizes) > 0 {
		for i, v := range cfg.Agent.DefaultICMPPayloadSizes {
			if v <= 0 {
				return fmt.Errorf("agent: default_icmp_payload_sizes[%d] = %d must be > 0", i, v)
			}
		}
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
	var shortestInterval time.Duration

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
			return fmt.Errorf("target %q: invalid probe_type %q (valid: tcp, http, icmp, mtu, dns, tls_cert, http_body, proxy_connect)", t.Name, t.ProbeType)
		}

		// Interval must be positive after defaults have been applied. Zero
		// or negative values would panic time.NewTicker in the scheduler.
		if t.Interval <= 0 {
			return fmt.Errorf("target %q: interval must be > 0 (set target.interval or agent.default_interval)", t.Name)
		}
		if shortestInterval == 0 || t.Interval < shortestInterval {
			shortestInterval = t.Interval
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

		// DNSResolver is guaranteed non-nil after applyDefaults; validate
		// the dereferenced value. Empty string means "system resolver",
		// non-empty must be IP literal with port.
		if t.DNSResolver != nil {
			if err := validateDNSResolver(*t.DNSResolver); err != nil {
				return fmt.Errorf("target %q: %w", t.Name, err)
			}
		}

		// Probe-type-specific validation.
		if err := validateProbeOpts(&cfg.Targets[i], cfg.Proxies); err != nil {
			return err
		}
	}

	if cfg.Agent.InitialProbeJitter > 0 && shortestInterval > 0 && cfg.Agent.InitialProbeJitter > shortestInterval {
		return fmt.Errorf("agent: initial_probe_jitter (%s) exceeds shortest target interval (%s)", cfg.Agent.InitialProbeJitter, shortestInterval)
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

func validateProxies(cfg *Config, lookupEnv envLookupFunc, readFile fileReadFunc) error {
	if len(cfg.Proxies) > MaxProxies {
		return fmt.Errorf("proxies has %d entries, maximum is %d; this guardrail catches configuration-generation mistakes", len(cfg.Proxies), MaxProxies)
	}

	names := make([]string, 0, len(cfg.Proxies))
	for name := range cfg.Proxies {
		names = append(names, name)
	}
	sort.Strings(names)

	endpoints := make(map[string]string, len(cfg.Proxies))
	for _, name := range names {
		if !proxyNameRe.MatchString(name) {
			return fmt.Errorf("proxy %q: proxy_name must match %s", name, proxyNameRe.String())
		}
		p := cfg.Proxies[name]
		if p.URL == "" {
			return fmt.Errorf("proxy %q: url is required", name)
		}
		u, err := proxyurl.Parse(p.URL)
		if err != nil {
			return fmt.Errorf("proxy %q: %w", name, err)
		}
		p.Endpoint = proxyurl.Endpoint(u)
		if p.TLSSkipVerify && u.Scheme != "https" {
			return fmt.Errorf("proxies.%s.tls_skip_verify=true is invalid for http:// proxy endpoint; proxy TLS verification only applies to https:// proxies", name)
		}
		if first, ok := endpoints[p.Endpoint]; ok {
			return fmt.Errorf("proxy endpoint %q is registered by both proxy %q and proxy %q", p.Endpoint, first, name)
		}
		endpoints[p.Endpoint] = name

		creds, err := resolveProxyCredentials(name, p, lookupEnv, readFile)
		if err != nil {
			return err
		}
		p.Credentials = creds
		cfg.Proxies[name] = p
	}
	return nil
}

func resolveProxyCredentials(name string, p ProxyConfig, lookupEnv envLookupFunc, readFile fileReadFunc) (ResolvedProxyCredentials, error) {
	usernameSources := nonEmptySourceCount(p.Username, p.UsernameEnv, p.UsernameFile)
	passwordSources := nonEmptySourceCount(p.PasswordEnv, p.PasswordFile)

	if p.Username != "" && p.UsernameEnv != "" {
		return ResolvedProxyCredentials{}, fmt.Errorf("proxy %q: cannot set both username and username_env", name)
	}
	if p.Username != "" && p.UsernameFile != "" {
		return ResolvedProxyCredentials{}, fmt.Errorf("proxy %q: cannot set both username and username_file", name)
	}
	if p.UsernameEnv != "" && p.UsernameFile != "" {
		return ResolvedProxyCredentials{}, fmt.Errorf("proxy %q: cannot set both username_env and username_file", name)
	}
	if p.PasswordEnv != "" && p.PasswordFile != "" {
		return ResolvedProxyCredentials{}, fmt.Errorf("proxy %q: cannot set both password_env and password_file", name)
	}
	if usernameSources == 0 && passwordSources == 0 {
		return ResolvedProxyCredentials{}, nil
	}
	if usernameSources == 0 {
		source := "password source"
		if p.PasswordEnv != "" {
			source = "password_env"
		} else if p.PasswordFile != "" {
			source = "password_file"
		}
		return ResolvedProxyCredentials{}, fmt.Errorf("proxy %q: %s is set but no username source; set username, username_env, or username_file", name, source)
	}
	if passwordSources == 0 {
		return ResolvedProxyCredentials{}, fmt.Errorf("proxy %q: username is set but no password source; set password_env or password_file", name)
	}

	username, err := resolveUsername(name, p, lookupEnv, readFile)
	if err != nil {
		return ResolvedProxyCredentials{}, err
	}
	password, err := resolvePassword(name, p, lookupEnv, readFile)
	if err != nil {
		return ResolvedProxyCredentials{}, err
	}
	return ResolvedProxyCredentials{username: username, password: password}, nil
}

func nonEmptySourceCount(values ...string) int {
	n := 0
	for _, v := range values {
		if v != "" {
			n++
		}
	}
	return n
}

func resolveUsername(name string, p ProxyConfig, lookupEnv envLookupFunc, readFile fileReadFunc) (string, error) {
	switch {
	case p.Username != "":
		return p.Username, nil
	case p.UsernameEnv != "":
		v, ok := lookupEnv(p.UsernameEnv)
		v = trimTrailingNewlines(v)
		if !ok || v == "" {
			return "", fmt.Errorf("proxy %q: username_env %q is not set or empty in environment", name, p.UsernameEnv)
		}
		return v, nil
	case p.UsernameFile != "":
		return readCredentialFile(name, "username_file", p.UsernameFile, readFile)
	default:
		return "", nil
	}
}

func resolvePassword(name string, p ProxyConfig, lookupEnv envLookupFunc, readFile fileReadFunc) (string, error) {
	switch {
	case p.PasswordEnv != "":
		v, ok := lookupEnv(p.PasswordEnv)
		v = trimTrailingNewlines(v)
		if !ok || v == "" {
			return "", fmt.Errorf("proxy %q: password_env %q is not set or empty in environment", name, p.PasswordEnv)
		}
		return v, nil
	case p.PasswordFile != "":
		return readCredentialFile(name, "password_file", p.PasswordFile, readFile)
	default:
		return "", nil
	}
}

func readCredentialFile(name, field, path string, readFile fileReadFunc) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("proxy %q: %s %q must be an absolute path", name, field, path)
	}
	data, err := readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("proxy %q: %s %q does not exist", name, field, path)
		}
		return "", fmt.Errorf("proxy %q: reading %s %q: %w", name, field, path, err)
	}
	value := trimTrailingNewlines(string(data))
	if value == "" {
		return "", fmt.Errorf("proxy %q: %s %q is empty after trimming trailing newlines", name, field, path)
	}
	return value, nil
}

func trimTrailingNewlines(s string) string {
	return strings.TrimRight(s, "\r\n")
}

func validateMetricsPath(metricsPath string) error {
	if !strings.HasPrefix(metricsPath, "/") {
		return fmt.Errorf("agent: metrics_path %q must start with /", metricsPath)
	}
	if metricsPath == "/healthz" || metricsPath == "/readyz" {
		return fmt.Errorf("agent: metrics_path %q conflicts with built-in health endpoints", metricsPath)
	}
	if strings.ContainsAny(metricsPath, "{}") {
		return fmt.Errorf("agent: metrics_path %q must be a plain path, not a ServeMux pattern", metricsPath)
	}
	for _, r := range metricsPath {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("agent: metrics_path %q must not contain whitespace or control characters", metricsPath)
		}
	}
	return nil
}

// validateAllowedTagKeys checks the agent.allowed_tag_keys list for
// length, duplicates, invalid Prometheus label names, and collisions with fixed labels.
func validateAllowedTagKeys(keys []string) error {
	if len(keys) > MaxGlobalTagKeys {
		return fmt.Errorf("agent: allowed_tag_keys has %d entries, maximum is %d", len(keys), MaxGlobalTagKeys)
	}
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
func validateProbeOpts(t *TargetConfig, proxies map[string]ProxyConfig) error {
	if t.ProbeOpts.ResponseBodyLimitBytes < 0 {
		return fmt.Errorf("target %q: response_body_limit_bytes must be >= 0 (0 or omitted uses the 1 MiB default; positive values set an explicit cap)", t.Name)
	}
	if t.ProbeOpts.RequestBodyBytes < 0 {
		return fmt.Errorf("target %q: request_body_bytes must be >= 0", t.Name)
	}
	if t.ProbeOpts.RequestBodyBytes > MaxHTTPRequestBodyBytes {
		return fmt.Errorf("target %q: request_body_bytes must be <= %d", t.Name, MaxHTTPRequestBodyBytes)
	}
	if t.ProbeOpts.RequestBodyBytes > 0 && t.ProbeType != ProbeTypeHTTP {
		return fmt.Errorf("target %q: request_body_bytes is supported only for probe_type 'http'", t.Name)
	}
	if len(t.ProbeOpts.ExpectedProxyConnectStatusCodes) > 0 && t.ProbeType != ProbeTypeProxyConnect {
		return fmt.Errorf("target %q: expected_proxy_connect_status_codes is supported only for probe_type 'proxy_connect'", t.Name)
	}
	if t.ProbeOpts.TLSEmitCertMetrics && t.ProbeType != ProbeTypeHTTP {
		return fmt.Errorf("target %q: tls_emit_cert_metrics is supported only for probe_type 'http'", t.Name)
	}
	if t.ProbeOpts.Method != "" && t.ProbeType != ProbeTypeHTTP && t.ProbeType != ProbeTypeHTTPBody {
		return fmt.Errorf("target %q: method is supported only for probe_type 'http' or 'http_body'", t.Name)
	}
	if len(t.ProbeOpts.Headers) > 0 && t.ProbeType != ProbeTypeHTTP && t.ProbeType != ProbeTypeHTTPBody {
		return fmt.Errorf("target %q: headers is supported only for probe_type 'http' or 'http_body'", t.Name)
	}
	if t.ProbeOpts.FollowRedirects && t.ProbeType != ProbeTypeHTTP && t.ProbeType != ProbeTypeHTTPBody {
		return fmt.Errorf("target %q: follow_redirects is supported only for probe_type 'http' or 'http_body'", t.Name)
	}
	if t.ProbeOpts.TLSSkipVerify && t.ProbeType != ProbeTypeHTTP && t.ProbeType != ProbeTypeHTTPBody && t.ProbeType != ProbeTypeTLSCert {
		return fmt.Errorf("target %q: tls_skip_verify is supported only for probe_type 'http', 'http_body', or 'tls_cert'", t.Name)
	}
	if len(t.ProbeOpts.ExpectedStatusCodes) > 0 && t.ProbeType != ProbeTypeHTTP && t.ProbeType != ProbeTypeHTTPBody && t.ProbeType != ProbeTypeProxyConnect {
		return fmt.Errorf("target %q: expected_status_codes is supported only for probe_type 'http' or 'http_body'", t.Name)
	}
	if t.ProbeOpts.ResponseBodyLimitBytes > 0 && t.ProbeType != ProbeTypeHTTP {
		return fmt.Errorf("target %q: response_body_limit_bytes is supported only for probe_type 'http'", t.Name)
	}
	if (t.ProbeOpts.BodyMatchRegex != "" || t.ProbeOpts.BodyMatchString != "") && t.ProbeType != ProbeTypeHTTPBody {
		return fmt.Errorf("target %q: body_match_regex and body_match_string are supported only for probe_type 'http_body'", t.Name)
	}
	if (t.ProbeOpts.PingCount != 0 || t.ProbeOpts.PingIntervalSec != 0) && t.ProbeType != ProbeTypeICMP {
		return fmt.Errorf("target %q: ping_count and ping_interval are supported only for probe_type 'icmp'", t.Name)
	}
	if (len(t.ProbeOpts.ICMPPayloadSizes) > 0 ||
		t.ProbeOpts.ExpectedMinMTU != 0 ||
		t.ProbeOpts.MTURetries != 0 ||
		t.ProbeOpts.MTUPerAttemptTimeout != 0) && t.ProbeType != ProbeTypeMTU {
		return fmt.Errorf("target %q: icmp_payload_sizes, expected_min_mtu, mtu_retries, and mtu_per_attempt_timeout are supported only for probe_type 'mtu'", t.Name)
	}
	if (t.ProbeOpts.DNSQueryName != "" ||
		t.ProbeOpts.DNSQueryType != "" ||
		t.ProbeOpts.DNSServer != "" ||
		len(t.ProbeOpts.DNSExpectedResults) > 0) && t.ProbeType != ProbeTypeDNS {
		return fmt.Errorf("target %q: dns_query_name, dns_query_type, dns_server, and dns_expected are supported only for probe_type 'dns'", t.Name)
	}

	switch t.ProbeType {
	case ProbeTypeTCP:
		if err := validateProxyNameUnsupported(*t); err != nil {
			return err
		}
		if err := validateTCPAddress(*t); err != nil {
			return err
		}
	case ProbeTypeHTTP, ProbeTypeHTTPBody:
		if err := resolveTargetProxy(t, proxies, false); err != nil {
			return err
		}
		if err := validateHTTPMethod(*t); err != nil {
			return err
		}
		if err := validateHTTPRequestBody(*t); err != nil {
			return err
		}
		if t.ProbeType == ProbeTypeHTTPBody {
			if t.ProbeOpts.BodyMatchRegex == "" && t.ProbeOpts.BodyMatchString == "" {
				return fmt.Errorf("target %q: probe_type 'http_body' requires 'body_match_regex' or 'body_match_string' in probe_opts", t.Name)
			}
			if err := validateBodyMatchRegex(*t); err != nil {
				return err
			}
		}
		if err := validateExpectedStatusCodes(*t); err != nil {
			return err
		}
	case ProbeTypeICMP:
		if err := validateProxyNameUnsupported(*t); err != nil {
			return err
		}
		if err := validateIPv4OnlyAddress(*t); err != nil {
			return err
		}
	case ProbeTypeMTU:
		if err := validateProxyNameUnsupported(*t); err != nil {
			return err
		}
		if err := validateIPv4OnlyAddress(*t); err != nil {
			return err
		}
		if err := validateICMPPayloadSizes(*t); err != nil {
			return err
		}
	case ProbeTypeDNS:
		if err := validateProxyNameUnsupported(*t); err != nil {
			return err
		}
		if err := validateDNSQueryType(*t); err != nil {
			return err
		}
		if err := validateDNSServer(t.ProbeOpts.DNSServer); err != nil {
			return fmt.Errorf("target %q: %w", t.Name, err)
		}
	case ProbeTypeTLSCert:
		if err := resolveTargetProxy(t, proxies, false); err != nil {
			return err
		}
	case ProbeTypeProxyConnect:
		if err := resolveTargetProxy(t, proxies, true); err != nil {
			return err
		}
		if err := validateProxyConnectAddress(*t); err != nil {
			return err
		}
		if len(t.ProbeOpts.ExpectedStatusCodes) > 0 {
			return fmt.Errorf("target %q: expected_status_codes is not supported for probe_type 'proxy_connect'; use expected_proxy_connect_status_codes instead", t.Name)
		}
		if err := validateExpectedProxyConnectStatusCodes(*t); err != nil {
			return err
		}
	}
	return nil
}

func validateTCPAddress(t TargetConfig) error {
	if strings.Contains(t.Address, "://") {
		return fmt.Errorf("target %q: probe_type 'tcp' address must be host:port", t.Name)
	}
	host, port, err := net.SplitHostPort(t.Address)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("target %q: probe_type 'tcp' address must be host:port", t.Name)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("target %q: probe_type 'tcp' address port must be in range 1-65535", t.Name)
	}
	return nil
}

func validateProxyConnectAddress(t TargetConfig) error {
	if strings.Contains(t.Address, "://") {
		return fmt.Errorf("target %q: probe_type 'proxy_connect' address must be host:port", t.Name)
	}
	host, port, err := net.SplitHostPort(t.Address)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("target %q: probe_type 'proxy_connect' address must be host:port", t.Name)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("target %q: probe_type 'proxy_connect' address port must be in range 1-65535", t.Name)
	}
	return nil
}

func validateProxyNameUnsupported(t TargetConfig) error {
	if t.ProxyName == "" {
		return nil
	}
	return fmt.Errorf("target %q: probe_type %q does not support 'proxy_name'", t.Name, t.ProbeType)
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

// validateIPv4OnlyAddress rejects literal IPv6 addresses and host:port forms
// for probe types that currently use IPv4-only ICMP sockets. Hostnames are
// allowed and resolved at runtime so config loading never depends on DNS
// availability.
func validateIPv4OnlyAddress(t TargetConfig) error {
	if _, _, err := net.SplitHostPort(t.Address); err == nil {
		return fmt.Errorf("target %q: probe_type %q address must not include a port", t.Name, t.ProbeType)
	}
	// Bracketed form "[...]" is URL-style IPv6 notation. SplitHostPort above
	// only catches "[...]:port"; "[...]" alone reaches here and would otherwise
	// pass because net.ParseIP rejects brackets, returning nil.
	if strings.HasPrefix(t.Address, "[") && strings.HasSuffix(t.Address, "]") {
		inner := t.Address[1 : len(t.Address)-1]
		if net.ParseIP(inner) != nil {
			return fmt.Errorf("target %q: probe_type %q supports IPv4 addresses only", t.Name, t.ProbeType)
		}
		return fmt.Errorf("target %q: probe_type %q address %q is malformed", t.Name, t.ProbeType, t.Address)
	}
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

func validateHTTPRequestBody(t TargetConfig) error {
	if t.ProbeOpts.RequestBodyBytes == 0 {
		return nil
	}
	if t.ProbeOpts.Method != "POST" {
		return fmt.Errorf("target %q: request_body_bytes requires explicit method: POST", t.Name)
	}
	return nil
}

// validateExpectedStatusCodes checks that all expected status codes are
// valid HTTP status codes (100-599). An empty or nil list is valid and means
// "accept any response that completes according to the probe's read semantics".
func validateExpectedStatusCodes(t TargetConfig) error {
	for _, code := range t.ProbeOpts.ExpectedStatusCodes {
		if code < 100 || code > 599 {
			return fmt.Errorf("target %q: invalid expected_status_codes value %d (valid: 100-599)", t.Name, code)
		}
	}
	return nil
}

func validateExpectedProxyConnectStatusCodes(t TargetConfig) error {
	for _, code := range t.ProbeOpts.ExpectedProxyConnectStatusCodes {
		if code < 100 || code > 599 {
			return fmt.Errorf("target %q: invalid expected_proxy_connect_status_codes value %d (valid: 100-599)", t.Name, code)
		}
	}
	return nil
}

func resolveTargetProxy(t *TargetConfig, proxies map[string]ProxyConfig, required bool) error {
	if t.ProxyName == "" {
		if required {
			return fmt.Errorf("target %q: probe_type 'proxy_connect' requires 'proxy_name'", t.Name)
		}
		return nil
	}
	if !proxyNameRe.MatchString(t.ProxyName) {
		return fmt.Errorf("target %q: proxy_name %q must match %s", t.Name, t.ProxyName, proxyNameRe.String())
	}
	if len(proxies) == 0 {
		return fmt.Errorf("target %q: proxy_name %q requires a top-level proxies registry", t.Name, t.ProxyName)
	}
	p, ok := proxies[t.ProxyName]
	if !ok {
		names := make([]string, 0, len(proxies))
		for name := range proxies {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Errorf("target %q: unknown proxy_name %q (known proxies: %s)", t.Name, t.ProxyName, strings.Join(names, ", "))
	}
	t.ResolvedProxy = &ResolvedProxyConfig{
		Endpoint:      p.Endpoint,
		TLSSkipVerify: p.TLSSkipVerify,
		Username:      p.Username,
		UsernameEnv:   p.UsernameEnv,
		UsernameFile:  p.UsernameFile,
		PasswordEnv:   p.PasswordEnv,
		PasswordFile:  p.PasswordFile,
		Credentials:   p.Credentials,
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

// validateDNSResolver checks that a dns_resolver value is either empty
// (meaning "use the host system resolver") or an IP literal with port.
// Hostnames are rejected so the override does not silently fall back to
// the system path: resolving the resolver itself would defeat the purpose
// of the configuration.
func validateDNSResolver(s string) error {
	if s == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("dns_resolver %q must be an IP literal with port (e.g. \"8.8.8.8:53\"), not a hostname — otherwise pre-resolution falls back to the system resolver, defeating the purpose of the override", s)
	}
	if net.ParseIP(host) == nil {
		return fmt.Errorf("dns_resolver %q must be an IP literal with port (e.g. \"8.8.8.8:53\"), not a hostname — otherwise pre-resolution falls back to the system resolver, defeating the purpose of the override", s)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return fmt.Errorf("dns_resolver %q port must be in range 1-65535", s)
	}
	return nil
}

// validateDNSServer checks the probe_opts.dns_server value used by
// `probe_type: dns`. Empty is allowed (falls back to the system resolver
// at probe time). A non-empty value must be an IP literal; a missing port
// is auto-completed to :53 by the prober, which the validator emulates
// before parsing.
//
// Hostnames are rejected — and this is a breaking change versus earlier
// behavior — to keep probe_dns_resolve_seconds honest. A hostname here
// would require pre-resolution before the probed query, contaminating the
// metric with two lookups instead of one.
func validateDNSServer(s string) error {
	if s == "" {
		return nil
	}
	candidate := s
	host, _, err := net.SplitHostPort(candidate)
	if err != nil {
		// The DNS prober auto-appends :53 when SplitHostPort fails. Mirror
		// that here so a bare IP literal validates the same way.
		candidate = net.JoinHostPort(s, "53")
		host, _, err = net.SplitHostPort(candidate)
		if err != nil {
			return fmt.Errorf("probe_opts.dns_server %q must be an IP literal (e.g. \"8.8.8.8:53\"), not a hostname. Hostnames require pre-resolution which contaminates probe_dns_resolve_seconds with two lookups instead of one. If you need to probe a named server, resolve its IP once (e.g. with `dig +short`) and configure that IP directly", s)
		}
	}
	if net.ParseIP(host) == nil {
		return fmt.Errorf("probe_opts.dns_server %q must be an IP literal (e.g. \"8.8.8.8:53\"), not a hostname. Hostnames require pre-resolution which contaminates probe_dns_resolve_seconds with two lookups instead of one. If you need to probe a named server, resolve its IP once (e.g. with `dig +short`) and configure that IP directly", s)
	}
	return nil
}
