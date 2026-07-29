# Preview Environment Auto-Expiry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a preview environment carry an optional expiration, and have bifrost reclaim it automatically once that time passes.

**Architecture:** An optional `bifrost/expires-at` namespace annotation (RFC3339) records when a preview may be reclaimed; absent means it lives forever. A goroutine inside prod bifrost wakes hourly, finds previews past their expiry, and tears them down through the existing `Orchestrator.Down` — so Neon branches are deleted too, not just the namespace.

**Tech Stack:** Go 1.26, `client-go`, existing `internal/preview` orchestrator + `internal/kube` client, `html/template` UI, Python stdlib `ib.py` CLI (separate repo).

Closes [bifrost#43](https://github.com/eswan18/bifrost/issues/43).

## Global Constraints

- **No implicit default TTL.** A preview created without an explicit TTL never expires. An unattended preview vanishing mid-demo is worse than a stale one.
- The annotation is `bifrost/expires-at`, RFC3339, in the same namespace-annotation family as `bifrost/branch|apps|phase|error|step|step-since`. Absent or unparseable means "no expiry" — never "expire immediately".
- Purge reuses `Orchestrator.Down`. Do not re-implement namespace or Neon deletion.
- Purge must never race a create: it goes through the same busy set (`acquire`/`release`) that `Up` and `Down` use.
- Every purge is logged with the tag and how long the preview had been expired. Silently deleting someone's environment is the failure mode to avoid.
- Teardown is already idempotent (Neon delete accepts 200 and 204), so a purge that runs twice is safe.
- CI runs `golangci-lint`, **not** the Makefile's `go vet` — gates must include `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...`.
- Every new test must discriminate: confirm it fails with its fix reverted.

---

### Task 1: Record an expiry at creation

**Files:**
- Modify: `internal/preview/orchestrator.go` (`Up` signature and the `EnsureNamespace` annotation map, ~line 149)
- Modify: `internal/web/previews_mutate.go` (`createPreviewRequest`, `CreatePreviewJSON`)
- Modify: `internal/web/previews.go` (`previewRecord`, `recordFromNamespace`)
- Test: `internal/preview/orchestrator_test.go`, `internal/web/previews_test.go`, `internal/web/previews_mutate_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `Orchestrator.Up(ctx context.Context, branch string, ttl time.Duration) error` — `ttl <= 0` means no expiry. `previewRecord.ExpiresAt time.Time` with tag `json:"expiresAt,omitzero"`.

**Design notes the implementer must follow:**

`Up` writes `bifrost/expires-at` in the *same* `EnsureNamespace` annotation map that already writes `bifrost/branch`/`apps`/`phase` and clears `error`/`step`/`step-since`. When `ttl <= 0`, write `""` — do **not** omit the key. That write merges onto whatever the namespace already carries, so omitting it would make a previously-set expiry survive a re-run that didn't ask for one. Re-running `ib preview up` is the documented recovery path, and the same reasoning already forced the error/step clearing directly above; follow that precedent and say so in a comment.

The absolute timestamp is computed once, at creation, from `ttl`. Do not store a duration.

- [ ] **Step 1: Write the failing tests**

In `internal/preview/orchestrator_test.go`:

```go
func TestUpWithTTLRecordsAnAbsoluteExpiry(t *testing.T) {
	d := newTwoMemberDeps(t)
	before := time.Now().UTC()
	if err := d.orch.Up(context.Background(), "hae-cadence", 8*time.Hour); err != nil {
		t.Fatalf("Up failed: %v", err)
	}
	got := d.kube.annotations(previewNamespace("hae-cadence"))["bifrost/expires-at"]
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("bifrost/expires-at = %q, not RFC3339: %v", got, err)
	}
	if delta := parsed.Sub(before.Add(8 * time.Hour)); delta < -time.Minute || delta > time.Minute {
		t.Errorf("expiry %v is not ~8h after creation", parsed)
	}
}

