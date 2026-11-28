//! `yugen-hostd`: one Yugen mailbox / retrieval host.
//!
//!   yugen-hostd -id=A -datadir=D -listen=127.0.0.1:PORT -node=127.0.0.1:RPC -rpcuser=U -rpcpassword=P
//!               [-network=regtest|testnet|main] [-poll_ms=500] [-operator_file=F.json]
//!               [-debug_log_queries=1]   (M-5: logs DPF keys and query parameters; devnet only)

use std::net::TcpListener;
use std::sync::{Arc, Mutex};

use yugen_hostd::host::Host;
use yugen_hostd::rpc::Rpc;

fn main() {
    let mut o = std::collections::HashMap::new();
    for a in std::env::args().skip(1) {
        let kv = a.trim_start_matches('-');
        let (k, v) = kv.split_once('=').unwrap_or((kv, ""));
        o.insert(k.to_string(), v.to_string());
    }
    let g = |k: &str, d: &str| o.get(k).cloned().unwrap_or_else(|| d.to_string());
    let network = match g("network", "regtest").as_str() {
        "main" => 0,
        "testnet" => 1,
        _ => 2,
    };
    let node = Rpc::new(&g("node", "127.0.0.1:18232"), &g("rpcuser", ""), &g("rpcpassword", ""));
    let mut h0 = Host::open(&g("id", "A"), g("datadir", ".").into(), network);
    h0.debug_log_queries = matches!(g("debug_log_queries", "0").as_str(), "1" | "true");
    if h0.debug_log_queries {
        let w = "WARNING: -debug_log_queries is on: DPF keys and query parameters are written to queries.log. \
                 Two-server PIR privacy (M-5) does not hold for this host. Devnet only.";
        eprintln!("{w}");
        h0.log(w);
    }
    if let Some(f) = o.get("operator_file") {
        let b = std::fs::read(f).unwrap_or_else(|e| panic!("operator_file {f}: {e}"));
        h0.operator = serde_json::from_slice(&b).unwrap_or_else(|e| panic!("operator_file {f}: {e}"));
    }
    h0.write_transparency();
    let host = Arc::new(Mutex::new(h0));
    let listener = TcpListener::bind(g("listen", "127.0.0.1:19501")).expect("listen");
    eprintln!("yugen-hostd {} on {}", g("id", "A"), listener.local_addr().unwrap());
    let h = host.clone();
    std::thread::spawn(move || yugen_hostd::server::serve(listener, h));
    let poll = std::time::Duration::from_millis(g("poll_ms", "500").parse().unwrap_or(500));
    loop {
        let r = host.lock().unwrap().poll(&node);
        if let Err(e) = r {
            host.lock().unwrap().log(&format!("poll error: {e}"));
        }
        std::thread::sleep(poll);
    }
}
