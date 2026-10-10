package api

import (
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Browser-origin defences for a deployment with authentication disabled.
//
// With auth.mode = "none" every caller is an admin, and the only thing between
// the API and a web page the operator happens to visit is the network
// position: the listener is bound to loopback. A browser on that machine can
// still reach it, in two ways that need no credentials at all:
//
//   - Cross-site simple requests. A page can POST text/plain or
//     multipart/form-data to http://127.0.0.1:8080 without a CORS preflight,
//     and the server acts on it. Requiring a JSON content type turns every
//     such request into one that needs a preflight, which CORS then refuses.
//   - DNS rebinding. A page on attacker.example re-points its own name at
//     127.0.0.1 and then reads and writes the API as a same-origin caller. The
//     request still carries Host: attacker.example, so refusing any host that
//     is not loopback or the configured public host closes it.
//
// With authentication enabled neither applies: a bearer token is not sent
// ambiently, and a session cookie is already paired with a CSRF header.

// jsonContentType reports whether the request declares an application/json
// body. Parameters such as charset are allowed.
func jsonContentType(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// uploadClientHeader marks a multipart upload as coming from the kiln UI's own
// script. multipart/form-data is one of the three content types a plain HTML
// form can send cross-site without a preflight, so the content type alone
// proves nothing; a custom header cannot be added without one.
const uploadClientHeader = "X-Requested-With"

// localGuard is the middleware for a server with Auth == nil. It refuses
// foreign Host headers and any POST that a cross-site page could have sent
// without a preflight.
func (s *Server) localGuard(next http.Handler) http.Handler {
	publicHost := ""
	if u, err := url.Parse(strings.TrimSpace(s.PublicURL)); err == nil {
		publicHost = strings.ToLower(u.Hostname())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The webhook is called by GitHub, not a browser, from a hostname that
		// is whatever the tunnel says; it authenticates with its own HMAC.
		if r.URL.Path == "/hooks/github" {
			next.ServeHTTP(w, r)
			return
		}
		if !localHostAllowed(r.Host, publicHost) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "host not allowed while authentication is disabled; use localhost or the configured public_url",
			})
			return
		}
		// POST is the only mutating method a page can send without a
		// preflight; PUT, PATCH and DELETE always get one, and CORS refuses it.
		if r.Method == http.MethodPost {
			mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			switch {
			case mt == "application/json":
			case mt == "multipart/form-data" && r.Header.Get(uploadClientHeader) != "":
			default:
				writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{
					"error": "send Content-Type: application/json (uploads: multipart/form-data with an " +
						uploadClientHeader + " header)",
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// localHostAllowed reports whether a Host header names this machine's
// loopback or the configured public host. The port is ignored: the point is
// the name, and a rebinding page cannot choose a name that passes.
func localHostAllowed(hostport, publicHost string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if publicHost != "" && host == publicHost {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