func TestUpWithoutTTLClearsAnyPriorExpiry(t *testing.T) {
	d := newTwoMemberDeps(t)
	if err := d.orch.Up(context.Background(), "hae-cadence", 8*time.Hour); err != nil {
		t.Fatalf("first Up failed: %v", err)
	}
	if err := d.orch.Up(context.Background(), "hae-cadence", 0); err != nil {
		t.Fatalf("second Up failed: %v", err)
	}
	if got := d.kube.annotations(previewNamespace("hae-cadence"))["bifrost/expires-at"]; got != "" {
		t.Errorf("bifrost/expires-at = %q after a no-TTL re-run, want cleared", got)
	}
}
```

In `internal/web/previews_test.go`, assert `recordFromNamespace` parses `bifrost/expires-at` into `ExpiresAt`, and that a namespace with the annotation absent or unparseable yields the zero time rather than erroring — mirror however the existing `stepSince` parse-failure case is written, since it has the identical contract.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/preview/ ./internal/web/ -run 'TTL|Expir' -v`
Expected: compile failure (`Up` takes 2 args), then assertion failures once the signature changes.

- [ ] **Step 3: Change the `Up` signature and write the annotation**

Add the `ttl time.Duration` parameter. In the `EnsureNamespace` annotation map add:

```go
"bifrost/expires-at": expiresAtAnnotation(ttl),
```

with:

```go
// expiresAtAnnotation renders ttl as an absolute RFC3339 instant, or "" for
// no expiry. Absolute rather than a duration so the reaper never has to know
// when the preview was created, and "" rather than an omitted key because
// EnsureNamespace merges: a re-run without --ttl must drop a previous
// expiry, exactly as it drops the previous run's error and step above.
func expiresAtAnnotation(ttl time.Duration) string {
	if ttl <= 0 {
		return ""
	}
	return time.Now().UTC().Add(ttl).Format(time.RFC3339)
}
```

Update every existing caller and test to pass `0`.

- [ ] **Step 4: Accept a TTL over the API**

Add `TTL string \`json:"ttl,omitempty"\`` to `createPreviewRequest`. In `CreatePreviewJSON`, after the branch checks and before `Busy`:

```go
var ttl time.Duration
if s := strings.TrimSpace(req.TTL); s != "" {
	d, err := time.ParseDuration(s)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "ttl must be a Go duration like 8h or 90m")
		return
	}
	if d <= 0 {
		writeJSONError(w, http.StatusBadRequest, "ttl must be positive")
		return
	}
	if d > maxPreviewTTL {
		writeJSONError(w, http.StatusBadRequest, "ttl must be at most "+maxPreviewTTL.String())
		return
	}
	ttl = d
}
```

with `const maxPreviewTTL = 30 * 24 * time.Hour` and a comment explaining it is a typo guard (`8760h` instead of `8h`), not a policy limit. Pass `ttl` into the `Up` call inside the existing goroutine.

- [ ] **Step 5: Expose it on the record**

Add `ExpiresAt time.Time \`json:"expiresAt,omitzero"\`` to `previewRecord` and parse it in `recordFromNamespace`, swallowing a parse error the same way `stepSince` does. Do not change any existing field's derivation — plan 5/6 goldens pin them.

- [ ] **Step 6: Run the gates**

`go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1` and golangci-lint. Then revert each new assertion's fix in turn and confirm it fails.

- [ ] **Step 7: Commit**

```bash
git add -A && git commit -m "Record an optional expiry when a preview is created"
```

---

### Task 2: Reclaim expired previews

**Files:**
- Create: `internal/preview/reaper.go`, `internal/preview/reaper_test.go`
- Modify: `cmd/bifrost/main.go` (start the loop)

**Interfaces:**
- Consumes: `Orchestrator.Down`, `Orchestrator.Busy`, `kube.Client.ListNamespaces`, `previewNSPrefix`.
- Produces: `func (o *Orchestrator) PurgeExpired(ctx context.Context, now time.Time) ([]string, error)` returning the tags it tore down; `func (o *Orchestrator) RunReaper(ctx context.Context, every time.Duration)` which loops until `ctx` is done.

**Design notes the implementer must follow:**

`PurgeExpired` takes `now` as a parameter so tests need no clock injection or sleeping.

