#!/usr/bin/env python3
"""Capture the behaviour of ib.py's promote/tag logic as JSON fixtures.

`infra/ib.py` is the oracle: it is what promotes to production today. This
script imports the REAL ib.py (never a reimplementation of it), drives its
pure decision functions over a matrix of inputs, and writes the results to
testdata/oracle/. The Go differential tests in internal/promote and
internal/preview then assert that the Go port produces identical answers.

Usage:
    scripts/capture-oracle.sh                    # defaults below
    python3 scripts/capture_oracle.py --ib-py /path/to/ib.py --out testdata/oracle

Everything written here must be deterministic: re-running against the same
ib.py must produce a byte-identical tree, so that a real behaviour change
shows up as a reviewable diff. That rules out timestamps, and it rules out
any case whose answer depends on Python set-iteration order (see
STDOUT_NONDETERMINISTIC below).
"""

from __future__ import annotations

import argparse
import importlib.util
import io
import json
import subprocess
import sys
import types
from contextlib import redirect_stdout
from pathlib import Path

DEFAULT_IB_PY = Path.home() / "Develop" / "ibormeith" / "infra" / "ib.py"
REPO_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_OUT = REPO_ROOT / "testdata" / "oracle"


# --------------------------------------------------------------------------
# Loading the oracle
# --------------------------------------------------------------------------


def load_ib(path: Path) -> types.ModuleType:
    """Import ib.py from an arbitrary path without installing it.

    ib.py guards its entrypoint with `if __name__ == "__main__"` and imports
    only the standard library, so importing it is side-effect free.
    """
    spec = importlib.util.spec_from_file_location("ib_oracle", path)
    if spec is None or spec.loader is None:
        raise SystemExit(f"cannot import {path}")
    module = importlib.util.module_from_spec(spec)
    sys.modules["ib_oracle"] = module
    spec.loader.exec_module(module)
    return module


def git_describe(path: Path) -> dict[str, str]:
    """Identify the exact ib.py these fixtures describe."""

    def git(*args: str) -> str:
        return subprocess.run(
            ["git", *args],
            cwd=path.parent,
            capture_output=True,
            text=True,
            check=False,
        ).stdout.strip()

    dirty = git("status", "--porcelain", "--", path.name)
    return {
        "path": str(path),
        "repoHeadSha": git("rev-parse", "HEAD"),
        "fileLastCommitSha": git("log", "-1", "--format=%H", "--", path.name),
        "fileUncommittedChanges": bool(dirty),
    }


# --------------------------------------------------------------------------
# The input matrix
# --------------------------------------------------------------------------

# Real registry path all fleet services publish to.
REG = "us-central1-docker.pkg.dev/ethans-services/containers"

EXTRACT_TAG_INPUTS = [
    f"{REG}/bifrost:abc1234",
    # footstrike-dashboard is the one service on the {sha}-{env} scheme.
    f"{REG}/footstrike-dashboard:abc1234-staging",
    f"{REG}/footstrike-dashboard:abc1234-prod",
    f"{REG}/bifrost:latest",
    # No tag at all -> the implicit "latest".
    f"{REG}/bifrost",
    # Empty tag after the colon.
    f"{REG}/bifrost:",
    # Registry host with a port: the LAST colon is the tag separator, and a
    # port with no tag is misparsed identically by both implementations.
    "localhost:5000/bifrost:abc1234",
    "localhost:5000/bifrost",
    # Digest-pinned reference.
    f"{REG}/bifrost@sha256:0123456789abcdef0123456789abcdef",
    "",
]

EXTRACT_SHA_INPUTS = [
    "abc1234",
    "1234567",
    "0000000",
    "deadbeefdeadbeefdeadbeef",
    # Suffixed scheme (footstrike-dashboard).
    "abc1234-staging",
    "abc1234-prod",
    # The suffixed branch of the regex has no 7-char minimum; the plain
    # branch does. Both sides must reproduce that asymmetry.
    "abc-staging",
    "a-prod",
    "abc123",
    # Mutable tags prod can fall back to when an Application is recreated
    # (bifrost#30). These must parse to "no SHA".
    "latest",
    "prod",
    "staging",
    "",
    "-staging",
    "-prod",
    "abc1234-preview",
    "abc1234-staging-prod",
    "abc1234staging",
    "ABC1234",
    "g123456",
    "abc1234-",
    # Python's `$` also matches just before a trailing newline; Go's does
    # not. Not reachable from a Kubernetes image string, but it is a real
    # difference between the two regex engines and is pinned deliberately.
    "abc1234\n",
    "abc1234-staging\n",
]

