# yugen-hostd: the Yugen mailbox / retrieval host (spec 0.5.2 §17; addendum §3.4; ruling 11)

One process = one host. A pair = two processes with separate data directories (and, on the devnet,
separate nodes). Build: `sh scripts/deps.sh; cargo build --release` (in `yugen/`). Run:
`yugen-hostd -id=H0 -datadir=D -listen=127.0.0.1:P -node=127.0.0.1:RPC -rpcuser=U -rpcpassword=W -network=regtest`.
Sources: ycashd `yugen_getinfo`, `yugen_getblockdata`, `getblockhash` (reorg: rebuild from activation).

Interface: JSON-RPC 1.0 over HTTP (no auth). Options: `-operator_file=F.json` (M-4 metadata),
`-debug_log_queries=1` (M-5, devnet only).

| Method | Params | Result | Spec |
|---|---|---|---|
| `yh_gettransparency` | | {version, host, network, operator, software {name, version, commit}, logging, sync_height, epochs [{h, n, aroot, note_root, digest}]} | M-4 |
| `yh_getinfo` | | host, network, activation, tip, sync_height, records | |
| `yh_getsyncroots` | [since?] | h (latest boundary), aroot, n, locs, counts [{h,n}] after `since`, note_root (root after block h), tree_size | §17.5, §17.8 item 2 |
| `yh_getlocators` | [from, h] | locators (hex32) of snapshot h from index `from` | §17.5 |
| `yh_putrecord` | [locator, record, del_token?] | {accepted, index} (R-27) | §12.3 |
| `yh_addrquery` | [h, dpf_key] | answer hex (ADDR_ENTRY bytes): XOR of the entries whose DPF bit is 1 | §17.6 |
| `yh_getpackages` | [epoch, bucket, h] | [{entry, nf1, txid, height, path}] (entries applied by boundary h; path = 20×3×48 B to note_root) | §17.4 |
| `yh_getpkgcount` | [epoch] | on-chain packages applied in the epoch's blocks (width rule P) | §17.4 |
| `yh_getslim` / `yh_getslimhash` / `yh_getslots` | [slice] | slim layer / H_slim / full slots | §17.1 |
| `yh_gettoken` | | one-use upload token | L-3 |
| `yh_uploadmailbox` | [bucket, entry, token?] | {accepted, reason?: malformed, not-applied, token} | §17.4 host rule |
| `yh_fetchmailbox` | [epoch, bucket, page] | {entries, pages} (16 a page) | §17.4 |

Snapshot rule: at each processed block h ≡ 0 (mod 16), h ≥ activation, the records held then form
snapshot h (`aroot_h`, `n_h`, `locs_h` per §17.5). Mailbox: 8 untokened entries per (epoch, bucket),
then one token per upload.

## Logging (D-PIR-3 M-5)

Default: `D/queries.log` gets one line per request holding the method name only. No parameters,
so no DPF keys, query indices, locators, records, tokens or mailbox entries; the client's network
address is never logged; nothing links a client to its queries. `-debug_log_queries=1` writes
`method params` (the DPF keys included) for the devnet's L-4 share inspection; the host then prints
a warning on stderr and as the first log line, and publishes `logs_dpf_keys: true`.
Acceptance: `cargo test --test privacy` runs a full two-host session over HTTP and searches both
logs for every key, seed, correction-word window, locator and record sent (none found by default;
found with the flag, as control). The devnet's `walk m3` reads keys from the log, so its host
command line needs `-debug_log_queries=1`.

## Transparency (D-PIR-3 M-4)

`yh_gettransparency` and the static file `D/transparency.json` (rewritten at startup and at each
sync boundary; an operator serves it over HTTPS) publish: the operator metadata from
`-operator_file` (a JSON object: name, jurisdiction, contact, as the operator declares), the
software name, version and git commit (`build.rs`, `git describe --dirty`), the logging policy
above, and per sync epoch (boundary h) the database digest:
`SHAKE256-512("yugen-hostd/epoch-digest/v1" || h_le32 || block_hash_hex || tree_size_le64 ||
note_root || n_le32 || locs_hex || aroot_hex)` (label and layout Original, the Ycash developers,
2026; host-side, not a spec domain). Two honest hosts on one chain publish identical digests; a
wallet compares the two hosts' digests for the boundary it syncs at and refuses both answers on a
mismatch. Acceptance: `transparency_matches_across_hosts` (equal digests per epoch, RPC = file).
Open: the §17.7 long-lived keys are not built, so not published; the wallet-side comparison is in
yew, not here; the commit is self-declared until reproducible builds exist.

Not built (open): §17.7 hybrid transport (L-5), §17.3 path records and §17.8 slot queries by DPF, the
gRPC face on lightwalletd (`GetSyncRoots`, …: the methods above are its content), restart history of
replaced records (a restarted host rebuilds snapshots from current records).

DPF: BGI16 tree construction (Boyle, Gilboa, Ishai, "Function Secret Sharing"; not read, from memory
through yugen-pir docs/pir-selection.md §3.1, as spec §17.6 states), adapted to a one-bit output
without early termination; PRG = spec §17.6 `H_dpf-prg`. Tests: `cargo test` (hostd/tests/host.rs).
