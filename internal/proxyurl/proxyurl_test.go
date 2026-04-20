package proxyurl

import (
	"strings"
	"testing"
)

func TestParse_Valid(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantHost string
		wantPort string
	}{
		{"http-no-port", "http://proxy.example.com", "proxy.example.com", ""},
		{"http-with-port", "http://proxy.example.com:8080", "proxy.example.com", "8080"},
		{"https-with-port", "https://proxy.example.com:8443", "proxy.example.com", "8443"},
		{"ipv4-with-port", "http://10.0.0.1:3128", "10.0.0.1", "3128"},
		{"ipv6-with-port", "http://[2001:db8::1]:8080", "2001:db8::1", "8080"},
		{"root-path", "http://proxy.example.com/", "proxy.example.com", ""},
		{"with-userinfo", "http://user:pass@proxy.example.com:8080", "proxy.example.com", "8080"},
		{"user-only", "http://user@proxy.example.com:8080", "proxy.example.com", "8080"},
		{"max-port", "http://proxy.example.com:65535", "proxy.example.com", "65535"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse(%q): unexpected error: %v", tt.input, err)
			}
			if got := u.Hostname(); got != tt.wantHost {
				t.Errorf("Hostname() = %q, want %q", got, tt.wantHost)
			}
			if got := u.Port(); got != tt.wantPort {
				t.Errorf("Port() = %q, want %q", got, tt.wantPort)
			}
		})
	}
}

func TestParse_Invalid(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantErrSubs string
	}{
		{"empty", "", "not a valid absolute URL"},
		{"bare-host-port", "proxy.example.com:8080", "must use scheme://host form"},
		{"wrong-scheme-ftp", "ftp://proxy.example.com", "scheme must be http or https"},
		{"wrong-scheme-socks5", "socks5://proxy.example.com:1080", "scheme must be http or https"},
		{"opaque", "http:proxy.example.com", "must use scheme://host form"},
		{"no-host", "http://", "host is required"},
		{"path", "http://proxy.example.com/path", "path is not allowed"},
		{"query", "http://proxy.example.com?x=1", "query is not allowed"},
		{"fragment-rejected-by-parse-request-uri", "http://proxy.example.com#frag", "not a valid absolute URL"},
		{"port-zero", "http://proxy.example.com:0", "port must be in range 1-65535"},
		{"port-too-large", "http://proxy.example.com:70000", "port must be in range 1-65535"},
		{"port-non-numeric", "http://proxy.example.com:abc", "not a valid absolute URL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.input)
			if err == nil {
				t.Fatalf("Parse(%q): expected error, got nil", tt.input)
			}
			if !strings.Contains(err.Error(), tt.wantErrSubs) {
				t.Errorf("Parse(%q) error = %q, want substring %q", tt.input, err.Error(), tt.wantErrSubs)
			}
		})
	}
}

func TestParse_RedactsCredentials(t *testing.T) {
	const secret = "supers3cret"
	input := "http://admin:" + secret + "@proxy.example.com/some-path"

	_, err := Parse(input)
	if err == nil {
		t.Fatal("expected error for proxy URL with path, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks password: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "xxxxx") {
		t.Errorf("error should contain redacted userinfo marker (xxxxx), got: %q", err.Error())
	}
}

func TestParse_InvalidURIDoesNotLeakInput(t *testing.T) {
	const secret = "supers3cret"
	input := "://admin:" + secret + "@not a url"

	_, err := Parse(input)
	if err == nil {
		t.Fatal("expected error for malformed URL, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error from ParseRequestURI path leaks password: %q", err.Error())
	}
}
