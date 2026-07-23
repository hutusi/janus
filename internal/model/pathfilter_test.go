package model

import "testing"

func TestMatchPath(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		// Literals are anchored whole-path matches.
		{"Makefile", "Makefile", true},
		{"Makefile", "sub/Makefile", false},
		{"Makefile", "Makefile.old", false},
		// `*` stays within one segment.
		{"*.go", "main.go", true},
		{"*.go", "cmd/main.go", false},
		{"cmd/*/main.go", "cmd/janus/main.go", true},
		{"cmd/*/main.go", "cmd/a/b/main.go", false},
		{"docs/*", "docs/a.md", true},
		{"docs/*", "docs/img/a.png", false},
		{"a*b", "ab", true},
		{"a*b", "axxxb", true},
		{"a*b", "a/b", false},
		// `**` crosses segments, including zero of them.
		{"docs/**", "docs/a.md", true},
		{"docs/**", "docs/img/deep/a.png", true},
		{"docs/**", "docs", false},
		{"docs/**", "other/docs/a.md", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "internal/server/ui.go", true},
		{"**/*.go", "ui.go.txt", false},
		{"**.md", "README.md", true},
		{"**.md", "docs/deep/a.md", true},
		{"src/**/img/*.png", "src/a/b/img/x.png", true},
		{"src/**/img/*.png", "src/img/x.png", true},
		{"src/**/img/*.png", "src/a/img/deep/x.png", false},
		// Star runs collapse to `**`.
		{"***", "a/b/c", true},
		{"docs/****", "docs/a/b", true},
		// `?` is a single non-separator character.
		{"a?c", "abc", true},
		{"a?c", "a/c", false},
		{"a?c", "ac", false},
		// Empty edge cases.
		{"", "", true},
		{"", "a", false},
		{"**", "", true},
		{"*", "", true},
	}
	for _, c := range cases {
		if got := MatchPath(c.pattern, c.path); got != c.want {
			t.Errorf("MatchPath(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestPathFilterMatches(t *testing.T) {
	changed := []string{"docs/guide.md", "internal/server/ui.go"}

	allow := &PathFilter{Paths: []string{"docs/**"}}
	if !allow.Matches(changed) {
		t.Error("paths filter should pass when any changed file matches")
	}
	if allow.Matches([]string{"cmd/main.go"}) {
		t.Error("paths filter should fail when no changed file matches")
	}
	if allow.Matches(nil) {
		t.Error("an empty changed set matches nothing")
	}

	ignore := &PathFilter{Ignore: []string{"docs/**", "*.md"}}
	if !ignore.Matches(changed) {
		t.Error("paths-ignore should pass while a non-ignored file changed")
	}
	if ignore.Matches([]string{"docs/a.md", "README.md"}) {
		t.Error("paths-ignore should fail when every changed file is ignored")
	}
	if ignore.Matches(nil) {
		t.Error("an empty changed set has no surviving file")
	}

	// Present-but-empty ignore list (paths-ignore: []) ignores nothing, so any
	// change passes.
	emptyIgnore := &PathFilter{Ignore: []string{}}
	if !emptyIgnore.Matches(changed) {
		t.Error("empty paths-ignore should pass on any change")
	}
}
