// Package previewapi is the wire shape of bifrost's preview API: the JSON
// bifrost's handlers write and `bif preview` reads, and nothing else.
//
// It exists because there were two definitions of these field names — one in
// internal/web's handlers, one hand-copied into infra/ib.py — and nothing made
// them agree. A renamed field on the server would have gone on serving 200s
// while the CLI silently rendered blanks, which is exactly what a `preview
// list` full of "-" would look like. Now there is one definition: internal/web
// marshals these types and cmd/bif unmarshals them, so a rename that breaks the
// CLI cannot compile the server.
//
// Deliberately narrow: types, their JSON tags, and derivations that are pure
// functions of the wire shape. No HTTP, no cluster access, no orchestration.
// That is what lets cmd/bif import it without importing bifrost the server —
// see cmd/bif/main_test.go's TestNoBifrostServerDependency, which still bans
// internal/web from the CLI outright.
package previewapi

import (
	"encoding/json"
	"time"
)

// Record is one preview environment as GET /api/previews/{tag} returns it, and
// as GET /api/previews returns each element of its list.
//
// Consumers must read an absent key as the field's zero value: several fields
// are omitempty/omitzero and are genuinely absent — not null, not false, not ""
// — for the ordinary preview.
type Record struct {
	Tag       string            `json:"tag"`
	Branch    string            `json:"branch"`
	Apps      []string          `json:"apps"`
	Phase     string            `json:"phase"`
	Health    string            `json:"health"`
	CreatedAt time.Time         `json:"createdAt"`
	URLs      map[string]string `json:"urls"`
	// Step and StepSince narrate what the orchestrator's Up is doing right
	// now (bifrost/step, bifrost/step-since — see internal/preview's
	// Orchestrator.step). Step is "" once a preview reaches ready (cleared
	// on success) but is deliberately left in place on a failed preview, so
	// it reads as "failed while building footstrike-api" rather than going
	// silent. StepSince is a timestamp, not a duration, so elapsed time is
	// always computed fresh by whoever renders it (the CLI, the UI) instead
	// of going stale between polls.
	Step      string    `json:"step,omitempty"`
	StepSince time.Time `json:"stepSince,omitzero"`
	// Error surfaces bifrost/error (already written by Orchestrator.fail)
	// so a failed preview's cause is visible to API consumers instead of
	// only the cluster-side annotation.
	Error string `json:"error,omitempty"`
	// ExpiresAt is the optional reclaim time recorded at creation
	// (bifrost/expires-at — see internal/preview's expiresAtAnnotation).
	// Zero, and so omitted from the JSON, means the preview never expires:
	// most previews carry no TTL, and that is the default by design.
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	// AutoUpdate reports whether the preview follows its branch
	// (bifrost/auto-update = "true" — see internal/preview's
	// autoUpdateAnnotation and PollAutoUpdates). Opt-in and false by
	// default, so it is omitted from the JSON for almost every preview.
	AutoUpdate bool `json:"autoUpdate,omitempty"`
	// Busy reports that the orchestrator currently holds this tag for an
	// in-flight Up or Down (internal/preview's Orchestrator.Busy). Alone
	// among this struct's fields it is NOT derived from the namespace: it is
	// bifrost's own in-process state, and that is exactly why it is worth
	// reporting. A busy tag is the reason a concurrent up/down gets a 409,
	// and until now nothing in the API said so, which left "That preview is
	// busy" and "No preview environments" as the two simultaneous answers to
	// the same question. False for a preview nobody is touching, so
	// omitempty keeps it out of the JSON for almost every record; a consumer
	// must read a missing key as false.
	Busy bool `json:"busy,omitempty"`
	// BuiltImages reports what each member's last SUCCESSFUL build produced
	// (bifrost/built-images — see internal/preview's builtImagesAnnotation),
	// keyed by service name. A member absent from the map has nothing
	// recorded for it — an older preview predating this annotation, or one
	// whose most recent build never succeeded — and that absence, like a
	// wholly missing annotation, is this field's clean zero value (nil map,
	// omitted from the JSON) rather than an error; see internal/web's
	// parseBuiltImagesAnnotation. It exists so a caller (the CLI, polling
	// across a re-run of `up`) can diff this map before and after and tell
	// whether anything was actually rebuilt: an unchanged preview reports the
	// identical commit for every member, which Step cannot show once the run
	// reaches ready (Step is cleared to "" on success, and a re-run that
	// reuses every image can finish inside a single poll interval).
	BuiltImages map[string]BuiltImage `json:"builtImages,omitempty"`
}

