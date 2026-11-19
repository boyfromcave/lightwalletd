// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

//go:build devnet

// The light attestor's regtest leg (beta4 item 3d, B-T1; workspace docs/plans/beta4/price-feed.md
// section 2.5 B-2, B-3). Run through scripts/attest-leg.py, which builds the chain (a ycash6 node
// whose wallet holds three seated attestors and, with --koganyd, a kogany node following it), starts
// one server per node and sets, besides openDevnet's variables:
//
//	LWD_ATTEST_LEG=1        run these tests (they skip otherwise)
//	LWD_ATTEST_SERVERS      name=host:port,... the servers under test (ycash6=..., kogany=...)
//	LWD_ATTEST_SEQS         the attestors' seqs (comma-separated), held by node 0's wallet
//
// B-2: a statement signed by a registered attestor (node 0's yed_signattestation) and sent through
// SubmitAttestation appears in GetAttestations and in a BuildBundle result on that server's node.
// B-3 against a real node: a wrong size, a non-member seq and a second statement inside the window
// are refused; the identical resubmit is answered. GetTag, GetTxOut and GetMempoolTxids answer.
package frontend

import (
	"context"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zcash/lightwalletd/walletrpc"
)

type attestLegServer struct {
	name string
	conn *grpc.ClientConn
}

func openAttestLeg(t *testing.T) (*devnet, []attestLegServer, []int) {
	t.Helper()
	if os.Getenv("LWD_ATTEST_LEG") != "1" {
		t.Skip("LWD_ATTEST_LEG is not 1: run through scripts/attest-leg.py")
	}
	d := openDevnet(t)
	var servers []attestLegServer
	for _, entry := range strings.Split(os.Getenv("LWD_ATTEST_SERVERS"), ",") {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			t.Fatalf("LWD_ATTEST_SERVERS: %q", entry)
		}
		conn := dial(t, parts[1])
		t.Cleanup(func() { conn.Close() })
		servers = append(servers, attestLegServer{name: parts[0], conn: conn})
	}
	var seqs []int
	for _, s := range strings.Split(os.Getenv("LWD_ATTEST_SEQS"), ",") {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("LWD_ATTEST_SEQS: %q", s)
		}
		seqs = append(seqs, n)
	}
	if len(seqs) < 2 {
		t.Fatalf("the leg needs at least mSelect (2) attestors, got %v", seqs)
	}
	return d, servers, seqs
}

