#!/usr/bin/env bash
# Regenerate testdata/oracle/ from the real infra/ib.py.
#
#   IB_PY=/path/to/ib.py scripts/capture-oracle.sh
#
# NOTE: ib.py was DELETED from the infra repo in the cutover, so the default
# path below no longer resolves and IB_PY is now required. The Python is
# recoverable from git history -- it was last present at infra commit 9db57a3:
#
#   git -C ~/Develop/ibormeith/infra show 9db57a3:ib.py > /tmp/ib.py
#   IB_PY=/tmp/ib.py scripts/capture-oracle.sh
#
# The committed fixtures under testdata/oracle/ are the frozen record of that
# behaviour and are what the tests read; this script is kept so the record
# stays reproducible rather than merely asserted.
#
# ib.py is read-only here -- it is imported, never modified and never
# reimplemented. Commit the resulting diff: a change in it is a change in what
# promotes to production.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ib_py="${IB_PY:-$HOME/Develop/ibormeith/infra/ib.py}"

# ib.py picks one image out of a set in a couple of places; pin the hash seed
# so any such path is at least reproducible run to run.
export PYTHONHASHSEED=0

exec python3 "$repo_root/scripts/capture_oracle.py" \
  --ib-py "$ib_py" \
  --out "$repo_root/testdata/oracle" \
  "$@"