// UnmarshalJSON decodes a Record, tolerating a stepSince that is not a usable
// RFC3339 instant: an unparseable or timezone-naive value leaves StepSince
// zero — "the server didn't tell us when this step started" — instead of
// failing the whole record.
//
// This is not defensive decoration. StepSince is a time.Time, so without this
// ONE bad character in that field would fail the entire GET, and the caller
// that reads it most is `bif preview up`'s poll loop, which would lose the
// whole record — phase, error, URLs — over a cosmetic elapsed-time display.
// ib.py hit the Python-shaped version of exactly this: a timezone-naive
// stepSince raised an uncaught TypeError out of its elapsed-time arithmetic
// and killed the poll loop mid-run, and its fix (treat it as unusable, fall
// back to a locally tracked start) is what this reproduces one layer earlier,
// where Go's typed decode puts the hazard.
//
// The policy is not new either: internal/web already swallows an unparseable
// bifrost/step-since annotation and leaves this field zero (see
// recordFromNamespace), so bifrost itself never emits a malformed value. This
// applies the same reading to the wire, so a non-bifrost server, a proxy, or a
// future format change degrades one field instead of the response.
//
// Every other field decodes exactly as the struct tags say. Marshalling is
// untouched — the server has no MarshalJSON here and its output is unchanged.
func (r *Record) UnmarshalJSON(data []byte) error {
	// wire has none of Record's methods, so decoding into it cannot recurse
	// back into this one. The shadowing StepSince sits at depth 0 and so wins
	// over the embedded one at depth 1, which is what keeps encoding/json from
	// ever parsing the timestamp itself.
	type wire Record
	var v struct {
		wire
		StepSince json.RawMessage `json:"stepSince"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*r = Record(v.wire)
	r.StepSince = time.Time{}

	var s string
	if err := json.Unmarshal(v.StepSince, &s); err != nil {
		// Absent, null, or not a string at all. Absence is the ordinary case —
		// the field is omitzero and a ready preview has no current step.
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		r.StepSince = t
	}
	return nil
}

// CreateRequest is POST /api/previews's JSON body: the shape `bif preview up`
// sends and internal/web's CreatePreviewJSON decodes.
//
// It lives here for the same reason Record does — "ttl" and "autoUpdate" are
// key names two programs have to agree on, and a server-side rename that the
// CLI did not follow would go on returning 202 while silently creating a
// preview without the expiry or the branch-following that was asked for. That
// is the exact failure warnIfTTLDropped exists to catch after the fact; sharing
// the type stops it at compile time instead.
//
// TTL is a Go duration string ("8h", "90m"). Absent or empty means the preview
// never expires, which is the default by design, so omitempty and an explicit
// "" are the same request as far as the server is concerned (it trims and
// treats empty as no expiry — see internal/web's TestCreatePreviewEmptyTTLIsNoExpiry).
// AutoUpdate is opt-in: omitempty keeps the key out entirely when it is false,
// which is what a re-POST that means "stop following the branch" looks like.
type CreateRequest struct {
	Branch     string `json:"branch"`
	TTL        string `json:"ttl,omitempty"`
	AutoUpdate bool   `json:"autoUpdate,omitempty"`
}

// BuiltImage is the JSON shape of one Record.BuiltImages entry: the full
// commit a member's image was built from, and the short SHA that build
// produced (internal/preview's `preview-<short sha>` image tag). It mirrors
// internal/preview's builtImage field-for-field — same two fields, same reason
// neither is derived from the other (see that type's doc) — but is its own
// type rather than a reuse of it: builtImage is unexported, and nothing forces
// the wire shape to move in lockstep with that package's internal
// representation.
type BuiltImage struct {
	Commit   string `json:"commit"`
	ShortSHA string `json:"shortSha"`
}

// ListResponse is the envelope GET /api/previews answers with. It is a named
// type rather than a map literal on the server and a hand-written struct in
// the client for the same reason Record is: "previews" is a field name two
// programs have to agree on, and this is where they agree on it.
type ListResponse struct {
	Previews []Record `json:"previews"`
}

// ErrorResponse is the body every non-2xx from the preview API carries. The
// CLI surfaces Error verbatim rather than inventing its own wording, so the
// server stays the only place a failure is described.
type ErrorResponse struct {
	Error string `json:"error"`
}

// PhaseBusy is Record.Phase for a synthesized record — a tag the orchestrator
// holds that has no namespace to describe it (internal/web's busyRecord).
//
// It is deliberately its own phase rather than a reuse of an existing one.
// "creating" and "terminating" would both be guesses: the busy set is a bare
// claim on a tag and does not record which of Up or Down took it, and the two
// windows that produce this state are one of each (Up before EnsureNamespace,
// Down after DeleteNamespace). "terminating" would be an outright lie besides
// — internal/web's recordFromNamespace reserves it for a namespace whose
// Kubernetes phase really is Terminating, and here there is no namespace at
// all. "unknown", which recordFromNamespace uses for a namespace missing its
// phase annotation, would throw away the one thing that IS known. So: busy,
// which claims exactly what bifrost can see.
const PhaseBusy = "busy"

// Teardownable reports whether DELETE /api/previews/{tag} has anything to act
// on for this record — i.e. whether a namespace is behind it. It exists for
// the Previews tab, which offers a teardown control per row and must not
// offer one that cannot work.
//
// A synthesized busy record (PhaseBusy — see internal/web's busyRecord) is the
// only record this excludes: it describes a tag the orchestrator holds with no
// namespace to describe it, which is precisely the case DeletePreviewJSON
// answers 404 for, since previewByTag deliberately does not synthesize. Every
// other phase, terminating included, has a namespace and so a real DELETE to
// make (a redundant one against an already-terminating preview is answered
// honestly by the handler rather than guessed at here).
//
// It is a method, not a field, so it stays out of the JSON: the API's shape is
// fixed and consumers derive this themselves from phase.
func (r Record) Teardownable() bool { return r.Phase != PhaseBusy }
