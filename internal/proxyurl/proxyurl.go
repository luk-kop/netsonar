// Package proxyurl validates HTTP proxy URLs used by config and probe code.
package proxyurl

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Parse validates raw as an HTTP proxy URL and returns the parsed URL.
// Returned errors use url.URL.Redacted so userinfo credentials are not leaked.
func Parse(raw string) (*url.URL, error) {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		// Do not wrap err: *url.Error includes the full input URL, including
		// userinfo credentials if present.
		return nil, fmt.Errorf("proxy url is not a valid absolute URL")
	}

	redacted := u.Redacted()
	switch {
	case u.Opaque != "":
		return nil, fmt.Errorf("invalid proxy url %q: must use scheme://host form", redacted)
	case strings.ToLower(u.Scheme) != "http" && strings.ToLower(u.Scheme) != "https":
		return nil, fmt.Errorf("invalid proxy url %q: scheme must be http or https", redacted)
	case u.Hostname() == "":
		return nil, fmt.Errorf("invalid proxy url %q: host is required", redacted)
	case u.User != nil:
		return nil, fmt.Errorf("invalid proxy url %q: userinfo is not supported; use username and password source fields instead", redacted)
	case u.Path != "":
		return nil, fmt.Errorf("invalid proxy url %q: path is not allowed", redacted)
	case u.RawQuery != "":
		return nil, fmt.Errorf("invalid proxy url %q: query is not allowed", redacted)
	case u.Fragment != "":
		return nil, fmt.Errorf("invalid proxy url %q: fragment is not allowed", redacted)
	}

	if port := u.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("invalid proxy url %q: port must be in range 1-65535", redacted)
		}
	}

	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.User = nil
	return u, nil
}

// Endpoint returns the canonical metric/hash identity for a parsed proxy URL.
func Endpoint(u *url.URL) string {
	out := *u
	out.Scheme = strings.ToLower(out.Scheme)
	out.Host = strings.ToLower(out.Host)
	out.User = nil
	out.Path = ""
	out.RawPath = ""
	out.RawQuery = ""
	out.Fragment = ""
	return out.String()
}
