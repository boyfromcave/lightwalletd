//! Host state: the chain index built from ycashd (`yugen_getblockdata`), the address records
//! (§12.3, §17.5) with their per-sync snapshots, the package buckets and mailbox (§17.4), the
//! incoming slices (§17.1), and the answers the wallet checks against the per-sync roots.

use std::collections::{BTreeMap, HashSet};
use std::path::PathBuf;

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use yugen_ref::address::host_accepts;
use yugen_ref::hostidx::{self, addr_entry, addr_leaf, addr_locs, addr_root, addr_tree};
use yugen_ref::params::*;
use yugen_ref::transfer::{self, Transfer};
use yugen_ref::tree::{leaf_hash, Tree};

use crate::dpf;
use crate::rpc::Rpc;

pub type B32 = [u8; 32];
pub type B48 = [u8; 48];

/// Untokened mailbox entries a bucket takes per epoch before uploads need a token (L-3).
pub const MAILBOX_FREE: usize = 8;
/// Entries per mailbox page (L-3 "pages on overflow").
pub const MAILBOX_PAGE: usize = 16;
/// Most boundaries one sync covers (§17.8 item 1).
pub const SYNC_MAX_PERIODS: usize = 96;

fn hx(b: &[u8]) -> String {
    hex::encode(b)
}
fn unhex<const N: usize>(s: &str) -> Result<[u8; N], String> {
    hex::decode(s).ok().and_then(|v| v.try_into().ok()).ok_or_else(|| format!("hex of {N} bytes"))
}

/// The anchor height a slot denotes in a block at `h` (spec §8.1), as the wallet resolves it.
pub fn resolve_anchor(slot: u8, h: u32, activation: u32) -> Option<u32> {
    let mut ha = h.checked_sub(1)? / ANCHOR_GRID * ANCHOR_GRID;
    while ha >= activation && h - ha <= ANCHOR_WINDOW {
        if transfer::anchor_slot(ha) == slot {
            return Some(ha);
        }
        ha = ha.checked_sub(ANCHOR_GRID)?;
    }
    None
}

#[derive(Clone, Debug)]
pub struct PkgItem {
    pub entry: Vec<u8>,
    pub nf1: B32,
    pub pos: u64,
    pub height: u32,
    pub txid: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Snap {
    pub h: u32,
    pub n: u32,
    pub locs: String,
    pub aroot: String,
    /// M-4: the database digest of this sync epoch (see `epoch_digest`); empty in files written
    /// before M-4 (recomputed on load when the note root is known).
    #[serde(default)]
    pub digest: String,
    #[serde(skip)]
    pub records: Vec<(B32, Vec<u8>)>,
}

#[derive(Default, Serialize, Deserialize)]
struct Saved {
    records: Vec<(String, String)>,
    snaps: Vec<Snap>,
    mailbox: Vec<(u32, u32, String)>,
}

pub struct Host {
    pub id: String,
    pub dir: PathBuf,
    pub network: u8,
    pub activation: Option<u32>,
    pub branch_id: u32,
    // chain index
    pub tip: Option<u32>,
    pub hashes: BTreeMap<u32, String>,
    pub cms: Vec<B48>,
    pub sizes: BTreeMap<u32, u64>,
    pub roots: BTreeMap<u32, B48>,
    pub slots: BTreeMap<u32, Vec<Vec<u8>>>,
    pub packages: BTreeMap<(u32, u16), Vec<PkgItem>>,
    pub pkg_counts: BTreeMap<u32, u64>,
    // host statements
    pub records: Vec<(B32, Vec<u8>)>,
    pub snaps: BTreeMap<u32, Snap>,
    pub mailbox: BTreeMap<(u32, u32), Vec<Vec<u8>>>,
    pub tokens: HashSet<B32>,
    /// M-5: when false (the default) the request log holds method names only: no parameters, so
    /// no DPF keys, query indices, record or mailbox contents, and no client addresses.
    pub debug_log_queries: bool,
    /// M-4: the operator-declared metadata (`-operator_file`, a JSON object), published as is.
    pub operator: Value,
    tree_cache: Option<(u64, Tree)>,
}

impl Host {
    pub fn open(id: &str, dir: PathBuf, network: u8) -> Self {
        let mut h = Host {
            id: id.into(),
            dir,
            network,
            activation: None,
            branch_id: BRANCH_ID,
            tip: None,
            hashes: BTreeMap::new(),
            cms: Vec::new(),
            sizes: BTreeMap::new(),
            roots: BTreeMap::new(),
            slots: BTreeMap::new(),
            packages: BTreeMap::new(),
            pkg_counts: BTreeMap::new(),
            records: Vec::new(),
            snaps: BTreeMap::new(),
            mailbox: BTreeMap::new(),
            tokens: HashSet::new(),
            debug_log_queries: false,
            operator: json!({}),
            tree_cache: None,
        };
        if let Some(s) = std::fs::read(h.dir.join("host.json")).ok().and_then(|b| serde_json::from_slice::<Saved>(&b).ok()) {
            h.records = s.records.iter().map(|(l, r)| (unhex::<32>(l).unwrap(), hex::decode(r).unwrap())).collect();
            for mut sn in s.snaps {
                sn.records = h.records[..sn.n as usize].to_vec();
                h.snaps.insert(sn.h, sn);
            }
            for (e, b, x) in s.mailbox {
                h.mailbox.entry((e, b)).or_default().push(hex::decode(x).unwrap());
            }
        }
        h
    }

