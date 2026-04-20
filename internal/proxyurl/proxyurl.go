// Package proxyurl validates HTTP proxy URLs used by config and probe code.
package proxyurl

import (
	"fmt"
	"net/url"
	"strconv"
)

// Parse validates raw as an HTTP proxy URL and returns the parsed URL.
// Returned errors use url.URL.Redacted so userinfo credentials are not leaked.
func Parse(raw string) (*url.URL, error) {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		// Do not wrap err: *url.Error includes the full input URL, including
		// userinfo credentials if present.
		return nil, fmt.Errorf("proxy_url is not a valid absolute URL")
	}

	redacted := u.Redacted()
	switch {
	case u.Opaque != "":
		return nil, fmt.Errorf("invalid proxy_url %q: must use scheme://host form", redacted)
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("invalid proxy_url %q: scheme must be http or https", redacted)
	case u.Hostname() == "":
		return nil, fmt.Errorf("invalid proxy_url %q: host is required", redacted)
	case u.Path != "" && u.Path != "/":
		return nil, fmt.Errorf("invalid proxy_url %q: path is not allowed", redacted)
	case u.RawQuery != "":
		return nil, fmt.Errorf("invalid proxy_url %q: query is not allowed", redacted)
	case u.Fragment != "":
		return nil, fmt.Errorf("invalid proxy_url %q: fragment is not allowed", redacted)
	}

	if port := u.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("invalid proxy_url %q: port must be in range 1-65535", redacted)
		}
	}

	return u, nil
}
