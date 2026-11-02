#!/usr/bin/env bash
# The kogany leg (workspace kogany plan W9-c; report-only): this fork's server in front of a regtest
# kogany node, against the same server in front of the ycash6 sidecar holding the same chain.
#
#   scripts/kogany-leg.sh [--pruned]
#
# --pruned (workspace pruning assessment KP-3, report-only): kogany runs Zakura's Pruned storage mode
# (tx_retention LWD_PRUNE_RETENTION, default 1001, the regtest floor) and the rig mines past the
# window; then the server that ingested the chain before it was pruned still serves it, and a server
# started from scratch on the pruned backend fails cleanly: GetBlockRange / GetBlock below
# pruneheight answer kogany's "Block not available (pruned data)", and its ingestor exits non-zero
# with that error (no hang, no panic). The deployment this leg argues for is in README.md
# ("Backends: archive, not pruned").
#
#   env: KOGANYD (kogany's koganyd, built with --features ycash: kogany's koganyd-bin artifact),
#        YCASHD (ycash6's ycashd: ycash6's node-bin artifact), LWD_BIN_DIR (where lightwalletd and
#        lwdinfo are built; default a temporary directory), KOGANY_RIG_DIR (default a temporary
#        directory), PYTHON (default python3), LWD_KOGANY_RUN (go test -run; default the kogany
#        tests and the mempool test, the devnet tests that hold on any backend)
#
# Steps: build the server and its probe; scripts/kogany-rig.py up (kogany, the ycash6 sidecar
# `connect=` kogany mining past the Vault activation, one server on each); lwdinfo -yellowback on
# both servers, compared method by method (KNOWN_GAPS below are reported, anything else fails);
# `go test -tags devnet` (frontend/kogany_rig_test.go: GetLightdInfo, GetBlockRange byte for byte,
# GetTreeState, the transparent views, SendTransaction through kogany); the rig down. Exit 0 when
# every check passes.
set -euo pipefail
# Copyright (c) 2026 The Ycash developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or https://www.opensource.org/licenses/mit-license.php .

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PY="${PYTHON:-python3}"
# ZAKURAD: the variable's name before kogany's daemon became koganyd (kogany dd834da8b).
KOGANYD="${KOGANYD:-${ZAKURAD:-}}"
: "${KOGANYD:?KOGANYD: kogany koganyd (--features ycash)}"
: "${YCASHD:?YCASHD: ycash6 ycashd}"
BIN="${LWD_BIN_DIR:-$(mktemp -d)}"
DIR="${KOGANY_RIG_DIR:-$(mktemp -d)/rig}"
RUN="${LWD_KOGANY_RUN:-TestKoganyRig|TestDevnetMempoolStreamsSaplingOnly}"

PRUNED=0
if [ "${1:-}" = "--pruned" ]; then
  PRUNED=1
fi

cd "$ROOT"
CGO_ENABLED=0 go build -mod=vendor -o "$BIN/lightwalletd" .
go build -mod=vendor -o "$BIN/lwdinfo" ./testtools/lwdinfo

rig() { "$PY" scripts/kogany-rig.py --dir "$DIR" "$@"; }
trap 'rig down || true' EXIT
if [ "$PRUNED" = 1 ]; then
  RETENTION="${LWD_PRUNE_RETENTION:-1001}"
  # compiled first: the fresh server below gives the test about 100 s before its ingestor exits
  go test -mod=vendor -tags devnet -c -o "$BIN/frontend.test" ./frontend/
  rig up --koganyd "$KOGANYD" --ycashd "$YCASHD" --lightwalletd "$BIN/lightwalletd" --lightwalletd-sidecar \
    --pruned "$RETENTION"
  # past the window: kogany's finalized tip trails its tip by up to 1,000 blocks, and the window
  # counts from the finalized tip
  rig mine $((RETENTION + 1000 + 200))
  fork=$("$PY" -c 'import json,sys; print(json.load(open(sys.argv[1]))["lightwalletd"])' "$DIR/rig.json")
  base=$("$PY" -c 'import json,sys; print(json.load(open(sys.argv[1]))["lightwalletd_sidecar"])' "$DIR/rig.json")
  fresh_json=$(rig lightwalletd --lightwalletd "$BIN/lightwalletd" --name fresh)
  fresh=$("$PY" -c 'import json,sys; print(json.loads(sys.argv[1])["addr"])' "$fresh_json")
  fresh_pid=$("$PY" -c 'import json,sys; print(json.loads(sys.argv[1])["pid"])' "$fresh_json")
  status=0
  LWD_KOGANY=1 LWD_KOGANY_PRUNED=1 LWD_DEVNET_DIR="$DIR" LWD_FORK_ADDR="$fork" LWD_BASELINE_ADDR="$base" \
    LWD_PRUNED_FRESH_ADDR="$fresh" \
    sh -c 'cd frontend && exec "$0" -test.count=1 -test.timeout=900s -test.v -test.run TestKoganyRigPruned' \
    "$BIN/frontend.test" || status=1
  # The fresh server's ingestor: kogany refuses the pruned heights with -32603, which the ingestor
  # retries (10 s apart) and then exits on: a clean exit with the error in its log, not a wait for a
  # block it reads as "not mined yet" (Zakura's own -8 "Block not found" would be that hang).
  for _ in $(seq 1 60); do
    kill -0 "$fresh_pid" 2>/dev/null || break
    sleep 5
  done
  log="$DIR/lightwalletd-fresh.log"
  out="$DIR/lightwalletd-fresh.out"
  if kill -0 "$fresh_pid" 2>/dev/null; then
    echo "FAIL the fresh lightwalletd on the pruned kogany is still running after 300 s (a hang)"
    status=1
  elif ! grep -q 'Block not available (pruned data)' "$log" "$out" 2>/dev/null; then
    echo "FAIL the fresh lightwalletd exited without kogany's pruned refusal in its log ($log)"
    status=1
  elif grep -q '^panic:\|^goroutine ' "$out" 2>/dev/null; then
    echo "FAIL the fresh lightwalletd panicked ($out)"
    status=1
  else
    echo "pruned: the fresh lightwalletd exited cleanly on kogany's pruned refusal:"
    grep -h 'pruned data' "$log" "$out" 2>/dev/null | tail -2
  fi
  exit $status