// waitServerTip waits until the server's cache holds the node's tip.
func waitServerTip(t *testing.T, ctx context.Context, conn *grpc.ClientConn, height uint64) {
	t.Helper()
	c := walletrpc.NewCompactTxStreamerClient(conn)
	deadline := time.Now().Add(60 * time.Second)
	for {
		b, err := c.GetLatestBlock(ctx, &walletrpc.ChainSpec{})
		if err == nil && b.Height >= height {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not reach height %d: %v %v", height, b, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestAttestLegSubmitAttestation(t *testing.T) {
	d, servers, seqs := openAttestLeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var tip uint64
	d.rpc(t, &tip, "getblockcount")
	cited := int64(tip) - 2 // REF_LAG, the agent's choice
	type signed struct {
		Hex           string `json:"hex"`
		Seq           int64  `json:"seq"`
		PriceMicroUsd int64  `json:"priceMicroUsd"`
		CitedHeight   int64  `json:"citedHeight"`
	}
	frames := map[int][]byte{}
	for _, seq := range seqs {
		var s signed
		d.rpc(t, &s, "yed_signattestation", seq, 1985000+seq, cited)
		frame, err := hex.DecodeString(s.Hex)
		if err != nil || len(frame) != 74 || s.CitedHeight != cited {
			t.Fatalf("yed_signattestation seq %d: %+v", seq, s)
		}
		frames[seq] = frame
	}
	// A second statement of the first seq one block earlier: inside the window once the first is in.
	var early signed
	d.rpc(t, &early, "yed_signattestation", seqs[0], 1985000+seqs[0], cited-1)
	earlyFrame, _ := hex.DecodeString(early.Hex)

	for _, srv := range servers {
		t.Run(srv.name, func(t *testing.T) {
			waitServerTip(t, ctx, srv.conn, tip)
			yb := walletrpc.NewYellowbackStreamerClient(srv.conn)
			// B-3 against the real node.
			_, err := yb.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: frames[seqs[0]][:73]})
			if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument || !strings.HasPrefix(st.Message(), "size:") {
				t.Fatalf("73 bytes: %v", err)
			}
			stranger := append([]byte(nil), frames[seqs[0]]...)
			stranger[0], stranger[1] = 0xe7, 0x03 // seq 999
			_, err = yb.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: stranger})
			if st, _ := status.FromError(err); st.Code() != codes.PermissionDenied || !strings.HasPrefix(st.Message(), "seq:") {
				t.Fatalf("seq 999: %v", err)
			}
			// B-2: every statement goes in.
			for _, seq := range seqs {
				res, err := yb.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: frames[seq]})
				if err != nil {
					t.Fatalf("SubmitAttestation seq %d: %v", seq, err)
				}
				if !res.Accepted || res.Seq != int64(seq) || res.CitedHeight != cited || res.PriceMicroUsd != int64(1985000+seq) {
					t.Fatalf("SubmitAttestation seq %d: %v", seq, res)
				}
				t.Logf("%s: SubmitAttestation seq %d price %d cited %d -> accepted, replaced %v, poolSize %d",
					srv.name, seq, res.PriceMicroUsd, res.CitedHeight, res.Replaced, res.PoolSize)
			}
			again, err := yb.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: frames[seqs[0]]})
			if err != nil || !again.Accepted || again.Seq != int64(seqs[0]) {
				t.Fatalf("identical resubmit: %v %v", again, err)
			}
			_, err = yb.SubmitAttestation(ctx, &walletrpc.YedAttestationFrame{Frame: earlyFrame})
			if st, _ := status.FromError(err); st.Code() != codes.ResourceExhausted || !strings.HasPrefix(st.Message(), "rate:") {
				t.Fatalf("a second statement inside the window: %v", err)
			}
			t.Logf("%s: B-3 refusals: size INVALID_ARGUMENT, seq 999 PERMISSION_DENIED, second statement RESOURCE_EXHAUSTED (rate); identical resubmit answered", srv.name)

			// GetAttestations shows each statement, byte for byte.
			stream, err := yb.GetAttestations(ctx, &walletrpc.Empty{})
			if err != nil {
				t.Fatal(err)
			}
			pooled := map[int64]*walletrpc.YedAttestation{}
			for {
				row, err := stream.Recv()
				if err != nil {
					break
				}
				pooled[row.Seq] = row
			}
			for _, seq := range seqs {
				row := pooled[int64(seq)]
				if row == nil || row.Hex != hex.EncodeToString(frames[seq]) || row.CitedHeight != cited || row.PriceMicroUsd != int64(1985000+seq) {
					t.Fatalf("GetAttestations: seq %d is %v, want the submitted statement", seq, row)
				}
			}
			t.Logf("%s: GetAttestations holds all %d submitted statements byte for byte", srv.name, len(seqs))

			// BuildBundle at the tip carries them.
			bundle, err := yb.BuildBundle(ctx, &walletrpc.YedBundleQuery{RefHeight: uint32(tip)})
			if err != nil {
				t.Fatalf("BuildBundle: %v", err)
			}
			in := map[int64]bool{}
			for _, s := range bundle.Seqs {
				in[s] = true
			}
			carried := 0
			for _, seq := range seqs {
				if in[int64(seq)] && strings.Contains(bundle.Hex, hex.EncodeToString(frames[seq][10:])) {
					carried++
				}
			}
			if carried < 2 || carried != len(bundle.Seqs) {
				t.Fatalf("BuildBundle seqs %v count %d hex %s: %d of the submitted statements carried", bundle.Seqs, bundle.Count, bundle.Hex, carried)
			}
			t.Logf("%s: BuildBundle(refHeight %d) seqs %v, count %d, aMint %d: every statement in it came through SubmitAttestation",
				srv.name, tip, bundle.Seqs, bundle.Count, bundle.AMint)
		})
	}
}

func TestAttestLegReads(t *testing.T) {
	d, servers, _ := openAttestLeg(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var addr string
	d.rpc(t, &addr, "getnewaddress")
	var txid string
	d.rpc(t, &txid, "sendtoaddress", addr, 1)
	var tip uint64
	d.rpc(t, &tip, "getblockcount")
	for _, srv := range servers {
		t.Run(srv.name, func(t *testing.T) {
			waitServerTip(t, ctx, srv.conn, tip)
			yb := walletrpc.NewYellowbackStreamerClient(srv.conn)
			tag, err := yb.GetTag(ctx, &walletrpc.HeightFilter{})
			if err != nil || tag.Height < int64(tip) || tag.Kind == "" {
				t.Fatalf("GetTag(0): %v %v", tag, err)
			}
			deadline := time.Now().Add(60 * time.Second)
			for {
				list, err := yb.GetMempoolTxids(ctx, &walletrpc.Empty{})
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(strings.Join(list.Txids, ","), txid) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("GetMempoolTxids never showed %s: %v", txid, list.Txids)
				}
				time.Sleep(500 * time.Millisecond)
			}
			found := false
			for vout := uint32(0); vout < 2 && !found; vout++ {
				out, err := yb.GetTxOut(ctx, &walletrpc.YedTxOutQuery{Txid: txid, Vout: vout, IncludeMempool: true})
				if err != nil {
					t.Fatal(err)
				}
				found = out.Found && out.ValueZat == 100000000 && out.ScriptPubKeyHex != ""
			}
			if !found {
				t.Fatalf("GetTxOut(%s, includeMempool) did not show the 1-coin output", txid)
			}
			out, err := yb.GetTxOut(ctx, &walletrpc.YedTxOutQuery{Txid: txid, Vout: 0})
			if err != nil || out.Found {
				t.Fatalf("GetTxOut without the mempool must be found=false for an unconfirmed output: %v %v", out, err)
			}
			t.Logf("%s: GetTag(0) height %d kind %s; GetMempoolTxids holds %s; GetTxOut found it in the mempool, not without", srv.name, tag.Height, tag.Kind, txid)
		})
	}
}
