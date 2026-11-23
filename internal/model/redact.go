package model

import (
	"regexp"
	"strings"
)

// urlUserinfoRe matches the userinfo@ segment of a scheme://-style URL,
// capturing the scheme (group 1, without "://") and the userinfo (group 2).
//
// The userinfo runs to the LAST "@" of the authority, as URL parsers and git
// read it, not the first. A valid URL percent-encodes "@" inside a password,
// but a hand-written https://user:p@ss@host is common, and stopping at the
// first "@" would leave the password's tail ("ss@") behind. The authority
// ends at "/", "?", "#" or whitespace, so the greedy match can't spill into
// the path, the query, or the next word of a longer message.
var urlUserinfoRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*)://([^/?#\s]*)@`)

// RedactURL strips credentials from every scheme://-style URL occurrence in s
// — a clone URL may carry embedded credentials (https://user:token@host/...),
// which must not reach the journal. It works on bare URLs and on URLs inside
// longer text: git error output echoes the URL the command was invoked with,
// so wrapped errors need the same scrubbing as the URL field itself.
//
// Userinfo is dropped entirely, except a bare username on an SSH URL
// (ssh://git@host/...): SSH authenticates with keys, so that username is the
// account git connects as, never a secret — and dropping it made a correct
// clone URL look wrong in the logs. Any other scheme loses even a bare
// username, because over HTTP(S) the username slot can hold a token
// (https://oauth2@host, https://<token>@host). scp-style git@host:path
// strings pass through unchanged for the same reason SSH usernames do, as
// does anything without a scheme.
func RedactURL(s string) string {
	return urlUserinfoRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := urlUserinfoRe.FindStringSubmatch(m)
		scheme, userinfo := sub[1], sub[2]
		// A bare username only: a ":" means a password. A further "@" means
		// the userinfo isn't one plain name, so it may hold a password's tail.
		if isSSHScheme(scheme) && !strings.ContainsAny(userinfo, ":@") {
			return m
		}
		return scheme + "://"
	})
}

// isSSHScheme reports whether scheme is one git dispatches to ssh. URL schemes
// are case-insensitive.
func isSSHScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "ssh", "git+ssh", "ssh+git":
		return true
	}
	return false
}
