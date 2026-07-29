package preview

import (
	"strings"
	"testing"

	"github.com/eswan18/bifrost/internal/config"
	"github.com/eswan18/bifrost/internal/registry"
)

// --- Cascade step 1: target is a preview member (or self) -> preview URL. ---

func TestEval_MemberCascade(t *testing.T) {
	tag := "hae-cadence"

	t.Run("url self", func(t *testing.T) {
		ctx := EvalContext{
			Service: "footstrike-api",
			Tag:     tag,
			Members: []string{"footstrike-api"},
			Cfg:     &config.Config{},
		}
		got, err := Eval("{{ url self }}", ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := previewURL("footstrike-api", tag); got != want {
			t.Errorf("Eval(url self) = %q, want %q", got, want)
		}
	})

	t.Run("internalUrl self", func(t *testing.T) {
		ctx := EvalContext{
			Service: "identity",
			Tag:     tag,
			Members: []string{"identity"},
			Cfg:     &config.Config{},
		}
		got, err := Eval("{{ internalUrl self }}", ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := internalPreviewURL("identity", tag); got != want {
			t.Errorf("Eval(internalUrl self) = %q, want %q", got, want)
		}
	})

	t.Run("url named member (not self)", func(t *testing.T) {
		ctx := EvalContext{
			Service: "footstrike-api",
			Tag:     tag,
			Members: []string{"footstrike-api", "identity"},
			Cfg:     &config.Config{},
		}
		got, err := Eval("{{ url identity }}", ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := previewURL("identity", tag); got != want {
			t.Errorf("Eval(url identity) = %q, want %q", got, want)
		}
	})

	t.Run("internalUrl named member (not self)", func(t *testing.T) {
		ctx := EvalContext{
			Service: "footstrike-api",
			Tag:     tag,
			Members: []string{"footstrike-api", "identity"},
			Cfg:     &config.Config{},
		}
		got, err := Eval("{{ internalUrl identity }}", ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := internalPreviewURL("identity", tag); got != want {
			t.Errorf("Eval(internalUrl identity) = %q, want %q", got, want)
		}
	})

	t.Run("target equal to Service by name (not the self keyword) still hits step 1", func(t *testing.T) {
		ctx := EvalContext{
			Service: "identity",
			Tag:     tag,
			Members: []string{"identity"},
			Cfg:     &config.Config{},
		}
		got, err := Eval("{{ url identity }}", ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := previewURL("identity", tag); got != want {
			t.Errorf("Eval(url identity) = %q, want %q", got, want)
		}
	})
}

// --- Cascade step 2: non-member WITH a baseline value -> that value, byte-for-byte. ---
//
// This is the whole point of the design: the baseline and the registry's
// staging URL are set to deliberately DIFFERENT strings, so a regression back
// to the naive "always look up the registry" behavior fails loudly instead of
// silently passing.

func TestEval_NonMemberWithBaseline(t *testing.T) {
	tag := "hae-cadence"

	t.Run("url: baseline wins over the registry's staging URL", func(t *testing.T) {
		ctx := EvalContext{
			Service: "footstrike-api",
			Tag:     tag,
			Members: []string{"footstrike-api"}, // identity NOT a member
			Cfg:     &config.Config{},
			Fleet: registry.Registry{
				"identity": {URLs: registry.URLs{Staging: "https://staging-lookup.example.net"}}, // must NOT win
			},
			Baseline: map[string]string{
				"JWT_ISSUER": "https://baseline-value.example.net", // must win
			},
			Key: "JWT_ISSUER",
		}
		got, err := Eval("{{ url identity }}", ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "https://baseline-value.example.net" {
			t.Errorf("Eval(url identity) = %q, want baseline value %q (not the registry lookup %q)",
				got, "https://baseline-value.example.net", ctx.Fleet["identity"].URLs.Staging)
		}
	})

	t.Run("internalUrl: baseline wins over the DNS convention", func(t *testing.T) {
		ctx := EvalContext{
			Service: "footstrike-api",
			Tag:     tag,
			Members: []string{"footstrike-api"}, // identity NOT a member
			Cfg:     &config.Config{},
			Baseline: map[string]string{
				"IDENTITY_PROVIDER_URL": "http://baseline-identity.internal.example",
			},
			Key: "IDENTITY_PROVIDER_URL",
		}
		got, err := Eval("{{ internalUrl identity }}", ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "http://baseline-identity.internal.example"
		if got != want {
			t.Errorf("Eval(internalUrl identity) = %q, want baseline value %q (not the DNS convention %q)",
				got, want, "http://identity.identity-staging.svc.cluster.local")
		}
	})
}

// --- Cascade step 3: non-member, NO baseline -> registry staging URL / DNS convention. ---

func TestEval_NonMemberNoBaseline(t *testing.T) {
	tag := "hae-cadence"

	t.Run("url: falls back to the registry's staging URL", func(t *testing.T) {
		ctx := EvalContext{
			Service: "footstrike-dashboard",
			Tag:     tag,
			Members: []string{"footstrike-dashboard"},
			Cfg:     &config.Config{},
			Fleet: registry.Registry{
				"footstrike-api": {URLs: registry.URLs{Staging: "https://api.staging.footstrike.run"}},
			},
			Baseline: map[string]string{}, // no baseline at all
			Key:      "APP_API_URL",
		}
		got, err := Eval("{{ url footstrike-api }}", ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "https://api.staging.footstrike.run"; got != want {
			t.Errorf("Eval(url footstrike-api) = %q, want %q", got, want)
		}
	})

	t.Run("url: falls back to the registry's staging URL when baseline is missing the key (but has others)", func(t *testing.T) {
		ctx := EvalContext{
			Service: "footstrike-dashboard",
			Tag:     tag,
			Members: []string{"footstrike-dashboard"},
			Cfg:     &config.Config{},
			Fleet: registry.Registry{
				"footstrike-api": {URLs: registry.URLs{Staging: "https://api.staging.footstrike.run"}},
			},
			Baseline: map[string]string{"UNRELATED_KEY": "irrelevant"},
			Key:      "APP_API_URL",
		}
		got, err := Eval("{{ url footstrike-api }}", ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "https://api.staging.footstrike.run"; got != want {
			t.Errorf("Eval(url footstrike-api) = %q, want %q", got, want)
		}
	})

	t.Run("internalUrl: falls back to the X.X-staging.svc.cluster.local convention", func(t *testing.T) {
		ctx := EvalContext{
			Service:  "footstrike-api",
			Tag:      tag,
			Members:  []string{"footstrike-api"},
			Cfg:      &config.Config{},
			Baseline: nil,
			Key:      "IDENTITY_PROVIDER_URL",
		}
		got, err := Eval("{{ internalUrl identity }}", ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "http://identity.identity-staging.svc.cluster.local"; got != want {
			t.Errorf("Eval(internalUrl identity) = %q, want %q", got, want)
		}
	})
}

// --- Cascade step 4: non-member, no baseline, no staging URL -> error. ---

func TestEval_Unresolvable(t *testing.T) {
	ctx := EvalContext{
		Service:  "footstrike-dashboard",
		Tag:      "hae-cadence",
		Members:  []string{"footstrike-dashboard"},
		Cfg:      &config.Config{}, // no Fleet entry for footstrike-api
		Baseline: map[string]string{},
		Key:      "APP_API_URL",
	}
	tmpl := "{{ url footstrike-api }}"
	_, err := Eval(tmpl, ctx)
	if err == nil {
		t.Fatal("expected an error for an unresolvable service, got nil")
	}
	if !strings.Contains(err.Error(), "APP_API_URL") {
		t.Errorf("error %q does not name the key %q", err.Error(), "APP_API_URL")
	}
	if !strings.Contains(err.Error(), "footstrike-api") {
		t.Errorf("error %q does not name the unresolvable service %q", err.Error(), "footstrike-api")
	}
	if !strings.Contains(err.Error(), tmpl) {
		t.Errorf("error %q does not name the offending template %q", err.Error(), tmpl)
	}
}

// --- config function ---

func TestEval_Config(t *testing.T) {
	t.Run("previewOAuthClientID resolves from cfg", func(t *testing.T) {
		ctx := EvalContext{Cfg: &config.Config{PreviewOAuthClientID: "preview-client-id"}}
		got, err := Eval("{{ config previewOAuthClientID }}", ctx)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "preview-client-id" {
			t.Errorf("Eval(config previewOAuthClientID) = %q, want %q", got, "preview-client-id")
		}
	})

	t.Run("unknown config key errors, naming the offending template", func(t *testing.T) {
		ctx := EvalContext{Cfg: &config.Config{}}
		tmpl := "{{ config bogusKey }}"
		_, err := Eval(tmpl, ctx)
		if err == nil {
			t.Fatal("expected an error for an unknown config key, got nil")
		}
		if !strings.Contains(err.Error(), tmpl) {
			t.Errorf("error %q does not name the offending template %q", err.Error(), tmpl)
		}
	})
}

// --- Literal passthrough, unknown function, empty template, malformed braces ---

func TestEval_LiteralPassthrough(t *testing.T) {
	ctx := EvalContext{Cfg: &config.Config{}}
	for _, tmpl := range []string{"ENV: staging", "staging", "http://example.com/no-templating-here"} {
		got, err := Eval(tmpl, ctx)
		if err != nil {
			t.Fatalf("Eval(%q) unexpected error: %v", tmpl, err)
		}
		if got != tmpl {
			t.Errorf("Eval(%q) = %q, want unchanged passthrough", tmpl, got)
		}
	}
}

func TestEval_EmptyTemplate(t *testing.T) {
	ctx := EvalContext{Cfg: &config.Config{}}
	got, err := Eval("", ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("Eval(\"\") = %q, want empty string", got)
	}
}

func TestEval_UnknownFunction(t *testing.T) {
	ctx := EvalContext{Cfg: &config.Config{}}
	tmpl := "{{ bogus self }}"
	_, err := Eval(tmpl, ctx)
	if err == nil {
		t.Fatal("expected an error for an unknown function, got nil")
	}
	if !strings.Contains(err.Error(), tmpl) {
		t.Errorf("error %q does not name the offending template %q", err.Error(), tmpl)
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error %q does not name the unknown function %q", err.Error(), "bogus")
	}
}

func TestEval_MalformedBraces(t *testing.T) {
	ctx := EvalContext{Cfg: &config.Config{}}
	cases := []string{
		"{{ url foo",    // missing closing braces
		"url foo }}",    // missing opening braces
		"{{ }}",         // empty inside
		"{{ url }}",     // missing argument
		"{{ url a b }}", // too many arguments
	}
	for _, tmpl := range cases {
		_, err := Eval(tmpl, ctx)
		if err == nil {
			t.Errorf("Eval(%q): expected an error, got nil", tmpl)
			continue
		}
		if !strings.Contains(err.Error(), tmpl) {
			t.Errorf("Eval(%q): error %q does not name the offending template", tmpl, err.Error())
		}
	}
}

// --- Whitespace inside braces is flexible. ---

func TestEval_WhitespaceFlexible(t *testing.T) {
	ctx := EvalContext{
		Service: "footstrike-api",
		Tag:     "t",
		Members: []string{"footstrike-api"},
		Cfg:     &config.Config{},
	}
	tight, err := Eval("{{url self}}", ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	loose, err := Eval("{{  url   self  }}", ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tight != loose {
		t.Errorf("whitespace variants disagree: %q vs %q", tight, loose)
	}
	if want := previewURL("footstrike-api", "t"); tight != want {
		t.Errorf("Eval({{url self}}) = %q, want %q", tight, want)
	}
}