# (staging_tag, prod_tag, staging_sha)
NEW_PROD_TAG_INPUTS = [
    # Plain {sha} scheme: every service except footstrike-dashboard.
    ("abc1234", "def5678", "abc1234"),
    ("abc1234", "abc1234", "abc1234"),
    # Suffixed {sha}-{env} scheme: footstrike-dashboard.
    ("abc1234-staging", "def5678-prod", "abc1234"),
    ("abc1234-staging", "abc1234-prod", "abc1234"),
    # Prod tag has no parseable SHA (bifrost#30: recreated Application fell
    # back to the mutable tag in the repo manifests).
    ("abc1234", "latest", "abc1234"),
    ("abc1234-staging", "prod", "abc1234"),
    ("abc1234-staging", "latest", "abc1234"),
    # The June 2026 forecasting outage in miniature: the service migrated to
    # environment-agnostic builds while prod still ran a legacy {sha}-prod
    # image. Keying off the prod tag here synthesises an image that was
    # never built.
    ("abc1234", "def5678-prod", "abc1234"),
    # ... and the reverse migration.
    ("abc1234-staging", "def5678", "abc1234"),
    # Missing / empty prod tag.
    ("abc1234", None, "abc1234"),
    ("abc1234-staging", None, "abc1234"),
    ("abc1234", "", "abc1234"),
    ("abc1234-staging", "", "abc1234"),
    # "-staging" is matched as a SUBSTRING, not a suffix. These pin that.
    ("abc1234-staging-extra", "def5678", "abc1234"),
    ("my-staging-branch", "def5678", "abc1234"),
    ("abc1234staging", "def5678", "abc1234"),
    ("abc1234-STAGING", "def5678", "abc1234"),
    # Prod-suffixed staging tag (nonsense input, but pins that only
    # "-staging" is consulted).
    ("abc1234-prod", "def5678-prod", "abc1234"),
    ("", "def5678", "abc1234"),
]

TAG_FOR_BRANCH_INPUTS = [
    # Character folding: these three MUST collapse to one tag. The many-to-one
    # property is deliberate on both sides.
    "feat/foo",
    "feat-foo",
    "feat_foo",
    "feat foo",
    # Uppercase.
    "FEAT/FOO",
    "Feat/Foo",
    "MAIN",
    # Leading / trailing / repeated separators.
    "-leading",
    "trailing-",
    "--both--",
    "/slash/",
    "___",
    "---",
    " ",
    "",
    "feat//foo",
    "feat/-/foo",
    "  spaces  ",
    # Truncation at 30 characters.
    "feature/add-user-profile-avatars-v1",
    "feature/add-user-profile-avatars-v2",
    "abcdefghijklmnopqrstuvwxyzabcd",  # exactly 30
    "abcdefghijklmnopqrstuvwxyzabcde",  # 31
    # The cut lands on a '-', which must then be trimmed.
    "abcdefghijklmnopqrstuvwxyzabc/d",
    "abcdefghijklmnopqrstuvwxyzab-cd-ef",
    # Dropped characters.
    "release/v1.2.3",
    "feat/#123-fix",
    "feat/foo@bar",
    "feat\tfoo",
    "feat.foo",
    # Unicode: lowercasing then dropping non-[a-z0-9-]. 'İ' is the
    # interesting one -- Python's str.lower() expands it to 'i' + U+0307
    # while Go's ToLower maps it to 'i'; both must land on the same tag.
    "feat/café",
    "fix/naïve-bug",
    "feat/日本語",
    "İstanbul",
    "STRASSE/ẞ",
    "ﬁx/ligature",
    "feat/🚀-rocket",
    "Ⅻ/roman",
    # A byte-vs-rune slicing trap: 40 multibyte characters that survive
    # lowercasing but are dropped, plus enough ASCII to cross the cap.
    "ααααααααα/abcdefghijklmnopqrstuvwxyzabcdefg",
]

