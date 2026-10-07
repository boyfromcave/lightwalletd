# lightwalletd-dd — review document for the Ycash maintainers

Written for: the Ycash developers who maintain the Ycash lightwalletd (`yodl/lightwalletd`,
`zcash/lightwalletd` 0.4.6 plus the Ycash address regex) and will decide whether to take this
change. It states what changed, what did not, how that was proven, and what a light client
gains. The design record is the workspace plan (`docs/plans/yellowback-lightwalletd-plan.md`).

## What this is

Ycash Yellowback (YED) is a dollar token on ordinary transparent outputs, built on the vaults of
the proposed Ycash vault upgrade (a network upgrade, branch ID `6d5b7a31`) and judged by the
node as consensus wherever that upgrade and the network's YED attestor set are configured
(`ycash-dd` or `ycash6` on `upgrade/vault`; the node needs no Yellowback flag). Light clients already see those outputs
(`GetTaddressTxids` + `GetTransaction`); what they cannot compute alone is the node's
**verdict** on a transaction, the **price and collateral** numbers, and the **attestation
bundle** a mint or claim must carry. This fork adds a **second gRPC service**,
`cash.z.wallet.sdk.rpc.YellowbackStreamer`, whose twenty-four methods are each a thin,
allow-listed proxy of one read-only node RPC on the node the server already talks to: nineteen
`yed_*` RPCs, the stock `getblockchaininfo` (`GetChainInfo`) and four read RPCs of the vault
primitive (`vault_getinfo`, `set_list`, `set_getinfo`, `vault_list`). The server holds no
Yellowback state and runs no Yellowback logic. The darkside service is the precedent for a
second service in this codebase; the new one follows it.

## What did not change

| File | Delta vs `master` `187a267` | Why it matters |
|---|---|---|
| `walletrpc/service.proto`, `walletrpc/compact_formats.proto`, `walletrpc/darkside.proto` and their generated code | **0** | the wire contract every YecLite and mobile client vendors |
| `parser/*` | **0** | block and transaction parsing; not on the YED path |
| `common/cache.go`, `common/common.go` (but one field) | **0** | the ingest loop and the block cache |
| `Dockerfile`, `docker-compose.yml`, `buildenv.sh` | **0** | the same image and binary, one new flag |
| `go.sum`, `vendor/` | **0** | no dependency added, none bumped |

## What changed in files that existed

| File | Lines | What |
|---|---|---|
| `cmd/root.go` | +39 | `--yellowback` (flag, viper bind and default, `Options` field) and the bounds flags, and after the darkside block: probe the node with `yed_getinfo` and register the service only when it answers with `rpcversion == 5` ("Method not found" means a node without the vault upgrade or the attestor set: the baseline surface, no error) |
| `common/common.go` | +1 | `Options.Yellowback` |
| `frontend/service.go` | +19 −3 | `YellowbackAddresses` and `taddrOf`: with the flag on, the three taddr RPCs map a YED address (`ye…`/`yt…`/`yr…`, the same key hash under other version bytes) to its transparent form before the existing `checkTaddress`, which stays the last check; off, every path is the baseline's |
| `Makefile` | +8 | `yellowback.proto` in `GENERATED_FILES`, `proto`, `update-grpc`, `doc`, `simpledoc` |
| `go.mod` | +1 | `github.com/btcsuite/btcutil` promoted from indirect to direct, at the version already vendored, for `base58` |
| `README.md` | +2 | one line pointing at `docs/yellowback.md` |

**With the flag off, or against a stock Ycash node, the binary is the baseline in every
observable way.** Proven on a regtest devnet: `GetLightdInfo` byte-identical and every
`CompactBlock` of `GetBlockRange` byte-identical between this build and a binary built from
`master 187a267`; every Yellowback method `UNIMPLEMENTED` on the baseline
(`frontend/yellowback_devnet_test.go`).

## What was added