Skip, and do not treat as errors:
- namespaces whose `bifrost/expires-at` is absent or does not parse (no expiry / malformed — never reclaim on ambiguity)
- namespaces whose expiry is in the future
- namespaces already `Terminating`
- previews in phase `creating` — let an in-flight create finish and expire on the next pass rather than tearing down underneath it
- tags where `Busy(tag)` is true

One failed `Down` must not abort the sweep: collect errors, keep going, and return them joined, the same way `Down` itself accumulates Neon errors.

`RunReaper` runs a `time.Ticker` and returns cleanly on `ctx.Done()`. It must not run a sweep immediately on start — bifrost restarts (spot-node preemptions are routine in this cluster) shouldn't each trigger an instant purge. Log each reclaimed tag with how long it had been expired, and log a sweep that reclaimed nothing at debug level only.

In `main.go`, start it next to the existing signal-handling setup, gated on `webH.Orch != nil` so a bifrost without preview config doesn't start it, using the same `ctx` from `signal.NotifyContext` so shutdown stops it.

**Note on replicas:** prod bifrost runs a single replica. Two replicas would double-fire, which is harmless because teardown is idempotent and busy-guarded, but say so in a comment rather than leaving it as an unstated assumption.

- [ ] **Step 1: Write the failing tests**

In `internal/preview/reaper_test.go`, driving the existing fake kube client:

```go
func TestPurgeExpiredReclaimsOnlyPastDuePreviews(t *testing.T) { /* expired -> torn down; future -> untouched */ }
func TestPurgeExpiredIgnoresMissingAndMalformedExpiry(t *testing.T) { /* no annotation, "", "not-a-time" -> untouched */ }
func TestPurgeExpiredSkipsCreatingAndTerminating(t *testing.T) { /* both untouched even when past due */ }
func TestPurgeExpiredSkipsBusyTags(t *testing.T) { /* acquire(tag) first, assert untouched */ }
func TestPurgeExpiredContinuesAfterAFailedTeardown(t *testing.T) { /* two expired, first Down errors; second still torn down, error returned */ }
func TestRunReaperStopsOnContextCancel(t *testing.T) { /* returns promptly */ }
```

Each must assert on what `Down` actually did (namespace deleted, Neon branch deleted), not merely that `PurgeExpired` returned a tag.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/preview/ -run 'Purge|Reaper' -v`
Expected: compile failure — `PurgeExpired` undefined.

- [ ] **Step 3: Implement `PurgeExpired` and `RunReaper`**

List with the existing preview label selector, derive each tag by trimming `previewNSPrefix`, apply the skip rules above, and call `o.Down(ctx, tag)` for the rest.

- [ ] **Step 4: Wire it into `main.go`**

```go
if webH.Orch != nil {
	go webH.Orch.RunReaper(ctx, previewReapInterval)
}
```

with `const previewReapInterval = time.Hour`.

- [ ] **Step 5: Run the gates and revert-check**

As Task 1 Step 6.

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "Reclaim expired previews on an hourly sweep"
```

---

### Task 3: Surface expiry in the UI and the runbook

**Files:**
- Modify: `templates/previews.html`
- Modify: `docs/preview-environments.md`
- Test: `internal/web/previews_test.go`

**Interfaces:**
- Consumes: `previewRecord.ExpiresAt` from Task 1.
- Produces: nothing consumed elsewhere.

**Design notes the implementer must follow:**

Reuse existing CSS classes — `static/style.css` is hand-written with no build step and must stay byte-identical. The desktop `jobs-grid` row has a fixed six-column layout shared with `jobs.html`; **do not add a seventh column.** Fold the expiry into an existing cell (the created-at cell is the natural home) or the mobile `job-card-meta` line. A preview with no expiry must render exactly as it does today.

Show remaining time, not the absolute instant — `expires in 6h` reads better than a timestamp, and the page already has a `reltime` helper for exactly this. An already-expired-but-not-yet-reaped preview (up to an hour) should read as expired rather than showing negative time.

The runbook must gain: the `bifrost/expires-at` annotation in its annotation list, the `ttl` field on `POST /api/previews`, `expiresAt` on the record, the hourly sweep and every one of its skip rules, and a Gotchas entry noting that expiry is opt-in and that a preview can outlive its expiry by up to an hour. Write only what the code does — this repo has repeatedly had docs assert things the code did not, and it was caught in review each time.

