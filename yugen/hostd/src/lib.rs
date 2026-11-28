//! The Yugen mailbox / retrieval host (spec 0.5.2 §17; beta4 Yugen addendum §3.4, L-2..L-4).
//! One process is one host of a pair; run two with separate data directories (the non-collusion
//! assumption of §17.6, SPEC-183, needs separate operators; on one machine the test checks process
//! and data separation only). Interface: JSON-RPC over HTTP, methods `yh_*` (docs/yugen-host.md).

pub mod dpf;
pub mod host;
pub mod rpc;
pub mod server;
