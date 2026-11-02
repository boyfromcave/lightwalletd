// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

//go:build devnet

// The kogany leg (workspace kogany plan W9-c, report-only): this fork's server in front of a
// kogany node (zakurad --features ycash, the third node line), compared with the same server in
// front of a ycash6 node holding the same chain. Run through scripts/kogany-leg.sh, which starts
// the rig (scripts/kogany-rig.py: kogany, and a ycash6 sidecar `connect=` kogany that holds the
// wallet and mines) and sets, besides openDevnet's variables:
//
//	LWD_KOGANY=1      run these tests (they skip otherwise)
//	LWD_FORK_ADDR     the server on kogany (the server under test)
//	LWD_BASELINE_ADDR the server on the ycash6 sidecar (the reference)
//
// Node 0 of the rig's devnet.json is the sidecar: the tests fund, sign and mine with its wallet,
// and every block and transaction reaches kogany over P2P (the sidecar's only peer is kogany).
// What differs between the two backends by construction (the node's build and subversion in
// GetLightdInfo) is masked; what differs and should not is a finding, logged with KOGANY-FINDING.
package frontend

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	"google.golang.org/grpc"

	"github.com/zcash/lightwalletd/walletrpc"
)

func openKoganyRig(t *testing.T) *devnet {
	t.Helper()
	if os.Getenv("LWD_KOGANY") != "1" {
		t.Skip("LWD_KOGANY is not 1: run through scripts/kogany-leg.sh")
	}
	return openDevnet(t)
}

// TestKoganyRigLightdInfo: GetLightdInfo is the same on both backends but for the node's own build
// and subversion, and the server under test really is in front of kogany.
func TestKoganyRigLightdInfo(t *testing.T) {
	d := openKoganyRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f, err := walletrpc.NewCompactTxStreamerClient(d.fork).GetLightdInfo(ctx, &walletrpc.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := walletrpc.NewCompactTxStreamerClient(d.baseline).GetLightdInfo(ctx, &walletrpc.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.ZcashdSubversion, "/kogany:") {
		t.Fatalf("the server under test is not in front of kogany: subversion %q", f.ZcashdSubversion)
	}
	if !strings.HasPrefix(b.ZcashdSubversion, "/YcashCpp:") {
		t.Fatalf("the reference server is not in front of ycashd: subversion %q", b.ZcashdSubversion)
	}
	if f.EstimatedHeight != b.EstimatedHeight {
		// getblockchaininfo.estimatedheight: kogany's is the network's estimate (the sidecar's
		// announced height), not its own tip; a block in flight shows here.
		t.Logf("KOGANY-FINDING estimated-height: kogany %d, ycash6 %d (blockHeight %d / %d)", f.EstimatedHeight, b.EstimatedHeight, f.BlockHeight, b.BlockHeight)
	}
	fm, bm := proto.Clone(f).(*walletrpc.LightdInfo), proto.Clone(b).(*walletrpc.LightdInfo)
	for _, m := range []*walletrpc.LightdInfo{fm, bm} {
		m.ZcashdBuild, m.ZcashdSubversion, m.EstimatedHeight = "", "", 0
	}
	fb, _ := proto.Marshal(fm)
	bb, _ := proto.Marshal(bm)
	if !bytes.Equal(fb, bb) {
		t.Fatalf("GetLightdInfo differs beyond the node's build:\n kogany %+v\n ycash6 %+v", f, b)
	}
	t.Logf("GetLightdInfo equal but for the node (%s %s / %s %s): height %d, branch %s",
		f.ZcashdSubversion, f.ZcashdBuild, b.ZcashdSubversion, b.ZcashdBuild, f.BlockHeight, f.ConsensusBranchId)
}

// TestKoganyRigBlockRange: every CompactBlock of the chain, byte for byte, kogany = ycash6.
func TestKoganyRigBlockRange(t *testing.T) {
	d := openKoganyRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tip := d.mine(t, ctx, 1)
	n := compareBlockRange(t, ctx, d, 1, tip)
	t.Logf("GetBlockRange [1..%d]: %d compact blocks identical on kogany and ycash6", tip, n)
}

func compareBlockRange(t *testing.T, ctx context.Context, d *devnet, start, end uint64) int {
	t.Helper()
	rng := &walletrpc.BlockRange{Start: &walletrpc.BlockID{Height: start}, End: &walletrpc.BlockID{Height: end}}
	fs, err := walletrpc.NewCompactTxStreamerClient(d.fork).GetBlockRange(ctx, rng)
	if err != nil {
		t.Fatal(err)
	}
	bs, err := walletrpc.NewCompactTxStreamerClient(d.baseline).GetBlockRange(ctx, rng)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		fblk, ferr := fs.Recv()
		bblk, berr := bs.Recv()
		if ferr == io.EOF && berr == io.EOF {
			break
		}
		if ferr != nil || berr != nil {
			t.Fatalf("after %d blocks: kogany %v, ycash6 %v", n, ferr, berr)
		}
		fb, _ := proto.Marshal(fblk)
		bb, _ := proto.Marshal(bblk)
		if !bytes.Equal(fb, bb) {
			t.Fatalf("CompactBlock %d differs:\n kogany %+v\n ycash6 %+v", fblk.Height, fblk, bblk)
		}
		n++
	}
	if uint64(n) != end-start+1 {
		t.Fatalf("streamed %d blocks for [%d..%d]", n, start, end)
	}
	return n
}

