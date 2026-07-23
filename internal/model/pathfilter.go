package model

// ChangedFiles is the set of files a push touched, when it could be
// determined. The zero value (Known false) means the set is unknown — a new
// branch with no base commit, a non-push event, a failed diff — and every
// path filter then fails open: it must never wrongly skip CI.
type ChangedFiles struct {
	Known bool
	Files []string
}

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

// A pattern token: a literal rune, or one of the wildcard kinds.
type patToken struct {
	kind byte // tokLit, tokQ, tokStar, tokGlobstar
	r    rune // the literal rune when kind == tokLit
}

const (
	tokLit byte = iota
	tokQ
	tokStar
	tokGlobstar
)

// MatchPath reports whether a slash-separated relative path matches a
// GitHub-Actions-style glob pattern: `*` matches any run of characters within
// one path segment, `?` matches a single non-separator character, `**`
// matches anything including separators (`**/` also matching zero segments,
// so `**/*.go` covers a root-level main.go), and every other character is
// literal. The match is anchored at both ends. This is the whole supported
// syntax — no character classes, no `!` negation (that is what paths-ignore
// is for), no escaping.
//
// Implemented as a dynamic program over pattern tokens × path runes: O(m·n)
// worst case, never backtracking. That bound matters — patterns come from
// repo-controlled pipeline files and this runs in the daemon's admission path
// with no cancellation, so an adversarial `*a*a*a…` pattern must not be able
// to pin a CPU the way a backtracking matcher would.
func MatchPath(pattern, path string) bool {
	var toks []patToken
	stars := 0
	flush := func() {
		if stars == 1 {
			toks = append(toks, patToken{kind: tokStar})
		} else if stars >= 2 { // any run of 2+ stars ≡ **
			toks = append(toks, patToken{kind: tokGlobstar})
		}
		stars = 0
	}
	for _, r := range pattern {
		if r == '*' {
			stars++
			continue
		}
		flush()
		if r == '?' {
			toks = append(toks, patToken{kind: tokQ})
		} else {
			toks = append(toks, patToken{kind: tokLit, r: r})
		}
	}
	flush()
	s := []rune(path)

	// prev = dp[i-1], cur = dp[i]; dp[i][j] means toks[:i] matches s[:j].
	// prevPrev (dp[i-2]) backs the zero-segment rule: when toks[i-1] is a
	// literal '/' right after `**`, the pair `**/` may also match nothing at
	// all (dp[i][j] |= dp[i-2][j]).
	prev := make([]bool, len(s)+1)
	cur := make([]bool, len(s)+1)
	prevPrev := make([]bool, len(s)+1)
	prev[0] = true // empty pattern matches empty path
	for i := 1; i <= len(toks); i++ {
		t := toks[i-1]
		zeroSeg := t.kind == tokLit && t.r == '/' && i >= 2 && toks[i-2].kind == tokGlobstar
		cur[0] = (t.kind == tokStar || t.kind == tokGlobstar) && prev[0]
		if zeroSeg && prevPrev[0] {
			cur[0] = true
		}
		for j := 1; j <= len(s); j++ {
			c := s[j-1]
			switch t.kind {
			case tokLit:
				cur[j] = prev[j-1] && c == t.r
			case tokQ:
				cur[j] = prev[j-1] && c != '/'
			case tokStar:
				cur[j] = prev[j] || (cur[j-1] && c != '/')
			case tokGlobstar:
				cur[j] = prev[j] || cur[j-1]
			}
			if zeroSeg && prevPrev[j] {
				cur[j] = true
			}
		}
		prevPrev, prev, cur = prev, cur, prevPrev
	}
	return prev[len(s)]
}
