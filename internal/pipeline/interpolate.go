package pipeline

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hutusi/janus/internal/model"
)

var (
	// placeholderRe matches any ${{ ... }} occurrence with a non-greedy inner
	// capture, so it finds expressions too (which then fail tokenRe below).
	placeholderRe = regexp.MustCompile(`\$\{\{(.*?)\}\}`)

	// tokenRe is the closed whitelist of allowed placeholder contents. Anything
	// not matching — operators, functions, secrets.*, github.*, steps.*,
	// matrix.* — is rejected. The regex's inability to express operators is the
	// point: it structurally guarantees there is no expression language.
	tokenRe = regexp.MustCompile(`^(?:ref|sha|short_sha|branch|tag|event|env\.[A-Za-z_][A-Za-z0-9_]*)$`)
)

// validateInterpolation ensures every ${{ ... }} placeholder anywhere in the
// workflow references a supported token. It is part of Parse so `janus validate`
// rejects expressions without needing runtime values.
func validateInterpolation(wf *model.Workflow) error {
	check := func(where, s string) error {
		for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
			tok := strings.TrimSpace(m[1])
			if !tokenRe.MatchString(tok) {
				return fmt.Errorf("%s: unsupported interpolation ${{ %s }}; "+
					"allowed tokens are env.NAME, ref, sha, short_sha, branch, tag, event", where, tok)
			}
		}
		// A "${{" with no closing "}}" matches nothing above and would reach
		// the shell verbatim — almost certainly a typo, so reject it here.
		if rest := placeholderRe.ReplaceAllString(s, ""); strings.Contains(rest, "${{") {
			return fmt.Errorf("%s: unterminated ${{ — missing closing }}", where)
		}
		return nil
	}

	if wf.Concurrency != nil {
		if err := check("concurrency.group", wf.Concurrency.Group); err != nil {
			return err
		}
		// sha/short_sha are valid tokens elsewhere, but in a group key every
		// run would form its own group — silently disabling the serialization
		// the key exists to provide. Reject rather than obey.
		for _, m := range placeholderRe.FindAllStringSubmatch(wf.Concurrency.Group, -1) {
			tok := strings.TrimSpace(m[1])
			if tok == "sha" || tok == "short_sha" {
				return fmt.Errorf("concurrency.group: ${{ %s }} would make every run its own group, "+
					"disabling concurrency control; use branch, ref, event, or env.NAME", tok)
			}
		}
	}
	for _, k := range sortedKeys(wf.Env) {
		if err := check("env."+k, wf.Env[k]); err != nil {
			return err
		}
	}
	for _, name := range sortedJobNames(wf) {
		job := wf.Jobs[name]
		if err := check(fmt.Sprintf("job %q working-directory", name), job.WorkingDir); err != nil {
			return err
		}
		for _, k := range sortedKeys(job.Env) {
			if err := check(fmt.Sprintf("job %q env.%s", name, k), job.Env[k]); err != nil {
				return err
			}
		}
		for i, s := range job.Steps {
			if err := check(fmt.Sprintf("job %q step %d run", name, i+1), s.Run); err != nil {
				return err
			}
			if err := check(fmt.Sprintf("job %q step %d working-directory", name, i+1), s.WorkingDir); err != nil {
				return err
			}
			for _, k := range sortedKeys(s.Env) {
				if err := check(fmt.Sprintf("job %q step %d env.%s", name, i+1, k), s.Env[k]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Context carries the values available to ${{ ... }} placeholders at run time.
type Context struct {
	Env      map[string]string
	Ref      string
	SHA      string
	ShortSHA string
	Branch   string
	Tag      string
	Event    string
}

// Interpolate substitutes ${{ ... }} placeholders in s using the context,
// bounding the result to max bytes. Interpolation can expand a small template
// into an arbitrarily large string (a big env value referenced many times), so
// it errors past max rather than materializing gigabytes and OOMing the shared
// daemon. Unknown env vars resolve to the empty string (shell-like). It assumes
// s has already passed validateInterpolation; any non-whitelisted token is left
// verbatim rather than guessed at.
func (c Context) Interpolate(s string, max int) (string, error) {
	var b strings.Builder
	last := 0
	for _, m := range placeholderRe.FindAllStringSubmatchIndex(s, -1) {
		repl := c.resolve(strings.TrimSpace(s[m[2]:m[3]]), s[m[0]:m[1]])
		if b.Len()+(m[0]-last)+len(repl) > max {
			return "", fmt.Errorf("interpolated value exceeds %d bytes", max)
		}
		b.WriteString(s[last:m[0]])
		b.WriteString(repl)
		last = m[1]
	}
	if b.Len()+(len(s)-last) > max {
		return "", fmt.Errorf("interpolated value exceeds %d bytes", max)
	}
	b.WriteString(s[last:])
	return b.String(), nil
}

// resolve returns the replacement for a placeholder token, or the original
// match for a non-whitelisted token (unreachable for validated input).
func (c Context) resolve(tok, match string) string {
	switch tok {
	case "ref":
		return c.Ref
	case "sha":
		return c.SHA
	case "short_sha":
		return c.ShortSHA
	case "branch":
		return c.Branch
	case "tag":
		return c.Tag
	case "event":
		return c.Event
	default:
		if name, ok := strings.CutPrefix(tok, "env."); ok {
			return c.Env[name]
		}
		return match
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
