package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/hutusi/janus/internal/model"
)

// GitCode handles GitCode (gitcode.com) push and merge-request webhooks. GitCode
// is GitLab-compatible on the wire — its payloads use GitLab's exact push /
// merge_request format — so Parse reuses the shared gitlabFormat parser, differing
// only in the header names. Verify accepts either a plaintext token in
// X-GitCode-Token (like GitLab's X-Gitlab-Token) or an HMAC-SHA256 body signature
// in X-GitCode-Signature-256 (sha256=<hex>, like GitHub's X-Hub-Signature-256).
type GitCode struct {
	SSH bool
}

func (GitCode) Name() string { return "gitcode" }

func (GitCode) Verify(r *http.Request, body []byte, secret string) error {
	if secret == "" {
		return fmt.Errorf("gitcode: no webhook secret configured")
	}
	// Token mode: the plaintext secret echoed in the header.
	if got := r.Header.Get("X-GitCode-Token"); subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1 {
		return nil
	}
	// Signature mode: HMAC-SHA256 over the raw body, as "sha256=<hex>".
	if sig := r.Header.Get("X-GitCode-Signature-256"); strings.HasPrefix(sig, "sha256=") {
		if want, err := hex.DecodeString(strings.TrimPrefix(sig, "sha256=")); err == nil {
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(body)
			if hmac.Equal(want, mac.Sum(nil)) {
				return nil
			}
		}
	}
	return ErrInvalidSignature
}

func (g GitCode) Parse(r *http.Request, body []byte) (*model.Event, error) {
	f := gitlabFormat{provider: "gitcode", ssh: g.SSH}
	switch r.Header.Get("X-GitCode-Event") {
	case "Push Hook":
		return f.parsePush(body)
	case "Merge Request Hook":
		return f.parseMR(body)
	default:
		return nil, ErrIgnoredEvent
	}
}
