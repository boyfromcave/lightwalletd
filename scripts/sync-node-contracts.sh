#!/usr/bin/env bash
# Copyright (c) 2026 The Ycash developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or https://www.opensource.org/licenses/mit-license.php .
#
# The node contracts this server is tested against are copies of ycash-dd's generated files
# (audit 06/L-1). Re-run this whenever the node's contracts change (a node PR that touches
# doc/vault-rpc.md, doc/vault-rpc-contract.json or doc/yellowback-rpc-contract.json), then
# `go test -mod=vendor ./frontend/ -run 'Vault|ContractFields|AllowList'` and commit the copies.
#
#   scripts/sync-node-contracts.sh [--check] [--tree DIR] [--ref REF]
#   scripts/sync-node-contracts.sh [--check] [--required] --github OWNER/REPO BRANCH
#
#   --tree DIR   a local ycash-dd clone (default: ../ycash-dd beside this repo, or the workspace's
#                ycash-dd when this is a worktree under wt/); --ref REF the commit to copy from
#                (default origin/upgrade/vault; run `git -C DIR fetch origin` first)
#   --github     fetch from raw.githubusercontent.com/OWNER/REPO/BRANCH instead (CI)
#   --check      compare only, write nothing; exit 1 on any difference
#   --required   with --github: an unreachable file is an error (exit 1), not a notice and a skip
#
# The copies, as <node path> -> <fixture>:
#   doc/yellowback-rpc-contract.json -> testdata/yellowback/contract.json
#   doc/vault-rpc-contract.json      -> testdata/vault/vault-rpc-contract.json
#   doc/vault-rpc.md                 -> testdata/vault/vault-rpc.md   (the contract's sha256 stamp)
# ycash-dd is the source: ycash6's contracts differ from it only in the embedded sha256 stamp.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PAIRS=(
  "doc/yellowback-rpc-contract.json:testdata/yellowback/contract.json"
  "doc/vault-rpc-contract.json:testdata/vault/vault-rpc-contract.json"
  "doc/vault-rpc.md:testdata/vault/vault-rpc.md"
)
check=0 required=0 tree="" ref="origin/upgrade/vault" gh_repo="" gh_branch=""
while [ $# -gt 0 ]; do
  case "$1" in
    --check) check=1 ;;
    --required) required=1 ;;
    --tree) tree=$2; shift ;;
    --ref) ref=$2; shift ;;
    --github) gh_repo=$2; gh_branch=$3; shift 2 ;;
    -h|--help) sed -n '5,26p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done
if [ -z "$gh_repo" ] && [ -z "$tree" ]; then
  for d in "$ROOT/../ycash-dd" "$ROOT/../../ycash-dd"; do
    if git -C "$d" rev-parse --git-dir >/dev/null 2>&1; then tree=$(cd "$d" && pwd); break; fi
  done
  [ -n "$tree" ] || { echo "no ycash-dd clone found beside $ROOT; pass --tree DIR or --github" >&2; exit 2; }
fi

tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
status=0
for pair in "${PAIRS[@]}"; do
  remote=${pair%%:*}; local=${pair#*:}; got="$tmp/$(basename "$remote")"
  if [ -n "$gh_repo" ]; then
    src="$gh_repo@$gh_branch $remote"
    if ! curl -fsSL -o "$got" "https://raw.githubusercontent.com/$gh_repo/$gh_branch/$remote"; then
      if [ "$required" = 1 ]; then
        echo "::error::$src unreachable; the fixture check cannot run"; exit 1
      fi
      echo "::notice::$gh_repo has no $gh_branch (or no $remote) yet; fixture comparison skipped"; continue
    fi
  else
    src="$tree@$(git -C "$tree" rev-parse --short "$ref") $remote"
    git -C "$tree" show "$ref:$remote" > "$got"
  fi
  if cmp -s "$ROOT/$local" "$got"; then
    echo "$local = $src"
  elif [ "$check" = 1 ]; then
    echo "::error::$local differs from $src; run scripts/sync-node-contracts.sh and commit the copy"
    status=1
  else
    cp "$got" "$ROOT/$local"; echo "$local <- $src (updated)"
  fi
done
exit $status
