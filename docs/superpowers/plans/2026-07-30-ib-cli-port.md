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

Also for Task 3: the patch bodies are semantically identical but not byte-identical (`json.dumps` emits `": "` separators, `encoding/json` does not), so Task 3's "byte-equivalent" wording had to be read as equivalent-after-decoding — corrected in place below.

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

- [x] **Step 1:** Port the decision path using `internal/promote` directly. No tag math may be reimplemented in `cmd/bif` — if something is missing, add it to `internal/promote` where the server shares it.

- [x] **Step 2:** Port the write. `ib.py` shells out to `kubectl patch application <app>-prod -n argocd --type=merge -p '<json>'` with a kustomize images override. In Go this is a dynamic-client merge patch on the ArgoCD Application CR. **The resulting patch must be semantically equivalent to the Python's** — assert that against a captured fixture by decoding both sides, because this is the step that changes what runs in production. (It cannot be byte-equivalent: `json.dumps` emits `": "` and `", "` separators and `encoding/json` emits neither. A merge patch is parsed, not compared, so the effect is identical — but an assertion on bytes would fail on whitespace while proving nothing about what prod runs. Task 1's findings called this; the wording above was wrong and is now corrected.)

- [x] **Step 3:** Preserve the confirmation prompt and `-y`. A promote is not reversible in one keystroke; do not quietly make it easier.

- [x] **Step 4:** Verify against the cluster **read-only** — `kubectl get application <app>-prod -n argocd -o json` before and after a dry run. Do not perform a real promotion as part of implementation.

- [x] **Step 5: Commit.**

**Findings (read before Task 5).**

1. **The override key diverges from `ib.py`, deliberately.** `ib.py` builds it as `REGISTRY + "/" + app`; `cmd/bif` builds it with `promote.ReplaceTag` over `promote.ImageBase`, from the image the Deployment is actually running. For a service whose image repository is not named after it, `ib.py` writes an override keyed on an image no manifest references — kustomize ignores it silently, so the promote reports success and prod never moves. `footstrike-api` is that service (its image path is still `fitness-api`), and `cmd/bif/promote_test.go`'s `TestOverrideKeyComesFromTheImageNotTheAppName` pins it, reading `ib.py`'s value from `image_base.json` so the divergence fails loudly if the Python ever changes. **This is a behaviour change against `ib.py` and a fix.**
2. **`bif status` now prints `To promote: bif promote <app>`.** The hint names the command that implements it. It is the only line of status output that departs from the oracle capture; the golden test applies that one substitution and guards against it becoming a no-op.
3. **The staging/prod mismatch asymmetry lives in `cmd/bif`, not `internal/promote`.** `StatusOf` calls both `MidDeploy`; refusing on staging and warning on prod is a statement about what a promote may do, not about cluster state, so no verdict was widened or altered to accommodate it.
4. **`promote.ReplaceTag` is new**, and `internal/web` now calls it instead of its own copy — one more decision the server and the CLI cannot drift apart on.
5. **One small divergence in the prompt:** on EOF from stdin, `ib.py` raises `EOFError` and dies with a traceback (exit 1) where `bif` prints `Aborted.` and exits 0. Both refuse the write.

---

### Task 4: `ib preview` — the HTTP client

**Files:**
- Create: `cmd/bif/preview.go`, `cmd/bif/preview_test.go`
- Reuse: the server's own record types rather than redeclaring them

- [x] **Step 1:** `preview list`, `up`, `down` with every flag the docstring documents: `--ttl`, `--auto-update`, `--no-wait`, `-y/--yes`. Argument parsing must reject unrecognized leftover tokens — `ib.py` learned this the hard way when `--ttl=8h` was silently accepted and ignored, producing a preview that never expired.

- [x] **Step 2:** Decode into the server's `previewRecord` shape instead of hand-written structs. This is a primary prize: the field names stop being copied by hand. Note the record type is currently unexported in `internal/web` — decide deliberately whether to export it, move it to a shared package, or define a client-side mirror pinned by a test, and justify the choice.

