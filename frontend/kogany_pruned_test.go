// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

//go:build devnet

// The kogany leg's pruned case (workspace pruning assessment KP-3, report-only): this fork's
// server in front of a kogany node running Zakura's Pruned storage mode. Run through
// `scripts/kogany-leg.sh --pruned`, which starts the rig with `--pruned`, mines past the window and
// sets, besides openKoganyRig's variables:
//
//	LWD_KOGANY_PRUNED=1    run this test (it skips otherwise)
//	LWD_PRUNED_FRESH_ADDR  a server started on the pruned kogany with an empty cache
//
// LWD_FORK_ADDR is the server that ingested the chain while kogany still held every block, so its
// cache predates the pruning; LWD_BASELINE_ADDR is the server on the ycash6 sidecar (archive).
package frontend

import (
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zcash/lightwalletd/walletrpc"
)

// prunedRefusal is kogany's (and ycashd's) getblock refusal for a pruned block (-32603).
const prunedRefusal = "Block not available (pruned data)"

func TestKoganyRigPruned(t *testing.T) {
	if os.Getenv("LWD_KOGANY_PRUNED") != "1" {
		t.Skip("LWD_KOGANY_PRUNED is not 1: run through scripts/kogany-leg.sh --pruned")
	}
	freshAddr := os.Getenv("LWD_PRUNED_FRESH_ADDR")
	if freshAddr == "" {
		t.Fatal("LWD_PRUNED_FRESH_ADDR is not set")
	}
	// The fresh server first: its ingestor gives up on the pruned heights after about 100 s.
	fresh := walletrpc.NewCompactTxStreamerClient(dial(t, freshAddr))
	d := openKoganyRig(t)
	kogany := koganyNode(t, d.dir)
	var info struct {
		Blocks      uint64 `json:"blocks"`
		Pruned      bool   `json:"pruned"`
		PruneHeight uint64 `json:"pruneheight"`
	}
	raw, err := kogany("getblockchaininfo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	if !info.Pruned || info.PruneHeight < 3 {
		t.Fatalf("kogany is not pruned past height 2: pruned=%v pruneheight=%d (mine more)", info.Pruned, info.PruneHeight)
	}
	ph := info.PruneHeight
	t.Logf("kogany: pruned, pruneheight %d, tip %d", ph, info.Blocks)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 1. The fresh server: a pruned height is kogany's pruned refusal, not "newer than latest block".
	_, err = fresh.GetBlock(ctx, &walletrpc.BlockID{Height: 1})
	if err == nil || !strings.Contains(err.Error(), prunedRefusal) {
		t.Fatalf("fresh server, GetBlock(1) on the pruned backend: %v (want %q)", err, prunedRefusal)
	}
	stream, err := fresh.GetBlockRange(ctx, &walletrpc.BlockRange{
		Start: &walletrpc.BlockID{Height: 1}, End: &walletrpc.BlockID{Height: 3}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	if err == nil || err == io.EOF || !strings.Contains(err.Error(), prunedRefusal) {
		t.Fatalf("fresh server, GetBlockRange [1..3] on the pruned backend: %v (want %q)", err, prunedRefusal)
	}
	// At and above pruneheight kogany still serves the block (from the node: the cache is empty).
	blk, err := fresh.GetBlock(ctx, &walletrpc.BlockID{Height: ph})
	if err != nil {
		t.Fatalf("fresh server, GetBlock(pruneheight %d): %v", ph, err)
	}
	if blk.Height != ph {
		t.Fatalf("fresh server, GetBlock(%d) answered height %d", ph, blk.Height)
	}
	t.Logf("fresh server: GetBlock / GetBlockRange below %d answer %q; GetBlock(%d) served", ph, prunedRefusal, ph)

	// 2. The server whose cache predates the pruning serves the whole chain as the archive
	// reference does, byte for byte.
	waitCache(t, ctx, walletrpc.NewCompactTxStreamerClient(d.fork), info.Blocks)
	n := compareBlockRange(t, ctx, d, 1, ph+5)
	t.Logf("cached server: GetBlockRange [1..%d] identical to the archive reference (%d blocks)", ph+5, n)

	// 3. GetTransaction of a transaction in a pruned block: a gRPC error from kogany's
	// getrawtransaction, the server stays up; the archive reference answers.
	var hdr struct {
		Tx []string `json:"tx"`
	}
	raw, err = d.node("getblock", []json.RawMessage{json.RawMessage(`"2"`), json.RawMessage(`1`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &hdr); err != nil || len(hdr.Tx) == 0 {
		t.Fatalf("getblock 2 on the sidecar: %v %s", err, raw)
	}
	txid := hdr.Tx[0]
	filter := &walletrpc.TxFilter{Hash: reversedHex(t, txid)}
	if _, err := walletrpc.NewCompactTxStreamerClient(d.fork).GetTransaction(ctx, filter); err == nil {
		t.Fatalf("GetTransaction(%s) in pruned block 2 answered on the pruned backend", txid)
	} else {
		t.Logf("cached server: GetTransaction(%s) in pruned block 2: %v", txid, err)
	}
	if _, err := walletrpc.NewCompactTxStreamerClient(d.baseline).GetTransaction(ctx, filter); err != nil {
		t.Fatalf("the archive reference refused GetTransaction(%s): %v", txid, err)
	}
	if _, err := walletrpc.NewCompactTxStreamerClient(d.fork).GetLatestBlock(ctx, &walletrpc.ChainSpec{}); err != nil {
		t.Fatalf("the cached server stopped answering after the refusal: %v", err)
	}
}

// koganyNode is the rig's kogany RPC (node "1" of rig.json; node 0 is the sidecar).
func koganyNode(t *testing.T, dir string) nodeClient {
	t.Helper()
	raw, err := ioutil.ReadFile(filepath.Join(dir, "rig.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		RPC map[string]struct {
			Port     int    `json:"port"`
			User     string `json:"user"`
			Password string `json:"password"`
		} `json:"rpc"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	k := state.RPC["1"]
	return newNodeClient(t, k.Port, k.User, k.Password)
}

// waitCache waits until the server's cache holds height tip.
func waitCache(t *testing.T, ctx context.Context, c walletrpc.CompactTxStreamerClient, tip uint64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		b, err := c.GetLatestBlock(ctx, &walletrpc.ChainSpec{})
		if err == nil && b.Height >= tip {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cached server did not reach %d: %v %v", tip, b, err)
		}
		time.Sleep(time.Second)
	}
}

// reversedHex is a display-order txid in the byte order TxFilter.Hash takes.
func reversedHex(t *testing.T, s string) []byte {
	t.Helper()
	if len(s) != 64 {
		t.Fatalf("txid %q", s)
	}
	b := make([]byte, 32)
	for i := 0; i < 32; i++ {
		var v byte
		for _, c := range s[2*i : 2*i+2] {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= byte(c - '0')
			case c >= 'a' && c <= 'f':
				v |= byte(c - 'a' + 10)
			default:
				t.Fatalf("txid %q", s)
			}
		}
		b[31-i] = v
	}
	return b
}
