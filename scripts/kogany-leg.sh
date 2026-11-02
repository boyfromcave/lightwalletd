#!/usr/bin/env bash
# The kogany leg (workspace kogany plan W9-c; report-only): this fork's server in front of a regtest
# kogany node, against the same server in front of the ycash6 sidecar holding the same chain.
#
#   scripts/kogany-leg.sh
#
#   env: ZAKURAD (kogany's zakurad, built with --features ycash: kogany's zakurad-bin artifact),
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
: "${ZAKURAD:?ZAKURAD: kogany zakurad (--features ycash)}"
: "${YCASHD:?YCASHD: ycash6 ycashd}"
BIN="${LWD_BIN_DIR:-$(mktemp -d)}"
DIR="${KOGANY_RIG_DIR:-$(mktemp -d)/rig}"
RUN="${LWD_KOGANY_RUN:-TestKoganyRig|TestDevnetMempoolStreamsSaplingOnly}"

cd "$ROOT"
CGO_ENABLED=0 go build -mod=vendor -o "$BIN/lightwalletd" .
go build -mod=vendor -o "$BIN/lwdinfo" ./testtools/lwdinfo

rig() { "$PY" scripts/kogany-rig.py --dir "$DIR" "$@"; }
trap 'rig down || true' EXIT
rig up --zakurad "$ZAKURAD" --ycashd "$YCASHD" --lightwalletd "$BIN/lightwalletd" --lightwalletd-sidecar
fork=$("$PY" -c 'import json,sys; print(json.load(open(sys.argv[1]))["lightwalletd"])' "$DIR/rig.json")
base=$("$PY" -c 'import json,sys; print(json.load(open(sys.argv[1]))["lightwalletd_sidecar"])' "$DIR/rig.json")

status=0
"$BIN/lwdinfo" -server "$fork" -timeout 10s -yellowback > "$DIR/lwdinfo-kogany.json"
"$BIN/lwdinfo" -server "$base" -timeout 10s -yellowback > "$DIR/lwdinfo-ycash6.json"
"$PY" - "$DIR/lwdinfo-kogany.json" "$DIR/lwdinfo-ycash6.json" <<'PYEOF' || status=1
import json, re, sys
# Where kogany answers otherwise, and why (kogany at the zakurad-bin pin; each one a finding of
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
