// Package provider turns an incoming git-server webhook into a normalized
// model.Event. Each provider verifies the request's authenticity and parses its
// payload. GitLab is implemented first; the interface is the seam where GitHub
// or Gitea would slot in without touching the rest of Janus.
package provider

import (
	"errors"
	"net/http"

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

var (
	// ErrIgnoredEvent means the webhook is well-formed but not actionable
	// (e.g. a tag push, a branch deletion, or an MR being closed). Handlers
	// should respond 2xx without starting a run.
	ErrIgnoredEvent = errors.New("event ignored")

	// ErrInvalidSignature means verification failed; respond 401.
	ErrInvalidSignature = errors.New("invalid webhook signature")
)
