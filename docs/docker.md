# Running Ycash lightwalletd in Docker

The image holds only lightwalletd (and `lwdinfo`, the probe). **There is no public Ycash node
image**, so you run `ycashd` yourself — on the host, on another machine, or in a compose project
of your own — and point the container at its RPC port. Everything here was verified on 2026-10-02
against a regtest `ycashd` on the Docker host (the transcript is in the workspace's audit notes).

## Build

```
make docker_img                      # = docker build --build-arg LWD_VERSION=$(git describe --tags) -t ycash/lightwalletd:local .
docker build --platform linux/amd64 -t ycash/lightwalletd:local .    # cross-build for an x86 server
```

Two stages ([Dockerfile](../Dockerfile)): `golang:1.24.7-alpine` compiles the vendored tree with
`CGO_ENABLED=0 go build -mod=vendor` (the same build CI runs), `alpine:3.22` runs the binary as
user `lightwalletd` (uid 2002) with its cache at `/var/lib/lightwalletd`. The image is ~60 MB.

## Configure

```
cp .env.example .env     # .env is gitignored
```

| Variable | Meaning |
|---|---|
| `YCASHD_RPC_HOST` / `_PORT` / `_USER` / `_PASSWORD` | the node, passed as `--rpchost/--rpcport/--rpcuser/--rpcpassword`. `host.docker.internal` is the Docker host. Port 8832 mainnet, 18832 testnet/regtest, or your `rpcport=`. |
| *(`YCASHD_RPC_HOST` empty)* | read a mounted `ycash.conf` instead: uncomment the `LWD_CONF_FILE` volume in `docker-compose.yml`; `docker/ycash.conf.example` shows the keys. |
| `LWD_YELLOWBACK=1` | `--yellowback`: serve the Yellowback (YED) service when the node is on the vault upgrade with a YED attestor set ([yellowback.md](yellowback.md)) |
| `LWD_YELLOWBACK_MAX_INFLIGHT`, `LWD_TRUSTED_PROXY_CIDRS` | `--yellowback-max-inflight`, `--trusted-proxy-cidr` (space-separated) |
| `LWD_INSECURE=1` | `--no-tls-very-insecure`. Regtest, or a reverse proxy that terminates TLS, only. |
| *(`LWD_INSECURE` unset)* | TLS from `/etc/lightwalletd/tls/cert.pem` and `cert.key`: uncomment the `LWD_TLS_DIR` volume. `docker/gen_cert.sh` makes a self-signed pair for a local server. |
| `LWD_BIND_HOST`, `LWD_GRPC_PORT`, `LWD_HTTP_PORT` | host side of the port mapping; `127.0.0.1` and 9067/9068 by default |
| `LWD_LOG_LEVEL`, `LWD_EXTRA_ARGS` | logrus level; anything else, verbatim |

[docker/entrypoint.sh](../docker/entrypoint.sh) turns these into flags; arguments to the container
are appended, so `docker run --rm ycash/lightwalletd:local --help` prints the full flag list.

The node needs `txindex=1`, `insightexplorer=1`, `experimentalfeatures=1` (for the stock address RPCs; the node refuses `insightexplorer` without it; Yellowback itself needs no node flag), `rpcuser`/`rpcpassword`
(lightwalletd does not read the RPC cookie), and an `rpcallowip`/`rpcbind` that admits the Docker
network — containers reach the host from the bridge network, not from 127.0.0.1.

## Run

```
docker compose up -d
docker compose logs -f                              # "Yellowback service started (node rpcversion 5, network …)"
go run -mod=vendor ./testtools/lwdinfo -server 127.0.0.1:9067 -yellowback     # plaintext only (LWD_INSECURE=1)
docker compose down                                 # add -v to drop the block cache volume
```

`docker compose` runs only the lightwalletd service, with a named volume for the block cache
and `restart: unless-stopped`. `make docker_img_run` runs the same image once in the foreground.
