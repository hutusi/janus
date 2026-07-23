package model

import "regexp"

// urlUserinfoRe matches the userinfo@ segment of a scheme://-style URL. The
// userinfo of a valid URL cannot contain an unencoded "/", "@", or
// whitespace, so the match can never spill past the authority into the path
// or across separate words of a longer message.
var urlUserinfoRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^@/\s]+@`)

// RedactURL strips userinfo from every scheme://-style URL occurrence in s —
// a clone URL may carry embedded credentials (https://user:token@host/...),
// which must not reach the journal. It works on bare URLs and on URLs inside
// longer text: git error output echoes the URL the command was invoked with,
// so wrapped errors need the same scrubbing as the URL field itself.
// scp-style git@host:path strings pass through unchanged (their "user" is not
// a secret), as does anything without a scheme.
func RedactURL(s string) string {
	return urlUserinfoRe.ReplaceAllString(s, "$1")
}
