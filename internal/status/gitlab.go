package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/hutusi/janus/internal/model"
)

const (
	// GitLab caps description, target_url, and ref at 255 characters (Unicode code
	// points, not bytes); an over-limit field is rejected and the whole status is
	// lost, so we clip or omit rather than overrun.
	maxDescLen      = 255
	maxTargetURLLen = 255
	maxRefLen       = 255
)

// gitlabDialect posts to GitLab's Commit Status API
// (POST /api/v4/projects/{id}/statuses/{sha}) with a PRIVATE-TOKEN header.
type gitlabDialect struct {
	token       string
	instanceURL *url.URL // instance base override; nil = derive per event
	logger      *slog.Logger
}

// newGitLabDialect validates the token and optional instance URL.
func newGitLabDialect(token, instanceRaw string, logger *slog.Logger) (gitlabDialect, error) {
	if strings.TrimSpace(token) == "" {
		return gitlabDialect{}, errors.New("gitlab api token is empty")
	}
	d := gitlabDialect{token: token, logger: logger}
	if instanceRaw != "" {
		u, err := validateBaseURL(instanceRaw)
		if err != nil {
			return gitlabDialect{}, fmt.Errorf("gitlab_url: %w", err)
		}
		d.instanceURL = u
	}
	return d, nil
}

func (d gitlabDialect) build(run *model.Run, state model.Status, targetURL string) (postJob, bool) {
	glState := gitlabState(state)
	if glState == "" {
		return postJob{}, false
	}
	ev := run.Event
	// The project is identified by the path derived from the clone URL, not by the
	// payload's project id: the allowlist gates RepoURL, so addressing the status
	// from that same string is what keeps a delivery from pairing an allowlisted
	// clone URL with someone else's project and having the token write there.
	// GitLab accepts a URL-encoded NAMESPACE/PROJECT path wherever :id appears.
	// fullOID guards the SHA for the same reason.
	base := d.instanceURL
	if base == nil {
		base = deriveBase(ev.RepoURL)
	}
	if base == nil {
		d.logger.Debug("commit status skipped: no resolvable GitLab API base", "run_id", run.ID)
		return postJob{}, false
	}
	project, ok := d.projectPath(ev.RepoURL, base)
	if !ok || !fullOID(ev.SHA) {
		return postJob{}, false
	}
	// Built by hand rather than with JoinPath: the project path must reach GitLab
	// as ONE segment with its separators percent-encoded, and JoinPath would treat
	// them as real separators (it also resolves "../") — the very behaviour this
	// addressing is defending against. base is validated (http/https, host, no
	// userinfo/query/fragment) and ev.SHA is a full hex OID, so this is unambiguous.
	endpoint := strings.TrimSuffix(base.String(), "/") +
		"/api/v4/projects/" + url.PathEscape(project) +
		"/statuses/" + ev.SHA

	// GitLab rejects an over-length target_url; drop it rather than lose the status.
	turl := targetURL
	if utf8.RuneCountInString(turl) > maxTargetURLLen {
		turl = ""
	}
	body, err := json.Marshal(statusBody{
		State:       glState,
		Context:     statusContext,
		Ref:         refName(ev.Ref),
		TargetURL:   turl,
		Description: clip(description(run, state), maxDescLen),
	})
	if err != nil {
		d.logger.Warn("commit status payload could not be encoded", "run_id", run.ID, "err", err)
		return postJob{}, false
	}
	label := "gitlab " + project
	return postJob{
		key:      label + "|" + ev.SHA + "|" + statusContext,
		endpoint: endpoint,
		body:     body,
		header:   map[string]string{"PRIVATE-TOKEN": d.token},
		runID:    run.ID,
		label:    label,
		overHTTP: base.Scheme == "http",
		retry409: true,
	}, true
}

// projectPath derives the NAMESPACE/PROJECT path GitLab addresses a project by
// from the clone URL, requiring at least two segments — a project always lives
// under a namespace, and unlike GitHub a nested group makes more than two legal.
func (d gitlabDialect) projectPath(repoURL string, base *url.URL) (string, bool) {
	segs := repoPathSegments(repoURL)
	// An instance hosted under a path (gitlab_url = https://host/gitlab) puts that
	// prefix on its web and API URLs, so it is not part of the project path — but
	// only for http(s) clone URLs on that same host. GitLab builds an ssh/scp clone
	// URL from the project's namespace path alone, so a leading segment there that
	// merely collides with the subpath is a real group. deriveBase is nil for
	// scp-style and ssh:// URLs, which is exactly that distinction. Erring this way
	// is deliberate: failing to strip only 404s a best-effort post, while stripping
	// wrongly would post to a different project than the one the allowlist gated.
	if rb := deriveBase(repoURL); rb != nil && strings.EqualFold(rb.Host, base.Host) {
		if prefix := repoPathSegments(base.Path); len(prefix) > 0 && len(segs) > len(prefix) &&
			slices.Equal(segs[:len(prefix)], prefix) {
			segs = segs[len(prefix):]
		}
	}
	if len(segs) < 2 {
		return "", false
	}
	return strings.Join(segs, "/"), true
}

// gitlabState maps a Janus status to a GitLab commit-status state, or "" for
// states GitLab has no equivalent for / that we deliberately don't post
// (pending, skipped). A skipped run means the workflow didn't apply to the
// commit; posting success would green-check code CI never validated.
func gitlabState(s model.Status) string {
	switch s {
	case model.StatusRunning:
		return "running"
	case model.StatusSuccess:
		return "success"
	case model.StatusFailed:
		return "failed"
	case model.StatusCancelled:
		return "canceled" // GitLab's spelling
	}
	return ""
}

// refName strips the refs/heads/ or refs/tags/ prefix so the status carries the
// short name GitLab associates the pipeline with — the pushed branch, an MR's
// source branch (parseGitLabMR sets Ref = "refs/heads/" + source_branch), or a
// pushed tag (GitLab's `ref` parameter takes a branch *or* tag name). Without it
// a status can attach to the wrong/null external pipeline and vanish from the
// MR. TrimPrefix alone is not enough: it returns its input unchanged on a miss,
// so a tag would post as the full refs/tags/v1.0.0 and name nothing.
// A ref over GitLab's 255-char cap (event refs are accepted up to 512) is
// omitted rather than truncated — a truncated ref would name a different branch,
// and the status still posts keyed on the SHA.
func refName(ref string) string {
	r := strings.TrimPrefix(ref, "refs/heads/")
	if tag := model.TagFromRef(ref); tag != "" {
		r = tag
	}
	if utf8.RuneCountInString(r) > maxRefLen { // characters, not bytes
		return ""
	}
	return r
}
