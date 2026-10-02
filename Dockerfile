# Copyright (c) 2026 The Ycash developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or https://www.opensource.org/licenses/mit-license.php .
#
# Ycash lightwalletd. Two stages: the Go toolchain CI uses (go.mod's `go 1.12` is a language
# floor, not a toolchain; .github/workflows/yellowback-tests.yml builds with 1.24.7) compiles
# the vendored tree statically, and a small Alpine image runs it as an unprivileged user.
# The node is NOT in this image: run ycashd yourself and point the server at its RPC port
# (docs/docker.md). Build: `make docker_img` (or `docker build -t ycash/lightwalletd .`).

FROM golang:1.24.7-alpine3.22 AS builder
ARG LWD_VERSION=docker
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath \
      -ldflags "-s -w -X github.com/zcash/lightwalletd/common.Version=${LWD_VERSION} -X github.com/zcash/lightwalletd/common.BuildDate=docker" \
      -o /out/lightwalletd . \
 && CGO_ENABLED=0 go build -mod=vendor -trimpath -o /out/lwdinfo ./testtools/lwdinfo

FROM alpine:3.22
ARG LWD_USER=lightwalletd
ARG LWD_UID=2002
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g ${LWD_UID} ${LWD_USER} \
 && adduser -D -u ${LWD_UID} -G ${LWD_USER} -h /srv/${LWD_USER} ${LWD_USER} \
 && mkdir -p /var/lib/lightwalletd /etc/lightwalletd \
 && chown ${LWD_UID}:${LWD_UID} /var/lib/lightwalletd
COPY --from=builder /out/lightwalletd /out/lwdinfo /usr/local/bin/
COPY docker/entrypoint.sh /usr/local/bin/lightwalletd-entrypoint
USER ${LWD_USER}
WORKDIR /srv/${LWD_USER}
VOLUME ["/var/lib/lightwalletd"]
EXPOSE 9067 9068
ENTRYPOINT ["lightwalletd-entrypoint"]
CMD []
