// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

// Offline tests for the light attestor's calls (beta4 item 3d, B-T1; workspace
// docs/plans/beta4/price-feed.md section 2.5 B-3): SubmitAttestation's guards against the
// contract's yed_listattestors (one member, seq 1) and yed_addattestation answers, GetTag,
// GetTxOut and GetMempoolTxids.
package frontend

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zcash/lightwalletd/common"
	"github.com/zcash/lightwalletd/walletrpc"
)

// attestFrame is a 74-byte statement: seq, price, citedHeight little-endian, then a signature
// (any 64 bytes: the fake node does not verify; fill makes two frames of one seq differ).
func attestFrame(seq uint16, price, cited uint32, fill byte) []byte {
	f := make([]byte, attestationSize)
	binary.LittleEndian.PutUint16(f[0:], seq)
	binary.LittleEndian.PutUint32(f[2:], price)
	binary.LittleEndian.PutUint32(f[6:], cited)
	for i := 10; i < attestationSize; i++ {
		f[i] = fill
	}
	return f
}

func countCalls(node *fakeNode, method string) int {
	n := 0
	for _, m := range node.calls {
		if m == method {
			n++
		}
	}
	return n
}

func wantCode(t *testing.T, what string, err error, code codes.Code, guard string) {
	t.Helper()
	st, _ := status.FromError(err)
	if st.Code() != code {
		t.Fatalf("%s: want %v, got %v", what, code, err)
	}
	if guard != "" && !strings.HasPrefix(st.Message(), guard+":") {
		t.Fatalf("%s: the message must name the %q guard first: %q", what, guard, st.Message())
	}
}

// B-3: a wrong size is refused before any node call.
func TestSubmitAttestationRefusesWrongSize(t *testing.T) {
	svc, node := newService(t)
	ctx := peerCtx(net.IPv4(10, 0, 0, 1))
	for _, n := range []int{0, 1, 73, 75, 148} {
		_, err := svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: make([]byte, n)})
		wantCode(t, "size", err, codes.InvalidArgument, "size")
	}
	_, err := svc.SubmitAttestation(ctx, nil)
	wantCode(t, "nil", err, codes.InvalidArgument, "size")
	if len(node.calls) != 0 {
		t.Fatalf("a wrong size reached the node: %v", node.calls)
	}
}

// B-3: a seq that yed_listattestors does not list is refused without yed_addattestation.
func TestSubmitAttestationRefusesNonMember(t *testing.T) {
	svc, node := newService(t)
	ctx := peerCtx(net.IPv4(10, 0, 0, 1))
	for _, seq := range []uint16{0, 2, 65535} {
		_, err := svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: attestFrame(seq, 1985000, 329, 0xaa)})
		wantCode(t, "non-member", err, codes.PermissionDenied, "seq")
	}
	if countCalls(node, "yed_addattestation") != 0 {
		t.Fatalf("a non-member's frame reached yed_addattestation: %v", node.calls)
	}
	// The contract's member, seq 1, passes the member guard and gets the node's answer.
	got, err := svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: attestFrame(1, 1985000, 329, 0xaa)})
	if err != nil {
		t.Fatal(err)
	}
	assertMatchesContract(t, "yed_addattestation", got, node.contract.returns(t, "yed_addattestation"))
	assertNonZeroFieldsPresent(t, "yed_addattestation", got, node.contract.returns(t, "yed_addattestation"))
	if p := node.params["yed_addattestation"]; len(p) != 1 || string(p[0]) != `"`+hex.EncodeToString(attestFrame(1, 1985000, 329, 0xaa))+`"` {
		t.Fatalf("yed_addattestation params: %s", p)
	}
	// One yed_listattestors answered every call above: the list is cached (no tip known here, so
	// the 60-second bound applies).
	if n := countCalls(node, "yed_listattestors"); n != 1 {
		t.Fatalf("yed_listattestors calls = %d, want 1 (cached)", n)
	}
}

