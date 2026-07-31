# Porting `ib` into bifrost Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the `ib` CLI from `infra/ib.py` into the bifrost repo as a second Go binary, so the logic it shares with the server has exactly one implementation.

**Architecture:** `cmd/bif` alongside `cmd/bifrost` in the same module. `bif status` and `bif promote` import `internal/promote`, `internal/registry`, and `internal/kube` **directly** and never contact the bifrost server. `bif preview` remains an HTTP client of bifrost's API, as today. The Python `ib.py` is the reference implementation and stays in place until the final cutover.

**Tech Stack:** Go 1.26, existing bifrost internal packages, `client-go` (already a dependency), `gcloud` shelled out for the preview API token exactly as today.

## Why this is worth doing

`internal/promote` is pure decision logic with no I/O — `ExtractTag`, `ImageBase`, `ExtractSHA`, `NewProdTag`, `StatusOf`. `ib.py` reimplements the same decisions in Python (`new_prod_tag_for` and friends) in order to promote. **Two implementations decide which image tag reaches production, and nothing tests them against each other.** That is the duplication this port exists to remove; everything else is a consequence.

Secondary wins: the `TagForBranch` mirror added to `ib.py` disappears, the hand-maintained JSON record shapes become the server's own structs, `SERVICES` stops being a hardcoded list, and a 1,131-line hand-rolled assert script is replaced by ordinary Go tests.

## The property that must not be lost

**`ib promote bifrost` has to work when bifrost is down.** It is the recovery path for the server itself, and that is not hypothetical — bifrost went down on a spot-node preemption during the preview-environments work and this is how it would have been recovered.

So `status` and `promote` must reach the cluster directly and must never require the bifrost server, its API, or its bearer token. `cmd/bif` is a local binary that calls packages; it is not a client of the service it manages. Any task that blurs this is wrong even if its tests pass.

Note the registry is `go:embed`ed, so reading service names from `internal/registry` **preserves** this property — the list is compiled in, not fetched.

## Global Constraints

- `cmd/bif` must not import anything that opens an HTTP connection to bifrost for `status` or `promote`.
- **`ib.py` is the oracle.** Behavior is defined by what it does today, not by what seems reasonable. Where the port deliberately differs, that is a decision to state in the report and the docs, not a silent improvement.
- **Exit codes and quiet-mode output are contracts.** `ib status -q` exits 0/1 and is scriptable. Preserve them exactly.
- The preview API token comes from `gcloud secrets versions access latest --secret=…`. Keep shelling out — it avoids a new dependency and new IAM.
- Preview API requests must keep the custom `User-Agent`. Cloudflare's WAF blocks Go's default agent as surely as it blocks `python-urllib`; a missing header breaks the CLI against prod.
- Gates for every task: `go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1` and `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run ./...`.
- Every new assertion must discriminate: revert the change, confirm the test fails, restore. Report per test.

---

### Task 1: Differential test harness against the Python oracle

Build the safety net before porting anything. This is what makes the port TDD rather than a rewrite-and-hope.

**Files:**
- Create: `internal/promote/differential_test.go`
- Create: `testdata/oracle/` (captured Python outputs)
- Create: `scripts/capture-oracle.sh`

**Interfaces:**
- Produces: golden files that later tasks assert against.

- [x] **Step 1: Capture the oracle.** Write a script that drives `infra/ib.py`'s pure functions over a matrix of inputs and writes the results as fixtures. Cover at minimum `new_prod_tag_for` across: staging and prod tags of the same shape, `{sha}` vs `{sha}-{env}` taggers (footstrike-dashboard uses the suffixed form, everything else doesn't), a prod tag with no parseable SHA, and equal staging/prod. Also capture `tag_for_branch` over the vectors that matter — character folding (`feat/foo`, `feat-foo`, `feat_foo`), truncation at 30 characters, leading and trailing dashes, uppercase, unicode, and empty.

