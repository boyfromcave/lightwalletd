//! JSON-RPC 1.0 over HTTP/1.1 (std only). Every request's method name is logged to
//! `<datadir>/queries.log`; the parameters (DPF keys and the rest) only under the off-by-default
//! `-debug_log_queries=1` (M-5; the devnet's L-4 share inspection). Peer addresses are never logged.

use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::{Arc, Mutex};

use serde_json::{json, Value};

use crate::host::Host;

fn handle(mut s: TcpStream, host: &Arc<Mutex<Host>>) -> std::io::Result<()> {
    let mut r = BufReader::new(s.try_clone()?);
    let mut len = 0usize;
    let mut line = String::new();
    r.read_line(&mut line)?;
    loop {
        let mut l = String::new();
        if r.read_line(&mut l)? == 0 || l == "\r\n" || l == "\n" {
            break;
        }
        if let Some((k, v)) = l.split_once(':') {
            if k.eq_ignore_ascii_case("content-length") {
                len = v.trim().parse().unwrap_or(0);
            }
        }
    }
    let mut body = vec![0u8; len.min(16 << 20)];
    r.read_exact(&mut body)?;
    let req: Value = serde_json::from_slice(&body).unwrap_or(Value::Null);
    let method = req["method"].as_str().unwrap_or("").to_string();
    let params = req["params"].clone();
    let res = {
        let mut h = host.lock().unwrap();
        h.log_request(&method, &params);
        h.call(&method, &params)
    };
    let out = match res {
        Ok(v) => json!({"result": v, "error": null, "id": req["id"]}),
        Err(e) => json!({"result": null, "error": {"code": -1, "message": e}, "id": req["id"]}),
    }
    .to_string();
    write!(s, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}", out.len(), out)
}

pub fn serve(listener: TcpListener, host: Arc<Mutex<Host>>) {
    for c in listener.incoming().flatten() {
        let h = host.clone();
        std::thread::spawn(move || {
            let _ = handle(c, &h);
        });
    }
}
