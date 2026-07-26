package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBranchFilterMatches(t *testing.T) {
	tests := []struct {
		name   string
		filter BranchFilter
		branch string
		want   bool
	}{
		{"empty filter matches all", BranchFilter{}, "anything", true},
		{"allowlist hit", BranchFilter{Branches: []string{"main"}}, "main", true},
		{"allowlist miss", BranchFilter{Branches: []string{"main"}}, "dev", false},
		{"denylist hit", BranchFilter{Ignore: []string{"master"}}, "master", false},
		{"denylist miss matches all", BranchFilter{Ignore: []string{"master"}}, "feature/x", true},
		// Both lists is rejected by pipeline validation, but hand-built
		// workflows can carry it — deny must win.
		{"deny wins over allow", BranchFilter{Branches: []string{"main"}, Ignore: []string{"main"}}, "main", false},
		{"present-but-empty denylist matches all", BranchFilter{Ignore: []string{}}, "anything", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.Matches(tc.branch); got != tc.want {
				t.Errorf("Matches(%q) = %v, want %v", tc.branch, got, tc.want)
			}
		})
	}
}

func TestTagFilterMatches(t *testing.T) {
	tests := []struct {
		name   string
		filter TagFilter
		tag    string
		want   bool
	}{
		{"empty filter matches all", TagFilter{}, "v1.0.0", true},
		{"exact allowlist hit", TagFilter{Tags: []string{"v1.0.0"}}, "v1.0.0", true},
		{"exact allowlist miss", TagFilter{Tags: []string{"v1.0.0"}}, "v1.0.1", false},
		// The reason tags glob and branches do not: one pattern must cover
		// every future release.
		{"glob hit", TagFilter{Tags: []string{"v*"}}, "v1.0.0", true},
		{"glob miss", TagFilter{Tags: []string{"v*"}}, "nightly", false},
		// `*` stops at a separator, like a path filter's; `**` crosses it.
		{"star does not cross a slash", TagFilter{Tags: []string{"v*"}}, "release/v1", false},
		{"globstar crosses a slash", TagFilter{Tags: []string{"**/v*"}}, "release/v1", true},
		{"denylist hit", TagFilter{Ignore: []string{"*-rc*"}}, "v1.0.0-rc1", false},
		{"denylist miss matches all", TagFilter{Ignore: []string{"*-rc*"}}, "v1.0.0", true},
		// Both lists is rejected by pipeline validation, but hand-built
		// workflows can carry it — deny must win, as it does for branches.
		{"deny wins over allow", TagFilter{Tags: []string{"v*"}, Ignore: []string{"v*"}}, "v1.0.0", false},
		{"present-but-empty denylist matches all", TagFilter{Ignore: []string{}}, "v1.0.0", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.Matches(tc.tag); got != tc.want {
				t.Errorf("Matches(%q) = %v, want %v", tc.tag, got, tc.want)
			}
		})
	}
}

func TestEventTarget(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		want  string
	}{
		{"branch wins", Event{Branch: "main", Tag: "v1", Ref: "refs/heads/main"}, "main"},
		{"tag when no branch", Event{Tag: "v1.0.0", Ref: "refs/tags/v1.0.0"}, "v1.0.0"},
		{"ref as the last resort", Event{Ref: "refs/heads/main"}, "refs/heads/main"},
		{"empty event", Event{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.event.Target(); got != tc.want {
				t.Errorf("Target() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEventMarshalJSONRedactsRepoURL(t *testing.T) {
	e := Event{
		Provider: "gitlab",
		Kind:     EventPush,
		RepoURL:  "https://ci:sekret@gitlab.example.com/acme/app.git",
		Branch:   "main",
		Ref:      "refs/heads/main",
	}
	out, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "sekret") || strings.Contains(s, "ci:") {
		t.Errorf("marshaled event leaks credentials: %s", s)
	}
	if !strings.Contains(s, "https://gitlab.example.com/acme/app.git") {
		t.Errorf("host/path should survive redaction: %s", s)
	}
	// The in-memory value is untouched — the checkout still needs the credentials.
	if e.RepoURL != "https://ci:sekret@gitlab.example.com/acme/app.git" {
		t.Errorf("MarshalJSON mutated the in-memory RepoURL: %q", e.RepoURL)
	}
}

func TestRunMarshalRedactsEmbeddedEvent(t *testing.T) {
	// The value receiver must fire for Event embedded in Run and RunSummary, so
	// both the detail and list API responses are covered.
	run := &Run{
		ID:     "r1",
		Status: StatusFailed,
		Event:  Event{Kind: EventManual, RepoURL: "https://u:p@host/repo.git"},
	}
	for name, v := range map[string]any{"run": run, "summary": run.Summary()} {
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(out), "u:p@") {
			t.Errorf("%s JSON leaks event credentials: %s", name, out)
		}
	}
}

// Reason is redacted before it is stored, but records written by earlier
// versions still hold raw git errors — which a credentialed clone URL can end
// up inside. Serializing must not hand those back out.
func TestRunMarshalRedactsReason(t *testing.T) {
	run := &Run{
		ID:     "r1",
		Status: StatusFailed,
		Reason: "checkout: fatal: could not read from https://u:p@host/repo.git",
	}
	out, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "u:p@") {
		t.Errorf("run JSON leaks credentials via reason: %s", out)
	}
	if !strings.Contains(string(out), "https://host/repo.git") {
		t.Errorf("redaction should keep the rest of the reason intact: %s", out)
	}
}
