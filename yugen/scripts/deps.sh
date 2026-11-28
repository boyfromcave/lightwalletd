#!/bin/sh
# Checks out the Yugen repositories the host builds against, at the commits in PINS, into
# yugen/deps/ (git-ignored). Source: $YUGEN_ROOT/<name> if YUGEN_ROOT is set (local clones),
# else $YUGEN_GIT_BASE/<name>.git (default https://github.com/boyfromcave). Re-run after a pin
# change; refuses a dirty checkout.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
base=${YUGEN_GIT_BASE:-https://github.com/boyfromcave}
mkdir -p "$here/deps"
while read -r name commit; do
	case $name in ''|'#'*) continue ;; esac
	d=$here/deps/$name
	if [ -n "${YUGEN_ROOT:-}" ]; then src=$YUGEN_ROOT/$name; else src=$base/$name.git; fi
	[ -d "$d/.git" ] || git clone -q --no-hardlinks --no-checkout "$src" "$d"
	if ! git -C "$d" cat-file -e "$commit^{commit}" 2>/dev/null; then git -C "$d" fetch -q "$src"; fi
	git -C "$d" -c advice.detachedHead=false checkout -q "$commit"
	[ -z "$(git -C "$d" status --porcelain)" ] || { echo "deps/$name is dirty" >&2; exit 1; }
	echo "$name $(git -C "$d" rev-parse --short HEAD)"
done < "$here/PINS"
