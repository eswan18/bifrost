# Preview Progress Reporting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `ib preview up` should narrate itself. Today it prints one line, goes silent for two-plus minutes while two Cloud Builds run serially, then prints URLs. The operator can't tell progress from a hang.

**Architecture:** The orchestrator already knows exactly what it's doing at each moment; that knowledge just never leaves the process. It gains a `step(ns, text)` helper writing two namespace annotations — `bifrost/step` (human text) and `bifrost/step-since` (RFC3339) — at each stage boundary. `previewRecord` exposes `step`/`stepSince`, plus `error` (missing today, which is why the CLI's failure path can only say "check the UI"). `ib preview up` polls faster (3s), renders a live spinner with the current step and its elapsed time on a TTY, and degrades to one plain line per step change when piped. One annotation write per step — not per poll tick — because the CLI computes elapsed from `step-since` locally.

**Why annotations rather than a stream:** the namespace is already the preview's record (no database, by design), the read path and its bearer auth already exist, and a crashed-and-restarted bifrost leaves the last step visible rather than dropping a stream.

**Tech Stack:** Go 1.26 (bifrost), Python stdlib (ib.py — no argparse, no new deps).

**Repos/branches:** bifrost → `preview-progress` (Tasks 1–2, one PR); infra → `preview-progress-cli` (Task 3, its own PR).

## Global Constraints

- Bifrost gates: `go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l .` (empty) AND `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...`. Infra gates: `uv run ruff check` AND `uv run ruff format --check .` (the Makefile's lint skips the format check and that has cost a red CI).
- Step text is operator-facing prose, lowercase, no secrets — it lands in a namespace annotation any cluster reader can see. Never interpolate a Neon URI, token, or secret value.
- A failure to write a step annotation must never fail the preview. Progress reporting is best-effort; log and continue.
- No change to the phase machine (`creating|ready|failed`), the sanctioned divergences, or any plan-5/6 equivalence test.
- `ib.py` stays stdlib-only with hand-rolled argv parsing; the module docstring is the help text.

---

### Task 1: The orchestrator narrates itself

**Files:**
- Modify: `internal/preview/orchestrator.go` (+test), `internal/web/previews.go` (+test)

**Interfaces (produced):**

```go
// step records what Up is doing now, for operators watching `ib preview up`.
// Best-effort: an annotation failure is logged, never fatal.
func (o *Orchestrator) step(ctx context.Context, ns, text string)
```

and on `previewRecord`:

```go
	Step      string    `json:"step,omitempty"`      // "" when not creating
	StepSince time.Time `json:"stepSince,omitzero"`
	Error     string    `json:"error,omitempty"`     // bifrost/error, surfaced for the CLI
```

Steps to emit, in `Up`'s existing order (wording is yours; these are the moments):
1. after membership resolution — name the members, e.g. `resolving members: footstrike-api, footstrike-dashboard`
2. before each build, with position — `building footstrike-api (1/2)`; the build poll loop already knows the member and index
3. before Neon work — `branching databases`
4. before secret/cert copies — `copying secrets`
5. before render+apply — `applying manifests`

Reaching `ready` should clear `bifrost/step` (set it to empty) so a finished preview doesn't display a stale step. A `failed` preview should KEEP its last step — that's the diagnostic ("failed while building footstrike-api").

**Care point:** `recordFromNamespace` is pure and heavily tested by plan 5/6 goldens. Adding fields is fine; changing existing ones is not. Confirm the golden equivalence tests still pass untouched.

- [ ] **Step 1:** Failing tests — `step()` writes both annotations; annotation failure doesn't fail `Up` (fake kube returning an error on AnnotateNamespace mid-run still reaches `ready`); `recordFromNamespace` surfaces step/stepSince/error; a ready preview reports empty step; a failed one retains its last step.
- [ ] **Step 2–4:** Verify failure, implement, confirm plan 5/6 tests pass unchanged.
- [ ] **Step 5:** Gates + commit — "Report per-step progress while a preview is created".

---

### Task 2: The Previews tab shows the step

**Files:**
- Modify: `templates/previews.html`, `internal/web/previews.go` if the view model needs it (+test)

- [ ] **Step 1:** For a preview whose phase is `creating`, show the step text next to the phase (and for `failed`, show the retained step plus the error). Reuse existing CSS classes — no new stylesheet rules, consistent with how this tab was built. The tab already polls every few seconds via the fragment endpoint, so this animates for free.
- [ ] **Step 2:** Extend the existing render smoke test with a creating-with-step fixture; assert the step text appears and that a ready preview shows none.
- [ ] **Step 3:** Gates + commit — "Show the current step on the Previews tab".

---

### Task 3: `ib preview up` renders live progress

**Repo:** `~/Develop/ibormeith/infra`, branch `preview-progress-cli` off up-to-date main.

**Files:**
- Modify: `ib.py`

Read the whole file first; match its hand-rolled style. The current loop is in `preview_up`: `time.sleep(10)`, poll, print only on phase change.

Required behavior:
- **Poll every 3s** while creating (10s is too coarse for step-level feedback).
- **On a TTY** (`sys.stdout.isatty()`): render a single updating line with `\r` — a spinner frame, the current step, and elapsed seconds computed from `stepSince` (e.g. `⠹ building footstrike-api (1/2) — 47s`). When the step changes, finalize the previous line with a checkmark and its total duration, then start a new one. Clear the line before printing the final URLs.
- **Not a TTY** (piped, CI): no spinner, no `\r`. Print one plain line per step change (`building footstrike-api (1/2)`), so logs stay readable.
- **On failure:** print the step it failed at and the `error` field now available from the API (replacing today's "check the Previews tab" fallback — keep a pointer to the tab as a secondary hint).
- Spinner frames and elapsed rendering must not depend on any non-stdlib module.
- If `step` is absent (older bifrost not yet promoted), fall back to today's behavior — print on phase change only. The CLI must not break against a server that predates Task 1.

- [ ] **Step 1:** Implement; verify by hand against the real API in three modes: a TTY run, a piped run (`| cat`), and a simulated old-server response (temporarily strip `step` from the parsed record, or point at a stub) to prove the fallback. Paste actual output in the report.
- [ ] **Step 2:** Update the module docstring if the UX description changes. Gates (`ruff check` AND `ruff format --check .`), commit, push, PR.

---

## Self-review notes

- **Ordering:** Task 1 must merge and be promoted before Task 3's output is visible — but Task 3 is written to degrade gracefully, so the PRs are independent and can merge in either order.
- **Deliberately not doing:** a streaming/websocket endpoint (the namespace-as-record design is the point), per-poll-tick annotation writes (the CLI computes elapsed locally from `step-since`), and build log URLs in the step text (they'd bloat an annotation; the failure path's `error` plus the Cloud Build console is enough — revisit if it proves insufficient).
- **The `error` field addition** closes a gap found during plan 4b: `previewRecord` had no error field, so `ib preview up`'s failure branch could only tell the operator to go look at the UI.
