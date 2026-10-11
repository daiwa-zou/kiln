package netguard

import "net/http"

// TransportOf exposes the *http.Transport inside a NewClient client to the
// external tests, which need to inspect it (Proxy) and re-route one name.
func TransportOf(c *http.Client) *http.Transport {
	return c.Transport.(*httpsOnly).next
}
