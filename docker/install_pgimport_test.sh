#!/usr/bin/env bash
# Regression guard for the install.sh --postgres pgimport ENTRYPOINT bug.
#
# Both controller images set ENTRYPOINT ["/app/controller"]. install.sh must
# therefore run pgimport with `compose run --entrypoint /app/pgimport`, NOT
# `compose run ... controller /app/pgimport ...` (which hands pgimport to the
# controller as argv and boots a rogue server against the empty PG instead of
# importing, and never exits). Found during the v1.23.0 farm SQLite->PG cutover.
set -euo pipefail
cd "$(dirname "$0")/.."
f=docker/install.sh

# 1. Both pgimport compose-run invocations override the entrypoint.
n=$(grep -c 'compose -f docker-compose.runtime.yml run --rm --entrypoint /app/pgimport' "$f")
if [ "$n" -ne 2 ]; then
  echo "FAIL: expected 2 pgimport runs with --entrypoint /app/pgimport, found $n" >&2
  exit 1
fi

# 2. pgimport must never be passed as argv to the controller entrypoint.
#    Ignore comment lines (the fix's explanatory comment quotes the buggy form).
if grep -v '^[[:space:]]*#' "$f" | grep -q 'controller /app/pgimport'; then
  echo "FAIL: pgimport passed as argv to the controller (ENTRYPOINT bug regressed)" >&2
  exit 1
fi

echo "OK: install.sh runs pgimport via --entrypoint (2 invocations, no argv form)"