# (app, staging_images, prod_images)
#
# Sets, in ib.py. Multi-image cases must stay in the "return early" region of
# status(), because ib.py's `next(iter(set))` on a multi-element set is
# hash-order dependent.
STATUS_INPUTS = [
    ("bifrost", [f"{REG}/bifrost:abc1234"], [f"{REG}/bifrost:abc1234"]),
    ("bifrost", [f"{REG}/bifrost:abc1234"], [f"{REG}/bifrost:def5678"]),
    (
        "footstrike-dashboard",
        [f"{REG}/footstrike-dashboard:abc1234-staging"],
        [f"{REG}/footstrike-dashboard:abc1234-prod"],
    ),
    (
        "footstrike-dashboard",
        [f"{REG}/footstrike-dashboard:abc1234-staging"],
        [f"{REG}/footstrike-dashboard:def5678-prod"],
    ),
    # bifrost#30: prod unpinned on a mutable tag.
    ("bifrost", [f"{REG}/bifrost:abc1234"], [f"{REG}/bifrost:latest"]),
    (
        "footstrike-dashboard",
        [f"{REG}/footstrike-dashboard:abc1234-staging"],
        [f"{REG}/footstrike-dashboard:prod"],
    ),
    # Staging unparseable.
    ("bifrost", [f"{REG}/bifrost:latest"], [f"{REG}/bifrost:abc1234"]),
    ("bifrost", [f"{REG}/bifrost:latest"], [f"{REG}/bifrost:latest"]),
    # Mid-deploy.
    (
        "bifrost",
        [f"{REG}/bifrost:abc1234", f"{REG}/bifrost:def5678"],
        [f"{REG}/bifrost:abc1234"],
    ),
    (
        "bifrost",
        [f"{REG}/bifrost:abc1234"],
        [f"{REG}/bifrost:abc1234", f"{REG}/bifrost:def5678"],
    ),
    # Duplicate images are one image (ib.py builds a set; Go dedupes).
    (
        "bifrost",
        [f"{REG}/bifrost:abc1234", f"{REG}/bifrost:abc1234"],
        [f"{REG}/bifrost:def5678"],
    ),
    # No pods.
    ("bifrost", [], [f"{REG}/bifrost:abc1234"]),
    ("bifrost", [f"{REG}/bifrost:abc1234"], []),
    ("bifrost", [], []),
    # Sidecar-style: two containers on different images is also "mid-deploy".
    (
        "bifrost",
        [f"{REG}/bifrost:abc1234", "docker.io/library/nginx:1.27"],
        [f"{REG}/bifrost:abc1234"],
    ),
]

# (app, staging_images, prod_images)
PROMOTE_INPUTS = [
    ("bifrost", [f"{REG}/bifrost:abc1234"], [f"{REG}/bifrost:def5678"]),
    ("bifrost", [f"{REG}/bifrost:abc1234"], [f"{REG}/bifrost:abc1234"]),
    (
        "footstrike-dashboard",
        [f"{REG}/footstrike-dashboard:abc1234-staging"],
        [f"{REG}/footstrike-dashboard:def5678-prod"],
    ),
    # bifrost#30 again, this time through the path that actually writes prod.
    ("bifrost", [f"{REG}/bifrost:abc1234"], [f"{REG}/bifrost:latest"]),
    (
        "footstrike-dashboard",
        [f"{REG}/footstrike-dashboard:abc1234-staging"],
        [f"{REG}/footstrike-dashboard:prod"],
    ),
    # Legacy suffixed prod, environment-agnostic staging (June 2026 outage).
    ("forecasting", [f"{REG}/forecasting:abc1234"], [f"{REG}/forecasting:def5678-prod"]),
    # Staging unparseable: refuses.
    ("bifrost", [f"{REG}/bifrost:latest"], [f"{REG}/bifrost:abc1234"]),
    # Mid-deploy on staging: refuses (exits 1).
    (
        "bifrost",
        [f"{REG}/bifrost:abc1234", f"{REG}/bifrost:def5678"],
        [f"{REG}/bifrost:abc1234"],
    ),
    # No pods: refuses (exits 1).
    ("bifrost", [], [f"{REG}/bifrost:abc1234"]),
    ("bifrost", [f"{REG}/bifrost:abc1234"], []),
]

