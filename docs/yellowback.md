# lightwalletd-dd — Yellowback (YED) notes

The working fork of Ycash's lightwalletd for Yellowback. The plan is the workspace's
`docs/plans/yellowback-lightwalletd-plan.md`; this file is the operator and developer record.
**Baseline:** branch `lightwalletd-legacy` = `yodl/lightwalletd` `master` `187a26765e`
(2021-07-13: `zcash/lightwalletd` 0.4.6 plus four commits, the last adapting the transparent
address regex to Ycash's `s…`). Work branch: `upgrade/vault` (the vault upgrade line; `harden/yellowback` keeps the no-upgrade
line). The service was
first built and proven on an earlier, different lineage (yecdev's Zecwallet-derived server) and
re-ported here on 2026-09-24 (plan Phase R0, §10); the design and its evidence are the same.

## What it adds

A second gRPC service, `cash.z.wallet.sdk.rpc.YellowbackStreamer` (`walletrpc/yellowback.proto`),
beside `CompactTxStreamer`. Twenty-four methods, each a thin, allow-listed proxy of one read-only
node RPC — nineteen `yed_*` RPCs, the stock `getblockchaininfo` (`GetChainInfo`, below) and, from
rpcversion 5, four read RPCs of the vault primitive YED runs on (below) — on
the node this server already talks to: verdicts (`GetTxInfo`,
`ValidateRawTransaction`), price and collateral (`GetPrice`, `GetStats`, `EstimateCollateral`,
`EstimateFee`, `GetFeePayee`), vaults (`GetVault`, `ListVaults`, `ListClaimable`, `GetNotice`),
attestation (`BuildBundle`, `GetSelection`, `GetAttestations`, `ListAttestors`), the YED UTXO set
of any address (`GetAddressTokens` → the node's `yed_listtokens`), `GetActivation`,
`DecodePayload`, `GetYellowbackInfo`. Field names are the node contract's JSON names
(`testdata/yellowback/contract.json`, kept equal to `ycash-dd/doc/yellowback-rpc-contract.json`
by the workspace's `make spec`), so results unmarshal straight into the generated messages.

**Switch:** `--yellowback` (cobra/viper, so also `YELLOWBACK=1` or the config file). At startup,
after `GetLightdInfo`, the server calls `yed_getinfo`; it registers the service only when the node
speaks `rpcversion 5`. ("Method not found" means a node without the yed_* commands: the node
registers them exactly where the vault upgrade and a YED attestor set are configured; no node flag
turns them on.) Off,
or on a stock node, the binary is the baseline in every observable way. With the flag on, the
taddr RPCs (`GetTaddressTxids`, `GetTaddressBalance`, `GetAddressUtxos`) also accept YED
addresses (`ye…`/`yt…`/`yr…`), mapped to the transparent form before the baseline's
`checkTaddress` (plan D-L-4; `frontend/yellowback_addr.go`, `taddrOf` in `frontend/service.go`).

**The node is reached through `common.RawRequest`**, the tree's own function variable, which
`main` assigns from the btcd client and tests replace with a stub — no new abstraction.
`common.CallYed` is the only path to a `yed_*` RPC; `common.YedMethods` (allow-list) and
`common.NotOffered` (with reasons) together cover every `yed_*` method of the contract, and the
offline suite fails when one appears in neither. `common.StockMethods` is the separate, one-entry
allow-list of stock RPCs `CallYed` may also reach (`getblockchaininfo`, for `GetChainInfo`).

**rpcversion 5 (the vault upgrade, `docs/plans/yellowback-upgrade-plan.md` §6, §15.10).** YED is a
consensus module of the network upgrade `Vault` (branch id `6d5b7a31`), so the messages carry
no activation-by-signalling or enforcement fields (the numbers those fields used are `reserved` in
the proto), `GetActivation` is the upgrade object (`status`, `activationHeight`, `branchId`,
`attestorSetId`, `claimDelay`, `height`, `name`; also `GetYellowbackInfo.upgrade`), the parameters
gain `attestorSetId` and `claimDelay`, a vault row gains `scriptPubKey` (its V template) and, while
`CLAIMING` (a new `ListVaults` status), `intents[]{txid, vout, role, height, releaseHeight}`;
`GetTxInfo` gains `reopenedVaults`, attestor rows `lastAct` and `bondFrozen`. `GetChainInfo`'s
`upgrades` includes `Vault`, and its `nextBlockBranchId` is what a client signs with across the
activation block.

**The vault primitive (rpcversion 5).** `GetVaultInfo` (`vault_getinfo`), `ListSets` (`set_list`),
`GetSet` (`set_getinfo setid [height]`) and `ListVaultOutputs` (`vault_list {tag, setid, owner,
kind}`) proxy the primitive's read RPCs (`ycash-dd/doc/vault-rpc.md`, copied to
`testdata/vault/vault-rpc.md` with answers captured from a devnet as the fixtures). They go
through `CallYed` on a third allow-list, `common.VaultMethods`; every other `set_*`/`vault_*`
command is in `common.NotOfferedVault` with a reason, and the offline suite fails when a command of
the document is in neither. `vault_list`'s `mine` filter and the node's `wallet` result fields are
the node's wallet, so neither is carried. YEC amounts are the node's decimal numbers (`double`),
`valuezat` is zatoshi.

**rpcversion 6 (in-term claims, branch `upgrade/vault-in-term`; `docs/plans/yellowback-in-term-claims-plan.md`
§4.1 in the workspace).** A vault can be claimed in term once its collateral is below θ × debt (θ = 125 %), and the
owner may redeem at any height, paying an early-redeem fee before `lockHeight`. The server speaks exactly 6 on this
line (5 and 7 are refused, as before). Additive proto fields only: `YellowbackParams` gains `claimThresholdBps`
(19), `earlyRedeemFeeBps` (20, one per class A/B/C), `sigmaMultMaxBps` (21) and `inTermClaims` (22);
`YellowbackClassParams` gains `earlyRedeemFeeBps` (5); `YedClaimable` gains `claimable` (15) and `lockHeight` (16).
`ListClaimable` now streams every ACTIVE vault whose claim branch is open, in term too: a row with
`claimable: false` is above the threshold and its `underwaterAt` is the claim price (µUSD) below which it becomes
claimable — a client offers a claim only on `claimable: true` rows. `yed_estimateredeem` (the wallet's quote) is
wallet context and is not offered; a light client computes the early-redeem fee as
`params.earlyRedeemFeeBps[class] × collateralZat / 10000` before `lockHeight`. `ListClaimable` sends no `count`, so
the node's default page (1000 rows) applies.

**rpcversion 7 (post-quantum vault owners, branch `upgrade/quantum-vaults`; `docs/plans/yellowback-quantum-spec.md`
§4 and §6.2 in the workspace).** Every vault's owner is a post-quantum key (scheme 1 SLH-DSA-SHA2-128s; scheme 2
FN-DSA-512 once Falcon is active), named by its scheme and 32-byte key hash; no owner public key is on chain. The
server speaks exactly 7 on this line: 6 is refused because its owner fields would be dropped silently (their
numbers are reserved below) and its `vault_list` owner filter takes a `02`/`03` public key, the same 66 hex
digits as a Falcon key id; 8 is refused as before. Node, server and wallets deploy in lockstep (clients survey
C-11). Proto changes:
- `YedPayload`: `ownerPubKey` (5) reserved; `ownerScheme` (12), `ownerHash` (13) added. A MINT's `version` is 4;
  every other payload type stays 3.
- `YedVault` (`GetVault`, `ListVaults`): `ownerKeyId` (15), `ownerPubKey` (16) reserved; `ownerScheme` (28),
  `ownerHash` (29) added; `ownerAddress` (14) is the owner's 53-character post-quantum address.
  `YedClaimable.ownerAddress` likewise.
- `VaultOutput` (`ListVaultOutputs`): `ownerkey` (14) reserved; `owner` (23, the `pqkeyid`: scheme byte `01`/`02`
  then the key hash, 66 hex digits) and `ownerscheme` (24) added. `VaultOutputFilter.owner` is a `pqkeyid`
  (`^0[12][0-9a-fA-F]{64}$`); a `03…` public key is refused at the edge. A `02…` value is a Falcon key id: the
  node renamed the type rather than reusing it so a key is never read as a key id. The node also takes a PQ address
  there; this server takes the `pqkeyid` only (the address carries the same bytes).
- `YellowbackParams.pq` (23) = `YellowbackPQParams {schemes, falconActive, falconHeight, feeRateZatPerKB}`: the
  owner schemes valid at the next block, the Falcon height (-1 = none), and the node's `-pqfeerate` (a wallet's
  fee for a PQ spend is `max(DEFAULT_FEE, rate × size / 1000)` on ycash-dd). `YellowbackAttestParams.mintPayloadVersion`
  (14) = 4.

Addresses: a YED address is P2PKH (35 characters) or post-quantum (53 characters, `Base58Check(pqVersion ‖ scheme
‖ keyHash)`). Mainnet is `yb…` (owner decision D-Q-21: P2PKH `0x1FDD`, PQ `0x56AB`; the earlier `ye…` `0x1FE4` no
longer maps), testnet `yt…` (`0x2007`, PQ `0x571E`), regtest `yr…` (`0x2002`, PQ `0x5710`). `GetAddressTokens`
takes either form (the node resolves both). The taddr RPCs (`GetTaddressTxids`, `GetTaddressBalance`,
`GetTaddressBalanceStream`, `GetAddressUtxos`, `GetAddressUtxosStream`) map a P2PKH YED address to its transparent
twin as before, take a plain-YEC post-quantum address (`sq…`/`tq…`/`rq…`, owner decision D-Q-20: PQ versions
`0x4DD9`/`0x4F61`/`0x4C51`, a `TX_PQPKH` output, which the node's address index covers as `CScript::P2PQPKH`) as
is, and map a PQ YED address to the plain PQ form of the same script (`yb`→`sq`, `yt`→`tq`, `yr`→`rq`; the mapping
ycash-infra's yindex makes). The node names a PQ address in its answers (`GetAddressUtxosReply.address`) as asked,
so a client gets the plain form back, as it gets `s…`/`sm…` for a P2PKH YED address. A plain PQ address is accepted
with or without `--yellowback` (it is a chain-wide Ycash address on this line); a PQ YED address only with it. The
O(1) length-and-prefix pre-check (audit E-1) covers both lengths. Unit tests: `TestTaddrOfMapsPQAddresses`,
`TestPQTaddrMethodsForwardPlainForm` (the node's `pq_standard_tests` mainnet vectors and a regtest node's answers);
regtest: `TestDevnetPQTaddrThroughServer` (Falcon active: YEC sent to a wallet `rq…` address, every taddr method
under the `rq…` and `yr…` forms compared with `getaddressbalance`/`getaddresstxids`/`getaddressutxos`).
`vault_getnewowner` (a new PQ key of the node's wallet) is wallet context and not offered. The offline suite has
reverse checks (`TestContractFieldsAreDeclared`, `TestVaultContractFieldsAreDeclared`): every field of an offered
method's node contract (and, for the primitive, the node's `doc/vault-rpc-contract.json`, copied to
`testdata/vault/`) is declared in the proto or listed in `notCarried` with a reason, so a field the node renames
fails a test instead of disappearing.

**The contract copies** (`testdata/yellowback/contract.json`, `testdata/vault/vault-rpc-contract.json`,
`testdata/vault/vault-rpc.md`) are ycash-dd's generated files, copied by `scripts/sync-node-contracts.sh`
(default: the workspace's `ycash-dd` at `origin/upgrade/vault`; `--tree DIR --ref REF` for another;
`--check` compares only). Re-run it whenever a node change touches `doc/vault-rpc.md`,
`doc/vault-rpc-contract.json` or `doc/yellowback-rpc-contract.json`, run the `Vault|ContractFields|AllowList`
tests and commit the copies. CI runs the same script with `--check --github` against the node branch that
matches this one, and on `upgrade/*`, `harden/*` and the quantum line an unreachable node file fails the job
instead of skipping it (audit 06/L-1); ycash6's vault contract is compared too, report-only, with the embedded
`sha256` stamp masked (it is the only line that differs between the two node lines).

**rpcversion 8 (the fee structure, branch `upgrade/quantum-vaults`; `docs/plans/yellowback-quantum-fees-spec.md`
§1–§3 and the node's `doc/yellowback-rpc.md` in the workspace).** The pool fee is a protocol fee in dollars, tiered
by the vault's debt and struck at the vault's stored mint price `pMint`; network fees of post-quantum and Yellowback
transactions are dollars per KB at the attested tip price. The server speaks exactly 8 on this line (7 is refused:
`yed_estimatefee` and `yed_getfeepayee` changed their arguments, so a 7 node would misread the requests). Proto changes:
- `YellowbackParams`: `feeBps` (5), `feeMinZat` (6) reserved; `fees` (24), `maxMintCents` (25) and `supplyCap` (26)
  added. `fees` = `YellowbackFeeParams {protocol {bands[] {uptoCents (0 = the unbounded last band, JSON null), bps},
  floorCents, attestFeeBps, priceSource}, earlyRedeemFeeBps[], network {priceMicroUsd (0 = no price), priceSource
  ("tip" | "fallback"), plain, yellowback (each {usdPerKBMicro, zatPerKB}; yellowback also maxTxFeeUsdMicro,
  maxTxFeeCeilingZat, maxTxFeeZat), floorZatPerKB, fallbackZatPerKB, flatZat, maxTxFeeZat}}`. A light wallet
  prices a post-quantum transaction by size at `plain.zatPerKB` and a vault or YED transaction at
  `yellowback.zatPerKB` (never under `flatZat`), within `yellowback.maxTxFeeZat`. `params.pq.feeRateZatPerKB` now equals
  `fees.network.plain.zatPerKB`. `earlyRedeemFeeBps` (here and on each class) is of the debt at `pMint`.
- `YedVault` (`GetVault`, `ListVaults`) `pMint` (30), `YedClaimable` `pMint` (17): the price every later fee of the
  vault is struck at. `YedCollateralEstimate` gains `protocolFeeCents` (17), `protocolFeeZat` (18).
- `EstimateFee`: `YedFeeQuery.collateralZat` (1) reserved; `mintedCents` (2), `priceMicroUsd` (3, optional) sent as
  `yed_estimatefee <mintedCents> [priceMicroUsd]`; `YedFeeEstimate` gains `attestFeeZat` (2), `feeCents` (3), `pMint` (4)
  (field numbers shared with YEW's proto copy).
- `GetFeePayee`: `YedPayeeQuery.collateralZat` (2) and `YedPayee.feeZat` (3) reserved; the call is
  `yed_getfeepayee <refHeight> [selectorHex]`.

**The per-vault cancel limit (CAN-2, CAN-3; the workspace's `docs/plans/yellowback-cancel-limit-design.md` and
upgrade plan §15.10; additive, rpcversion unchanged).** `YedVault` (`GetVault`, `ListVaults`) carries the node's
four cancel-limit fields, exactly as `yed_getvault` / `yed_listvaults` answer them:
- `cancelCount` (31): the position's cancels counted by the escalation schedule (it survives every re-creation;
  0 at the mint).
- `cancelsRequired` (32): how many distinct current attestor-set members must co-sign a cancel of its claim if
  that cancel confirms in the next block: `min(q + 1, CANCEL_SIGNERS_MAX)`, `q` = `cancelCount` inside the quiet
  period, else 0 (`CANCEL_SIGNERS_MAX` 4, regtest 3).
- `cancelQuietUntil` (33): the first height at which the schedule starts from one again (`lastCancelHeight +
  CANCEL_QUIET`, 8,064 blocks, regtest 40). The node answers `null` for a vault never cancelled; that is `0` here.
- `heldResiduals` (34, `repeated YedOutpoint`; empty when none, the node omits the key): the residual intents of
  cancelled claims this position holds. `vault_release` of one is refused (`intent-residual-held`) until the
  position is CLOSED or CLAIMED, or ACTIVE at or after `cancelQuietUntil`.

They were in `notCarried` until this change. `YedClaimable` (`ListClaimable`) carries the first three too
(`cancelCount` 18, `cancelsRequired` 19, `cancelQuietUntil` 20; the node's `yed_listclaimable` gained them on
`fix/cancel-fields-2`, additive), so a claimant sees before it claims how many attestors a cancel would need without
a `GetVault` per row. `YedVault.refundHolder` (35, hex scriptPubKey, `""` unless CLAIMING): CAN-1 — a cancelled claim
refunds its burn; the scriptPubKey the claim's burned YED came from (the token at the claim's lowest-index YED input),
which a cancel re-issues the whole burn (`burnedCents`) to as one YED token. `TestVaultCancelLimitFields` passes a
cancelled vault with a held residual (and a CLAIMING one's `refundHolder`) through `GetVault` and `ListVaults`,
`TestClaimableCancelLimitFields` the claimable rows; on regtest `TestDevnetCancelLimitFieldsThroughServer` compares
every `ListVaults` row and its `GetVault`, and every `ListClaimable` row, with the node's answers.

**SendTransaction and the fee ceiling (review M-2).** A transaction priced at the published rate can pay more than
the node's `-maxtxfee`, which `sendrawtransaction` refuses as "absurdly high". On such a refusal, and only when the
node serves `yed_getinfo` at rpcversion 8, the server establishes the fee — from the node's message (ycash-dd states
it) or by computing it from the transaction and `gettxout` (ycash6 does not) — and resubmits with
`allowhighfees = true` when it is at most `params.fees.network.yellowback.maxTxFeeCeilingZat` (`-yellowbackmaxtxfee`,
default 10 YEC); above it the answer keeps the node's error code and names the fee and the ceiling. When the fee
cannot be established (an unparsable transaction, an unknown prevout) or the node has no Yellowback service, the
node's refusal stands. The first submission is the stock one, so a transaction under `-maxtxfee` costs nothing more.

## Light-client sync notes (x402 `lwdnext`, 2026-10-04)

What a light client (the x402 agent, YEW) gets from this server for syncing and signing, and what
it does not, on **both** node lines (ycash-dd 4.5.0 and ycash6 6.21.0):

- **`GetChainInfo(Empty) → YedChainInfo`** relays `getblockchaininfo`: `chainName`, `blockHeight`,
  `bestBlockHash`, `consensusBranchId` (= `consensus.chaintip`, the id the frozen `GetLightdInfo`
  already reports), **`nextBlockBranchId`** (= `consensus.nextblock`), `saplingActivationHeight`,
  and `upgrades[]{branchId, name, activationHeight, status}` sorted by height then id. ZIP 243
  binds a transaction's sighash to the branch id of the block that will include it, so on the one
  block before a network upgrade a client that signs with `chaintip` is rejected; it must sign
  with `nextblock` (x402 X-F71). `service.proto` is frozen (the gate), so the field lives on this
  service, not on `LightdInfo`. Cached per tip like the other parameterless answers; cost light.
- **No `GetSubtreeRoots`** (zcash/lightwalletd ≥ 0.4.14). It needs the node's
  `z_getsubtreesbyindex`: **absent on 4.5.0** (`Method not found`) and present on 6.21.0 only
  behind `-experimentalfeatures -lightwalletd` (`ycash6/src/rpc/blockchain.cpp:2009`). Only what
  both lines support is offered, so the fast-sync path is **`GetTreeState` at a checkpoint height,
  then compact-block scanning from there**. `GetTreeState` is proven at arbitrary heights and by
  hash on both lines, equal to the node's `z_gettreestate` and byte-identical to the baseline,
  before and after a Sapling note (`TestDevnetTreeStateAtArbitraryHeights`; closes plan F-11's
  open item). On the 0.4.6 lineage `GetTreeState` is served live from the node per call (no cache),
  so a client should ask for one checkpoint, not one per block.
- **`GetMempoolTx` streams Sapling transactions only** (X-F70, pinned by
  `TestDevnetMempoolStreamsSaplingOnly`): `frontend/service.go` keeps a mempool transaction only
  when `tx.HasSaplingElements()`, so a purely transparent transaction — every YED transfer, every
  x402 transparent payment — is never streamed, on the fork exactly as on the baseline. A light
  client cannot see a pending spend of its own transparent coin through this method; it learns of
  it at the next block (`GetAddressUtxos` / `GetAddressTokens` list confirmed outputs) or as a
  broadcast refusal. Unchanged: the method is the frozen service's.

**Errors:** a node RPC error → `FAILED_PRECONDITION` with the node's message verbatim
(`bundle-insufficient: …`, `vault-not-found: …`); transport failure → `UNAVAILABLE`; malformed
input → `INVALID_ARGUMENT` before any node call; a peer past its burst on any method →
`RESOURCE_EXHAUSTED` (a bucket of 20 tokens refilled one per second per peer; the methods that
make the node walk its index or attestation pool — `EstimateCollateral`, `BuildBundle`,
`ValidateRawTransaction`, `GetAddressTokens`, `GetSelection`, `ListVaults`, `ListClaimable`,
`GetAttestations` — cost a token, the index reads a fifth of one, and `ValidateRawTransaction` one
token per started 200 kB of the transaction, so a 2 MB one costs ten;
`frontend/yellowback_ratelimit.go`, no new dependency). `ValidateRawTransaction` admits a raw
transaction up to the node's own limit, `MAX_TX_SIZE_AFTER_SAPLING` = 2 000 000 bytes
(`ycash-dd/src/consensus/consensus.h:29`), the size the gRPC receive bound in `cmd/root.go` is set
for; it was 200 kB, which refused YEW's largest YED transfer (250 Falcon-held inputs, ~394 kB;
audit 06/L-2). The peer is the connection address;
`x-real-ip` / `x-forwarded-for` are believed only from the networks named by
`--trusted-proxy-cidr` (repeatable), since gRPC metadata is set by the client (audit E-2).
The peer map is capped at 10 000 entries.

**Node work in flight:** at most `--yellowback-max-inflight` (default 16) `yed_*` calls run at
once; a call that cannot get a slot within 2 s is `RESOURCE_EXHAUSTED`; every call carries a
15 s deadline and ends when the client departs (`DEADLINE_EXCEEDED` / `CANCELED`), though the
node call itself, once sent, runs to completion and frees its slot then. The parameterless
per-tip answers (`GetYellowbackInfo`, `GetPrice` at the tip, `GetStats`, `GetActivation`,
`ListClaimable`, `GetAttestations`) are served from one node call for 2 s. A YED address is
checked for length and prefix before it is base58-decoded, and one incoming gRPC message is
at most 2 MiB + 64 KiB (`cmd/root.go`; audit E-1, E-3, E-4).

## The read cache (ycash-infra plan K3b)

`common.CallYed` remembers what the node answered, per **(method, canonical JSON of the
arguments, chain tip hash)**, and serves a repeat of the same question at the same tip from
memory (`common/yedcache.go`). It never computes an answer (D-L-2 "proxy, not indexer";
contract rule 3: the verdict is the node's); it only repeats one the node gave at this tip. The
tip is the ingestor's (`BlockCache.GetLatestHash`), which the server tracks anyway, so no
`getbestblockhash` is spent per request; an entry can be at most one ingestor poll (2 s at the
tip) staler than the node, the bound the 2-second per-tip answers above already had. A new block
changes the key, so every old entry goes cold and the LRU evicts it; nothing is invalidated by
hand. Only successful answers are stored: a node error (`vault-not-found`, `tx-not-found`, the
index unhealthy) reaches the node again every time. Before the ingestor knows a tip, every call
bypasses the cache.

**Bounds and switches.** In-process LRU, `--yellowback-cache-entries` (default 10 000; 0 = off)
and `--yellowback-cache-bytes` (default 64 MiB; an answer larger than the budget is passed
through, not stored); `LWD_YELLOWBACK_CACHE_ENTRIES` in the container. Metrics:
`lightwalletd_yellowback_cache_requests_total{method, result="hit|miss|bypass"}` on the
`/metrics` endpoint the server already serves. **Not Redis:** the chart's Redis would let
replicas share entries, but no Go Redis client is vendored and the plan's D-L-6 says no
dependency changes; the backend is behind a two-method interface (`yedCacheStore`) with the
vendoring step named in the TODO (`go get github.com/redis/go-redis/v9 && go mod vendor`, a
plan decision first). Per-tip keys make a shared cache a convenience, not a correctness need.

**What is cached and what is not** (`common.YedCacheable`; the test
`TestYedCacheAllowListIsWithinTheOffered` keeps it inside the offered methods):

| Node RPC | Cached | Why |
|---|---|---|
| `yed_getinfo`, `yed_getprice`, `yed_getstats`, `yed_getactivation` | yes | the index at the tip and the parameters; `yed_getinfo.miner` is the node's own quote state, which a serving node has none of, and its `attest.poolSize/poolFresh` summary is the only pool-derived field (already served 2 s stale by the per-tip answers) |
| `yed_gettxinfo` | yes | the TxLog record at the tip; its `mempool`/wallet reads serve only the error and the node's-own-wallet `expired` paths, and once expired a transaction cannot re-enter the mempool |
| `yed_decodepayload` | yes | a pure function of the hex (cheap, but harmless to remember) |
| `yed_getvault`, `yed_listvaults`, `yed_listclaimable`, `yed_getnotice` | yes | index state at the tip; `yed_getnotice` reads the block at `notice.height` |
| `yed_estimatecollateral`, `yed_estimatefee`, `yed_getfeepayee` | yes | the reference snapshot (`tip − REF_LAG`) and the parameters |
| `yed_listtokens` | yes | the YED UTXO set at the tip — the K1 target: every `GetAddressTokens` was a node call |
| `getblockchaininfo` | yes | `GetChainInfo` relays only tip-pure fields (`size_on_disk`, which drifts, is not carried) |
| `vault_getinfo`, `set_list`, `set_getinfo`, `vault_list` | yes | the primitive's state at the tip; the wallet-only `mine` filter is never sent |
| `yed_validaterawtransaction` | **no** | the dry run is against the mempool (`CCoinsViewMemPool`), which changes between blocks |
| `yed_buildbundle`, `yed_getselection` | **no** | built from this node's attestation pool (W5, W9), which fills as attestations arrive off-chain; `poolFresh`, `reachable`, `missing` and the bundle hex change within a tip |
| `yed_getattestations` | **no** | the attestation pool itself |
| `yed_listattestors` | **no** | each row's `poolFresh` is pool state; kept exact rather than up to a block stale |

**Concurrency note.** A burst of equal requests on a fresh tip all miss together (there is no
single-flight): with ghz's 50 workers the node saw 50 `yed_listtokens` per tip change, then
none. Coalescing those is a follow-up if a block boundary ever shows in the node's queue.

**Measured (2026-10-09)** on the kind cluster's regtest node (`ycash-infra`, blocks every
~6 s), this image built from the worktree and run beside the cluster on the `kind` network
(`host.docker.internal` stalled the ingestor; the node itself answered in milliseconds), ghz
n=2000 c=50 with a distinct `x-real-ip` per request (the infra `load/run.sh` invocation, so the
per-peer bucket is not the ceiling), two runs each after a warm-up:

| Method | cache off | cache on | node calls (6 000 requests) |
|---|---|---|---|
| `GetAddressTokens` (one address, 9 700 outputs) | 1 851 / 1 827 rps, p50 26 / 24 ms, p99 38 / 49 ms | 32 512 / 30 825 rps, p50 0.9 / 1.0 ms, p99 2.4 / 2.4 ms | 6 000 → 50 (one burst at the first tip) |
| `GetPrice` (at the tip) | 29 766 / 22 783 rps, p50 1.0 / 1.1 ms, p99 2.3 / 2.4 ms | 28 937 / 26 882 rps, p50 1.3 / 1.3 ms, p99 2.4 / 2.6 ms | 50 either way: the 2-second per-tip answer already served it |

`GetAddressTokens` goes from the K1 number's order (1 340 rps through the gateway) to the
other methods' (17× here), because a repeat between blocks is no longer a `yed_listtokens`.

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
| `common/yedcache.go` (+ `yedcache_test.go`) | the tip-hash-keyed read cache: `YedCacheable`, `YedTipHash`, the LRU, the counter |
| `frontend/yellowback.go` | the handlers (embed `UnimplementedYellowbackStreamerServer`; validate at the edge) |
| `frontend/yellowback_addr.go`, `taddrOf` in `frontend/service.go` | D-L-4 |
| `frontend/yellowback_ratelimit.go` | the token bucket |
| `frontend/yellowback_test.go` | the offline suite (every method against the contract; `GetChainInfo` against a fixed `getblockchaininfo` answer) |
| `frontend/yellowback_devnet_test.go` | the regtest suite (`-tags devnet`) |
| `testtools/lwdinfo/main.go` | `go run -mod=vendor ./testtools/lwdinfo -server host:port [-yellowback]`: `GetLightdInfo` + `GetLatestBlock` (+ every Yellowback method once, incl. `GetChainInfo`'s two branch ids) as JSON; the devnet's `check` uses it |
| `scripts/build-baseline.sh`, `scripts/check-generated.sh`, `scripts/devnet-test.sh` | the baseline binary, the generated-code gate, the regtest driver |
| `.github/workflows/yellowback-tests.yml` | build, vet, gofmt (the fork's files), tests, generated-code check |

**Changed in files that existed at `187a267`:** `cmd/root.go` (+53: the flags, their binds and
defaults, the `Options` field reads, the registration block after darkside's, the cache's
wiring), `common/common.go` (+9: `Options.Yellowback`, the bounds), `frontend/service.go` (+19 −3: `YellowbackAddresses`, `taddrOf`, the
three call sites), `Makefile` (+8: the proto in every list), `go.mod` (+1: `btcutil` promoted
from indirect to direct, same version, for `base58`), `README.md` (+2). **Zero:** both original
`.proto` files and their generated code, `parser/`, `common/cache.go`, the rest of
`common/common.go`, `go.sum`, `vendor/`, `Dockerfile`, `docker-compose.yml`.

## Baseline findings (this lineage)

- **F-R1** The baseline's own `frontend` tests fail and then hang: `TestGetTaddressTxids`
  ("wrong address" — yodl's regex commit invalidated upstream's `t…` test data), then the RPC
  stubs run out of order (`TestGetBlock`: "unexpected call to getblockStub") and
  `TestGetBlockRange` waits forever. Reproduced on the pristine `lightwalletd-legacy` export.
  **Fixed** (2026-10-13, lane resync-a): the test's address table and stub now use Ycash's
  `s…` prefix (`checkTaddress` is unchanged), so the package passes and CI runs `./frontend/`
  whole instead of the Yellowback tests by name. `cmd`, `common`, `common/logging`, `parser`,
  `walletrpc` pass.
- `gofmt -l` at the baseline: `walletrpc/compact_formats.pb.go`, `parser/fuzz.go` (left alone).
- The default `--data-dir /var/lib/lightwalletd` and `--log-file ./server.log` need overriding on
  a laptop; the Makefile's `coverage` target uses GNU `sed -i` (fails on macOS).

## Testing against a regtest `ycash-dd` (the rule: never mainnet)

Offline: `go test -mod=vendor -run 'Unary|Streaming|Params|NodeErrors|InputValidation|AllowList|Probe|YedTo|TaddrOf|RateLimit|GetChainInfo|StockAllowList|CallYed|ListVaults|TipAnswers' ./frontend/`.

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
`--data-dir node0/lightwalletd`, `--log-file node0/lightwalletd.log` — `--rpcpassword` on the
command line is visible to every local user in `ps` and is for the devnet only; a real
deployment reads the credentials from the node's conf file (`--zcash-conf-path`, audit E-8);
`--baseline` runs the
`lightwalletd-legacy` build; `check` probes through `testtools/lwdinfo` (and, with
`--extra=--yellowback`, every Yellowback method). On regtest the sapling height resolves to 0
and the cache fills instantly.

`scripts/devnet-test.sh` builds both binaries, starts them (fork 9067 with `--yellowback`,
legacy 9068) and runs `go test -tags devnet`. Environment: `YCASH_DD` (the node repo —
`ycash6` runs the same suite against a 6.21.0 devnet), `LWD_PYTHON`, `LWD_FORK_BIN`,
`LWD_FORK_PORT` / `LWD_BASELINE_PORT`:

| Case | What it proves |
|---|---|
| `TestDevnetBaselineByteEquality` | **the backward-compatibility gate**: `GetLightdInfo` and every `CompactBlock` of `GetBlockRange` byte-identical between the fork and the `187a267` binary |
| `TestDevnetBaselineHasNoYellowback` | the legacy binary answers `UNIMPLEMENTED` |
| `TestDevnetWalletMintSeenThroughServer` | node 0's `yed_mint` (a miner goroutine beside it, on a second RPC client); `GetAddressTokens` equals `yed_listunspent`; `GetTxInfo` `ok` |
| `TestDevnetRawMintThroughServer` | plan §5 item 5: a mint assembled by `ycash-dd/contrib/yellowback/devnet/lwd-rawmint` from numbers the server gave (`EstimateCollateral`, `GetFeePayee`, `BuildBundle` when armed), `ValidateRawTransaction`, `SendTransaction`, a block, `GetTxInfo` `ok`, the token in `GetAddressTokens` |
| `TestDevnetChainInfoMatchesNode` | `GetChainInfo` equals node 0's `getblockchaininfo` (both branch ids, height, hash, every upgrade, sorted); its `consensusBranchId` is `GetLightdInfo`'s; the baseline answers `UNIMPLEMENTED` |
| `TestDevnetTreeStateAtArbitraryHeights` | stock `GetTreeState` at 1, 2, tip/3, tip/2, tip−1, tip, by height and by hash: equals `z_gettreestate`, byte-identical to the baseline; then a Sapling note (`z_shieldcoinbase`) is mined and the tree at that height is non-empty while the previous height's is unchanged; past the tip fails |
| `TestDevnetMempoolStreamsSaplingOnly` | X-F70: with a transparent and a Sapling transaction in node 0's mempool, `GetMempoolTx` streams the Sapling one and never the transparent one, fork and baseline identical |

**Evidence (R0, 2026-09-24)**, five-node `--no-attest` devnet: 388 compact blocks [1..388]
byte-identical between fork and the `187a267` binary, `GetLightdInfo` identical; the baseline
`UNIMPLEMENTED`; wallet mint `86a0ef4a…` verdict `ok`, 10 tokens on 10 addresses identical to
`yed_listunspent`; raw mint `5ac1457b…` validated, sent and confirmed with verdict `ok`; 6.6 s.
**Evidence (lwdnext, 2026-10-04)**, five-node `--no-attest --lean` devnets of both lines: all
seven cases pass on ycash-dd 4.5.0 (seed 321; 232 compact blocks identical; tree 6 → 70 hex
chars after the note at 236) and on ycash6 6.21.0 (seed 323; 232 identical; tree 6 → 134 after
the note at 236); `GetChainInfo` tip = next = `19bd2d2f` (Canopy) on both; `devnet check` passes
with the fork binary.
Three harness lessons (in the workspace's `docs/mapping.md` §15): mine on a **pool** node, whose
blocks carry the quote tag (node 0's are untagged and empty the price windows); re-quote the
pools first (`yellowback-devnet price`) and warm the price up to the slow window; wait for the
transaction in the pool's mempool before generating, and for the index to reach the chain tip
before reading a verdict. The node's nightly (`ycash-dd/.github/workflows/yellowback-tests.yml`, "lightwalletd
against the devnet") runs the same driver with `--up --down`.

## Operator runbook

```
# node (ycash-dd or ycash6 on the vault upgrade). Yellowback needs no node flag: YED is consensus
# wherever the upgrade and the network's YED attestor set are configured. These three lines are
# for the stock CompactTxStreamer's address RPCs (GetTaddressTxids, GetTaddressBalance,
# GetAddressUtxos): -insightexplorer needs -txindex, and the node refuses it without
# -experimentalfeatures ("Insight explorer requires -experimentalfeatures").
experimentalfeatures=1
insightexplorer=1
txindex=1
server=1 rpcuser=… rpcpassword=… rpcbind=127.0.0.1 rpcport=8832

# server (TLS as the README describes, or nginx in front)
lightwalletd --zcash-conf-path ycash.conf --data-dir /var/lib/lightwalletd --grpc-bind-addr 127.0.0.1:9067 --yellowback \
  --trusted-proxy-cidr 127.0.0.1/32      # only if nginx sets x-real-ip; omit otherwise
```
Credentials come from `ycash.conf`: never put `--rpcpassword` on a production command line.

The log says either `Yellowback service started (node rpcversion 5, network …)` or why not.
**Upgrade order:** node first (`ycash-dd` or `ycash6` on the vault upgrade), then the server binary (no change
until the flag), then `--yellowback`. A server ahead of its node, or on a stock node, logs one
line and serves the baseline surface. `docs/review.md` is the review packet for the maintainers.