// B-3: above the per-seq rate a new frame is refused; a byte-identical resubmit is answered from
// memory and does not count; the window is in citedHeight from the last accepted statement.
func TestSubmitAttestationRateWindow(t *testing.T) {
	svc, node := newService(t)
	ctx := peerCtx(net.IPv4(10, 0, 0, 1))
	first := attestFrame(1, 1985000, 329, 0xaa)
	accepted, err := svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: first})
	if err != nil || !accepted.Accepted {
		t.Fatalf("first statement: %v %v", accepted, err)
	}
	adds := countCalls(node, "yed_addattestation")
	// A new frame of seq 1 within 5 blocks of citedHeight (and one citing an older height).
	for _, cited := range []uint32{329, 330, 333, 300} {
		_, err := svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: attestFrame(1, 1990000, cited, 0xbb)})
		wantCode(t, "rate", err, codes.ResourceExhausted, "rate")
	}
	// The identical resubmit: the remembered answer, no node call, and it does not count.
	for i := 0; i < 3; i++ {
		again, err := svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: append([]byte(nil), first...)})
		if err != nil {
			t.Fatalf("identical resubmit %d: %v", i, err)
		}
		if again.Seq != accepted.Seq || again.CitedHeight != accepted.CitedHeight || again.PoolSize != accepted.PoolSize || !again.Accepted {
			t.Fatalf("identical resubmit %d: %v, want %v", i, again, accepted)
		}
	}
	if countCalls(node, "yed_addattestation") != adds {
		t.Fatalf("refused or identical frames reached yed_addattestation: %v", node.calls)
	}
	// 5 blocks on, the next statement is accepted.
	if _, err := svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: attestFrame(1, 1990000, 334, 0xcc)}); err != nil {
		t.Fatalf("a statement 5 blocks on: %v", err)
	}
	if countCalls(node, "yed_addattestation") != adds+1 {
		t.Fatalf("the statement 5 blocks on did not reach the node")
	}
	// The window is the flag's.
	svc.SetAttestSeqBlocks(1)
	if _, err := svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: attestFrame(1, 1990000, 335, 0xdd)}); err != nil {
		t.Fatalf("window 1: %v", err)
	}
}

// A statement the node refuses does not move the window (a forged frame cannot block the seat).
func TestSubmitAttestationNodeRefusalDoesNotCount(t *testing.T) {
	svc, node := newService(t)
	ctx := peerCtx(net.IPv4(10, 0, 0, 1))
	node.fail["yed_addattestation"] = errors.New("-26: attest-invalid: the signature does not verify")
	_, err := svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: attestFrame(1, 1985000, 329, 0x01)})
	wantCode(t, "node refusal", err, codes.FailedPrecondition, "attest-invalid")
	delete(node.fail, "yed_addattestation")
	if _, err := svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: attestFrame(1, 1985000, 329, 0x02)}); err != nil {
		t.Fatalf("the seat's own statement after a refused forgery: %v", err)
	}
}

// The per-IP limiter applies to SubmitAttestation too (a whole token).
func TestSubmitAttestationPerIPLimit(t *testing.T) {
	svc, node := newService(t)
	now := time.Unix(1700000000, 0)
	svc.limiter = newRateLimiter(1, time.Hour)
	svc.limiter.now = func() time.Time { return now }
	ctx := peerCtx(net.IPv4(10, 0, 0, 1))
	if _, err := svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: attestFrame(1, 1985000, 329, 0xaa)}); err != nil {
		t.Fatal(err)
	}
	calls := len(node.calls)
	_, err := svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: attestFrame(1, 1985000, 329, 0xaa)})
	wantCode(t, "per-IP", err, codes.ResourceExhausted, "")
	if len(node.calls) != calls {
		t.Fatalf("a limited call reached the node")
	}
}

