#!/usr/bin/env bash
# The regtest integration run (plan section 6.3): this fork's server (with -yellowback) and the
# baseline server, both against node 0 of a ycash-dd devnet, then `go test -tags devnet`.
#
#   scripts/devnet-test.sh [--up] [--down] [--dir DIR] [--portseed N]
#
#   --up        bring the devnet up first (`yellowback-devnet up --no-attest`, ~2 min); default:
#               use the one already at DIR
#   --down      tear it down afterwards (`down --wipe`)
#   --dir       devnet directory (default ~/yb-devnet-lwd); --portseed (default 8) with --up
#
# Never mainnet: the devnet is regtest by construction. Needs go, protoc is not needed, and the
# workspace venv for the devnet script and lwd-rawmint. Exit status is go test's.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKSPACE="$(dirname "$ROOT")"
NODE_REPO="${YCASH_DD:-$WORKSPACE/ycash-dd}"
PY="${LWD_PYTHON:-$WORKSPACE/.venv/bin/python}"
DEVNET="$NODE_REPO/contrib/yellowback/devnet/yellowback-devnet"
DIR="${LWD_DEVNET_DIR:-$HOME/yb-devnet-lwd}"
PORTSEED=8; UP=0; DOWN=0
FORK_PORT="${LWD_FORK_PORT:-9067}"; BASE_PORT="${LWD_BASELINE_PORT:-9068}"
while [ $# -gt 0 ]; do
  case "$1" in
    --up) UP=1 ;; --down) DOWN=1 ;;
    --dir) DIR="$2"; shift ;; --portseed) PORTSEED="$2"; shift ;;
    -h|--help) sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "devnet-test: unknown option $1" >&2; exit 2 ;;
  esac; shift
done
[ -x "$PY" ] || { echo "devnet-test: python not found at $PY (make bootstrap)" >&2; exit 2; }
[ -x "$DEVNET" ] || { echo "devnet-test: devnet script not found at $DEVNET" >&2; exit 2; }

say() { printf '\033[1m%s\033[0m\n' "$*"; }

if [ "$UP" -eq 1 ]; then
  say "devnet up ($DIR, portseed $PORTSEED)"
  "$PY" "$DEVNET" up --no-attest --dir "$DIR" --portseed "$PORTSEED"
fi

say "baseline binary"
# yellowback-devnet looks up the baseline at <parent of ycash-dd>/wt/lightwalletd-legacy-bin.
# That is this script's default only when the two repos are siblings. CI clones lightwalletd
# under RUNNER_TEMP, so the binary has to be written where the devnet script will open it.
BASE_OUT="$(dirname "$NODE_REPO")/wt/lightwalletd-legacy-bin"
"$ROOT/scripts/build-baseline.sh" "$BASE_OUT"
say "fork binary"
FORK_BIN="$WORKSPACE/wt/lightwalletd-dd-bin/lightwalletd"
mkdir -p "$(dirname "$FORK_BIN")"
( cd "$ROOT" && CGO_ENABLED=0 go build -mod=vendor -o "$FORK_BIN" . )

say "servers: fork on $FORK_PORT (--yellowback), baseline on $BASE_PORT"
"$PY" "$DEVNET" lightwalletd stop --dir "$DIR" >/dev/null 2>&1 || true
"$PY" "$DEVNET" lightwalletd start --dir "$DIR" --port "$FORK_PORT" --bin "$FORK_BIN" --extra=--yellowback
BASE_CMD="$("$PY" "$DEVNET" lightwalletd start --baseline --port "$BASE_PORT" --print --dir "$DIR")"
# The baseline gets its own log file and data dir. A glob replace of "--data-dir *" eats the
# rest of the command (bash patterns are greedy), so append -legacy to that one argument.
BASE_CMD="${BASE_CMD/lightwalletd.log/lightwalletd-legacy.log}"
BASE_CMD="$(printf '%s\n' "$BASE_CMD" | sed -E 's|(--data-dir )([^ ]+)|\1\2-legacy|')"
mkdir -p "$(printf '%s\n' "$BASE_CMD" | sed -n 's/.*--data-dir \([^ ]*\).*/\1/p')"
pkill -f "lightwalletd-legacy .*-bind-addr 127.0.0.1:$BASE_PORT" 2>/dev/null || true
nohup $BASE_CMD >/dev/null 2>&1 &
BASE_PID=$!
cleanup() {
  kill "$BASE_PID" 2>/dev/null || true
  "$PY" "$DEVNET" lightwalletd stop --dir "$DIR" >/dev/null 2>&1 || true
  if [ "$DOWN" -eq 1 ]; then "$PY" "$DEVNET" down --wipe --dir "$DIR"; fi
}
trap cleanup EXIT
ready=0
for _ in $(seq 1 30); do
  if ( cd "$ROOT" && go run -mod=vendor ./testtools/lwdinfo -server "127.0.0.1:$BASE_PORT" -timeout 2s >/dev/null 2>&1 ); then ready=1; break; fi
  sleep 1
done
[ "$ready" -eq 1 ] || { echo "devnet-test: baseline lightwalletd did not answer on 127.0.0.1:$BASE_PORT" >&2; exit 1; }

say "fresh pool quotes (the price windows need tagged blocks with a live quote)"
"$PY" "$DEVNET" price 50 --dir "$DIR"

say "go test -tags devnet"
cd "$ROOT"
LWD_DEVNET_DIR="$DIR" LWD_FORK_ADDR="127.0.0.1:$FORK_PORT" LWD_BASELINE_ADDR="127.0.0.1:$BASE_PORT" \
LWD_RAWMINT="$NODE_REPO/contrib/yellowback/devnet/lwd-rawmint" LWD_PYTHON="$PY" \
  go test -mod=vendor -tags devnet -count=1 -v -run 'TestDevnet' ./frontend/