- [x] **Step 3:** Port the behaviors the Python earned through real bugs. Every one of these exists because it broke: a 404 means "not created yet" for `up` but a genuine error for `down`; a requested `--ttl` or `--auto-update` missing from the resulting record warns on stderr but exits 0; a tag belonging to a different branch fails loudly; `builtImages` differences produce `nothing rebuilt` or `rebuilt: <members>`, and absence produces **silence** rather than a guess.

- [x] **Step 4:** Port the progress rendering — 3-second polling, a redrawn spinner line on a TTY, plain one-line-per-step when piped. Test both, driving the TTY decision through an injected value; a Go test's stdout is not a terminal, so the branch cannot be exercised by accident.

- [x] **Step 5:** Delete the `TagForBranch` mirror. `cmd/bif` calls `preview.TagForBranch` directly. This is the moment that duplication dies.

- [x] **Step 6: Commit.**

**Findings from `up` (read before Task 5).** `up` is ported; `ib.py` is now fully mirrored in Go and nothing in `cmd/bif` prints a not-ported message.

1. **The `TagForBranch` mirror is dead.** `cmd/bif/preview.go` calls `preview.TagForBranch` for the pre-POST lookup, and `ib.py`'s hand-written copy has no counterpart in Go. `TestPreviewUpDerivesTheTagWithTheServersOwnRule` asserts the pre-POST GET's path against the function's own answer for `Feat/Billing_Fix`, so a reintroduced copy that disagreed anywhere would have to disagree there. The mirror's safety property survives it: `TestPreviewUpDropsThePrePostSnapshotOnATagMismatch` pins that a POST naming a different tag throws the snapshot away, so a future bifrost that stops deriving tags client-derivably costs one wasted GET and today's output, never a wrong verb or a false "nothing rebuilt".
2. **`previewapi.Record` gained a tolerant `UnmarshalJSON` for `stepSince` alone.** In Python an unusable timestamp was a `TypeError` inside the elapsed-time arithmetic; in Go the same input fails the whole typed decode one layer earlier, losing the phase, the error and the URLs over a cosmetic display. The decoder now leaves the field zero for anything not RFC3339 — the same reading `internal/web`'s `recordFromNamespace` already applies to the annotation, so bifrost itself never emits a value this rejects. Marshalling is untouched.
3. **`createPreviewRequest` moved to `previewapi.CreateRequest`**, so `ttl` and `autoUpdate` are declared once for the handler that decodes them and the CLI that sends them — the same prize as `Record`. One consequence: `--ttl ""` now omits the key rather than sending `"ttl":""`. Indistinguishable to bifrost (it trims and treats empty as no expiry; `TestCreatePreviewEmptyTTLIsNoExpiry`), and the CLI still warns, because `ttlSet` is tracked client-side exactly as `ib.py` tracks `None` versus `""`.
4. **A failed preview with no step still prints a bare `  failed` line**, and one with a step does not. `ib.py`'s comment says the failed branch prints no phase line, and that is true of the branch itself — but a step-less record has already gone through the phase-change branch above it. The asymmetry is the oracle's and is pinned by `wantPhaseLine` in `TestPreviewUpReportsAFailure`.
5. **Two cosmetic divergences, both consistent with earlier tasks.** Quoted values in the branch-mismatch and unrecognized-phase messages use Go's `%q` (double quotes) where Python used `!r` (single); and every `ib preview …` hint reads `bif preview …`.
6. **`golang.org/x/term` became a direct dependency** (it was already in the module graph via client-go) for the TTY check. The `os.ModeCharDevice` heuristic was rejected deliberately: `/dev/null` is a character device, so a CI job redirecting there would have got the spinner. `TestIsTerminalIsFalseForEverythingButATerminal` pins that.

**Findings from `list`/`down` (read before porting `preview up`).**