fi
rig up --koganyd "$KOGANYD" --ycashd "$YCASHD" --lightwalletd "$BIN/lightwalletd" --lightwalletd-sidecar
fork=$("$PY" -c 'import json,sys; print(json.load(open(sys.argv[1]))["lightwalletd"])' "$DIR/rig.json")
base=$("$PY" -c 'import json,sys; print(json.load(open(sys.argv[1]))["lightwalletd_sidecar"])' "$DIR/rig.json")

status=0
"$BIN/lwdinfo" -server "$fork" -timeout 10s -yellowback > "$DIR/lwdinfo-kogany.json"
"$BIN/lwdinfo" -server "$base" -timeout 10s -yellowback > "$DIR/lwdinfo-ycash6.json"
"$PY" - "$DIR/lwdinfo-kogany.json" "$DIR/lwdinfo-ycash6.json" <<'PYEOF' || status=1
import json, re, sys
# Where kogany answers otherwise, and why (kogany at the koganyd-bin pin; each one a finding of
# plan W9-c). A method listed here may differ; any other difference fails the leg.
KNOWN_GAPS = {
    'EstimateCollateral': 'yed_estimatecollateral is not served by kogany yet (plan W7-b)',
    'GetPrice': 'yed_gettag is not served by kogany yet (plan W7-b)',
    'GetNotice': 'yed_getnotice of an unknown vault: kogany answers, ycash6 refuses vault-not-found',
}
k, y = (json.load(open(p)) for p in sys.argv[1:3])
fail = []
for key in ('blockHeight', 'chainName', 'consensusBranchId', 'saplingActivationHeight', 'taddrSupport'):
    if k.get(key) != y.get(key):
        fail.append('%s: kogany %r, ycash6 %r' % (key, k.get(key), y.get(key)))
kyb, yyb = k.get('yellowback', {}), y.get('yellowback', {})
methods = sorted({m.split('#')[0] for m in list(kyb) + list(yyb)})
same = 0
for m in methods:
    kv = {x: v for x, v in kyb.items() if x.split('#')[0] == m}
    yv = {x: v for x, v in yyb.items() if x.split('#')[0] == m}
    if kv == yv:
        same += 1
    elif m in KNOWN_GAPS:
        print('KOGANY-FINDING %s: %s (kogany %s / ycash6 %s)' % (m, KNOWN_GAPS[m], kv.get(m), yv.get(m)))
    else:
        fail.append('%s: kogany %s, ycash6 %s' % (m, kv, yv))
print('lwdinfo -yellowback: %d of %d YellowbackStreamer methods answer identically on kogany and ycash6, %d known gaps'
      % (same, len(methods), len([m for m in methods if m in KNOWN_GAPS])))
for f in fail:
    print('FAIL ' + f)
sys.exit(1 if fail else 0)
PYEOF

LWD_KOGANY=1 LWD_DEVNET_DIR="$DIR" LWD_FORK_ADDR="$fork" LWD_BASELINE_ADDR="$base" \
  go test -mod=vendor -tags devnet -count=1 -timeout 900s -v ./frontend/ -run "$RUN" || status=1
exit $status