// The member list is refreshed when the tip moves; a failed refresh keeps the old list.
func TestSubmitAttestationMemberCacheFollowsTip(t *testing.T) {
	svc, node := newService(t)
	ctx := peerCtx(net.IPv4(10, 0, 0, 1))
	tip := []byte{1}
	previous := common.YedTipHash
	common.YedTipHash = func() []byte { return tip }
	t.Cleanup(func() { common.YedTipHash = previous })
	_, _ = svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: attestFrame(2, 1985000, 329, 0xaa)})
	_, _ = svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: attestFrame(3, 1985000, 329, 0xaa)})
	if n := countCalls(node, "yed_listattestors"); n != 1 {
		t.Fatalf("same tip: %d yed_listattestors calls, want 1", n)
	}
	tip = []byte{2}
	node.fail["yed_listattestors"] = errors.New("-1: yellowback-unhealthy")
	if _, err := svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: attestFrame(1, 1985000, 329, 0xaa)}); err != nil {
		t.Fatalf("a failed refresh must keep the old list: %v", err)
	}
	if n := countCalls(node, "yed_listattestors"); n != 2 {
		t.Fatalf("new tip: %d yed_listattestors calls, want 2", n)
	}
	// With no list ever fetched, a failure is UNAVAILABLE, naming the seq guard.
	svc2, node2 := newService(t)
	node2.fail["yed_listattestors"] = errors.New("-1: yellowback-unhealthy")
	_, err := svc2.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: attestFrame(1, 1985000, 329, 0xaa)})
	wantCode(t, "no list", err, codes.Unavailable, "seq")
}

// An accepted statement drops GetAttestations' shared 2-second answer.
func TestSubmitAttestationRefreshesAttestations(t *testing.T) {
	svc, node := newService(t)
	ctx := peerCtx(net.IPv4(10, 0, 0, 1))
	stream := &attestationStream{}
	if err := svc.GetAttestations(&walletrpc.Empty{}, stream); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: attestFrame(1, 1985000, 329, 0xaa)}); err != nil {
		t.Fatal(err)
	}
	if err := svc.GetAttestations(&walletrpc.Empty{}, &attestationStream{}); err != nil {
		t.Fatal(err)
	}
	if n := countCalls(node, "yed_getattestations"); n != 2 {
		t.Fatalf("yed_getattestations calls = %d, want 2 (the cache dropped after a submission)", n)
	}
}

func TestGetTag(t *testing.T) {
	svc, node := newService(t)
	ctx := context.Background()
	got, err := svc.GetTag(ctx, &walletrpc.HeightFilter{})
	if err != nil {
		t.Fatal(err)
	}
	example := node.contract.returns(t, "yed_gettag")
	got.Height = 0 // the server's own field
	assertMatchesContract(t, "yed_gettag", got, example)
	assertNonZeroFieldsPresent(t, "yed_gettag", got, example)
	if p := node.params["yed_gettag"]; len(p) != 1 || string(p[0]) != `"232"` {
		t.Fatalf("height 0 must ask for the tip (232) as a string: %s", p)
	}
	got, err = svc.GetTag(ctx, &walletrpc.HeightFilter{Height: 120})
	if err != nil || got.Height != 120 {
		t.Fatalf("height 120: %v %v", got, err)
	}
	if p := node.params["yed_gettag"]; string(p[0]) != `"120"` {
		t.Fatalf("height 120: params %s", p)
	}
	node.fail["yed_gettag"] = errors.New("-8: block height out of range")
	_, err = svc.GetTag(ctx, &walletrpc.HeightFilter{Height: 999999})
	wantCode(t, "out of range", err, codes.FailedPrecondition, "")
}

