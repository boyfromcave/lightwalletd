#!/bin/sh
# Copyright (c) 2026 The Ycash developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or https://www.opensource.org/licenses/mit-license.php .
#
# Container entrypoint: turns the environment (.env.example documents every variable) into
# lightwalletd flags, then execs the server. Any arguments given to the container are appended,
# so `docker run <image> --help` still prints the flag list, and a flag given explicitly wins
# over the one the environment derived (cobra takes the last occurrence).
set -eu

args=""
add() { args="$args $1"; }

# The node. Either the four RPC variables (they bypass the conf file entirely) or a mounted
# ycash.conf, read through the inherited --zcash-conf-path flag. lightwalletd has no cookie
# support: the node needs rpcuser/rpcpassword.
if [ -n "${YCASHD_RPC_HOST:-}" ]; then
  add "--rpchost ${YCASHD_RPC_HOST}"
  add "--rpcport ${YCASHD_RPC_PORT:-8832}"
  add "--rpcuser ${YCASHD_RPC_USER:?YCASHD_RPC_USER is required with YCASHD_RPC_HOST}"
  add "--rpcpassword ${YCASHD_RPC_PASSWORD:?YCASHD_RPC_PASSWORD is required with YCASHD_RPC_HOST}"
else
  add "--zcash-conf-path ${LWD_CONF_PATH:-/etc/lightwalletd/ycash.conf}"
fi

# TLS: a mounted certificate and key, or no TLS at all when the operator says so (regtest and
# a reverse proxy that terminates TLS itself are the only reasons).
if [ "${LWD_INSECURE:-0}" = "1" ]; then
  add "--no-tls-very-insecure"
else
  add "--tls-cert ${LWD_TLS_CERT:-/etc/lightwalletd/tls/cert.pem}"
  add "--tls-key ${LWD_TLS_KEY:-/etc/lightwalletd/tls/cert.key}"
fi

[ "${LWD_YELLOWBACK:-0}" = "1" ] && add "--yellowback"
[ -n "${LWD_YELLOWBACK_MAX_INFLIGHT:-}" ] && add "--yellowback-max-inflight ${LWD_YELLOWBACK_MAX_INFLIGHT}"
for cidr in ${LWD_TRUSTED_PROXY_CIDRS:-}; do add "--trusted-proxy-cidr $cidr"; done

add "--grpc-bind-addr 0.0.0.0:${LWD_GRPC_PORT_IN:-9067}"
add "--http-bind-addr 0.0.0.0:${LWD_HTTP_PORT_IN:-9068}"
add "--data-dir ${LWD_DATA_DIR:-/var/lib/lightwalletd}"
add "--log-file ${LWD_LOG_FILE:-/dev/stdout}"
add "--log-level ${LWD_LOG_LEVEL:-4}"
[ -n "${LWD_EXTRA_ARGS:-}" ] && add "${LWD_EXTRA_ARGS}"

# shellcheck disable=SC2086
exec lightwalletd $args "$@"