    pub fn save(&self) {
        let s = Saved {
            records: self.records.iter().map(|(l, r)| (hx(l), hx(r))).collect(),
            snaps: self.snaps.values().cloned().collect(),
            mailbox: self.mailbox.iter().flat_map(|((e, b), v)| v.iter().map(move |x| (*e, *b, hx(x)))).collect(),
        };
        let _ = std::fs::create_dir_all(&self.dir);
        let tmp = self.dir.join("host.json.tmp");
        if std::fs::write(&tmp, serde_json::to_vec(&s).unwrap()).is_ok() {
            let _ = std::fs::rename(tmp, self.dir.join("host.json"));
        }
    }

    /// Logs one request. M-5: by default only the method name is written; the parameters (DPF
    /// keys, indices, records, mailbox entries) only with `-debug_log_queries=1` (devnet only).
    /// The client's network address is never logged.
    pub fn log_request(&self, method: &str, params: &Value) {
        let m: String = method.chars().filter(|c| c.is_ascii_alphanumeric() || *c == '_').take(64).collect();
        if self.debug_log_queries {
            self.log(&format!("{m} {params}"));
        } else {
            self.log(&m);
        }
    }

    /// The logging policy this host runs under (M-4 publishes it, M-5 sets the default).
    pub fn logging_policy(&self) -> Value {
        json!({
            "request_log": "queries.log",
            "logs_method_names": true,
            "logs_parameters": self.debug_log_queries,
            "logs_dpf_keys": self.debug_log_queries,
            "logs_query_indices": false,
            "logs_client_addresses": false,
            "retains_dpf_keys_after_answer": self.debug_log_queries,
            "debug_log_queries": self.debug_log_queries,
        })
    }

    /// M-4: the database digest of the sync epoch ending at boundary `h`: SHAKE256 (64 bytes)
    /// over a host-side label and the boundary's height, block hash, note-tree size and root,
    /// and the §17.5 address snapshot (n, locs, aroot). Two honest hosts on one chain give the same
    /// digest; a wallet compares the two hosts' digests before using either answer.
    pub fn epoch_digest(&self, h: u32) -> Option<String> {
        let sn = self.snaps.get(&h)?;
        let root = self.roots.get(&h)?;
        let size = self.sizes.get(&h)?;
        let hash = self.hashes.get(&h).cloned().unwrap_or_default();
        let mut out = [0u8; 64];
        yugen_ref::hash::shake256(
            &[b"yugen-hostd/epoch-digest/v1", &h.to_le_bytes(), hash.as_bytes(), &size.to_le_bytes(), root,
              &sn.n.to_le_bytes(), sn.locs.as_bytes(), sn.aroot.as_bytes()],
            &mut out,
        );
        Some(hx(&out))
    }

