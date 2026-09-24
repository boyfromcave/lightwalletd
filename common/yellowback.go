// Yellowback (YED) support: the node-side half of the YellowbackStreamer service
// (docs/plans/yellowback-lightwalletd-plan.md section 4.2, re-ported onto zcash/lightwalletd 0.4.6 in section 10).
//
// This file is the ONLY way a yed_* RPC is reached from this server. CallYed consults an
// allow-list of read-only, node-context methods; anything else — every wallet RPC, the
// miner-local yed_setquote, the RPC-auth-only yed_addattestation / yed_signattestation — is
// unreachable by construction. ProbeYellowback runs once at startup and decides whether the
// service is registered at all (plan D-L-5).
package common

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ServerVersion is reported by GetYellowbackInfo.serverVersion. LightdInfo.version (the old
// service) keeps the baseline's "0.1-yec-lightwalletd" string untouched.
const ServerVersion = "0.2-yec-lightwalletd"

// KnownRPCVersion is the node contract this server was written against
// (ycash-dd/doc/yellowback-rpc-contract.json "rpcversion").
const KnownRPCVersion = 3

// YedMethods is the allow-list: every node RPC the service may call, and nothing else.
// The offline test asserts this set against the contract: every yed_* method there is either
// here or in NotOffered, so a new node RPC cannot be forgotten silently.
var YedMethods = map[string]bool{
	"yed_getinfo":                true,
	"yed_getprice":               true,
	"yed_getstats":               true,
	"yed_getactivation":          true,
	"yed_gettxinfo":              true,
	"yed_validaterawtransaction": true,
	"yed_decodepayload":          true,
	"yed_getvault":               true,
	"yed_listvaults":             true,
	"yed_listclaimable":          true,
	"yed_getnotice":              true,
	"yed_estimatecollateral":     true,
	"yed_estimatefee":            true,
	"yed_getfeepayee":            true,
	"yed_buildbundle":            true,
	"yed_getselection":           true,
	"yed_getattestations":        true,
	"yed_listattestors":          true,
	"yed_listtokens":             true,
}

// NotOffered lists the contract's other node-context RPCs and why the service does not proxy
// them; wallet-context RPCs need the node's keys and are never candidates. Adding a method to
// the service means moving it from here to YedMethods, a proto change and a row in the plan.
var NotOffered = map[string]string{
	"yed_getstatehash":       "test and operator tooling",
	"yed_gethistory":         "test and operator tooling (the model rebuild)",
	"yed_getblockverdict":    "operator tooling",
	"yed_gettag":             "operator tooling; yed_getprice carries the tip's tag",
	"yed_listminers":         "operator tooling",
	"yed_setquote":           "miner-local state",
	"yed_addattestation":     "RPC-auth only (plan v3 section 4.5)",
	"yed_signattestation":    "RPC-auth only; signs with a wallet key",
	"yed_getnewaddress":      "wallet",
	"yed_validateaddress":    "wallet context in the node; clients decode addresses locally (plan section 5)",
	"yed_getbalance":         "wallet",
	"yed_listunspent":        "wallet; GetAddressTokens (Phase L2) is the node-context answer",
	"yed_mint":               "wallet",
	"yed_send":               "wallet",
	"yed_sendmany":           "wallet",
	"yed_redeem":             "wallet",
	"yed_claim":              "wallet",
	"yed_sweep":              "wallet",
	"yed_claimnotice":        "wallet",
	"yed_sweepcarriers":      "wallet",
	"yed_registerattestor":   "wallet",
	"yed_withdrawbond":       "wallet",
	"yed_revive":             "wallet",
	"yed_reportequivocation": "wallet",
	"yed_listpositions":      "wallet",
	"yed_listtransactions":   "wallet",
	"yed_lockcoins":          "wallet",
	"yed_unlockcoin":         "wallet",
	"yed_estimatesend":       "wallet",
}

// The node is reached through the package's RawRequest function variable (common.go), which
// main assigns from the btcd client and tests replace with a stub — this tree's own pattern
// (common_test.go, frontend_test.go, darkside.go).

// YellowbackCapability is what ProbeYellowback learned about the node.
type YellowbackCapability struct {
	Enabled    bool   // "yellowback" is among the node's experimental features
	RPCVersion int64  // yed_getinfo.rpcversion
	Network    string // yed_getinfo.network
}

// ProbeYellowback asks the node whether Yellowback is enabled and which contract it speaks.
// A stock node (no "yellowback" feature) yields Enabled=false and no error; a transport
// failure or an unknown rpcversion is an error. The caller registers the service only on
// (Enabled && err == nil).
func ProbeYellowback() (YellowbackCapability, error) {
	var capability YellowbackCapability
	raw, err := RawRequest("getexperimentalfeatures", nil)
	if err != nil {
		return capability, fmt.Errorf("getexperimentalfeatures: %v", err)
	}
	var features []string
	if err := json.Unmarshal(raw, &features); err != nil {
		return capability, fmt.Errorf("getexperimentalfeatures: unexpected result %s", string(raw))
	}
	for _, f := range features {
		if f == "yellowback" {
			capability.Enabled = true
		}
	}
	if !capability.Enabled {
		return capability, nil
	}
	raw, err = RawRequest("yed_getinfo", nil)
	if err != nil {
		return capability, fmt.Errorf("yed_getinfo: %v", err)
	}
	var info struct {
		RPCVersion int64  `json:"rpcversion"`
		Network    string `json:"network"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return capability, fmt.Errorf("yed_getinfo: unexpected result")
	}
	capability.RPCVersion, capability.Network = info.RPCVersion, info.Network
	if info.RPCVersion != KnownRPCVersion {
		return capability, fmt.Errorf("node speaks yed rpcversion %d, this server knows %d", info.RPCVersion, KnownRPCVersion)
	}
	return capability, nil
}

// CallYed forwards one allow-listed yed_* RPC. params are already-encoded JSON values.
// Errors are gRPC statuses (plan section 3.3): a node RPC error is FAILED_PRECONDITION carrying
// the node's message verbatim (the contract's error identifiers are its first word); a
// transport failure is UNAVAILABLE; a method outside the allow-list is INTERNAL, because that
// is a programming error in this server, never a client's doing.
func CallYed(method string, params ...json.RawMessage) (json.RawMessage, error) {
	if !YedMethods[method] {
		return nil, status.Errorf(codes.Internal, "%s is not an allow-listed Yellowback RPC", method)
	}
	raw, err := RawRequest(method, params)
	if err == nil {
		return raw, nil
	}
	// btcd's rpcclient renders a JSON-RPC error as "code: message" (btcjson.RPCError.Error);
	// nothing outside vendor/ imports btcjson in this tree, and neither does this.
	if parts := strings.SplitN(err.Error(), ":", 2); len(parts) == 2 {
		if _, convErr := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 32); convErr == nil {
			return nil, status.Error(codes.FailedPrecondition, strings.TrimSpace(parts[1]))
		}
	}
	return nil, status.Errorf(codes.Unavailable, "node: %v", err)
}

// JSON helpers for building params: the node's RPCs take strings for hex/txid/address and
// numbers for heights and amounts. JSONString quotes safely (a client value never reaches the
// node unescaped — the baseline's GetAddressTxids splices strings, plan F-5; this does not).
func JSONString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return json.RawMessage(b)
}

// JSONNumber encodes an integer parameter.
func JSONNumber(n int64) json.RawMessage {
	return json.RawMessage(strconv.FormatInt(n, 10))
}
