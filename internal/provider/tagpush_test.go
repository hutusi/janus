package provider

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/hutusi/janus/internal/model"
)

// Every provider must normalize a tag push the same way, because the runner
// matches one rule against all four: Tag set, Branch empty (a tag is on no
// branch), and Before empty (a tag push has no base to diff, so path filters
// stay inert rather than diffing against something meaningless).
func TestParseTagPush(t *testing.T) {
	// The fixtures deliberately model an *annotated* tag: `after` is the tag
	// object, and only checkout_sha / head_commit.id name the commit. Picking
	// the wrong one fails the run — the workspace verifies HEAD against the
	// SHA it was asked for, and checkout peels a tag object to its commit.
	const commit = "da1560886d4f094c3e6c9ef40349f7d38b5d27d7"
	const tagObject = "82b3d5ae55f7080f1e6022629cdb57bfae7cccc7"

	giteeTag := []byte(`{
	  "ref": "refs/tags/v1.0.0",
	  "before": "0000000000000000000000000000000000000000",
	  "after": "` + tagObject + `",
	  "deleted": false,
	  "head_commit": {"id": "` + commit + `", "message": "Fix the bug"},
	  "repository": {"full_name": "acme/app", "clone_url": "https://gitee.com/acme/app.git"}
	}`)
	gitcodeTag := []byte(`{
	  "object_kind": "tag_push",
	  "ref": "refs/tags/v1.0.0",
	  "before": "0000000000000000000000000000000000000000",
	  "after": "` + tagObject + `",
	  "checkout_sha": "` + commit + `",
	  "project": {"id": 1848674, "git_http_url": "https://gitcode.com/acme/app.git"}
	}`)

	tests := []struct {
		name     string
		provider Provider
		header   [2]string
		body     []byte
		wantRepo string
	}{
		{"gitlab", GitLab{}, [2]string{"X-Gitlab-Event", "Tag Push Hook"}, payload(t, "gitlab_tag_push.json"), "https://gitlab.example.com/acme/app.git"},
		{"github", GitHub{}, [2]string{"X-GitHub-Event", "push"}, payload(t, "github_tag_push.json"), "https://github.com/acme/app.git"},
		{"gitee", Gitee{}, [2]string{"X-Gitee-Event", "Tag Push Hook"}, giteeTag, "https://gitee.com/acme/app.git"},
		{"gitcode", GitCode{}, [2]string{"X-GitCode-Event", "Tag Push Hook"}, gitcodeTag, "https://gitcode.com/acme/app.git"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/webhooks/"+tc.name, nil)
			r.Header.Set(tc.header[0], tc.header[1])

			ev, err := tc.provider.Parse(r, tc.body)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if ev.Kind != model.EventPush {
				t.Errorf("kind = %s, want push — a tag push is a push with a tag", ev.Kind)
			}
			if ev.Tag != "v1.0.0" {
				t.Errorf("tag = %q, want v1.0.0", ev.Tag)
			}
			if ev.Branch != "" {
				t.Errorf("branch = %q, want empty: a tag is not on a branch", ev.Branch)
			}
			if ev.Ref != "refs/tags/v1.0.0" {
				t.Errorf("ref = %q, want refs/tags/v1.0.0", ev.Ref)
			}
			if ev.SHA != commit {
				t.Errorf("sha = %q, want the commit %q (not the tag object)", ev.SHA, commit)
			}
			if ev.Before != "" {
				t.Errorf("before = %q, want empty so path filters stay inert", ev.Before)
			}
			if ev.RepoURL != tc.wantRepo {
				t.Errorf("repo = %q, want %q", ev.RepoURL, tc.wantRepo)
			}
			if ev.Target() != "v1.0.0" {
				t.Errorf("Target() = %q, want the tag", ev.Target())
			}
		})
	}
}

// A tag deletion is not a build: `after` is all zeros (and GitHub/Gitee also
// set deleted), exactly like a branch deletion.
func TestParseTagDeletionIgnored(t *testing.T) {
	const zero = "0000000000000000000000000000000000000000"
	glDel := []byte(`{"object_kind":"tag_push","ref":"refs/tags/v1.0.0","before":"` + zero + `","after":"` + zero + `","checkout_sha":null,"project":{"git_http_url":"https://gitlab.example.com/acme/app.git"}}`)
	geDel := []byte(`{"ref":"refs/tags/v1.0.0","deleted":true,"after":"` + zero + `","repository":{"clone_url":"https://gitee.com/acme/app.git"}}`)

	tests := []struct {
		name     string
		provider Provider
		header   [2]string
		body     []byte
	}{
		{"gitlab", GitLab{}, [2]string{"X-Gitlab-Event", "Tag Push Hook"}, glDel},
		{"gitcode", GitCode{}, [2]string{"X-GitCode-Event", "Tag Push Hook"}, glDel},
		{"gitee", Gitee{}, [2]string{"X-Gitee-Event", "Tag Push Hook"}, geDel},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/webhooks/"+tc.name, nil)
			r.Header.Set(tc.header[0], tc.header[1])
			if _, err := tc.provider.Parse(r, tc.body); !errors.Is(err, ErrIgnoredEvent) {
				t.Errorf("tag deletion: got %v, want ErrIgnoredEvent", err)
			}
		})
	}
}

func TestRefTarget(t *testing.T) {
	tests := []struct {
		ref, branch, tag string
	}{
		{"refs/heads/main", "main", ""},
		{"refs/heads/feature/login", "feature/login", ""},
		{"refs/tags/v1.0.0", "", "v1.0.0"},
		{"refs/tags/release/v1", "", "release/v1"},
		// Hosting-side namespaces belong to neither and must be ignored.
		{"refs/merge-requests/7/head", "", ""},
		{"refs/pull/7/head", "", ""},
		{"refs/keep-around/abc", "", ""},
		{"", "", ""},
		// Not a prefix match on a shorter string.
		{"refs/heads", "", ""},
		{"refs/tags", "", ""},
	}
	for _, tc := range tests {
		branch, tag := refTarget(tc.ref)
		if branch != tc.branch || tag != tc.tag {
			t.Errorf("refTarget(%q) = (%q, %q), want (%q, %q)", tc.ref, branch, tag, tc.branch, tc.tag)
		}
	}
}