// normalizeTree re-encodes a legacy CommitmentTree (zcashd's `finalState`: Option<left>,
// Option<right>, CompactSize n, n x Option<parent>) without its trailing empty parents. zcashd
// writes the parents up to the last filled one; Zakura writes all 31 (KOGANY-FINDING tree-encoding):
// the same tree, other bytes.
func normalizeTree(t *testing.T, s string) string {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("tree %q: %v", s, err)
	}
	i := 0
	opt := func() []byte {
		if i >= len(b) {
			t.Fatalf("tree %q: truncated at %d", s, i)
		}
		if b[i] == 0 {
			i++
			return nil
		}
		if b[i] != 1 || i+33 > len(b) {
			t.Fatalf("tree %q: bad option at %d", s, i)
		}
		v := b[i+1 : i+33]
		i += 33
		return v
	}
	left, right := opt(), opt()
	if i >= len(b) || b[i] >= 0xfd {
		t.Fatalf("tree %q: bad parent count at %d", s, i)
	}
	count := int(b[i])
	i++
	parents := make([][]byte, count)
	for k := range parents {
		parents[k] = opt()
	}
	if i != len(b) {
		t.Fatalf("tree %q: %d trailing bytes", s, len(b)-i)
	}
	for len(parents) > 0 && parents[len(parents)-1] == nil {
		parents = parents[:len(parents)-1]
	}
	var out bytes.Buffer
	put := func(v []byte) {
		if v == nil {
			out.WriteByte(0)
			return
		}
		out.WriteByte(1)
		out.Write(v)
	}
	put(left)
	put(right)
	out.WriteByte(byte(len(parents)))
	for _, p := range parents {
		put(p)
	}
	return hex.EncodeToString(out.Bytes())
}