    /// M-4: the transparency statement: operator metadata, software, logging policy and the
    /// digest of every sync epoch held. Served by `yh_gettransparency` and written to
    /// `<datadir>/transparency.json` at startup and at each boundary.
    pub fn transparency(&self) -> Value {
        let epochs: Vec<Value> = self
            .snaps
            .values()
            .map(|sn| json!({"h": sn.h, "n": sn.n, "aroot": sn.aroot,
                             "note_root": self.roots.get(&sn.h).map(|r| hx(r)),
                             "digest": self.epoch_digest(sn.h).unwrap_or_else(|| sn.digest.clone())}))
            .collect();
        json!({
            "version": 1,
            "host": self.id,
            "network": self.network,
            "operator": self.operator,
            "software": {"name": env!("CARGO_PKG_NAME"), "version": env!("CARGO_PKG_VERSION"),
                         "commit": env!("YUGEN_HOSTD_COMMIT")},
            "logging": self.logging_policy(),
            "sync_height": self.latest_boundary(),
            "epochs": epochs,
        })
    }

    pub fn write_transparency(&self) {
        let _ = std::fs::create_dir_all(&self.dir);
        let tmp = self.dir.join("transparency.json.tmp");
        if std::fs::write(&tmp, serde_json::to_vec_pretty(&self.transparency()).unwrap()).is_ok() {
            let _ = std::fs::rename(tmp, self.dir.join("transparency.json"));
        }
    }

    pub fn log(&self, line: &str) {
        use std::io::Write;
        if let Ok(mut f) = std::fs::OpenOptions::new().create(true).append(true).open(self.dir.join("queries.log")) {
            let _ = writeln!(f, "{line}");
        }
    }

    /// Clears the chain index (a reorg below the indexed tip: rebuilt from activation).
    fn reset_chain(&mut self) {
        self.tip = None;
        self.hashes.clear();
        self.cms.clear();
        self.sizes.clear();
        self.roots.clear();
        self.slots.clear();
        self.packages.clear();
        self.pkg_counts.clear();
        self.tree_cache = None;
    }

    /// The tree of the first `size` leaves.
    fn tree(&mut self, size: u64) -> Tree {
        if let Some((s, t)) = &self.tree_cache {
            if *s == size {
                return t.clone();
            }
        }
        let mut t = Tree::new();
        for cm in &self.cms[..size as usize] {
            t.append(cm);
        }
        self.tree_cache = Some((size, t.clone()));
        t
    }

    /// Applies one block (the W-RPC-2 shape).
    pub fn apply_block(&mut self, v: &Value) -> Result<(), String> {
        let height = v["height"].as_u64().ok_or("height")? as u32;
        let act = self.activation.ok_or("no activation")?;
        if let Some(n) = v["tree_size_before"].as_u64() {
            if n != self.cms.len() as u64 {
                return Err(format!("block {height}: node tree size {n}, host {}", self.cms.len()));
            }
        }
        if height == act && self.cms.is_empty() {
            // §13.2: the canary commitment is leaf 0, appended in the activation block.
            self.cms.push(yugen_ref::tree::canary_nullifier().1);
        }
        for t in v["transfers"].as_array().cloned().unwrap_or_default() {
            let bytes = hex::decode(t["transfer"].as_str().ok_or("transfer")?).map_err(|e| e.to_string())?;
            let tr = Transfer::decode(&bytes).map_err(|e| format!("transfer decode: {e:?}"))?;
            let ha = resolve_anchor(tr.anchor_slot, height, act).unwrap_or(height.saturating_sub(1) / ANCHOR_GRID * ANCHOR_GRID);
            for j in 1..=2u8 {
                let i = (j - 1) as usize;
                let pos = self.cms.len() as u64;
                self.cms.push(tr.cm[i]);
                // §17.1: one full slot per applied output, in position order, sliced by the day.
                let slot = hostidx::itag_slot(&tr.itag[i], 1, pos, j, &tr.nf[0], &tr.iamt[i]);
                self.slots.entry(height / DAY_BLOCKS).or_default().push(slot);
                // §17.4: an on-chain package, filed under (anchor epoch, pkg_bucket).
                if let Some(p) = &tr.pkg[i] {
                    let bucket = u16::from_le_bytes([p[0], p[1]]);
                    let entry = hostidx::mailbox_entry(&tr.cm[i], j, pos, &p[2..]);
                    let epoch = ha / MAILBOX_EPOCH_BLOCKS;
                    self.packages.entry((epoch, bucket)).or_default().push(PkgItem { entry, nf1: tr.nf[0], pos, height, txid: t["txid"].as_str().unwrap_or("").to_string() });
                    *self.pkg_counts.entry(height / MAILBOX_EPOCH_BLOCKS).or_default() += 1;
                }
            }
        }
        self.sizes.insert(height, self.cms.len() as u64);
        let size = self.cms.len() as u64;
        let root = self.tree(size).root();
        if let Some(r) = v["root_after"].as_str() {
            if unhex::<48>(r)? != root {
                return Err(format!("block {height}: node root differs from the host's"));
            }
        }
        self.roots.insert(height, root);
        self.hashes.insert(height, v["hash"].as_str().unwrap_or("").to_string());
        self.tip = Some(height);
        if height % SYNC_PERIOD_BLOCKS == 0 && height >= act && !self.snaps.contains_key(&height) {
            self.snapshot(height);
        }
        Ok(())
    }