# Apps whose deployed image repository does not match REGISTRY/{app}. ib.py
# builds the kustomize override key from REGISTRY + app name and never looks
# at the running image; bifrost parses it out of the running image. They agree
# only while the two names agree.
IMAGE_BASE_INPUTS = [
    ("bifrost", f"{REG}/bifrost:abc1234"),
    ("footstrike-dashboard", f"{REG}/footstrike-dashboard:abc1234-prod"),
    # The rename footstrike-api went through in July 2026, had the image repo
    # not been renamed with it.
    ("footstrike-api", f"{REG}/fitness-api:abc1234"),
    ("bifrost", "localhost:5000/bifrost:abc1234"),
]


# --------------------------------------------------------------------------
# Drivers
# --------------------------------------------------------------------------


def call_capturing(fn) -> tuple[str, object]:
    """Run `fn`, returning (stdout, return value). SystemExit is a result."""
    buf = io.StringIO()
    try:
        with redirect_stdout(buf):
            value: object = fn()
    except SystemExit as exc:
        return buf.getvalue(), {"systemExit": exc.code}
    return buf.getvalue(), value


def capture_extract_tag(ib: types.ModuleType) -> list[dict]:
    return [{"image": i, "tag": ib.extract_tag(i)} for i in EXTRACT_TAG_INPUTS]


def capture_extract_sha(ib: types.ModuleType) -> list[dict]:
    return [{"tag": t, "sha": ib.extract_sha(t)} for t in EXTRACT_SHA_INPUTS]


def capture_new_prod_tag(ib: types.ModuleType) -> list[dict]:
    return [
        {
            "stagingTag": staging,
            "prodTag": prod,
            "stagingSha": sha,
            "newProdTag": ib.new_prod_tag_for(staging, prod, sha),
        }
        for staging, prod, sha in NEW_PROD_TAG_INPUTS
    ]


def capture_tag_for_branch(ib: types.ModuleType) -> list[dict]:
    return [{"branch": b, "tag": ib.tag_for_branch(b)} for b in TAG_FOR_BRANCH_INPUTS]


def _with_images(ib: types.ModuleType, app: str, staging: list[str], prod: list[str]):
    """Point ib.get_deployed_images at a fixed answer instead of kubectl.

    ib.py itself is never modified -- only the attribute on the imported
    module object. Returns the original function so the caller can restore it.
    """
    namespaces = {f"{app}-staging": set(staging), f"{app}-prod": set(prod)}
    original = ib.get_deployed_images
    ib.get_deployed_images = lambda ns: namespaces[ns]
    return original


def capture_status(ib: types.ModuleType) -> list[dict]:
    rows = []
    for app, staging, prod in STATUS_INPUTS:
        original = _with_images(ib, app, staging, prod)
        try:
            quiet_out, quiet_ret = call_capturing(lambda: ib.status(app, quiet=True))
            verbose_out, verbose_ret = call_capturing(
                lambda: ib.status(app, quiet=False)
            )
        finally:
            ib.get_deployed_images = original

        # ib.status returns True (in sync) / False (out of sync) / None
        # (indeterminate). Quiet mode distinguishes mid-deploy from the other
        # indeterminate cases by printing a trailing '*'.
        if quiet_ret is True:
            state = "in_sync"
        elif quiet_ret is False:
            state = "out_of_sync"
        elif quiet_out.strip() == f"{app}*":
            state = "mid_deploy"
        else:
            state = "unknown"

        rows.append(
            {
                "app": app,
                "stagingImages": staging,
                "prodImages": prod,
                "state": state,
                "stagingTag": _tag_line(verbose_out, "staging:"),
                "prodTag": _tag_line(verbose_out, "prod:"),
                "newProdTag": _deploy_line(verbose_out),
                "quietReturn": quiet_ret,
                "quietStdout": quiet_out,
                "verboseReturn": verbose_ret,
                "verboseStdout": verbose_out,
            }
        )
    return rows


def _tag_line(stdout: str, label: str) -> str | None:
    """The single tag ib.status printed for an environment, if it printed one."""
    for line in stdout.splitlines():
        stripped = line.strip()
        if stripped.startswith(label):
            value = stripped[len(label) :].strip()
            return None if value == "(no pods found)" or not value else value
    return None


