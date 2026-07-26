// Package provider turns an incoming git-server webhook into a normalized
// model.Event. Each provider verifies the request's authenticity and parses its
// payload. GitLab is implemented first; the interface is the seam where GitHub
// or Gitea would slot in without touching the rest of Janus.
package provider

import (
	"errors"
	"net/http"
	"strings"

	"github.com/hutusi/janus/internal/model"
)

// Provider verifies and parses one git server's webhooks.
type Provider interface {
	// Name is the provider's URL segment, e.g. "gitlab" for /webhooks/gitlab.
	Name() string
	// Verify authenticates the request against the configured secret.
	Verify(r *http.Request, body []byte, secret string) error
	// Parse converts the payload into a normalized Event. It returns
	// ErrIgnoredEvent for event types Janus does not act on.
	Parse(r *http.Request, body []byte) (*model.Event, error)
}

// refTarget splits a pushed ref into the branch or tag name it names — exactly
// one is non-empty, and both are empty for anything else, which every caller
// treats as an ignored event. Hosting-side namespaces are the reason for that
// last case: GitLab pushes refs/merge-requests/* and refs/keep-around/*, and
// running a pipeline against one is never what the repository meant.
//
// The ref decides, not the webhook's event name. GitHub delivers tag pushes on
// its ordinary push event while GitLab has a separate Tag Push Hook, so the
// header only says "a push happened" — the ref says what moved.
func refTarget(ref string) (branch, tag string) {
	if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return b, ""
	}
	if t, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
		return "", t
	}
	return "", ""
}

var (
	// ErrIgnoredEvent means the webhook is well-formed but not actionable
	// (e.g. a branch or tag deletion, a push to a hosting-side ref namespace,
	// or an MR being closed). Handlers should respond 2xx without starting a
	// run.
	ErrIgnoredEvent = errors.New("event ignored")

	// ErrInvalidSignature means verification failed; respond 401.
	ErrInvalidSignature = errors.New("invalid webhook signature")
)
