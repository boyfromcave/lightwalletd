//! M-5 (no key material in logs by default) and M-4 (transparency statement) through the real
//! HTTP server: a full two-host query session, then the logs are searched for every DPF key,
//! seed, record, locator and mailbox entry the session sent.

use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::{Arc, Mutex};

use serde_json::{json, Value};
use yugen_hostd::dpf;
use yugen_hostd::host::Host;
use yugen_ref::hostidx::check_addr_answer;
use yugen_ref::params::*;

fn tmp(n: &str) -> std::path::PathBuf {
    let d = std::env::temp_dir().join(format!("yugen-hostd-priv-{n}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&d);
    d
}

fn start(id: &str, dir: std::path::PathBuf, debug: bool) -> (String, std::path::PathBuf, Arc<Mutex<Host>>) {
    let mut h = Host::open(id, dir.clone(), NET_REGTEST);
    h.activation = Some(112);
    h.debug_log_queries = debug;
    h.operator = json!({"name": format!("operator {id}"), "jurisdiction": "test", "contact": "ops@example.invalid"});
    let l = TcpListener::bind("127.0.0.1:0").unwrap();
    let a = l.local_addr().unwrap().to_string();
    let h = Arc::new(Mutex::new(h));
    let h2 = h.clone();
    std::thread::spawn(move || yugen_hostd::server::serve(l, h2));
    (a, dir, h)
}

fn call(addr: &str, method: &str, params: Value) -> Value {
    let body = json!({"method": method, "params": params, "id": 1}).to_string();
    let mut s = TcpStream::connect(addr).unwrap();
    write!(s, "POST / HTTP/1.1\r\nContent-Length: {}\r\n\r\n{}", body.len(), body).unwrap();
    let mut out = String::new();
    s.read_to_string(&mut out).unwrap();
    let v: Value = serde_json::from_str(out.split("\r\n\r\n").nth(1).unwrap()).unwrap();
    assert!(v["error"].is_null(), "{method}: {}", v["error"]);
    v["result"].clone()
}

fn session(debug: bool, tag: &str) -> (Vec<String>, String, String, Value, Value) {
    let (aa, da, hosta) = start("A", tmp(&format!("{tag}a")), debug);
    let (ab, db, hostb) = start("B", tmp(&format!("{tag}b")), debug);
    let mut secrets = Vec::new();
    for i in 1..=3u8 {
        let loc = hex::encode([i ^ 0x5a; 32]);
        let mut r = vec![i; 1700];
        r[0] = 0xd0;
        r[1..33].copy_from_slice(&[0xc3 ^ i; 32]);
        let rec = hex::encode(&r);
        for a in [&aa, &ab] {
            assert_eq!(call(a, "yh_putrecord", json!([loc, rec, null]))["accepted"], true);
        }
        secrets.push(loc);
        secrets.push(hex::encode(&r[1..33]));
    }
    for h in [&hosta, &hostb] {
        let mut h = h.lock().unwrap();
        for height in 112..=128u32 {
            h.apply_block(&json!({"height": height, "hash": format!("{height:064x}"), "transfers": []})).unwrap();
        }
    }
    let ra = call(&aa, "yh_getsyncroots", json!([]));
    let (h, n) = (ra["h"].as_u64().unwrap() as u32, ra["n"].as_u64().unwrap() as u32);
    let locs: [u8; 32] = hex::decode(ra["locs"].as_str().unwrap()).unwrap().try_into().unwrap();
    let aroot: [u8; 32] = hex::decode(ra["aroot"].as_str().unwrap()).unwrap().try_into().unwrap();
    for i in 0..n {
        let (ka, kb) = dpf::gen_random(i as u64, ADDR_TREE_DEPTH);
        let (ea, eb) = (hex::encode(ka.encode()), hex::encode(kb.encode()));
        let x: Vec<u8> = hex::decode(call(&aa, "yh_addrquery", json!([h, ea])).as_str().unwrap()).unwrap().iter()
            .zip(hex::decode(call(&ab, "yh_addrquery", json!([h, eb])).as_str().unwrap()).unwrap())
            .map(|(p, q)| p ^ q).collect();
        assert!(check_addr_answer(&x, i, h, n, &locs, &aroot).is_some());
        // the whole key, its seed (bytes 1..33) and a correction-word window
        for e in [&ea, &eb] {
            secrets.push(e.clone());
            secrets.push(e[2..66].to_string());
            secrets.push(e[66..130.min(e.len())].to_string());
        }
    }
    let ta = call(&aa, "yh_gettransparency", json!([]));
    let tb = call(&ab, "yh_gettransparency", json!([]));
    let la = std::fs::read_to_string(da.join("queries.log")).unwrap();
    let lb = std::fs::read_to_string(db.join("queries.log")).unwrap();
    (secrets, la, lb, ta, tb)
}

#[test]
fn default_logs_hold_no_key_material() {
    let (secrets, la, lb, ta, _) = session(false, "def");
    for log in [&la, &lb] {
        assert!(log.contains("yh_addrquery"), "method names are logged");
        for s in &secrets {
            assert!(!log.contains(s.as_str()), "secret {}... in the default log", &s[..16]);
        }
        // no hex run of 32+ characters at all, no parameters, no peer address
        for line in log.lines() {
            assert!(line.chars().all(|c| c.is_ascii_alphanumeric() || c == '_'), "log line with more than a method: {line}");
        }
        assert!(!log.contains("127.0.0.1"));
    }
    assert_eq!(ta["logging"]["logs_dpf_keys"], false);
    assert_eq!(ta["logging"]["debug_log_queries"], false);
}

#[test]
fn debug_flag_logs_keys_and_says_so() {
    // the control: the same grep finds the keys when the off-by-default flag is on
    let (secrets, la, _, ta, _) = session(true, "dbg");
    assert!(secrets.iter().any(|s| la.contains(s.as_str())), "debug log carries the keys");
    assert_eq!(ta["logging"]["logs_dpf_keys"], true);
}

#[test]
fn transparency_matches_across_hosts() {
    let (_, _, _, ta, tb) = session(false, "tr");
    assert_eq!(ta["software"]["name"], "yugen-hostd");
    assert!(!ta["software"]["commit"].as_str().unwrap().is_empty());
    assert_eq!(ta["operator"]["name"], "operator A");
    let (ea, eb) = (ta["epochs"].as_array().unwrap(), tb["epochs"].as_array().unwrap());
    assert_eq!(ea.iter().map(|e| e["h"].as_u64().unwrap()).collect::<Vec<_>>(), vec![112, 128]);
    assert_eq!(ea[1]["digest"].as_str().unwrap().len(), 128);
    assert_ne!(ea[0]["digest"], ea[1]["digest"]);
    assert_eq!(ea, eb, "two honest hosts give one digest per epoch");
    // the static file says the same
    let dir = std::env::temp_dir().join(format!("yugen-hostd-priv-tra-{}", std::process::id()));
    let f: Value = serde_json::from_slice(&std::fs::read(dir.join("transparency.json")).unwrap()).unwrap();
    assert_eq!(f["epochs"], ta["epochs"]);
}