1. **The record type moved rather than being exported or mirrored.** `previewRecord`/`builtImageRecord` are now `previewapi.Record`/`previewapi.BuiltImage` in `internal/previewapi`, imported by both `internal/web` and `cmd/bif`, along with `ListResponse` and `ErrorResponse` so even the `previews` and `error` envelope keys are declared once. Exporting from `internal/web` was rejected because `cmd/bif` must not import the server at all; a hand-copied mirror is the exact duplication the port exists to remove. `up` gets `Step`, `StepSince`, `Error` and `BuiltImages` for free and must not redeclare them.
2. **`TestNoBifrostServerDependency` was narrowed, not deleted.** Every non-test file in `cmd/bif` except `preview.go` is still banned from `net/http`, `internal/web`, `internal/auth`, `internal/oracle` and now `internal/previewclient` — a whitelist by exception rather than by enumeration, so a new file is checked automatically. A companion test walks the bifrost-internal dependency closure of what `main.go`/`status.go`/`promote.go` import and fails if `internal/web` or `internal/previewclient` appears below `cmd/bif`. Keep `up` inside `preview.go`, or the exemption has to widen.
3. **A synthesized busy record renders `busy*`, not a bare `busy`.** `ib.py`'s docstring says otherwise, but `previewRecord.Phase` has never had `omitempty` and `busyRecord` has always filled it with `"busy"`, so `preview_phase_display`'s `phase is None` branch is dead against any bifrost that ever shipped. The port keeps the branch (it answers an older or non-bifrost server) and pins both cases. The docstring's claim, not the code, is what was wrong.
4. **Two deliberate divergences from `ib.py`, both stated in code comments.** `list` and `down` reject leftover arguments where `ib.py` silently drops them — the same discipline Step 1 demands for `--ttl`; and the hints in the 409 and not-found messages say `bif preview list`, matching the substitution `bif status`'s promote hint already makes. The `User-Agent` deliberately still reads `ib-preview-cli`: it is a token an external, dashboard-managed WAF sees, and nothing in this repo can prove no rule keys on it.
5. **The token is memoized** on `previewclient.Client` after the first successful fetch. `ib.py` re-runs `gcloud` per request, which is a subprocess every 3 seconds once `up`'s poll loop exists.

---

### Task 5: Cutover

Only after Tasks 1–4 are merged and the Go CLI has been used for real work.

- [x] **Step 1:** Run both CLIs side by side against the same cluster over a real preview lifecycle — create, re-run unchanged, push and re-run, tear down — and diff the output. Record the differences and confirm each is intended.

- [x] **Step 2:** Remove `ib.py` and `verify_preview_progress.py` from the infra repo. Leave a pointer at bifrost.

- [x] **Step 3:** Update `~/Develop/ibormeith/.claude/CLAUDE.md`, bifrost's README, `docs/preview-environments.md`, and `docs/adding-a-service.md` — every one of them documents `ib` commands. Verify each claim against the new code.

- [x] **Step 4:** Note in the infra repo that `SERVICES` no longer needs maintaining there, since it was a deliberate duplicate of the registry.

- [x] **Step 5: Commit.** (PRs deliberately not opened; both branches are committed and unpushed.)

**Findings.**