// TestKoganyRigTreeState: GetTreeState by height and by hash, before and after a Sapling note,
// names the same block and the same tree on both backends (the tree compared after
// normalizeTree; a byte difference is logged as a finding, not failed).
func TestKoganyRigTreeState(t *testing.T) {
	d := openKoganyRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	byteDiffs := 0
	check := func(id *walletrpc.BlockID) *walletrpc.TreeState {
		t.Helper()
		f, err := walletrpc.NewCompactTxStreamerClient(d.fork).GetTreeState(ctx, id)
		if err != nil {
			t.Fatalf("kogany GetTreeState(%+v): %v", id, err)
		}
		b, err := walletrpc.NewCompactTxStreamerClient(d.baseline).GetTreeState(ctx, id)
		if err != nil {
			t.Fatalf("ycash6 GetTreeState(%+v): %v", id, err)
		}
		if f.Network != b.Network || f.Height != b.Height || f.Hash != b.Hash || f.Time != b.Time {
			t.Fatalf("GetTreeState(%+v) names another block:\n kogany %+v\n ycash6 %+v", id, f, b)
		}
		if normalizeTree(t, f.Tree) != normalizeTree(t, b.Tree) {
			t.Fatalf("GetTreeState(%+v): another tree:\n kogany %s\n ycash6 %s", id, f.Tree, b.Tree)
		}
		if f.Tree != b.Tree {
			if byteDiffs == 0 {
				t.Logf("KOGANY-FINDING tree-encoding: height %d, kogany %s, ycash6 %s (same tree, trailing empty parents)", f.Height, f.Tree, b.Tree)
			}
			byteDiffs++
		}
		return f
	}
	var tip uint64
	d.rpc(t, &tip, "getblockcount")
	heights := []uint64{1, 2, tip / 2, tip}
	for _, h := range heights {
		ts := check(&walletrpc.BlockID{Height: h})
		check(&walletrpc.BlockID{Hash: mustHex(t, ts.Hash)})
	}
	empty := check(&walletrpc.BlockID{Height: 1}).Tree
	txid := d.shieldCoinbase(t)
	mined := d.mine(t, ctx, 1)
	after := check(&walletrpc.BlockID{Height: mined})
	if normalizeTree(t, after.Tree) == normalizeTree(t, empty) {
		t.Fatalf("the tree after %s at %d is still the empty tree", txid, mined)
	}
	if _, err := walletrpc.NewCompactTxStreamerClient(d.fork).GetTreeState(ctx, &walletrpc.BlockID{Height: mined + 1000}); err == nil {
		t.Fatal("kogany: GetTreeState past the tip must fail")
	}
	t.Logf("GetTreeState: %d heights by height and hash and the note at %d (%s) name the same block and tree on kogany and ycash6; %d answers differ in bytes only",
		len(heights)+2, mined, txid[:8], byteDiffs)
}

// TestKoganyRigTransparent: the transparent address views (GetTaddressBalance, GetAddressUtxos,
// GetTaddressTxids) of an address the sidecar's wallet pays, kogany = ycash6.
func TestKoganyRigTransparent(t *testing.T) {
	d := openKoganyRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var addr string
	d.rpc(t, &addr, "getnewaddress")
	var txids []string
	for _, amount := range []float64{1.25, 0.5} {
		var txid string
		d.rpc(t, &txid, "sendtoaddress", addr, amount)
		d.waitRelayed(t, txid)
		txids = append(txids, txid)
	}
	tip := d.mine(t, ctx, 1)
	both := func(name string, call func(conn *grpc.ClientConn) ([]byte, error)) []byte {
		t.Helper()
		f, err := call(d.fork)
		if err != nil {
			t.Fatalf("kogany %s: %v", name, err)
		}
		b, err := call(d.baseline)
		if err != nil {
			t.Fatalf("ycash6 %s: %v", name, err)
		}
		if !bytes.Equal(f, b) {
			t.Fatalf("%s differs between kogany and ycash6", name)
		}
		return f
	}
	balance := both("GetTaddressBalance", func(conn *grpc.ClientConn) ([]byte, error) {
		r, err := walletrpc.NewCompactTxStreamerClient(conn).GetTaddressBalance(ctx, &walletrpc.AddressList{Addresses: []string{addr}})
		if err != nil {
			return nil, err
		}
		if r.ValueZat != 175_000_000 {
			return nil, fmt.Errorf("balance %d, want 175000000", r.ValueZat)
		}
		return proto.Marshal(r)
	})
	both("GetAddressUtxos", func(conn *grpc.ClientConn) ([]byte, error) {
		r, err := walletrpc.NewCompactTxStreamerClient(conn).GetAddressUtxos(ctx, &walletrpc.GetAddressUtxosArg{Addresses: []string{addr}})
		if err != nil {
			return nil, err
		}
		if len(r.AddressUtxos) != 2 {
			return nil, fmt.Errorf("%d utxos, want 2", len(r.AddressUtxos))
		}
		return proto.Marshal(r)
	})
	both("GetTaddressTxids", func(conn *grpc.ClientConn) ([]byte, error) {
		s, err := walletrpc.NewCompactTxStreamerClient(conn).GetTaddressTxids(ctx, &walletrpc.TransparentAddressBlockFilter{
			Address: addr, Range: &walletrpc.BlockRange{Start: &walletrpc.BlockID{Height: 1}, End: &walletrpc.BlockID{Height: tip}}})
		if err != nil {
			return nil, err
		}
		var rows []string
		for {
			tx, err := s.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			rows = append(rows, fmt.Sprintf("%d:%x", tx.Height, tx.Data))
		}
		if len(rows) != len(txids) {
			return nil, fmt.Errorf("%d transactions, want %d", len(rows), len(txids))
		}
		sort.Strings(rows)
		return []byte(strings.Join(rows, ",")), nil
	})
	t.Logf("transparent views of %s (2 coins, %x...) identical on kogany and ycash6", addr, balance[:4])
}