| File | What |
|---|---|
| `walletrpc/yellowback.proto` (+ `yellowback.pb.go`, `yellowback_grpc.pb.go`, generated with the baseline's own protoc-gen-go v1.26.0 / protoc-gen-go-grpc v1.1.0) | the service and its messages; field names are the node contract's JSON names |
| `common/yellowback.go` | the probe; `CallYed`, the **only** path to a `yed_*` RPC, over the tree's `common.RawRequest`; the allow-list `YedMethods` and the reasoned `NotOffered` list; error mapping |
| `frontend/yellowback.go` | the handlers: validate at the edge, call, unmarshal, return; embeds `UnimplementedYellowbackStreamerServer` |
| `frontend/yellowback_addr.go` | YED → transparent address conversion (base58check, the node's version bytes) |
| `frontend/yellowback_ratelimit.go` | per-peer token bucket, every method (the node-work ones cost a token, index reads a fifth); peers keyed on the connection unless `--trusted-proxy-cidr` |
| `frontend/yellowback_test.go`, `frontend/yellowback_devnet_test.go` | offline suite against the node's RPC contract; regtest suite (`-tags devnet`) |
| `testtools/lwdinfo/main.go` | a probe tool the devnet's `check` uses |
| `scripts/*.sh`, `.github/workflows/yellowback-tests.yml`, `docs/*.md`, `testdata/yellowback/contract.json` | tooling, CI, records, the contract fixture |

### The allow-list

`common.YedMethods` names the nineteen node-context, read-only `yed_*` RPCs the service may call;
`common.StockMethods` (`getblockchaininfo`) and `common.VaultMethods` (the four vault primitive
reads) are separate allow-lists, and `common.NotOfferedVault` gives the reason for every other
`set_*` / `vault_*` command.
`common.NotOffered` names every other `yed_*` RPC of the contract with the reason: every wallet
RPC (they need the node's keys), `yed_setquote` (miner-local), `yed_addattestation` and
`yed_signattestation` (RPC-auth only), and the operator/test tooling. The offline suite fails
when a node RPC appears in neither list.

### Errors

A node RPC error is relayed as gRPC `FAILED_PRECONDITION` with the node's message verbatim; a
transport failure is `UNAVAILABLE`; malformed input is `INVALID_ARGUMENT` and never reaches the
node; a peer past its burst is `RESOURCE_EXHAUSTED` (20 tokens, one back per second, per
connection address); so is a call that finds all `--yellowback-max-inflight` (16) node slots
busy for 2 s. A node call ends with the client's context or a 15 s deadline
(`DEADLINE_EXCEEDED` / `CANCELED`). These bounds are the 2026-10-01 audit's E-1..E-5.

## How it was proven

- **Offline** (`go test -mod=vendor ./frontend/ -run …`, 17 tests): every method compared
  field by field with the node contract's example values through a stubbed `common.RawRequest`
  (the tree's own test pattern); params encoding; error mapping; input validation; allow-list
  completeness; the probe on a node without `yed_*` ("Method not found"), an `rpcversion` other
than 5, a transport failure; address
  vectors; the rate limiter. `go vet` clean.
- **Regtest** (`scripts/devnet-test.sh`, a five-node `ycash-dd` devnet, never mainnet): the
  byte-equality gate above; a mint made by node 0's wallet is seen through `GetAddressTokens`
  exactly as `yed_listunspent` sees it and `GetTxInfo` says `ok`; a mint **assembled from raw
  parts with every number taken from the server** is dry-run by `ValidateRawTransaction`,
  broadcast by `SendTransaction`, and confirmed with verdict `ok` — the proof that a light
  client can mint against this surface. The node's nightly runs the same driver.
- **Generated code**: `scripts/check-generated.sh` regenerates every `.pb.go` and
  `_grpc.pb.go` with the pinned generators and diffs; the baseline's own files reproduce
  exactly, so the pin is right.

**Gaps the offline and regtest suites leave** (audit E-10): the armed carrier path
(`BuildBundle` feeding a mint, plan §9 Q6) has not run on a devnet, and
`TestDevnetRawMintThroughServer` skips unless `LWD_RAWMINT` is set, so the raw-mint proof is
only as fresh as the last run that set it; the baseline's three taddr paths in `service.go`
run under no regression test beyond `TestTaddrOfMapsYedAddresses` and
`TestTaddrOfRejectsLongGarbageCheaply` (F-R1 excludes the baseline's own frontend tests).

**A note on the baseline's tests.** At `187a267` the `frontend` package's own tests fail and
then hang (the Ycash regex commit invalidated upstream's `t…` test data; then the RPC stubs run
out of order and `TestGetBlockRange` never returns). This fork does not touch them; CI runs the
Yellowback frontend tests by name and every other package in full (`docs/yellowback.md`, F-R1).

## Node side

The server's node is a `ycash-dd` or `ycash6` build of `upgrade/vault`. Yellowback needs no node
flag there; the node runs `insightexplorer=1 txindex=1` for the stock address RPCs, and
`experimentalfeatures=1` because the node refuses `insightexplorer` without it. One node RPC was
added for this server, `yed_listtokens` (the YED outputs of any address; an index read, zero
consensus lines).

## Trust statement (for the client to show once)

The server relays the node's verdicts and prices. It can withhold, or show a stale view, and it
can misreport a verdict — which affects what the wallet *displays*, never what the chain
*applies*. The dry run is advice; the chain is the judge. A client verifies every attestation
signature it is handed against the seated set it reads from `ListAttestors`.

## Upgrade order

Node first (`ycash-dd` or `ycash6` on the vault upgrade), then the server binary (no behaviour change until
the flag), then `--yellowback`. A server upgraded ahead of its node, or pointed at a stock node,
logs one line and serves the baseline surface.
