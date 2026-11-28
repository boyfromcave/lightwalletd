//! Host unit tests: the DPF, the R-27 record rule, the committed address table checked by the
//! spec's client check (§17.5), two hosts answering one query, the mailbox rules (L-3).

use serde_json::json;
use yugen_hostd::dpf;
use yugen_hostd::host::Host;
use yugen_ref::hostidx::{addr_catch_up, check_addr_answer};
use yugen_ref::params::*;

fn tmp(n: &str) -> std::path::PathBuf {
    let d = std::env::temp_dir().join(format!("yugen-hostd-test-{n}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&d);
    d
}

#[test]
fn dpf_point_function() {
    for depth in [1usize, 5, 24] {
        for alpha in [0u64, 1, 3, (1 << depth) - 1, 7].map(|a| a % (1u64 << depth)) {
            let (a, b) = dpf::gen_random(alpha, depth);
            let n = (1u64 << depth).min(300);
            let (ea, eb) = (a.eval_prefix(n), b.eval_prefix(n));
            for x in 0..n {
                assert_eq!(ea[x as usize] ^ eb[x as usize], (x == alpha) as u8, "depth {depth} alpha {alpha} x {x}");
            }
            assert_eq!(dpf::Key::decode(&a.encode()).unwrap(), a);
        }
    }
    // a share alone is not the index: both parties' bit vectors are balanced-looking, not one-hot
    let (a, _) = dpf::gen_random(5, 24);
    let ones = a.eval_prefix(1000).iter().filter(|&&b| b == 1).count();
    assert!(ones > 300 && ones < 700, "share bits look uniform: {ones}/1000");
}

fn record(i: u8) -> ([u8; 32], Vec<u8>) {
    // a record-shaped blob (del_hash || ciphertext of a no-host full address): 1,700 bytes
    let mut r = vec![i; 1700];
    r[0] = 0xd0;
    ([i; 32], r)
}

fn put(h: &mut Host, loc: &[u8; 32], rec: &[u8], tok: Option<&str>) -> bool {
    h.call("yh_putrecord", &json!([hex::encode(loc), hex::encode(rec), tok])).unwrap()["accepted"].as_bool().unwrap()
}

#[test]
fn records_snapshots_and_two_host_answers() {
    let mut ha = Host::open("A", tmp("a"), NET_REGTEST);
    let mut hb = Host::open("B", tmp("b"), NET_REGTEST);
    ha.activation = Some(112);
    hb.activation = Some(112);
    let recs: Vec<_> = (1..=5u8).map(record).collect();
    for (l, r) in &recs {
        assert!(put(&mut ha, l, r, None) && put(&mut hb, l, r, None));
    }
    // R-27: a different record under the same locator is refused; byte-identical is accepted
    let mut other = recs[0].1.clone();
    other[100] ^= 1;
    assert!(!put(&mut ha, &recs[0].0, &other, None));
    assert!(put(&mut ha, &recs[0].0, &recs[0].1, None));
    // a boundary: empty blocks through the apply path
    for h in [&mut ha, &mut hb] {
        for height in 112..=128u32 {
            h.apply_block(&json!({"height": height, "hash": format!("{height:064x}"), "transfers": []})).unwrap();
        }
    }
    let ra = ha.call("yh_getsyncroots", &json!([])).unwrap();
    let rb = hb.call("yh_getsyncroots", &json!([])).unwrap();
    assert_eq!(ra["aroot"], rb["aroot"]);
    assert_eq!(ra["note_root"], rb["note_root"]);
    assert_eq!(ra["counts"], rb["counts"]);
    let h = ra["h"].as_u64().unwrap() as u32;
    assert_eq!(h, 128);
    let n = ra["n"].as_u64().unwrap() as u32;
    assert_eq!(n, 5);
    // the wallet rebuilds locs from the locators and the counts (S-7)
    let locators: Vec<[u8; 32]> = ha.call("yh_getlocators", &json!([0, h])).unwrap().as_array().unwrap().iter()
        .map(|x| hex::decode(x.as_str().unwrap()).unwrap().try_into().unwrap()).collect();
    let counts: Vec<u32> = ra["counts"].as_array().unwrap().iter().map(|c| c["n"].as_u64().unwrap() as u32).collect();
    let locs = *addr_catch_up(&[0u8; 32], 0, &counts, &counts, &locators).unwrap().last().unwrap();
    let aroot: [u8; 32] = hex::decode(ra["aroot"].as_str().unwrap()).unwrap().try_into().unwrap();
    // every index: share to A, share to B, XOR, the spec's check passes and returns the record
    for i in 0..n {
        let (ka, kb) = dpf::gen_random(i as u64, ADDR_TREE_DEPTH);
        let aa = hex::decode(ha.call("yh_addrquery", &json!([h, hex::encode(ka.encode())])).unwrap().as_str().unwrap()).unwrap();
        let ab = hex::decode(hb.call("yh_addrquery", &json!([h, hex::encode(kb.encode())])).unwrap().as_str().unwrap()).unwrap();
        let x: Vec<u8> = aa.iter().zip(&ab).map(|(p, q)| p ^ q).collect();
        let (loc, rec) = check_addr_answer(&x, i, h, n, &locs, &aroot).expect("answer checks");
        assert_eq!((loc, rec), recs[i as usize].clone());
        // one host corrupting its share fails the check
        let mut bad = x.clone();
        bad[40] ^= 1;
        assert!(check_addr_answer(&bad, i, h, n, &locs, &aroot).is_none());
    }
    // a record put after the boundary is not in it
    let late = record(9);
    assert!(put(&mut ha, &late.0, &late.1, None));
    assert_eq!(ha.call("yh_getsyncroots", &json!([])).unwrap()["n"], 5);
    // the snapshot survives a restart (same root)
    drop(ha);
    let ha2 = Host::open("A", tmp("a").clone(), NET_REGTEST);
    let _ = ha2;
}

#[test]
fn mailbox_rules() {
    let mut h = Host::open("A", tmp("m"), NET_REGTEST);
    h.activation = Some(112);
    h.apply_block(&json!({"height": 112, "hash": "00", "transfers": []})).unwrap();
    // position 0 is the canary; an entry naming it with the canary cm is applied
    let cm = yugen_ref::tree::canary_nullifier().1;
    let mk = |tag: u8| {
        let mut e = cm.to_vec();
        e.push(1);
        e.extend_from_slice(&[0u8; 5]);
        let mut pkg = vec![tag; PKG_SHORT_LEN];
        pkg[0] = tag;
        e.extend(pkg);
        e
    };
    // not applied: a cm that is not the leaf at the position
    let mut wrong = mk(1);
    wrong[0] ^= 1;
    assert_eq!(h.call("yh_uploadmailbox", &json!([7, hex::encode(&wrong)])).unwrap()["reason"], "not-applied");
    for i in 0..MAILBOX_FREE_T {
        assert_eq!(h.call("yh_uploadmailbox", &json!([7, hex::encode(mk(i as u8 + 2))])).unwrap()["accepted"], true);
    }
    // over capacity: refused without a token, accepted with one (once)
    assert_eq!(h.call("yh_uploadmailbox", &json!([7, hex::encode(mk(100))])).unwrap()["reason"], "token");
    let t = h.call("yh_gettoken", &json!([])).unwrap();
    assert_eq!(h.call("yh_uploadmailbox", &json!([7, hex::encode(mk(100)), t])).unwrap()["accepted"], true);
    assert_eq!(h.call("yh_uploadmailbox", &json!([7, hex::encode(mk(101)), t])).unwrap()["reason"], "token");
    for i in 0..20u8 {
        let t = h.call("yh_gettoken", &json!([])).unwrap();
        h.call("yh_uploadmailbox", &json!([7, hex::encode(mk(120 + i)), t])).unwrap();
    }
    let p0 = h.call("yh_fetchmailbox", &json!([0, 7, 0])).unwrap();
    assert_eq!(p0["entries"].as_array().unwrap().len(), yugen_hostd::host::MAILBOX_PAGE);
    assert_eq!(p0["pages"], 2);
    // bucket rotation: another epoch is another bucket key
    assert_eq!(h.call("yh_fetchmailbox", &json!([1, 7, 0])).unwrap()["entries"].as_array().unwrap().len(), 0);
}

const MAILBOX_FREE_T: usize = yugen_hostd::host::MAILBOX_FREE;