- [ ] **Step 1: Write the failing render tests**

Assert: a record with a future `ExpiresAt` renders its remaining time; a record with the zero value renders no expiry text and no stray separator; a past-due record reads as expired. Follow `TestPreviewsPageCreatingWithoutStepRendersClean`'s shape — it uses a negative assertion, which is what makes it a real guard.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/web/ -run Expir -v`

- [ ] **Step 3: Implement the template change**

- [ ] **Step 4: Update `docs/preview-environments.md`**

- [ ] **Step 5: Run the gates, revert-check, confirm `git diff --stat static/style.css` is empty**

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "Show preview expiry on the Previews tab and document it"
```

---

### Task 4: `ib preview up --ttl` (infra repo)

**Files:**
- Modify: `ib.py` (argument parsing, `preview_up`, module docstring, `preview_list` table)
- Modify: `verify_preview_progress.py` (add expiry cases)

**Interfaces:**
- Consumes: `POST /api/previews` `ttl` field and `expiresAt` on the record, both from Task 1.

**Design notes the implementer must follow:**

`ib.py` is **stdlib only** — no third-party imports, no argparse, hand-rolled `sys.argv` parsing, `urllib.request` for HTTP. The custom `User-Agent` must stay: Cloudflare's WAF blocks the default `python-urllib`, and removing it breaks the CLI against prod entirely. The module docstring is the help text and must match real behavior.

`--ttl 8h` is passed through verbatim as the request's `ttl` — the server owns parsing and validation, so the CLI must not reimplement duration parsing. Surface the server's 400 message as-is rather than inventing client-side rules that would drift.

The module docstring must say that re-running `ib preview up` **without** `--ttl` clears an expiry the preview already had (Task 1 Step 3's unconditional write). The runbook documents this twice, but the CLI is the only place a human actually meets it: help text that describes `--ttl` as purely additive leaves "I re-ran `up` to rebuild and my 24h expiry vanished" reading as a bug rather than as the documented trade. One sentence, next to the flag.

`expiresAt` is `omitzero`, so the key is **absent** for a preview with no expiry. Use `.get()` with a default everywhere; never `KeyError`. `ib preview list` shows remaining time, or a blank/`-` for no expiry — not `None`.

- [ ] **Step 1: Add the flag and thread it through**

- [ ] **Step 2: Show expiry in `preview_list`**

- [ ] **Step 3: Update the module docstring**

Cover `--ttl`'s duration format, that omitting it means the preview never expires, and that re-running `up` without it clears an existing expiry.

- [ ] **Step 4: Extend `verify_preview_progress.py`**

Cover: a record with `expiresAt` absent, one with a future expiry, and one already past due. Run it and paste the output.

- [ ] **Step 5: Run the gates**

CI runs `ruff check` + `ty check` (verify in `.github/workflows/`). Run `ruff format --check ib.py verify_preview_progress.py` too.

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "ib preview up: accept --ttl and show expiry in list"
```

---

## Self-Review

**Spec coverage.** The issue asks for two things: an optional per-preview expiration (Task 1 annotation + API, Task 3 UI, Task 4 CLI) and an hourly purge (Task 2). The issue's open question — goroutine versus CronJob — is settled as the goroutine, for the reason given there: it reuses the orchestrator in-process including the busy mutex, so a purge cannot race a create, and a preview outliving its expiry because bifrost was restarting is not a real problem.

**Deliberately not built.** The issue floats extending or clearing an expiry on a live preview. Re-running `ib preview up <branch> --ttl 24h` already rewrites it, and `up` without `--ttl` clears it (Task 1 Step 3), so a dedicated endpoint would be a second way to do the same thing. Left out; revisit if re-running proves too blunt because it also rebuilds every image.

**Type consistency.** `ttl` is a duration string on the wire and a `time.Duration` in Go; `expires-at` is an absolute RFC3339 instant in the annotation and a `time.Time` on the record. `Up` takes the duration; everything downstream reads the instant.
