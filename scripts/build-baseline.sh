#!/usr/bin/env bash
# Build the BASELINE server — the `lightwalletd-legacy` branch, yodl/lightwalletd master 187a26765e
# (zcash/lightwalletd 0.4.6 + the Ycash regex) — so the regtest suite can compare this fork's
# answers against it byte for byte (plan §6.3, the backward-compatibility gate).
#
#   scripts/build-baseline.sh [output-dir]      default: <workspace>/wt/lightwalletd-legacy-bin
#
# The tree is exported with `git archive`, never checked out, so the working tree is untouched.
# Output: <output-dir>/lightwalletd-legacy and <output-dir>/COMMIT (the exact commit built).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASE_BRANCH="${BASELINE_BRANCH:-lightwalletd-legacy}"
OUT="${1:-$(dirname "$ROOT")/wt/lightwalletd-legacy-bin}"

commit="$(git -C "$ROOT" rev-parse --short "$BASE_BRANCH")" || { echo "build-baseline: branch $BASE_BRANCH not found" >&2; exit 1; }
if [ -x "$OUT/lightwalletd-legacy" ] && [ "$(cat "$OUT/COMMIT" 2>/dev/null)" = "$commit" ]; then
  echo "build-baseline: $OUT/lightwalletd-legacy is already built from $commit"; exit 0
fi

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
git -C "$ROOT" archive --format=tar "$BASE_BRANCH" | tar -x -C "$TMP"
mkdir -p "$OUT"
( cd "$TMP" && CGO_ENABLED=0 go build -mod=vendor -o "$OUT/lightwalletd-legacy" . )
printf '%s\n' "$commit" > "$OUT/COMMIT"
echo "build-baseline: built $BASE_BRANCH ($commit) -> $OUT/lightwalletd-legacy"
