package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
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
	if ev.ProjectID <= 0 || ev.SHA == "" {
		return postJob{}, false
	}
	base := d.instanceURL
	if base == nil {
		base = deriveBase(ev.RepoURL)
	}
	if base == nil {
		d.logger.Debug("commit status skipped: no resolvable GitLab API base", "run_id", run.ID, "project", ev.ProjectID)
		return postJob{}, false
	}
	endpoint := base.JoinPath("api", "v4", "projects", strconv.FormatInt(ev.ProjectID, 10), "statuses", ev.SHA).String()

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
	label := "gitlab project " + strconv.FormatInt(ev.ProjectID, 10)
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

// refName strips the refs/heads/ prefix so the status carries the branch name
// GitLab associates the pipeline with — the pushed branch, or an MR's source
// branch (parseGitLabMR sets Ref = "refs/heads/" + source_branch). Without it a
// status can attach to the wrong/null external pipeline and vanish from the MR.
// A ref over GitLab's 255-char cap (event refs are accepted up to 512) is
// omitted rather than truncated — a truncated ref would name a different branch,
// and the status still posts keyed on the SHA.
func refName(ref string) string {
	r := strings.TrimPrefix(ref, "refs/heads/")
	if utf8.RuneCountInString(r) > maxRefLen { // characters, not bytes
		return ""
	}
	return r
}
