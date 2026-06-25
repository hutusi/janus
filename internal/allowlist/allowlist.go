// Package allowlist decides whether a repository clone URL is permitted to run.
//
// Janus runs a triggered repo's pipeline as host processes with no isolation,
// so an authenticated caller who controls the repo URL (a leaked webhook secret
// or API token) could otherwise get host code execution from an arbitrary repo.
// The allowlist is defense-in-depth and fail-closed: an empty list denies
// everything; a single "*" entry allows everything.
package allowlist

import (
	"fmt"
	"net/url"
	"strings"
)

// Allowlist is a validated set of permitted repo-URL prefixes (or the single
// "*" wildcard). Construct it with New.
type Allowlist []string

// New validates entries and returns an Allowlist. An entry is either "*" (allow
// all) or something that looks like a URL/path — it must contain "://", start
// with "/", or contain ":" (scp-style like git@host:path). A bare host such as
// "gitlab.example.com" is rejected with a hint, since it would silently match
// nothing.
func New(entries []string) (Allowlist, error) {
	out := make(Allowlist, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if e != "*" && !looksLikeURLOrPath(e) {
			return nil, fmt.Errorf("invalid allow_repos entry %q: include a scheme, e.g. %q", e, "https://"+e)
		}
		out = append(out, e)
	}
	return out, nil
}

func looksLikeURLOrPath(e string) bool {
	return strings.Contains(e, "://") || strings.HasPrefix(e, "/") || strings.Contains(e, ":")
}

// Allows reports whether rawURL is permitted:
//   - any "*" entry        -> allow
//   - empty list / no match -> deny (fail-closed)
//   - otherwise             -> normalized prefix match with a path boundary,
//     so a host- or group-level entry permits everything beneath it while
//     "…/acme" does not match "…/acmecorp" and "…example.com" does not match
//     "…example.com.evil.com".
func (a Allowlist) Allows(rawURL string) bool {
	n := normalize(rawURL)
	for _, e := range a {
		if e == "*" {
			return true
		}
		ne := normalize(e)
		if ne == "" {
			continue
		}
		if n == ne || strings.HasPrefix(n, ne+"/") {
			return true
		}
	}
	return false
}

// normalize canonicalizes a repo URL (or allowlist entry) so candidate and
// entry are compared on equal footing. For real URLs it drops userinfo, query,
// fragment, and default ports, lowercases scheme+host, and trims a trailing "/"
// then ".git". Non-URL remotes (scp-style, file paths) are treated opaquely:
// only trailing "/" and ".git" are trimmed.
func normalize(s string) string {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && strings.Contains(s, "://") {
		host := strings.ToLower(u.Hostname())
		if p := u.Port(); p != "" && !isDefaultPort(strings.ToLower(u.Scheme), p) {
			host += ":" + p
		}
		s = strings.ToLower(u.Scheme) + "://" + host + u.EscapedPath()
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	return s
}

func isDefaultPort(scheme, port string) bool {
	switch scheme {
	case "https":
		return port == "443"
	case "http":
		return port == "80"
	case "ssh", "git":
		return port == "22"
	}
	return false
}