def _deploy_line(stdout: str) -> str | None:
    marker = "This will deploy "
    for line in stdout.splitlines():
        if marker in line:
            return line.split(marker, 1)[1].removesuffix(" to prod").strip()
    return None


class _FakeCompleted:
    returncode = 0
    stdout = ""
    stderr = ""


def capture_promote(ib: types.ModuleType) -> list[dict]:
    """Drive ib.promote() with -y, intercepting the kubectl invocation.

    This is the path that decides what actually runs in production, so the
    exact argv and patch body are recorded verbatim for Task 3 to assert
    byte-equivalence against.
    """
    rows = []
    for app, staging, prod in PROMOTE_INPUTS:
        recorded: list[list[str]] = []

        def fake_run(cmd, **_kwargs):
            recorded.append(list(cmd))
            return _FakeCompleted()

        original_images = _with_images(ib, app, staging, prod)
        original_subprocess = ib.subprocess
        ib.subprocess = types.SimpleNamespace(run=fake_run)
        try:
            stdout, ret = call_capturing(lambda: ib.promote(app, yes=True))
        finally:
            ib.get_deployed_images = original_images
            ib.subprocess = original_subprocess

        argv = recorded[0] if recorded else None
        rows.append(
            {
                "app": app,
                "stagingImages": staging,
                "prodImages": prod,
                "promoted": argv is not None,
                "kubectlArgv": argv,
                "patch": argv[-1] if argv else None,
                "kustomizeImage": _kustomize_image(argv),
                "return": ret,
                "stdout": stdout,
            }
        )
    return rows


def _kustomize_image(argv: list[str] | None) -> str | None:
    if not argv:
        return None
    images = json.loads(argv[-1])["spec"]["source"]["kustomize"]["images"]
    return images[0]


def capture_image_base(ib: types.ModuleType) -> list[dict]:
    """ib.py's kustomize override key.

    There is no function to import: promote() computes it inline as
    `f"{REGISTRY}/{app}"`, from the app name alone. That expression is
    reproduced here (it is one line and cannot drift silently -- the
    promote_decision.json fixture captures the same value end-to-end through
    the real promote(), and the Go test cross-checks the two).
    """
    return [
        {
            "app": app,
            "deployedImage": image,
            "imageBaseFromAppName": f"{ib.REGISTRY}/{app}",
        }
        for app, image in IMAGE_BASE_INPUTS
    ]


# --------------------------------------------------------------------------


def write_json(path: Path, payload: object) -> None:
    path.write_text(json.dumps(payload, indent=2, ensure_ascii=False) + "\n")
    print(f"wrote {path.relative_to(REPO_ROOT)}")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ib-py", type=Path, default=DEFAULT_IB_PY)
    parser.add_argument("--out", type=Path, default=DEFAULT_OUT)
    args = parser.parse_args()

    ib_py = args.ib_py.resolve()
    if not ib_py.is_file():
        raise SystemExit(f"no ib.py at {ib_py}")
    ib = load_ib(ib_py)

    out = args.out
    out.mkdir(parents=True, exist_ok=True)

    write_json(
        out / "meta.json",
        {
            "note": (
                "Captured from the real infra/ib.py by scripts/capture-oracle.sh. "
                "ib.py is the oracle: where Go and Python disagree, Python is "
                "what production does today. Regenerate, do not hand-edit."
            ),
            "source": git_describe(ib_py),
            "python": sys.version.split()[0],
            "counts": {},
        },
    )

    captures = {
        "extract_tag.json": capture_extract_tag(ib),
        "extract_sha.json": capture_extract_sha(ib),
        "new_prod_tag_for.json": capture_new_prod_tag(ib),
        "tag_for_branch.json": capture_tag_for_branch(ib),
        "status.json": capture_status(ib),
        "promote_decision.json": capture_promote(ib),
        "image_base.json": capture_image_base(ib),
    }
    for name, rows in captures.items():
        write_json(out / name, rows)

    meta = json.loads((out / "meta.json").read_text())
    meta["counts"] = {name: len(rows) for name, rows in captures.items()}
    write_json(out / "meta.json", meta)


if __name__ == "__main__":
    main()
