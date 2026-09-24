#!/usr/bin/env bash
# Regenerate every walletrpc/*.proto with the pinned generators and diff against the checked-in
# .pb.go and _grpc.pb.go (plan D-L-6). Exit 1 on any difference.
#
#   scripts/check-generated.sh [--update]      --update writes the regenerated files in place
#
# The pins are the versions that reproduce the baseline's own generated files exactly:
# protoc-gen-go v1.26.0 (google.golang.org/protobuf) and protoc-gen-go-grpc v1.1.0, invoked as
# the Makefile's `proto` target does (paths=source_relative). What legitimately differs between
# protoc releases is the gzipped FileDescriptorProto each .pb.go embeds and the version line in
# the header, so the comparison masks those (and comment re-wrapping); the Go API must match.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GEN_GO="v1.26.0"; GEN_GRPC="v1.1.0"
UPDATE=0
[ "${1:-}" = "--update" ] && UPDATE=1

command -v protoc >/dev/null || { echo "check-generated: protoc not found (brew install protobuf / apt install protobuf-compiler)" >&2; exit 2; }

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
GOBIN="$TMP/bin" go install "google.golang.org/protobuf/cmd/protoc-gen-go@$GEN_GO" 2>/dev/null
GOBIN="$TMP/bin" go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@$GEN_GRPC" 2>/dev/null
mkdir -p "$TMP/gen"

cd "$ROOT/walletrpc"
protos=(*.proto)
PATH="$TMP/bin:$PATH" protoc --go_out="$TMP/gen" --go_opt=paths=source_relative \
  --go-grpc_out="$TMP/gen" --go-grpc_opt=paths=source_relative "${protos[@]}"

# Mask what the protoc release changes: the header version line, the descriptor bytes, comment whitespace.
mask() { awk '
  /^\/\/[ \t]+protoc[ \t]+v/ { next }
  /^[ \t]+0x[0-9a-f][0-9a-f],/  { next }
  { sub(/^\/\/ +/, "// "); print }
' "$1"; }

status=0
for f in "$TMP"/gen/*.go; do
  name="$(basename "$f")"
  if [ ! -f "$name" ]; then
    status=1; echo "check-generated: $name is generated but not checked in" >&2
    if [ "$UPDATE" -eq 1 ]; then cp "$f" "$name"; echo "  added $name" >&2; fi
    continue
  fi
  if ! diff -u <(mask "$name") <(mask "$f") >"$TMP/$name.diff"; then
    status=1
    echo "check-generated: $name differs from what protoc-gen-go $GEN_GO / protoc-gen-go-grpc $GEN_GRPC generate:" >&2
    head -40 "$TMP/$name.diff" >&2
    if [ "$UPDATE" -eq 1 ]; then cp "$f" "$name"; echo "  updated $name" >&2; fi
  else
    echo "check-generated: $name reproduces (protoc $(protoc --version | awk '{print $2}'), protoc-gen-go $GEN_GO, protoc-gen-go-grpc $GEN_GRPC)"
  fi
done
exit $status
