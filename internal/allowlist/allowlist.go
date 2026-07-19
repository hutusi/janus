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
		if e != "*" {
			if !looksLikeURLOrPath(e) {
				return nil, fmt.Errorf("invalid allow_repos entry %q: include a scheme, e.g. %q", e, "https://"+e)
			}
			if normalize(e) == "" {
				return nil, fmt.Errorf("invalid allow_repos entry %q: matches nothing ('.'/'..'/percent-encoded path segments and a bare %q are rejected)", e, "/")
			}
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
//     "…example.com.evil.com". URLs with "."/".."/percent-encoded path
//     segments are always denied — they could resolve across the boundary.
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
//
// Anything containing "." or ".." path segments — literal or hidden behind
// percent-encoding — normalizes to "", which matches no entry (and New rejects
// such entries): git, HTTP infrastructure, or the filesystem may resolve those
// segments AFTER the prefix match, escaping the boundary Allows enforces. No
// legitimate clone URL contains them, so rejection is fail-closed and free.
func normalize(s string) string {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && strings.Contains(s, "://") {
		// "." / ".." and the escapes of the traversal alphabet — %2e ("."),
		// %2f ("/"), %25 ("%", the double-encoding vector) — are checked on
		// both the decoded and the raw path so one decoding layer cannot
		// hide a segment from the other.
		esc := strings.ToLower(u.EscapedPath())
		if hasDotSegments(u.Path) ||
			strings.Contains(esc, "%2e") || strings.Contains(esc, "%2f") || strings.Contains(esc, "%25") {
			return ""
		}
		host := strings.ToLower(u.Hostname())
		if p := u.Port(); p != "" && !isDefaultPort(strings.ToLower(u.Scheme), p) {
			host += ":" + p
		}
		// Userinfo is significant: git connects as that user, so
		// ssh://git@host and ssh://root@host are different authorities and must
		// not compare equal. Kept case-sensitively (unlike the host).
		auth := host
		if u.User != nil {
			auth = u.User.String() + "@" + auth
		}
		s = strings.ToLower(u.Scheme) + "://" + auth + u.EscapedPath()
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	// After the trims so "/acme/...git" (→ "/acme/..") is caught; also covers
	// scp-style remotes and local paths, which skip the URL branch above.
	if hasDotSegments(s) {
		return ""
	}
	return s
}

// hasDotSegments reports whether any "/"-separated segment of s is "." or "..".
func hasDotSegments(s string) bool {
	for _, seg := range strings.Split(s, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

func isDefaultPort(scheme, port string) bool {
	switch scheme {
	case "https":
		return port == "443"
	case "http":
		return port == "80"
	case "ssh":
		return port == "22"
	case "git":
		return port == "9418" // the git protocol's own port, not ssh's 22
	}
	return false
}
