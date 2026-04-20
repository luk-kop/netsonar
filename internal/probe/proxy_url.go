package probe

import (
	"fmt"
	"net/url"

	"netsonar/internal/proxyurl"
)

func mustProxyURL(caller, raw string) *url.URL {
	u, err := proxyurl.Parse(raw)
	if err != nil {
		panic(fmt.Sprintf("%s: %s", caller, err.Error()))
	}

	return u
}
