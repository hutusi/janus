package model

import "strings"

// PathFilter restricts a trigger or job to pushes whose changed files match.
// Paths is an allowlist: at least one changed file must match at least one
// pattern. Ignore is a denylist: at least one changed file must match none of
// the patterns (i.e. the filter passes unless everything that changed is
// ignored). Validation rejects declaring both; slice nil-ness (key absent vs
// present-but-empty) is preserved from the YAML so that check can tell them
// apart, mirroring BranchFilter.
type PathFilter struct {
	Paths  []string
	Ignore []string
}

// Matches reports whether the changed-file set passes the filter. An empty
// set matches nothing, so a filtered trigger or job skips on a push where
// nothing changed — callers must not invoke this when the changed set is
// unknown (path filters fail open there).
func (f *PathFilter) Matches(changed []string) bool {
	if f.Ignore != nil {
		for _, file := range changed {
			if !matchAny(f.Ignore, file) {
				return true
			}
		}
		return false
	}
	for _, file := range changed {
		if matchAny(f.Paths, file) {
			return true
		}
	}
	return false
}

func matchAny(patterns []string, file string) bool {
	for _, p := range patterns {
		if MatchPath(p, file) {
			return true
		}
	}
	return false
}

// MatchPath reports whether a slash-separated relative path matches a
// GitHub-Actions-style glob pattern: `*` matches any run of characters within
// one path segment, `?` matches a single non-separator character, `**`
// matches anything including separators, and every other character is
// literal. The match is anchored at both ends. This is the whole supported
// syntax — no character classes, no `!` negation (that is what paths-ignore
// is for), no escaping.
func MatchPath(pattern, path string) bool {
	for {
		if len(pattern) >= 2 && pattern[0] == '*' && pattern[1] == '*' {
			rest := strings.TrimLeft(pattern, "*") // any run of 2+ stars ≡ **
			// `**/` also matches zero segments: **/*.go matches a root-level
			// main.go, a/**/b matches a/b.
			if rest != "" && rest[0] == '/' && MatchPath(rest[1:], path) {
				return true
			}
			for i := 0; ; i++ {
				if MatchPath(rest, path[i:]) {
					return true
				}
				if i >= len(path) {
					return false
				}
			}
		}
		if pattern == "" {
			return path == ""
		}
		switch pattern[0] {
		case '*':
			rest := pattern[1:]
			for i := 0; ; i++ {
				if MatchPath(rest, path[i:]) {
					return true
				}
				if i >= len(path) || path[i] == '/' {
					return false
				}
			}
		case '?':
			if path == "" || path[0] == '/' {
				return false
			}
		default:
			if path == "" || path[0] != pattern[0] {
				return false
			}
		}
		pattern, path = pattern[1:], path[1:]
	}
}