    /// §17.5: the snapshot serving the syncs at boundary `h`: the records accepted before block `h`.
    fn snapshot(&mut self, h: u32) {
        let (prev_n, prev_locs) = match self.snaps.range(..h).next_back() {
            Some((_, s)) => (s.n, unhex::<32>(&s.locs).unwrap()),
            None => (0, [0u8; 32]),
        };
        let n = self.records.len() as u32;
        let appended: Vec<B32> = self.records[prev_n as usize..n as usize].iter().map(|r| r.0).collect();
        let locs = addr_locs(&prev_locs, &appended);
        let leaves: Vec<B32> = self.records.iter().map(|(l, r)| addr_leaf(l, r)).collect();
        let (top, _) = addr_tree(&leaves);
        let aroot = addr_root(h, n, &top, &locs);
        self.snaps.insert(h, Snap { h, n, locs: hx(&locs), aroot: hx(&aroot), digest: String::new(), records: self.records.clone() });
        if let Some(d) = self.epoch_digest(h) {
            self.snaps.get_mut(&h).unwrap().digest = d;
        }
        self.save();
        self.write_transparency();
    }

    /// One poll of the node: index every new block, rewinding on a reorg.
    pub fn poll(&mut self, node: &Rpc) -> Result<u32, String> {
        let info = node.call("yugen_getinfo", json!([])).map_err(|e| e.to_string())?;
        let act = info["activation_height"].as_i64().unwrap_or(-1);
        if act < 0 {
            return Ok(0);
        }
        self.activation = Some(act as u32);
        self.branch_id = info["branch_id"].as_u64().unwrap_or(BRANCH_ID as u64) as u32;
        let node_tip = info["tip_height"].as_u64().unwrap_or(0) as u32;
        if let Some(t) = self.tip {
            let ours = self.hashes.get(&t).cloned().unwrap_or_default();
            let theirs = if t <= node_tip { node.call("getblockhash", json!([t])).map_err(|e| e.to_string())?.as_str().unwrap_or("").to_string() } else { String::new() };
            if ours != theirs {
                self.log(&format!("reorg at {t}: rebuilding the index"));
                self.reset_chain();
            }
        }
        let from = self.tip.map(|t| t + 1).unwrap_or(act as u32);
        let mut n = 0;
        for h in from..=node_tip {
            let v = node.call("yugen_getblockdata", json!([h])).map_err(|e| e.to_string())?;
            self.apply_block(&v)?;
            n += 1;
        }
        Ok(n)
    }

    fn snap(&self, h: u32) -> Result<&Snap, String> {
        self.snaps.get(&h).ok_or_else(|| format!("no snapshot at {h}"))
    }

    /// The entries of snapshot `h`, in index order (`ADDR_ENTRY` bytes each).
    pub fn entries(&self, h: u32) -> Result<Vec<Vec<u8>>, String> {
        let s = self.snap(h)?;
        let leaves: Vec<B32> = s.records.iter().map(|(l, r)| addr_leaf(l, r)).collect();
        let (_, paths) = addr_tree(&leaves);
        Ok(s.records.iter().zip(paths).map(|((l, r), p)| addr_entry(l, r, &p)).collect())
    }

    pub fn latest_boundary(&self) -> Option<u32> {
        self.snaps.keys().next_back().copied()
    }

    // ---------------------------------------------------------------- the RPC methods