- [x] **Step 2: Assert Go matches.** Table tests reading those fixtures and calling `promote.NewProdTag`, `promote.ExtractSHA`, `promote.StatusOf`, and `preview.TagForBranch`. **Expect failures here** — where Go and Python disagree, that is the port's first real finding. Do not "fix" either side without deciding which is correct and saying so.

- [x] **Step 3: Commit** the harness and fixtures, with any disagreements recorded in the report rather than papered over.

**Findings (read before Task 2 and Task 3).** `new_prod_tag_for` agrees with `promote.NewProdTag` on all 19 vectors, including both tagging schemes, missing/empty prod tags, and the June-2026 legacy-suffixed-prod case — the stop-the-project risk did not materialise. `tag_for_branch` agrees on all 38 vectors, so `ib.py`'s "mirror" claim is now true rather than assumed. Three disagreements, all pinned with analysis in `internal/promote/differential_test.go`:

1. **`ib status` on an unpinned prod tag** (`bifrost#30`) — `ib.py` reports indeterminate, `promote.StatusOf` reports out-of-sync. Go is right (`ib.py`'s own `promote()` promotes from this state), but **Task 2 must state the change**: `ib status -q` will print the app and exit 1 where the Python exited 0.
2. **`promote.Status` drops the tags `ib.py` still displays** when one side is mid-deploy or has no pods. Task 2 needs the tags from somewhere else, or `Status` has to widen.
3. **The kustomize override key** — `ib.py` builds it from `REGISTRY + app name`, `promote.ImageBase` parses it from the running image. Identical for the fleet as named today; Go is right and Task 3 must not port `ib.py`'s version.

Also for Task 3: the patch bodies are semantically identical but not byte-identical (`json.dumps` emits `": "` separators, `encoding/json` does not).

---

### Task 2: `cmd/bif` skeleton, and `bif status`

**Files:**
- Create: `cmd/bif/main.go`, `cmd/bif/status.go`, `cmd/bif/status_test.go`
- Modify: `Makefile` (an install target)

**Interfaces:**
- Consumes: `internal/registry` for service names, `internal/kube` for images.
- Produces: the argument-dispatch shape later tasks extend.

- [ ] **Step 1:** Argument dispatch. `ib` with no arguments prints usage; unknown commands exit non-zero. Mirror `ib.py`'s hand-rolled style — this is a small tool, not a place for a CLI framework.

- [ ] **Step 2:** `ib status`, `ib status <app>`, `ib status -q`, `ib status <app> -q`. Read service names from the embedded registry rather than a hardcoded list. Match the Python's table layout and its quiet-mode exit codes exactly; the `-q` forms are scriptable and their behavior is a contract.

- [ ] **Step 3:** Tests, including a golden comparison of the rendered table against the Python's output for the same cluster state.

- [ ] **Step 4:** `make install` building to a location on `PATH`. Document it — distribution is the one thing the Python version got for free and the port must not leave implicit.

- [ ] **Step 5: Commit.**

---

### Task 3: `ib promote`

The task that justifies the project. **Read Task 1's findings before starting.**

**Files:**
- Create: `cmd/bif/promote.go`, `cmd/bif/promote_test.go`
- Modify: `internal/kube` if a patch helper is needed

- [ ] **Step 1:** Port the decision path using `internal/promote` directly. No tag math may be reimplemented in `cmd/bif` — if something is missing, add it to `internal/promote` where the server shares it.

- [ ] **Step 2:** Port the write. `ib.py` shells out to `kubectl patch application <app>-prod -n argocd --type=merge -p '<json>'` with a kustomize images override. In Go this is a dynamic-client merge patch on the ArgoCD Application CR. **The resulting patch must be byte-equivalent to the Python's** — assert that against a captured fixture, because this is the step that changes what runs in production.

- [ ] **Step 3:** Preserve the confirmation prompt and `-y`. A promote is not reversible in one keystroke; do not quietly make it easier.

- [ ] **Step 4:** Verify against the cluster **read-only** — `kubectl get application <app>-prod -n argocd -o json` before and after a dry run. Do not perform a real promotion as part of implementation.

- [ ] **Step 5: Commit.**

---

### Task 4: `ib preview` — the HTTP client

**Files:**
- Create: `cmd/bif/preview.go`, `cmd/bif/preview_test.go`
- Reuse: the server's own record types rather than redeclaring them

- [ ] **Step 1:** `preview list`, `up`, `down` with every flag the docstring documents: `--ttl`, `--auto-update`, `--no-wait`, `-y/--yes`. Argument parsing must reject unrecognized leftover tokens — `ib.py` learned this the hard way when `--ttl=8h` was silently accepted and ignored, producing a preview that never expired.

- [ ] **Step 2:** Decode into the server's `previewRecord` shape instead of hand-written structs. This is a primary prize: the field names stop being copied by hand. Note the record type is currently unexported in `internal/web` — decide deliberately whether to export it, move it to a shared package, or define a client-side mirror pinned by a test, and justify the choice.

- [ ] **Step 3:** Port the behaviors the Python earned through real bugs. Every one of these exists because it broke: a 404 means "not created yet" for `up` but a genuine error for `down`; a requested `--ttl` or `--auto-update` missing from the resulting record warns on stderr but exits 0; a tag belonging to a different branch fails loudly; `builtImages` differences produce `nothing rebuilt` or `rebuilt: <members>`, and absence produces **silence** rather than a guess.

- [ ] **Step 4:** Port the progress rendering — 3-second polling, a redrawn spinner line on a TTY, plain one-line-per-step when piped. Test both, driving the TTY decision through an injected value; a Go test's stdout is not a terminal, so the branch cannot be exercised by accident.

- [ ] **Step 5:** Delete the `TagForBranch` mirror. `cmd/bif` calls `preview.TagForBranch` directly. This is the moment that duplication dies.

- [ ] **Step 6: Commit.**

---

### Task 5: Cutover

Only after Tasks 1–4 are merged and the Go CLI has been used for real work.

- [ ] **Step 1:** Run both CLIs side by side against the same cluster over a real preview lifecycle — create, re-run unchanged, push and re-run, tear down — and diff the output. Record the differences and confirm each is intended.

- [ ] **Step 2:** Remove `ib.py` and `verify_preview_progress.py` from the infra repo. Leave a pointer at bifrost.

- [ ] **Step 3:** Update `~/Develop/ibormeith/.claude/CLAUDE.md`, bifrost's README, `docs/preview-environments.md`, and `docs/adding-a-service.md` — every one of them documents `ib` commands. Verify each claim against the new code.

- [ ] **Step 4:** Note in the infra repo that `SERVICES` no longer needs maintaining there, since it was a deliberate duplicate of the registry.

- [ ] **Step 5: Commit** and open the paired PRs.

---

## Self-Review

**Sequencing.** Task 1 first is the point: the Python is a working oracle, and capturing it before changing anything turns this from a rewrite into a port with a test for every behavior. Tasks 2 and 3 are independently useful — they kill the promote-logic duplication even if Task 4 never happens.

**The interim split.** Between Tasks 3 and 4 there are two CLIs: a Go `ib` doing `status`/`promote` and the Python one doing `preview`. That is genuinely awkward and should be short-lived. If Task 4 is going to be deferred, keep `ib.py` as the only entry point and have it shell out — do not ask the operator to remember which binary does what.

**Deliberately not in scope.** Replacing the `gcloud` shell-out for the token with the Secret Manager API (new dependency and IAM for no gain); making `preview` use internal packages instead of HTTP (the server owns the busy set, the write credentials, and the orchestration — the CLI must not); and any change to what `status`/`promote` actually do. This is a port, not a redesign.

**Risk that would abort it.** If Task 1 finds that Go and Python disagree on a promote tag in a case that occurs in practice, stop and resolve that before porting anything. A silent behavior change in which image reaches production is the one failure this plan cannot absorb.
