#!/bin/bash
# Self-signed certificate for a LOCAL lightwalletd (regtest, a laptop). Clients reject
# self-signed certificates unless they are given the certificate out of band; a public server
# needs a certificate from a real CA (README.md, "TLS"). Writes cert.key and cert.pem into
# ./docker/tls, which docker-compose.yml mounts at /etc/lightwalletd/tls.
#
#   docker/gen_cert.sh [hostname]          default: lightwalletd.local
set -eu
HOST="${1:-lightwalletd.local}"
OUT="$(cd "$(dirname "$0")" && pwd)/tls"
mkdir -p "$OUT"
openssl req -x509 -nodes -newkey rsa:2048 -days 3650 \
  -keyout "$OUT/cert.key" -out "$OUT/cert.pem" \
  -subj "/O=Ycash lightwalletd/CN=$HOST" \
  -addext "subjectAltName=DNS:$HOST,DNS:localhost,IP:127.0.0.1"
chmod 600 "$OUT/cert.key"
echo "wrote $OUT/cert.pem and $OUT/cert.key (CN=$HOST)"
