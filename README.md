# Ycash lightwalletd

[![yellowback-tests](https://github.com/boyfromcave/lightwalletd/actions/workflows/yellowback-tests.yml/badge.svg?branch=upgrade/vault)](https://github.com/boyfromcave/lightwalletd/actions/workflows/yellowback-tests.yml)

lightwalletd is a backend service that gives light wallets a bandwidth-efficient, Sapling-era
interface to the Ycash blockchain: compact blocks, transparent-address lookups, transaction
broadcast. This tree is the Ycash lineage — [zcash/lightwalletd](https://github.com/zcash/lightwalletd)
0.4.6 with the Ycash transparent-address regex (`s…`), as maintained at
[yodl/lightwalletd](https://github.com/yodl/lightwalletd) — plus one addition, described next.

## Ycash Yellowback (YED) on this branch

This branch (`upgrade/vault`) serves light wallets for a proposed Ycash network upgrade, **the vault
upgrade**, which adds **vaults** to Ycash: YEC locked on chain under rules every node enforces,
released only with the approval of a bonded **signer set**. **Ycash Yellowback (YED)** is a dollar
token built on vaults: lock YEC in a vault to mint YED (`1 YED = 1 US dollar`), return the YED to
get the YEC back.

- **What the server adds:** with `--yellowback`, a second gRPC service, `YellowbackStreamer`
  ([walletrpc/yellowback.proto](walletrpc/yellowback.proto)). Its 24 methods only *read*: YED
  prices, balances, vaults and attestations, the consensus branch ID a wallet must sign for, and the
  vaults and signer sets of the upgrade. Each is a thin proxy of one read-only node RPC. Compact
  blocks and the standard `CompactTxStreamer` service are unchanged, and without the flag the
  binary behaves exactly like upstream. Clients: the YEW mobile wallet and desktop light wallets.
- **Node it needs:** a ycashd built from the `upgrade/vault` branch of
  [ycash-dd](https://github.com/boyfromcave/ycash-dd) or
  [ycash6](https://github.com/boyfromcave/ycash6). Against a stock ycashd the service simply does
  not start.
- **Status: proposed, not live.** It runs on a local test network (regtest) only. It has not been
  adopted by the Ycash Foundation, has not been audited, and has no activation height on mainnet
  or testnet.
- **Try it:** with a built ycash-dd checkout beside this one (or `YCASH_DD=<path>`),
  `scripts/devnet-test.sh --up --down` brings up the node's local test network, starts this server
  against it and runs the regtest suite (`--help` lists the options; the Python it needs is in
  the script header). [docs/yellowback.md](docs/yellowback.md) has the operator and developer notes.

# Security disclaimer

From the upstream project, and as true here: lightwalletd is under active development, some
features are more stable than others. The code has not been subjected to a thorough review by an
external auditor. Developers should familiarize themselves with the
[wallet app threat model](https://zcash.readthedocs.io/en/latest/rtd_pages/wallet_threat_model.html),
since it contains important information about the security and privacy limitations of light
wallets that use lightwalletd. The Yellowback service has had an internal review, not an
external audit.

# Building

[Go](https://go.dev/dl/) 1.24 or later (CI builds with 1.24.7; `go.mod`'s `go 1.12` is the
language floor). Dependencies are vendored, so no module download is needed:

```
make                                   # go build with version ldflags → ./lightwalletd
CGO_ENABLED=0 go build -mod=vendor .   # the same build, static, as CI and the Dockerfile do it
```

`lightwalletd --help` lists every flag. The gRPC interface is documented in
[docs/rtd/index.html](docs/rtd/index.html), generated from the four `walletrpc/*.proto` files by
`make doc` (needs Docker; it runs the `pseudomuto/protoc-gen-doc` image). Regenerate it when a
proto changes.

# Running against ycashd

The node is `ycashd` (Ycash 4.5.0 or later; for the Yellowback service, the `upgrade/vault` build
above). Its `ycash.conf` must contain:

```
txindex=1
insightexplorer=1
experimentalfeatures=1   # required by insightexplorer; nothing Yellowback-related
rpcuser=…
rpcpassword=…
rpcport=8832          # 18832 on testnet and regtest
```

`txindex` and `insightexplorer` take effect only after a one-time `ycashd -reindex` on an existing
datadir (hours, and more disk). lightwalletd has no RPC-cookie support, so `rpcuser`/`rpcpassword`
are required. Without `rpcport` the server assumes Ycash's defaults, 8832 on mainnet and 18832 on
testnet and regtest. The node is reached with `getinfo`, `getblockchaininfo`,
`getblock`, `getrawtransaction`, `getrawmempool`, `getaddresstxids`, `getaddressbalance`,
`getaddressutxos`, `sendrawtransaction`, `z_gettreestate`, and, with `--yellowback`,
`yed_getinfo`, the read-only `yed_*` methods listed in `common.YedMethods` and the vault
primitive's read-only `vault_getinfo`, `set_list`, `set_getinfo`, `vault_list` (`common.VaultMethods`).

Credentials come from the conf file or from flags:

```
./lightwalletd --zcash-conf-path /path/to/ycash.conf --data-dir /var/lib/lightwalletd --yellowback
./lightwalletd --rpchost 127.0.0.1 --rpcport 18832 --rpcuser … --rpcpassword … …   # the four flags bypass the file
```

The flag is still called `--zcash-conf-path`; it is inherited and the file it reads is ycashd's.
Prefer it on a real deployment — a password on the command line is visible to every local user.
If you restart the node on a different network, restart lightwalletd too.

**Yellowback.** `--yellowback` (or `YELLOWBACK=1`, or `yellowback: true` in the config file).
At startup the server asks the node for `yed_getinfo` and registers the service only when the node
speaks Yellowback `rpcversion 5` (a node without the yed_* commands answers "Method not found": no
service, no error); the log says `Yellowback service started (node rpcversion 5, network …)` or
why not. Two operator flags
belong to it: `--yellowback-max-inflight N` (node calls in flight at once, default 16) and
`--trusted-proxy-cidr NET` (repeatable; the per-peer rate limiter believes `x-real-ip` /
`x-forwarded-for` only from these networks — set it when, and only when, a reverse proxy sets
them). Upgrade order: node first, then the server binary, then the flag.

**TLS.** Clients expect TLS: pass `--tls-cert` and `--tls-key`. Use a certificate from a real CA
with a modern signature algorithm; wallets reject self-signed certificates unless they were given
them out of band. A free option is Let's Encrypt (`certbot certonly --standalone -d your.host`),
passing the resulting `fullchain.pem`/`privkey.pem`. `--no-tls-very-insecure` is for regtest and
for a reverse proxy that terminates TLS in front of a loopback bind; never for a public port.
`--gen-cert-very-insecure` makes a throwaway self-signed certificate for the same cases.

**Block cache.** lightwalletd caches every block from Sapling activation to the tip under
`--data-dir` (default `/var/lib/lightwalletd`). The first fill takes a while on mainnet; the server
answers meanwhile. It checks the files at startup, logs `CORRUPTION` and re-downloads from that
height if they are damaged. `--redownload` discards the cache.

Other flags: `--grpc-bind-addr` (default `127.0.0.1:9067`), `--http-bind-addr` (`127.0.0.1:9068`),
`--log-file` (`./server.log`; `/dev/stdout` for a supervisor), `--log-level` (logrus 1–7),
`--config lightwalletd.yml` ([lightwalletd-example.yml](lightwalletd-example.yml) names every key).

# Docker

`make docker_img` builds `ycash/lightwalletd:local` from this tree (static Go build in
`golang:1.24.7-alpine`, `alpine:3.22` runtime, unprivileged uid 2002, cache at
`/var/lib/lightwalletd`). `docker-compose.yml` runs that one service against a `ycashd` you run
elsewhere — there is no public Ycash node image — configured by `.env` (copy
[.env.example](.env.example)): `YCASHD_RPC_HOST/PORT/USER/PASSWORD` or a mounted `ycash.conf`,
`LWD_YELLOWBACK=1`, a mounted certificate or `LWD_INSECURE=1`, ports bound to 127.0.0.1.
[docs/docker.md](docs/docker.md) has the details and the verification.

# Testing

```
go build -mod=vendor ./... && go vet -mod=vendor ./cmd/... ./common/... ./frontend/... ./walletrpc/...
go test -mod=vendor ./cmd/... ./common/... ./parser/... ./walletrpc/...
go test -mod=vendor -count=1 -timeout 120s -run 'Unary|Streaming|Params|NodeErrors|InputValidation|AllowList|Probe|YedTo|TaddrOf|RateLimit|CallYed|ListVaults|TipAnswers' ./frontend/
scripts/check-generated.sh         # the committed .pb.go files match the protos (pinned generators)
```

The baseline's own `frontend` tests fail and hang at the `187a267` pin (the Ycash address regex
invalidated upstream's test data, [docs/yellowback.md](docs/yellowback.md) "Known baseline
defects"); CI runs the Yellowback frontend tests by name and every other package in full.
Against a real node: the regtest devnet of the Yellowback node repository
(`ycash-dd/contrib/yellowback/devnet/yellowback-devnet`) and `scripts/devnet-test.sh`, which
proves byte-equality of every compact block with the baseline binary and a mint seen through the
server — see [docs/yellowback.md](docs/yellowback.md). Never test against mainnet.
[docs/darksidewalletd.md](docs/darksidewalletd.md) describes the mock-node mode for client
integration tests. CI is [.github/workflows/yellowback-tests.yml](.github/workflows/yellowback-tests.yml):
build, vet, gofmt, the tests above, the generated-code check, and a `docker build` of the image.

# Pull requests

Keep Go code `gofmt`-formatted; a pre-commit hook that refuses unformatted files:

```
#!/bin/sh
modified_go_files=$(git diff --cached --name-only -- '*.go')
if test "$modified_go_files"; then
    need_formatting=$(gofmt -l $modified_go_files)
    if test "$need_formatting"; then
        echo "files need formatting (then don't forget to git add):"
        echo gofmt -w $need_formatting
        exit 1
    fi
fi
```

(`chmod +x .git/hooks/pre-commit`.) Changes under `walletrpc/` must keep the three baseline
protos and their generated code at zero delta; the Yellowback contract lives in
`walletrpc/yellowback.proto`.

# License

MIT, see [COPYING](COPYING) and [LICENSE](LICENSE): Copyright (c) 2019 Electric Coin Company,
(c) 2020 The Zcash developers, (c) 2026 The Ycash developers.
