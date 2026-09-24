# lightwalletd-dd — Yellowback (YED) notes

The working fork of Ycash's lightwalletd for Yellowback. The plan is the workspace's
`docs/plans/yellowback-lightwalletd-plan.md`; this file is the operator and developer record.
**Baseline:** branch `lightwalletd-legacy` = `yodl/lightwalletd` `master` `187a26765e`
(2021-07-13: `zcash/lightwalletd` 0.4.6 plus four commits, the last adapting the transparent
address regex to Ycash's `s…`). Work branch: `feature/yellowback-price-attest`. The service was
first built and proven on an earlier, different lineage (yecdev's Zecwallet-derived server) and
re-ported here on 2026-09-24 (plan Phase R0, §10); the design and its evidence are the same.

## What it adds

A second gRPC service, `cash.z.wallet.sdk.rpc.YellowbackStreamer` (`walletrpc/yellowback.proto`),
beside `CompactTxStreamer`. Nineteen methods, each a thin, allow-listed proxy of one read-only
`yed_*` RPC on the node this server already talks to: verdicts (`GetTxInfo`,
`ValidateRawTransaction`), price and collateral (`GetPrice`, `GetStats`, `EstimateCollateral`,
`EstimateFee`, `GetFeePayee`), vaults (`GetVault`, `ListVaults`, `ListClaimable`, `GetNotice`),
attestation (`BuildBundle`, `GetSelection`, `GetAttestations`, `ListAttestors`), the YED UTXO set
of any address (`GetAddressTokens` → the node's `yed_listtokens`), `GetActivation`,
`DecodePayload`, `GetYellowbackInfo`. Field names are the node contract's JSON names
(`testdata/yellowback/contract.json`, kept equal to `ycash-dd/doc/yellowback-rpc-contract.json`
by the workspace's `make spec`), so results unmarshal straight into the generated messages.

**Switch:** `--yellowback` (cobra/viper, so also `YELLOWBACK=1` or the config file). At startup,
after `GetLightdInfo`, the server calls `getexperimentalfeatures` and, when `"yellowback"` is
listed, `yed_getinfo`; it registers the service only when the node speaks `rpcversion 3`. Off,
or on a stock node, the binary is the baseline in every observable way. With the flag on, the
taddr RPCs (`GetTaddressTxids`, `GetTaddressBalance`, `GetAddressUtxos`) also accept YED
addresses (`ye…`/`yt…`/`yr…`), mapped to the transparent form before the baseline's
`checkTaddress` (plan D-L-4; `frontend/yellowback_addr.go`, `taddrOf` in `frontend/service.go`).

**The node is reached through `common.RawRequest`**, the tree's own function variable, which
`main` assigns from the btcd client and tests replace with a stub — no new abstraction.
`common.CallYed` is the only path to a `yed_*` RPC; `common.YedMethods` (allow-list) and
`common.NotOffered` (with reasons) together cover every `yed_*` method of the contract, and the
offline suite fails when one appears in neither.

**Errors:** a node RPC error → `FAILED_PRECONDITION` with the node's message verbatim
(`bundle-insufficient: …`, `vault-not-found: …`); transport failure → `UNAVAILABLE`; malformed
input → `INVALID_ARGUMENT` before any node call; a peer past its burst on the four node-work
methods (`EstimateCollateral`, `BuildBundle`, `ValidateRawTransaction`, `GetAddressTokens`) →
`RESOURCE_EXHAUSTED` (20 calls, then one per second, per `x-real-ip` or connection address;
`frontend/yellowback_ratelimit.go`, no new dependency).

## Toolchain and generated code

| Tool | Version | Notes |
|---|---|---|
| Go | 1.27.1 here; `go.mod` says `go 1.12` | builds and tests run `-mod=vendor`, as the Makefile does |
| protoc | 36.1 (Homebrew) | to regenerate `walletrpc/*.pb.go` |
| protoc-gen-go | **v1.26.0** | the baseline's own (header of every `.pb.go`); `scripts/check-generated.sh` pins it |
| protoc-gen-go-grpc | **v1.1.0** | reproduces the baseline's `*_grpc.pb.go` exactly |

`make proto` regenerates all four protos (`yellowback.proto` is in `GENERATED_FILES`, `proto`,
`update-grpc`, `doc` and `simpledoc`). `scripts/check-generated.sh` regenerates and diffs every
`.pb.go`/`_grpc.pb.go`, masking the header version line, the gzipped descriptor bytes (they
change with the protoc release) and comment re-wrapping; CI runs it.

## Files

| File | What |
|---|---|
| `walletrpc/yellowback.proto` (+ `yellowback.pb.go`, `yellowback_grpc.pb.go`) | the service |
| `common/yellowback.go` | `ProbeYellowback`, `CallYed`, `YedMethods`, `NotOffered`, error mapping, JSON params |
| `frontend/yellowback.go` | the handlers (embed `UnimplementedYellowbackStreamerServer`; validate at the edge) |
| `frontend/yellowback_addr.go`, `taddrOf` in `frontend/service.go` | D-L-4 |
| `frontend/yellowback_ratelimit.go` | the token bucket |
| `frontend/yellowback_test.go` | the offline suite (11 tests, every method against the contract) |
| `frontend/yellowback_devnet_test.go` | the regtest suite (`-tags devnet`) |
| `testtools/lwdinfo/main.go` | `go run -mod=vendor ./testtools/lwdinfo -server host:port [-yellowback]`: `GetLightdInfo` + `GetLatestBlock` (+ every Yellowback method once) as JSON; the devnet's `check` uses it |
| `scripts/build-baseline.sh`, `scripts/check-generated.sh`, `scripts/devnet-test.sh` | the baseline binary, the generated-code gate, the regtest driver |
| `.github/workflows/yellowback-tests.yml` | build, vet, gofmt (the fork's files), tests, generated-code check |

**Changed in files that existed at `187a267`:** `cmd/root.go` (+21: the flag, its bind and
default, the `Options` field read, the registration block after darkside's), `common/common.go`
(+1: `Options.Yellowback`), `frontend/service.go` (+19 −3: `YellowbackAddresses`, `taddrOf`, the
three call sites), `Makefile` (+8: the proto in every list), `go.mod` (+1: `btcutil` promoted
from indirect to direct, same version, for `base58`), `README.md` (+2). **Zero:** both original
`.proto` files and their generated code, `parser/`, `common/cache.go`, the rest of
`common/common.go`, `go.sum`, `vendor/`, `Dockerfile`, `docker-compose.yml`.

## Baseline findings (this lineage)

- **F-R1** The baseline's own `frontend` tests fail and then hang: `TestGetTaddressTxids`
  ("wrong address" — yodl's regex commit invalidated upstream's `t…` test data), then the RPC
  stubs run out of order (`TestGetBlock`: "unexpected call to getblockStub") and
  `TestGetBlockRange` waits forever. Reproduced on the pristine `lightwalletd-legacy` export.
  Not this fork's to fix; CI runs the Yellowback frontend tests by name and every other package
  in full. `cmd`, `common`, `common/logging`, `parser`, `walletrpc` pass.
- `gofmt -l` at the baseline: `walletrpc/compact_formats.pb.go`, `parser/fuzz.go` (left alone).
- The default `--data-dir /var/lib/lightwalletd` and `--log-file ./server.log` need overriding on
  a laptop; the Makefile's `coverage` target uses GNU `sed -i` (fails on macOS).

## Testing against a regtest `ycash-dd` (the rule: never mainnet)

Offline: `go test -mod=vendor -run 'Unary|Streaming|Params|NodeErrors|InputValidation|AllowList|Probe|YedTo|TaddrOf|RateLimit' ./frontend/`.

Regtest, the way `yecwallet-dd` is tested — a real server against a real node:

```
cd ycash-dd
../.venv/bin/python contrib/yellowback/devnet/yellowback-devnet up --no-attest --dir ~/yb-devnet-lwd --portseed 8
../.venv/bin/python contrib/yellowback/devnet/yellowback-devnet lightwalletd start --extra=--yellowback --dir ~/yb-devnet-lwd
../.venv/bin/python contrib/yellowback/devnet/yellowback-devnet check --dir ~/yb-devnet-lwd
cd ../lightwalletd-dd && scripts/devnet-test.sh [--up] [--down]      # the whole suite, both servers
```

The devnet's `lightwalletd` subcommand starts this lineage with `--rpcuser/--rpcpassword/
--rpchost/--rpcport` (the four flags bypass the conf file), `--no-tls-very-insecure`,
`--grpc-bind-addr 127.0.0.1:<port>`, `--http-bind-addr 127.0.0.1:<port+2000>`,
`--data-dir node0/lightwalletd`, `--log-file node0/lightwalletd.log`; `--baseline` runs the
`lightwalletd-legacy` build; `check` probes through `testtools/lwdinfo` (and, with
`--extra=--yellowback`, every Yellowback method). On regtest the sapling height resolves to 0
and the cache fills instantly.

`scripts/devnet-test.sh` builds both binaries, starts them (fork 9067 with `--yellowback`,
legacy 9068) and runs `go test -tags devnet`:

| Case | What it proves |
|---|---|
| `TestDevnetBaselineByteEquality` | **the backward-compatibility gate**: `GetLightdInfo` and every `CompactBlock` of `GetBlockRange` byte-identical between the fork and the `187a267` binary |
| `TestDevnetBaselineHasNoYellowback` | the legacy binary answers `UNIMPLEMENTED` |
| `TestDevnetWalletMintSeenThroughServer` | node 0's `yed_mint` (a miner goroutine beside it, on a second RPC client); `GetAddressTokens` equals `yed_listunspent`; `GetTxInfo` `ok` |
| `TestDevnetRawMintThroughServer` | plan §5 item 5: a mint assembled by `ycash-dd/contrib/yellowback/devnet/lwd-rawmint` from numbers the server gave (`EstimateCollateral`, `GetFeePayee`, `BuildBundle` when armed), `ValidateRawTransaction`, `SendTransaction`, a block, `GetTxInfo` `ok`, the token in `GetAddressTokens` |

**Evidence (R0, 2026-09-24)**, five-node `--no-attest` devnet: 388 compact blocks [1..388]
byte-identical between fork and the `187a267` binary, `GetLightdInfo` identical; the baseline
`UNIMPLEMENTED`; wallet mint `86a0ef4a…` verdict `ok`, 10 tokens on 10 addresses identical to
`yed_listunspent`; raw mint `5ac1457b…` validated, sent and confirmed with verdict `ok`; 6.6 s.
Three harness lessons (in the workspace's `docs/mapping.md` §15): mine on a **pool** node, whose
blocks carry the quote tag (node 0's are untagged and empty the price windows); re-quote the
pools first (`yellowback-devnet price`) and warm the price up to the slow window; wait for the
transaction in the pool's mempool before generating, and for the index to reach the chain tip
before reading a verdict. The node's nightly (`ycash-dd/.github/workflows/yellowback-tests.yml`, "lightwalletd
against the devnet") runs the same driver with `--up --down`.

## Operator runbook

```
# node (ycash-dd), a relay's configuration
experimentalfeatures=1
yellowback=1
yellowbackenforce=0        # a relay follows the chain; enforcement is for miners (v2 plan §3.9)
insightexplorer=1
txindex=1
server=1 rpcuser=… rpcpassword=… rpcbind=127.0.0.1 rpcport=8232

# server (TLS as the README describes, or nginx in front)
lightwalletd --zcash-conf-path ycash.conf --data-dir /var/lib/lightwalletd --grpc-bind-addr 127.0.0.1:9067 --yellowback
```

The log says either `Yellowback service started (node rpcversion 3, network …)` or why not.
**Upgrade order:** node first (`ycash-dd` with `-yellowback`), then the server binary (no change
until the flag), then `--yellowback`. A server ahead of its node, or on a stock node, logs one
line and serves the baseline surface. `docs/review.md` is the review packet for the maintainers.