    pub fn call(&mut self, method: &str, p: &Value) -> Result<Value, String> {
        let u = |i: usize| p.get(i).and_then(Value::as_u64);
        let s = |i: usize| p.get(i).and_then(Value::as_str).map(String::from);
        match method {
            // M-4: operator metadata, software version and commit, logging policy, epoch digests.
            "yh_gettransparency" => Ok(self.transparency()),
            "yh_getinfo" => Ok(json!({"host": self.id, "network": self.network, "activation": self.activation,
                "tip": self.tip, "sync_height": self.latest_boundary(), "records": self.records.len()})),
            // §17.5, §17.8 item 2: the per-sync roots at the latest boundary, and the count n at every
            // boundary after `since` (oldest first, at most SYNC_MAX_PERIODS).
            "yh_getsyncroots" => {
                let h = self.latest_boundary().ok_or("no boundary yet")?;
                let since = u(0).map(|x| x as u32);
                let counts: Vec<Value> = self
                    .snaps
                    .range(since.map(|x| x + 1).unwrap_or(0)..=h)
                    .map(|(bh, sn)| json!({"h": bh, "n": sn.n}))
                    .collect();
                if counts.len() > SYNC_MAX_PERIODS {
                    return Err("more than 96 periods: catch up in steps".into());
                }
                let sn = self.snap(h)?;
                let root = self.roots.get(&h).ok_or("no note root at the boundary")?;
                Ok(json!({"h": h, "aroot": sn.aroot, "n": sn.n, "locs": sn.locs, "counts": counts,
                          "note_root": hx(root), "tree_size": self.sizes.get(&h)}))
            }
            // §17.5: the locators appended since index `from` (from one host).
            "yh_getlocators" => {
                let h = u(1).ok_or("yh_getlocators [from, h]")? as u32;
                let sn = self.snap(h)?;
                let from = u(0).unwrap_or(0) as usize;
                Ok(json!(sn.records.iter().skip(from).map(|(l, _)| hx(l)).collect::<Vec<_>>()))
            }
            // §12.3, R-27: the first record under a locator stands; byte-identical or with the token.
            "yh_putrecord" => {
                let loc = unhex::<32>(&s(0).ok_or("yh_putrecord [locator, record, del_token?]")?)?;
                let rec = hex::decode(s(1).ok_or("record")?).map_err(|e| e.to_string())?;
                if rec.len() > ADDR_RECORD_MAX {
                    return Err("record too long".into());
                }
                let tok = s(2).map(|t| unhex::<32>(&t)).transpose()?;
                let at = self.records.iter().position(|(l, _)| *l == loc);
                let ok = host_accepts(at.map(|i| &self.records[i].1[..]), &rec, tok.as_ref());
                if !ok {
                    return Ok(json!({"accepted": false}));
                }
                let index = match at {
                    Some(i) => {
                        self.records[i].1 = rec;
                        i
                    }
                    None => {
                        self.records.push((loc, rec));
                        self.records.len() - 1
                    }
                };
                self.save();
                Ok(json!({"accepted": true, "index": index}))
            }
            // §17.6: one DPF share over snapshot h's address table.
            "yh_addrquery" => {
                let h = u(0).ok_or("yh_addrquery [h, key]")? as u32;
                let key = dpf::Key::decode(&hex::decode(s(1).ok_or("key")?).map_err(|e| e.to_string())?).ok_or("bad DPF key")?;
                if key.depth() != ADDR_TREE_DEPTH {
                    return Err("DPF key depth".into());
                }
                let e = self.entries(h)?;
                Ok(json!(hx(&dpf::answer(&key, &e, ADDR_ENTRY))))
            }
            // §17.4: an epoch's package bucket, whole, with each entry's membership path against
            // the note root at boundary `h` (entries applied after `h` wait for the next boundary).
            "yh_getpackages" => {
                let (e, b, h) = (u(0).ok_or("yh_getpackages [epoch, bucket, h]")? as u32, u(1).ok_or("bucket")? as u16, u(2).ok_or("h")? as u32);
                let size = *self.sizes.get(&h).ok_or("unknown boundary")?;
                let items: Vec<PkgItem> = self.packages.get(&(e, b)).cloned().unwrap_or_default().into_iter().filter(|x| x.pos < size).collect();
                let tree = self.tree(size);
                Ok(json!(items
                    .iter()
                    .map(|x| {
                        let path: Vec<u8> = tree.path(x.pos).iter().flatten().flatten().copied().collect();
                        json!({"entry": hx(&x.entry), "nf1": hx(&x.nf1), "height": x.height, "txid": x.txid, "path": hx(&path)})
                    })
                    .collect::<Vec<_>>()))
            }
            "yh_getpkgcount" => Ok(json!(self.pkg_counts.get(&(u(0).unwrap_or(0) as u32)).copied().unwrap_or(0))),
            // §17.1: the slim layer of a slice, and its hash (asked of the other host).
            "yh_getslim" => {
                let sl = self.slots.get(&(u(0).unwrap_or(0) as u32)).cloned().unwrap_or_default();
                Ok(json!(hx(&hostidx::slim_slice(&sl))))
            }
            "yh_getslimhash" => {
                let sl = self.slots.get(&(u(0).unwrap_or(0) as u32)).cloned().unwrap_or_default();
                Ok(json!(hx(&hostidx::slim_hash(&hostidx::slim_slice(&sl)))))
            }
            "yh_getslots" => {
                let sl = self.slots.get(&(u(0).unwrap_or(0) as u32)).cloned().unwrap_or_default();
                Ok(json!(hx(&sl.concat())))
            }
            // L-3: an anti-spam token for one upload over a bucket's free capacity.
            "yh_gettoken" => {
                let mut t = [0u8; 32];
                getrandom::getrandom(&mut t).map_err(|e| e.to_string())?;
                self.tokens.insert(t);
                Ok(json!(hx(&t)))
            }
            // §17.4 host rule: accepted only after the transfer is applied and if H_leaf(cm) is the
            // leaf at `position`; filed under (epoch of the applying block, bucket).
            "yh_uploadmailbox" => {
                let bucket = u(0).ok_or("yh_uploadmailbox [bucket, entry, token?]")? as u32;
                let entry = hex::decode(s(1).ok_or("entry")?).map_err(|e| e.to_string())?;
                if bucket >= 1 << BUCKET_BITS || (entry.len() != 54 + PKG_SHORT_LEN && entry.len() != 54 + PKG_LONG_LEN) {
                    return Ok(json!({"accepted": false, "reason": "malformed"}));
                }
                let cm: B48 = entry[..48].try_into().unwrap();
                let pos = u64::from_le_bytes([entry[49], entry[50], entry[51], entry[52], entry[53], 0, 0, 0]);
                let leaf_ok = (pos as usize) < self.cms.len() && leaf_hash(&self.cms[pos as usize]) == leaf_hash(&cm);
                if !leaf_ok {
                    return Ok(json!({"accepted": false, "reason": "not-applied"}));
                }
                let applied_at = self.sizes.iter().find(|(_, s)| **s > pos).map(|(h, _)| *h).unwrap_or(0);
                let k = (applied_at / MAILBOX_EPOCH_BLOCKS, bucket);
                let used = self.mailbox.get(&k).map_or(0, Vec::len);
                if used >= MAILBOX_FREE {
                    let tok = s(2).map(|t| unhex::<32>(&t)).transpose()?;
                    if !tok.map_or(false, |t| self.tokens.remove(&t)) {
                        return Ok(json!({"accepted": false, "reason": "token"}));
                    }
                }
                if self.mailbox.get(&k).map_or(false, |v| v.contains(&entry)) {
                    return Ok(json!({"accepted": true, "duplicate": true}));
                }
                self.mailbox.entry(k).or_default().push(entry);
                self.save();
                Ok(json!({"accepted": true, "epoch": k.0}))
            }
            "yh_fetchmailbox" => {
                let k = (u(0).ok_or("yh_fetchmailbox [epoch, bucket, page]")? as u32, u(1).ok_or("bucket")? as u32);
                let page = u(2).unwrap_or(0) as usize;
                let v = self.mailbox.get(&k).cloned().unwrap_or_default();
                let items: Vec<String> = v.iter().skip(page * MAILBOX_PAGE).take(MAILBOX_PAGE).map(|x| hx(x)).collect();
                Ok(json!({"entries": items, "pages": v.len().div_ceil(MAILBOX_PAGE).max(1)}))
            }
            _ => Err(format!("unknown method {method}")),
        }
    }
}