1. **Step 1 was performed on the read-only paths only, and that is the one claim in this cutover not to overstate.** `status` in all four forms, `status -q`, per-app `status`, and `preview list` were run side by side against the live cluster and are **byte-identical including exit codes**, with exactly one intended difference: the out-of-sync hint reads `bif promote <app>` where `ib.py` said `ib promote <app>` (`cmd/bif/status_test.go`'s `retargetPromoteHint` applies that substitution to the oracle capture, and `TestOutOfSyncHintNamesBif` fails if it becomes a no-op).

   **`promote`, `preview up` and `preview down` were NOT compared live.** They mutate — a real side-by-side would mean promoting to prod twice and standing up and tearing down a preview twice, which is a production write and a Cloud Build spend to prove something the fixtures already pin. Their equivalence rests on the oracle fixtures captured in Task 1 and the mutation-checked tests from Tasks 3 and 4: the patch body decoded against `promote_decision.json`, the override key read from `image_base.json` (`TestOverrideKeyComesFromTheImageNotTheAppName`), and `up`/`down`'s 404 asymmetry, tag-mismatch, TTL/auto-update warning and `builtImages` diffing tests. That is the actual evidence, and the docs written in Step 3 say nothing that implies more.

2. **The Python's packaging entry point mattered more than the file.** `infra/pyproject.toml` carried `[project.scripts] ib = "ib:main"`. Deleting `ib.py` without it leaves `uv sync` **succeeding** and still installing a `ib` console script that dies with `ModuleNotFoundError: No module named 'ib'` — verified by building the old `pyproject.toml` against a stub. Infra CI (`.github/workflows/pull_request.yaml`: `uv sync`, `uv run ruff check`, `uv run ty check`) would not have caught it, because nothing there runs `ib`. The entry point is removed.

3. **Infra's `ty check` step is now vacuous.** `[tool.ty.src] exclude = ["__main__.py"]` was the only exclusion, and `ib.py`/`verify_preview_progress.py` were the only other Python files. `ty` now reports `WARN No python files found under the given path(s)` and exits 0, so CI stays green while type-checking nothing. Left as-is — it costs nothing and starts working again the moment a non-Pulumi module lands — but it is not a signal any more.

4. **Three classes of `ib` reference, treated differently.** Commands a reader should now type became `bif` (all of `docs/preview-environments.md`, the command blocks in the README and `docs/adding-a-service.md`, and the empty-state hint in `templates/previews.html`, which is user-facing UI text). Historical and explanatory references to the Python stayed `ib`/`ib.py` and now say it is retired — the README's "Two deliberate differences from the retired `ib.py`", `internal/promote/differential_test.go` (whose whole subject is the oracle comparison), and the oracle constant `oraclePromoteHint`, which is a captured fixture value and must not move. The dead `infra/ib.py` path is named in exactly one place, the README sentence that says it was deleted.

5. **One doc claim was false and is fixed.** The README said "`bif` reaches the cluster directly through client-go and never calls bifrost's API" — true when it was written, and untrue from the moment Task 4 landed `preview`. It is now scoped to `status` and `promote`, with the `preview` exception stated and pointed at `TestNoBifrostServerDependency`, which is what actually enforces it.

6. **`adding-a-service.md`'s "Gotcha: `ib.py`'s own service list" is resolved but replaced, not deleted.** The cross-repo duplicate is gone. What survives is weaker and worth keeping written down: `bif` carries the registry that was `go:embed`ed when its binary was built, so a newly added service is refused by `validateApp` until you `make install` again. `bif preview up` is exempt — it never checks the name against the registry, and membership is resolved by prod bifrost.

7. **Deleting `ib.py` broke the oracle *regeneration* path, not the oracle.** `scripts/capture-oracle.sh` defaulted to `~/Develop/ibormeith/infra/ib.py`, which no longer exists. The committed `testdata/oracle/` fixtures are what the tests read, so nothing fails — but the scripts are the reason those fixtures are reproducible rather than merely asserted, so they are kept and now say `IB_PY` is required and where to recover the Python from (`git -C ~/Develop/ibormeith/infra show 9db57a3:ib.py`, verified to resolve). `testdata/oracle/meta.json` still records the old absolute path; it is generated and marked do-not-hand-edit, so it was left alone.

8. **Task 2's step checkboxes are still unticked** despite its work being merged (it is the only task with no findings section). Not touched here; flagging it so the omission is deliberate rather than lost.

---

## Self-Review

**Sequencing.** Task 1 first is the point: the Python is a working oracle, and capturing it before changing anything turns this from a rewrite into a port with a test for every behavior. Tasks 2 and 3 are independently useful — they kill the promote-logic duplication even if Task 4 never happens.

**The interim split.** Between Tasks 3 and 4 there are two CLIs: a Go `ib` doing `status`/`promote` and the Python one doing `preview`. That is genuinely awkward and should be short-lived. If Task 4 is going to be deferred, keep `ib.py` as the only entry point and have it shell out — do not ask the operator to remember which binary does what.

**Deliberately not in scope.** Replacing the `gcloud` shell-out for the token with the Secret Manager API (new dependency and IAM for no gain); making `preview` use internal packages instead of HTTP (the server owns the busy set, the write credentials, and the orchestration — the CLI must not); and any change to what `status`/`promote` actually do. This is a port, not a redesign.

**Risk that would abort it.** If Task 1 finds that Go and Python disagree on a promote tag in a case that occurs in practice, stop and resolve that before porting anything. A silent behavior change in which image reaches production is the one failure this plan cannot absorb.