// stockAnswers wraps the fake node with gettxout / getrawmempool answers (stock RPCs, not in the
// yed_* contract).
func stockAnswers(t *testing.T, node *fakeNode, answers map[string]string) {
	t.Helper()
	previous := common.RawRequest
	common.RawRequest = func(method string, params []json.RawMessage) (json.RawMessage, error) {
		if a, ok := answers[method]; ok {
			node.calls = append(node.calls, method)
			node.params[method] = params
			return json.RawMessage(a), nil
		}
		return node.RawRequest(method, params)
	}
	t.Cleanup(func() { common.RawRequest = previous })
}

func TestGetTxOut(t *testing.T) {
	svc, node := newService(t)
	ctx := context.Background()
	// ycashd's answer (value with 8 decimals).
	stockAnswers(t, node, map[string]string{"gettxout": `{"bestblock":"0a27ab27467ff304d7d602ea5227a0d5410425a9809bdbb9331f8b4df0e15278",
		"confirmations":3,"value":12.50000001,"scriptPubKey":{"asm":"x","hex":"76a914000000000000000000000000000000000000000088ac",
		"reqSigs":1,"type":"pubkeyhash","addresses":["tmX"]},"version":4,"coinbase":true}`})
	got, err := svc.GetTxOut(ctx, &walletrpc.YedTxOutQuery{Txid: exampleTxid, Vout: 2, IncludeMempool: true})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.ValueZat != 1250000001 || got.Confirmations != 3 || !got.Coinbase ||
		got.ScriptPubKeyHex != "76a914000000000000000000000000000000000000000088ac" || got.Bestblock == "" {
		t.Fatalf("gettxout: %v", got)
	}
	if p := node.params["gettxout"]; len(p) != 3 || string(p[0]) != `"`+exampleTxid+`"` || string(p[1]) != "2" || string(p[2]) != "true" {
		t.Fatalf("gettxout params: %s", p)
	}
	// kogany's float rendering, and null.
	stockAnswers(t, node, map[string]string{"gettxout": `{"bestblock":"00","confirmations":0,"value":1e-8,"scriptPubKey":{"hex":"51"},"version":4,"coinbase":false}`})
	got, err = svc.GetTxOut(ctx, &walletrpc.YedTxOutQuery{Txid: exampleTxid})
	if err != nil || got.ValueZat != 1 || !got.Found {
		t.Fatalf("1e-8: %v %v", got, err)
	}
	if p := node.params["gettxout"]; string(p[2]) != "false" {
		t.Fatalf("includeMempool false: %s", p)
	}
	stockAnswers(t, node, map[string]string{"gettxout": `null`})
	got, err = svc.GetTxOut(ctx, &walletrpc.YedTxOutQuery{Txid: exampleTxid})
	if err != nil || got.Found {
		t.Fatalf("null: %v %v", got, err)
	}
	_, err = svc.GetTxOut(ctx, &walletrpc.YedTxOutQuery{Txid: "xyz"})
	wantCode(t, "bad txid", err, codes.InvalidArgument, "")
}

func TestGetMempoolTxids(t *testing.T) {
	svc, node := newService(t)
	ctx := context.Background()
	stockAnswers(t, node, map[string]string{"getrawmempool": `["` + exampleTxid + `"]`})
	got, err := svc.GetMempoolTxids(ctx, &walletrpc.Empty{})
	if err != nil || len(got.Txids) != 1 || got.Txids[0] != exampleTxid {
		t.Fatalf("getrawmempool: %v %v", got, err)
	}
	if p := node.params["getrawmempool"]; len(p) != 0 {
		t.Fatalf("getrawmempool takes no params here: %s", p)
	}
	stockAnswers(t, node, map[string]string{"getrawmempool": `[]`})
	if got, err := svc.GetMempoolTxids(ctx, &walletrpc.Empty{}); err != nil || len(got.Txids) != 0 {
		t.Fatalf("empty mempool: %v %v", got, err)
	}
	stockAnswers(t, node, map[string]string{"getrawmempool": `["not-a-txid"]`})
	_, err = svc.GetMempoolTxids(ctx, &walletrpc.Empty{})
	wantCode(t, "garbage", err, codes.Internal, "")
}