// TestKoganyRigSendTransaction: a transaction the sidecar's wallet signs but does not send goes in
// through the server on kogany (kogany's sendrawtransaction), reaches the sidecar by relay, is
// mined, and both servers return it.
func TestKoganyRigSendTransaction(t *testing.T) {
	d := openKoganyRig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var to string
	d.rpc(t, &to, "getnewaddress")
	var unspent []struct {
		Txid      string  `json:"txid"`
		Vout      int     `json:"vout"`
		Amount    float64 `json:"amount"`
		Spendable bool    `json:"spendable"`
	}
	d.rpc(t, &unspent, "listunspent", 101)
	var coin *struct {
		Txid      string  `json:"txid"`
		Vout      int     `json:"vout"`
		Amount    float64 `json:"amount"`
		Spendable bool    `json:"spendable"`
	}
	for i := range unspent {
		if unspent[i].Spendable && unspent[i].Amount > 1 {
			coin = &unspent[i]
			break
		}
	}
	if coin == nil {
		t.Fatal("the sidecar has no mature coin")
	}
	var unsigned string
	// ZIP 317: one input, one output, two grace actions at 5,000 zatoshis (kogany's mempool
	// refuses less).
	d.rpc(t, &unsigned, "createrawtransaction", []map[string]interface{}{{"txid": coin.Txid, "vout": coin.Vout}},
		map[string]float64{to: float64(int64(coin.Amount*1e8)-10_000) / 1e8})
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	d.rpc(t, &signed, "signrawtransaction", unsigned)
	if !signed.Complete {
		t.Fatal("the sidecar could not sign")
	}
	raw := mustHex(t, signed.Hex)
	resp, err := walletrpc.NewCompactTxStreamerClient(d.fork).SendTransaction(ctx, &walletrpc.RawTransaction{Data: raw})
	if err != nil || resp.ErrorCode != 0 {
		t.Fatalf("SendTransaction through kogany: %v %+v", err, resp)
	}
	txid := strings.Trim(resp.ErrorMessage, `"`) // the node's JSON result, a quoted txid
	sent := time.Now()
	for {
		var pool []string
		d.rpc(t, &pool, "getrawmempool")
		if len(pool) > 0 && strings.Contains(strings.Join(pool, ","), txid) {
			break
		}
		if time.Since(sent) > 2*time.Minute {
			t.Fatalf("%s sent through kogany never reached the sidecar's mempool", txid)
		}
		time.Sleep(250 * time.Millisecond)
	}
	relay := time.Since(sent).Round(100 * time.Millisecond)
	h := d.mine(t, ctx, 1)
	hash := mustHex(t, txid)
	var rev []byte
	for i := len(hash) - 1; i >= 0; i-- {
		rev = append(rev, hash[i])
	}
	var got [2][]byte
	for i, conn := range []*grpc.ClientConn{d.fork, d.baseline} {
		tx, err := walletrpc.NewCompactTxStreamerClient(conn).GetTransaction(ctx, &walletrpc.TxFilter{Hash: rev})
		if err != nil {
			t.Fatalf("GetTransaction %s on server %d: %v", txid, i, err)
		}
		if !bytes.Equal(tx.Data, raw) || tx.Height != h {
			t.Fatalf("GetTransaction %s on server %d: height %d (mined %d), same bytes %v", txid, i, tx.Height, h, bytes.Equal(tx.Data, raw))
		}
		got[i], _ = proto.Marshal(tx)
	}
	if !bytes.Equal(got[0], got[1]) {
		t.Fatal("GetTransaction differs between kogany and ycash6")
	}
	var onNode json.RawMessage
	d.rpc(t, &onNode, "getrawtransaction", txid, 1)
	t.Logf("SendTransaction through kogany: %s relayed to the sidecar in %s, mined at %d, GetTransaction identical on both", txid[:16], relay, h)
}
