package preview

import (
	"fmt"
	"slices"
	"strings"

	"github.com/eswan18/bifrost/internal/config"
)

// EvalContext is what a registry env template (registry.yaml's Service.Env
// values) resolves against.
type EvalContext struct {
	// Service is the service whose env is being rendered; a template's
	// "self" argument means this.
	Service string
	// Tag is the preview's slug, used to build preview URLs/DNS names.
	Tag string
	// Members is the full set of services in this preview (Service is
	// usually, but need not be, included in it explicitly -- see resolveURL).
	Members []string
	// Cfg is bifrost's static config: StagingURLs and PreviewOAuthClientID.
	Cfg *config.Config
	// Baseline is the target app's own staging ConfigMap data. Cascade step
	// 2 defers to it: if Key already has a value here, that value wins, so
	// bifrost never restates a fact the app already owns.
	Baseline map[string]string
	// Key is the env key currently being rendered. It is only used to look
	// itself up in Baseline (step 2) and to name itself in unresolvable-
	// service errors (step 4); Eval never inspects any other Baseline entry.
	Key string
}

// Eval renders one template string against ctx.
//
// A template containing neither "{{" nor "}}" is a literal and passes
// through unchanged (e.g. "ENV: staging"). Otherwise the whole (trimmed)
// string must be exactly one "{{ func arg }}" form; anything else --
// unbalanced braces, a missing or extra argument, an unrecognized func --
// is an error naming the offending template, so a typo in registry.yaml
// fails loudly at render time instead of producing a silently empty env
// var.
//
// Supported forms (exactly these):
//   - "{{ url X }}" / "{{ url self }}": X's (or, for self, ctx.Service's)
//     externally-reachable preview URL, subject to the resolution cascade
//     -- see resolveURL.
//   - "{{ internalUrl X }}": the same cascade, but the in-cluster form.
//   - "{{ config KEY }}": an operator-supplied value from ctx.Cfg. Only
//     "previewOAuthClientID" is recognized; any other KEY is an error.
//
// Whitespace inside the braces is flexible: "{{url X}}" and "{{ url  X }}"
// both parse the same way.
func Eval(tmpl string, ctx EvalContext) (string, error) {
	hasOpen := strings.Contains(tmpl, "{{")
	hasClose := strings.Contains(tmpl, "}}")
	if !hasOpen && !hasClose {
		return tmpl, nil
	}

	trimmed := strings.TrimSpace(tmpl)
	if !strings.HasPrefix(trimmed, "{{") || !strings.HasSuffix(trimmed, "}}") {
		return "", fmt.Errorf("preview: template %q: malformed braces", tmpl)
	}
	inner := strings.TrimSpace(trimmed[2 : len(trimmed)-2])
	fields := strings.Fields(inner)
	if len(fields) != 2 {
		return "", fmt.Errorf("preview: template %q: malformed braces", tmpl)
	}
	fn, arg := fields[0], fields[1]

	switch fn {
	case "url":
		return resolveURL(tmpl, arg, ctx, false)
	case "internalUrl":
		return resolveURL(tmpl, arg, ctx, true)
	case "config":
		return resolveConfig(tmpl, arg, ctx)
	default:
		return "", fmt.Errorf("preview: template %q: unknown function %q", tmpl, fn)
	}
}

// resolveURL implements the resolution cascade for "{{ url X }}" and
// "{{ internalUrl X }}" (internal selects the latter). It stops at the
// first hit:
//
//  1. arg (or, if arg is "self", ctx.Service) is the service being rendered
//     or one of this preview's members -> its own preview URL
//     (previewURL/internalPreviewURL -- the single source of truth for the
//     URL shape, shared with render.go).
//  2. Else, if ctx.Key already has a value in the target app's own staging
//     ConfigMap (ctx.Baseline) -> that value, untouched. This is the point
//     of the whole cascade: resolving to cfg.StagingURLs instead would give
//     bifrost a second, independently-maintained copy of a fact the app
//     already states, and the two would silently drift. Deferring to the
//     baseline means bifrost has no opinion where the app already has one.
//  3. Else -> cfg.StagingURLs[svc] for url, or the
//     "http://svc.svc-staging.svc.cluster.local" convention for
//     internalUrl. The DNS convention is a pure string formula, so it
//     always succeeds; only url's StagingURLs lookup can miss.
//  4. Else (only reachable for url) -> an error naming ctx.Key and the
//     unresolvable service.
func resolveURL(tmpl, arg string, ctx EvalContext, internal bool) (string, error) {
	svc := arg
	if svc == "self" {
		svc = ctx.Service
	}

	if svc == ctx.Service || slices.Contains(ctx.Members, svc) {
		if internal {
			return internalPreviewURL(svc, ctx.Tag), nil
		}
		return previewURL(svc, ctx.Tag), nil
	}

	if v, ok := ctx.Baseline[ctx.Key]; ok && v != "" {
		return v, nil
	}

	if internal {
		return fmt.Sprintf("http://%s.%s-staging.svc.cluster.local", svc, svc), nil
	}
	var stagingURLs map[string]string
	if ctx.Cfg != nil {
		stagingURLs = ctx.Cfg.StagingURLs
	}
	if url := stagingURLs[svc]; url != "" {
		return url, nil
	}
	return "", fmt.Errorf(
		"preview: template %q: key %s: %s is not in this preview and has no staging URL configured",
		tmpl, ctx.Key, svc,
	)
}

// resolveConfig implements "{{ config KEY }}": an operator-supplied value
// read straight from ctx.Cfg. Only "previewOAuthClientID" is recognized
// today; any other KEY is an error naming the offending template. An empty
// PreviewOAuthClientID is not itself an error here -- that is a
// required-key concern, checked by the registry's Required list once this
// is threaded into envConfigFor (Task 3), not by Eval.
func resolveConfig(tmpl, key string, ctx EvalContext) (string, error) {
	switch key {
	case "previewOAuthClientID":
		if ctx.Cfg == nil {
			return "", nil
		}
		return ctx.Cfg.PreviewOAuthClientID, nil
	default:
		return "", fmt.Errorf("preview: template %q: unknown config key %q", tmpl, key)
	}
}
