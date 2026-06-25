package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/hutusi/janus/internal/model"
	"gopkg.in/yaml.v3"
)

// Parse decodes and fully validates a pipeline YAML document. It rejects any
// unsupported key (if, matrix, uses, with, runs-on, ...), any unsupported
// interpolation, and any structural problem (missing trigger, empty run, unknown
// or cyclic needs). On success it returns an immutable, validated Workflow.
func Parse(data []byte) (*model.Workflow, error) {
	var raw rawWorkflow
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, friendlyDecodeError(err)
	}
	wf := raw.toModel()
	if err := validate(wf); err != nil {
		return nil, err
	}
	return wf, nil
}

// forbiddenKeyHelp maps known-unsupported keys to an explanation, so the decode
// error points at the spec decision instead of an opaque "field not found".
var forbiddenKeyHelp = map[string]string{
	"if":          "`if:`/conditionals are not supported — Janus has no expression language",
	"matrix":      "matrix builds are not supported",
	"strategy":    "`strategy:` (matrix) is not supported",
	"uses":        "`uses:` (third-party actions) is not supported — Janus runs host commands only",
	"with":        "`with:` is not supported — a step only has `run`",
	"runs-on":     "`runs-on:` is not supported — jobs run on the Janus host",
	"container":   "containers are not supported — jobs run as host processes",
	"services":    "service containers are not supported",
	"secrets":     "`secrets:` is not supported — use host environment variables",
	"cache":       "caching is not supported",
	"permissions": "`permissions:` is not supported",
	"concurrency": "`concurrency:` is not supported",
	"outputs":     "`outputs:` are not supported",
	"shell":       "`shell:` is not supported — steps run via /bin/sh",
	"id":          "step `id:` is not supported",
	"name":        "step `name:` is not supported — use `run`",
}

var fieldNotFoundRe = regexp.MustCompile(`field (\S+) not found in type \S+`)

// friendlyDecodeError turns yaml decode failures into actionable messages,
// upgrading "field X not found" into spec-aware guidance where possible.
func friendlyDecodeError(err error) error {
	if errors.Is(err, io.EOF) {
		return errors.New("empty pipeline: expected a YAML document")
	}
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return fmt.Errorf("invalid pipeline: %w", err)
	}
	msgs := make([]string, 0, len(te.Errors))
	for _, e := range te.Errors {
		m := fieldNotFoundRe.FindStringSubmatch(e)
		if m == nil {
			msgs = append(msgs, e)
			continue
		}
		prefix := e[:strings.Index(e, "field")] // keep "line N: "
		field := strings.Trim(m[1], "`")
		if help, ok := forbiddenKeyHelp[field]; ok {
			msgs = append(msgs, prefix+help)
		} else {
			msgs = append(msgs, fmt.Sprintf("%sunsupported key %q", prefix, field))
		}
	}
	return fmt.Errorf("invalid pipeline:\n  %s", strings.Join(msgs, "\n  "))
}
