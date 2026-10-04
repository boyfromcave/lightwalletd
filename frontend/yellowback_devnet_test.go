// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

//go:build devnet

// Regtest integration test (docs/plans/yellowback-lightwalletd-plan.md section 6.3): this fork's
// server and the BASELINE server, both against node 0 of a running ycash-dd devnet. Never
// mainnet. Run through scripts/devnet-test.sh, which starts the servers and sets:
//
//	LWD_DEVNET_DIR    the devnet directory (devnet.json inside)
//	LWD_FORK_ADDR     this fork's server, started with -yellowback (default 127.0.0.1:9067)
//	LWD_BASELINE_ADDR the lightwalletd-legacy server (default 127.0.0.1:9068)
//	LWD_RAWMINT       path to ycash-dd/contrib/yellowback/devnet/lwd-rawmint
//	LWD_PYTHON        the workspace venv's python
//
// What it proves, in order: the old service answers byte-for-byte identically on both servers
// (GetLightdInfo, GetBlockRange over the whole cached chain); the baseline has no Yellowback
// service; YED activity made by node 0's wallet is seen through the new service exactly as the
// node sees it (GetAddressTokens vs yed_listunspent, GetTxInfo verdict); and a mint assembled
// from raw parts with numbers taken only from the server (section 5 item 5) validates,
// broadcasts and confirms with verdict ok.
package frontend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/rpcclient"
	"github.com/golang/protobuf/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zcash/lightwalletd/common"
	"github.com/zcash/lightwalletd/walletrpc"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

type nodeClient func(method string, params []json.RawMessage) (json.RawMessage, error)

func newNodeClient(t *testing.T, port int, user, password string) nodeClient {
	t.Helper()
	c, err := rpcclient.New(&rpcclient.ConnConfig{Host: fmt.Sprintf("127.0.0.1:%d", port), User: user, Pass: password,
		HTTPPostMode: true, DisableTLS: true}, nil)
	if err != nil {
		t.Fatalf("node 0 rpc: %v", err)
	}
	return c.RawRequest
}

type devnet struct {
	dir      string
	node     nodeClient
	miner    nodeClient // a POOL node: its blocks carry a quote tag, so the price windows stay filled (node 0's do not)
	fork     *grpc.ClientConn
	baseline *grpc.ClientConn
}

func dial(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, addr, grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		t.Fatalf("dial %s: %v (is the devnet up and the server started? see scripts/devnet-test.sh)", addr, err)
	}
	return conn
}

func openDevnet(t *testing.T) *devnet {
	t.Helper()
	dir := os.Getenv("LWD_DEVNET_DIR")
	if dir == "" {
		t.Skip("LWD_DEVNET_DIR not set: run through scripts/devnet-test.sh")
	}
	raw, err := ioutil.ReadFile(filepath.Join(dir, "devnet.json"))
	if err != nil {
		t.Fatalf("devnet.json: %v", err)
	}
	var state struct {
		Pools []int `json:"pools"`
		RPC   map[string]struct {
			Port     int    `json:"port"`
			User     string `json:"user"`
			Password string `json:"password"`
		} `json:"rpc"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("devnet.json: %v", err)
	}
	n0 := state.RPC["0"]
	if len(state.Pools) == 0 {
		t.Fatal("devnet.json names no pool node")
	}
	pool := state.RPC[fmt.Sprint(state.Pools[0])]
	// devnet.json's url embeds the credentials (and they are not URL-safe); rpcclient wants host:port.
	d := &devnet{dir: dir, node: newNodeClient(t, n0.Port, n0.User, n0.Password), miner: newNodeClient(t, pool.Port, pool.User, pool.Password)}
	d.fork = dial(t, env("LWD_FORK_ADDR", "127.0.0.1:9067"))
	d.baseline = dial(t, env("LWD_BASELINE_ADDR", "127.0.0.1:9068"))
	t.Cleanup(func() { d.fork.Close(); d.baseline.Close() })
	// Right after start a server's cache is legitimately empty for a few seconds (the ingestor polls).
	for _, conn := range []*grpc.ClientConn{d.fork, d.baseline} {
		c := walletrpc.NewCompactTxStreamerClient(conn)
		deadline := time.Now().Add(60 * time.Second)
		for {
			if _, err := c.GetLatestBlock(context.Background(), &walletrpc.ChainSpec{}); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("a server's cache stayed empty for 60s")
			}
			time.Sleep(time.Second)
		}
	}
	return d
}

func (d *devnet) rpc(t *testing.T, out interface{}, method string, params ...interface{}) {
	t.Helper()
	var raw []json.RawMessage
	for _, p := range params {
		b, _ := json.Marshal(p)
		raw = append(raw, json.RawMessage(b))
	}
	res, err := d.node(method, raw)
	if err != nil {
		t.Fatalf("node %s: %v", method, err)
	}
	if out != nil {
		if err := json.Unmarshal(res, out); err != nil {
			t.Fatalf("node %s: %v", method, err)
		}
	}
}

// mine n blocks on the pool node (tagged, so the price windows stay filled) and wait for node 0
// and both servers' caches to reach the new height.
func (d *devnet) mine(t *testing.T, ctx context.Context, n int) uint64 {
	t.Helper()
	b, _ := json.Marshal(n)
	if _, err := d.miner("generate", []json.RawMessage{json.RawMessage(b)}); err != nil {
		t.Fatalf("pool generate: %v", err)
	}
	var height uint64
	deadline := time.Now().Add(30 * time.Second)
	for {
		var h uint64
		d.rpc(t, &h, "getblockcount")
		if h >= height && h > 0 {
			height = h
		}
		var mh uint64
		raw, _ := d.miner("getblockcount", nil)
		_ = json.Unmarshal(raw, &mh)
		if h == mh {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node 0 did not sync to the pool's height %d (at %d)", mh, h)
		}
		time.Sleep(300 * time.Millisecond)
	}
	// Node 0's Yellowback index is applied a moment after the block; GetTxInfo reads the index.
	y := walletrpc.NewYellowbackStreamerClient(d.fork)
	for time.Now().Before(deadline) {
		info, err := y.GetYellowbackInfo(ctx, &walletrpc.Empty{})
		if err == nil && info.Height >= int64(height) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, conn := range []*grpc.ClientConn{d.fork, d.baseline} {
		c := walletrpc.NewCompactTxStreamerClient(conn)
		deadline := time.Now().Add(30 * time.Second)
		for {
			latest, err := c.GetLatestBlock(ctx, &walletrpc.ChainSpec{})
			if err == nil && latest.Height >= height {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("server did not reach height %d: %v", height, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	return height
}

// TestDevnetBaselineByteEquality: the backward-compatibility gate. Every CompactBlock the fork
// serves is byte-identical to the baseline's, and so is GetLightdInfo.
func TestDevnetBaselineByteEquality(t *testing.T) {
	d := openDevnet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	fork := walletrpc.NewCompactTxStreamerClient(d.fork)
	base := walletrpc.NewCompactTxStreamerClient(d.baseline)

	fi, err := fork.GetLightdInfo(ctx, &walletrpc.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	bi, err := base.GetLightdInfo(ctx, &walletrpc.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	fb, _ := proto.Marshal(fi)
	bb, _ := proto.Marshal(bi)
	if !bytes.Equal(fb, bb) {
		t.Fatalf("GetLightdInfo differs:\n fork     %+v\n baseline %+v", fi, bi)
	}

	latest, err := base.GetLatestBlock(ctx, &walletrpc.ChainSpec{})
	if err != nil {
		t.Fatal(err)
	}
	start := uint64(fi.SaplingActivationHeight)
	if start == 0 {
		start = 1
	}
	rng := &walletrpc.BlockRange{Start: &walletrpc.BlockID{Height: start}, End: &walletrpc.BlockID{Height: latest.Height}}
	fs, err := fork.GetBlockRange(ctx, rng)
	if err != nil {
		t.Fatal(err)
	}
	bs, err := base.GetBlockRange(ctx, rng)
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
			t.Fatalf("after %d blocks: fork %v, baseline %v", n, ferr, berr)
		}
		fb, _ := proto.Marshal(fblk)
		bb, _ := proto.Marshal(bblk)
		if !bytes.Equal(fb, bb) {
			t.Fatalf("CompactBlock %d differs between fork and baseline", fblk.Height)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no blocks streamed")
	}
	t.Logf("byte-equality gate: %d compact blocks [%d..%d] identical; GetLightdInfo identical", n, start, latest.Height)
}

// TestDevnetBaselineHasNoYellowback: the legacy binary answers UNIMPLEMENTED.
func TestDevnetBaselineHasNoYellowback(t *testing.T) {
	d := openDevnet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := walletrpc.NewYellowbackStreamerClient(d.baseline).GetYellowbackInfo(ctx, &walletrpc.Empty{})
	if st, _ := status.FromError(err); st.Code() != codes.Unimplemented {
		t.Fatalf("baseline: want UNIMPLEMENTED, got %v", err)
	}
}

type unspentRow struct {
	Address string `json:"address"`
	Cents   uint64 `json:"cents"`
	Height  int64  `json:"height"`
	Txid    string `json:"txid"`
	Vout    uint32 `json:"vout"`
}

func tokensOf(t *testing.T, ctx context.Context, y walletrpc.YellowbackStreamerClient, addresses []string) []*walletrpc.YedToken {
	t.Helper()
	s, err := y.GetAddressTokens(ctx, &walletrpc.YedAddressList{Addresses: addresses})
	if err != nil {
		t.Fatal(err)
	}
	var out []*walletrpc.YedToken
	for {
		row, err := s.Recv()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
}

func key(txid string, vout uint32, cents uint64, height int64) string {
	return fmt.Sprintf("%s:%d:%d:%d", txid, vout, cents, height)
}

// TestDevnetWalletMintSeenThroughServer: node 0 mints with its wallet; the server's view of the
// wallet's addresses equals the wallet's own, and the verdict is ok.
func TestDevnetWalletMintSeenThroughServer(t *testing.T) {
	d := openDevnet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	y := walletrpc.NewYellowbackStreamerClient(d.fork)

	info, err := y.GetYellowbackInfo(ctx, &walletrpc.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Rpcversion != common.KnownRPCVersion || info.Network != "regtest" || !info.Enabled {
		t.Fatalf("unexpected node: %+v", info)
	}
	d.warmPrice(t, ctx, y)
	var mint struct {
		Txid string `json:"txid"`
	}
	// yed_mint is two transactions (v3 section 4.6): it broadcasts the carrier and blocks until a
	// block confirms it, so a miner runs beside the call, as the functional tests' two_step does.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Second):
				var hashes []string
				_ = json.Unmarshal(func() json.RawMessage {
					b, _ := json.Marshal(1)
					r, _ := d.miner("generate", []json.RawMessage{json.RawMessage(b)})
					return r
				}(), &hashes)
			}
		}
	}()
	d.rpc(t, &mint, "yed_mint", 10000, 48)
	close(stop)
	d.waitRelayed(t, mint.Txid)
	d.mine(t, ctx, 1)
	var unspent []unspentRow
	d.rpc(t, &unspent, "yed_listunspent")
	if len(unspent) == 0 {
		t.Fatal("node 0 has no YED after the mint")
	}
	addrSet := map[string]bool{}
	var addresses []string
	for _, u := range unspent {
		if !addrSet[u.Address] {
			addrSet[u.Address] = true
			addresses = append(addresses, u.Address)
		}
	}
	tokens := tokensOf(t, ctx, y, addresses)
	var want, have []string
	for _, u := range unspent {
		want = append(want, key(u.Txid, u.Vout, u.Cents, u.Height))
	}
	for _, tk := range tokens {
		have = append(have, key(tk.Txid, tk.Vout, tk.Cents, tk.Height))
	}
	sort.Strings(want)
	sort.Strings(have)
	if strings.Join(want, "\n") != strings.Join(have, "\n") {
		t.Fatalf("GetAddressTokens != yed_listunspent:\n server %v\n node   %v", have, want)
	}
	tx, err := y.GetTxInfo(ctx, &walletrpc.YedTxid{Txid: mint.Txid})
	if err != nil {
		t.Fatal(err)
	}
	if tx.Verdict != "ok" || tx.Type != "mint" || len(tx.Assigned) != 1 || tx.Assigned[0].Vout != 1 {
		t.Fatalf("mint %s: %+v", mint.Txid, tx)
	}
	t.Logf("wallet mint %s: verdict ok; %d tokens on %d addresses identical to yed_listunspent", mint.Txid, len(tokens), len(addresses))
}

// TestDevnetRawMintThroughServer: section 5 item 5 — a client-style mint built from raw parts
// with every number from the server (collateral, fee payee, bundle when armed), dry-run,
// broadcast and judged through the server.
func TestDevnetRawMintThroughServer(t *testing.T) {
	d := openDevnet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	y := walletrpc.NewYellowbackStreamerClient(d.fork)
	old := walletrpc.NewCompactTxStreamerClient(d.fork)
	rawmint := env("LWD_RAWMINT", "")
	if rawmint == "" {
		t.Skip("LWD_RAWMINT not set")
	}

	const cents, lockBlocks = 10000, 48
	d.warmPrice(t, ctx, y)
	est, err := y.EstimateCollateral(ctx, &walletrpc.YedMintQuery{Cents: cents, LockBlocks: lockBlocks})
	if err != nil {
		t.Fatal(err)
	}
	if est.RequiredZat == 0 {
		t.Fatalf("no price after warming: %+v", est)
	}
	args := []string{rawmint, "--dir", d.dir, "--cents", fmt.Sprint(cents), "--lock-blocks", fmt.Sprint(lockBlocks),
		"--ref-height", fmt.Sprint(est.RefHeight), "--collateral-zat", fmt.Sprint(est.RequiredZat)}
	if payee, err := y.GetFeePayee(ctx, &walletrpc.YedPayeeQuery{RefHeight: uint32(est.RefHeight), CollateralZat: est.RequiredZat}); err == nil {
		addr := payee.Preferred
		if addr == "" && payee.Default != nil {
			addr = payee.Default.PayoutAddress
		}
		if addr != "" {
			args = append(args, "--fee-addr", addr)
		}
	} else if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition {
		t.Fatal(err)
	} // fee-no-eligible-payee (FEE-0): no fee output
	info, _ := y.GetYellowbackInfo(ctx, &walletrpc.Empty{})
	if info.GetAttest().GetArmed() {
		bundle, err := y.BuildBundle(ctx, &walletrpc.YedBundleQuery{RefHeight: uint32(est.RefHeight)})
		if err != nil {
			t.Fatalf("armed devnet, BuildBundle: %v", err)
		}
		args = append(args, "--bundle-hex", bundle.Hex)
		if est.AttestFeeZat > 0 {
			sel, err := y.GetSelection(ctx, &walletrpc.YedBundleQuery{RefHeight: uint32(est.RefHeight)})
			if err != nil || len(sel.Selected) == 0 {
				t.Fatalf("GetSelection: %v", err)
			}
			args = append(args, "--attest-fee-addr", sel.Selected[0].BondKeyAddress, "--attest-fee-zat", fmt.Sprint(est.AttestFeeZat))
		}
	}
	cmd := exec.Command(env("LWD_PYTHON", "python3"), args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("lwd-rawmint: %v", err)
	}
	var built struct {
		Hex, Txid, OwnerPubKey string
		CarrierTxid            *string
	}
	if err := json.Unmarshal(out, &built); err != nil {
		t.Fatalf("lwd-rawmint output: %v: %s", err, out)
	}
	rawTx := &walletrpc.RawTransaction{Data: mustHex(t, built.Hex)}

	v, err := y.ValidateRawTransaction(ctx, rawTx)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Valid || v.Verdict != "ok" || v.Type != "mint" {
		t.Fatalf("dry run refused the raw mint: %+v", v)
	}
	sent, err := old.SendTransaction(ctx, rawTx)
	if err != nil || sent.ErrorCode != 0 {
		t.Fatalf("SendTransaction: %v %+v", err, sent)
	}
	d.waitRelayed(t, built.Txid)
	d.mine(t, ctx, 1)
	tx, err := y.GetTxInfo(ctx, &walletrpc.YedTxid{Txid: built.Txid})
	if err != nil {
		t.Fatal(err)
	}
	if tx.Verdict != "ok" || tx.Type != "mint" {
		t.Fatalf("raw mint %s: %+v", built.Txid, tx)
	}
	var unspent []unspentRow
	d.rpc(t, &unspent, "yed_listunspent")
	var addr string
	for _, u := range unspent {
		if u.Txid == built.Txid {
			addr = u.Address
		}
	}
	if addr == "" {
		t.Fatalf("raw mint %s is not in node 0's yed_listunspent", built.Txid)
	}
	found := false
	for _, tk := range tokensOf(t, ctx, y, []string{addr}) {
		if tk.Txid == built.Txid && tk.Vout == 1 && tk.Cents == cents {
			found = true
		}
	}
	if !found {
		t.Fatalf("raw mint %s:1 not in GetAddressTokens(%s)", built.Txid, addr)
	}
	t.Logf("raw mint %s (carrier %v): validated, sent and confirmed through the server with verdict ok", built.Txid, built.CarrierTxid)
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b := make([]byte, len(s)/2)
	for i := 0; i < len(b); i++ {
		var v byte
		if _, err := fmt.Sscanf(s[2*i:2*i+2], "%02x", &v); err != nil {
			t.Fatalf("bad hex at %d", i)
		}
		b[i] = v
	}
	return b
}

// warmPrice mines tagged pool blocks until the node has a mint price (the fast window must hold
// enough quote tags; untagged blocks — anything node 0 generates — push quotes out of it).
func (d *devnet) warmPrice(t *testing.T, ctx context.Context, y walletrpc.YellowbackStreamerClient) {
	t.Helper()
	mined := 0
	for {
		price, err := y.GetPrice(ctx, &walletrpc.HeightFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if price.PMint != 0 {
			return
		}
		// pMint needs the slow window's minimum fill of tagged blocks; mine a fast window at a time
		// and give up past two slow windows.
		fast, slow := int(price.GetFill().GetFast().GetWindow()), int(price.GetFill().GetSlow().GetWindow())
		if fast == 0 {
			fast = 8
		}
		if slow == 0 {
			slow = 64
		}
		if mined >= 2*slow {
			t.Fatalf("the node has no mint price after mining %d tagged blocks (halts: %v)", mined, price)
		}
		d.mine(t, ctx, fast)
		mined += fast
	}
}

// waitRelayed blocks until the pool node's mempool holds txid: node 0 broadcasts, the pool mines,
// and a block generated before the relay would simply not contain the transaction.
func (d *devnet) waitRelayed(t *testing.T, txid string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := d.miner("getrawmempool", nil)
		var pool []string
		if err == nil {
			_ = json.Unmarshal(raw, &pool)
		}
		for _, id := range pool {
			if id == txid {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not reach the pool node's mempool within 30s", txid)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---- x402 lwdnext: the next block's branch id, tree state at arbitrary heights, mempool ----

type nodeChainInfo struct {
	Chain         string `json:"chain"`
	Blocks        uint64 `json:"blocks"`
	BestBlockHash string `json:"bestblockhash"`
	Consensus     struct {
		Chaintip  string `json:"chaintip"`
		Nextblock string `json:"nextblock"`
	} `json:"consensus"`
	Upgrades map[string]json.RawMessage `json:"upgrades"`
}

// TestDevnetChainInfoMatchesNode: GetChainInfo relays getblockchaininfo's consensus ids, and its
// chaintip id is the one the frozen GetLightdInfo already reports (X-F71: the frozen message has
// no nextblock field; this is where a client reads it).
func TestDevnetChainInfoMatchesNode(t *testing.T) {
	d := openDevnet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	y := walletrpc.NewYellowbackStreamerClient(d.fork)
	var before, after nodeChainInfo
	d.rpc(t, &before, "getblockchaininfo")
	got, err := y.GetChainInfo(ctx, &walletrpc.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	d.rpc(t, &after, "getblockchaininfo")
	if got.ConsensusBranchId != before.Consensus.Chaintip || got.NextBlockBranchId != before.Consensus.Nextblock {
		t.Fatalf("branch ids: server tip %s next %s, node tip %s next %s", got.ConsensusBranchId, got.NextBlockBranchId,
			before.Consensus.Chaintip, before.Consensus.Nextblock)
	}
	if got.ChainName != before.Chain || got.BlockHeight < before.Blocks || got.BlockHeight > after.Blocks {
		t.Fatalf("chain/height: server %s/%d, node %s/%d..%d", got.ChainName, got.BlockHeight, before.Chain, before.Blocks, after.Blocks)
	}
	if got.BlockHeight == before.Blocks && got.BestBlockHash != before.BestBlockHash {
		t.Fatalf("bestBlockHash %s, node %s", got.BestBlockHash, before.BestBlockHash)
	}
	if len(got.Upgrades) != len(before.Upgrades) || len(got.Upgrades) == 0 {
		t.Fatalf("upgrades: server %d, node %d", len(got.Upgrades), len(before.Upgrades))
	}
	for i := 1; i < len(got.Upgrades); i++ {
		a, b := got.Upgrades[i-1], got.Upgrades[i]
		if a.ActivationHeight > b.ActivationHeight || (a.ActivationHeight == b.ActivationHeight && a.BranchId >= b.BranchId) {
			t.Fatalf("upgrades not sorted: %v", got.Upgrades)
		}
	}
	li, err := walletrpc.NewCompactTxStreamerClient(d.fork).GetLightdInfo(ctx, &walletrpc.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if li.ConsensusBranchId != got.ConsensusBranchId || li.SaplingActivationHeight != got.SaplingActivationHeight || li.ChainName != got.ChainName {
		t.Fatalf("GetLightdInfo %+v disagrees with GetChainInfo %+v", li, got)
	}
	_, err = walletrpc.NewYellowbackStreamerClient(d.baseline).GetChainInfo(ctx, &walletrpc.Empty{})
	if st, _ := status.FromError(err); st.Code() != codes.Unimplemented {
		t.Fatalf("baseline GetChainInfo: want UNIMPLEMENTED, got %v", err)
	}
	t.Logf("GetChainInfo: chain %s height %d tip %s next %s, %d upgrades, sapling %d; GetLightdInfo agrees",
		got.ChainName, got.BlockHeight, got.ConsensusBranchId, got.NextBlockBranchId, len(got.Upgrades), got.SaplingActivationHeight)
}

type nodeTreeState struct {
	Height  int    `json:"height"`
	Hash    string `json:"hash"`
	Sapling struct {
		SkipHash    string `json:"skipHash"`
		Commitments struct {
			FinalRoot  string `json:"finalRoot"`
			FinalState string `json:"finalState"`
		} `json:"commitments"`
	} `json:"sapling"`
}

// shieldCoinbase puts one Sapling note on the chain from node 0's coinbase (z_shieldcoinbase), the
// cheapest shielded transaction a devnet wallet can make; returns the txid once it is in the pool
// node's mempool (NOT yet mined).
func (d *devnet) shieldCoinbase(t *testing.T) string {
	t.Helper()
	var zaddr string
	d.rpc(t, &zaddr, "z_getnewaddress", "sapling")
	var op struct {
		Opid string `json:"opid"`
	}
	d.rpc(t, &op, "z_shieldcoinbase", "*", zaddr, 0.0001, 5)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var ops []struct {
			Status string `json:"status"`
			Error  struct {
				Message string `json:"message"`
			} `json:"error"`
			Result struct {
				Txid string `json:"txid"`
			} `json:"result"`
		}
		d.rpc(t, &ops, "z_getoperationstatus", []string{op.Opid})
		if len(ops) == 1 && ops[0].Status == "success" {
			d.waitRelayed(t, ops[0].Result.Txid)
			return ops[0].Result.Txid
		}
		if len(ops) == 1 && ops[0].Status == "failed" {
			t.Fatalf("z_shieldcoinbase: %s", ops[0].Error.Message)
		}
		if time.Now().After(deadline) {
			t.Fatalf("z_shieldcoinbase %s did not finish in 2 min", op.Opid)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// treeStateBoth fetches GetTreeState(id) from the fork and the baseline, asserts byte equality, and
// returns the fork's answer.
func treeStateBoth(t *testing.T, ctx context.Context, d *devnet, id *walletrpc.BlockID) *walletrpc.TreeState {
	t.Helper()
	f, err := walletrpc.NewCompactTxStreamerClient(d.fork).GetTreeState(ctx, id)
	if err != nil {
		t.Fatalf("fork GetTreeState(%+v): %v", id, err)
	}
	b, err := walletrpc.NewCompactTxStreamerClient(d.baseline).GetTreeState(ctx, id)
	if err != nil {
		t.Fatalf("baseline GetTreeState(%+v): %v", id, err)
	}
	fb, _ := proto.Marshal(f)
	bb, _ := proto.Marshal(b)
	if !bytes.Equal(fb, bb) {
		t.Fatalf("GetTreeState(%+v) differs between fork and baseline:\n fork %+v\n base %+v", id, f, b)
	}
	return f
}

// TestDevnetTreeStateAtArbitraryHeights: the stock GetTreeState, this fork's checkpoint source
// for a light client that scans compact blocks (neither node line has z_getsubtreesbyindex on
// 4.5.0, so GetSubtreeRoots is not offered), answers at any height and by hash, equals the node's
// z_gettreestate, is byte-identical to the baseline's, and reflects a Sapling note once one is
// mined (closes plan F-11's open item: the tree was only ever checked empty).
func TestDevnetTreeStateAtArbitraryHeights(t *testing.T) {
	d := openDevnet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	check := func(height uint64) *walletrpc.TreeState {
		t.Helper()
		var node nodeTreeState
		d.rpc(t, &node, "z_gettreestate", fmt.Sprint(height))
		byHeight := treeStateBoth(t, ctx, d, &walletrpc.BlockID{Height: height})
		byHash := treeStateBoth(t, ctx, d, &walletrpc.BlockID{Hash: mustHex(t, node.Hash)})
		for _, ts := range []*walletrpc.TreeState{byHeight, byHash} {
			if ts.Height != height || ts.Hash != node.Hash || ts.Tree != node.Sapling.Commitments.FinalState || ts.Network != "regtest" {
				t.Fatalf("GetTreeState(%d) = %+v, node z_gettreestate = %+v", height, ts, node)
			}
		}
		return byHeight
	}
	var tip uint64
	d.rpc(t, &tip, "getblockcount")
	emptyTree := check(1).Tree
	for _, h := range []uint64{2, tip / 3, tip / 2, tip - 1, tip} {
		check(h)
	}
	txid := d.shieldCoinbase(t)
	mined := d.mine(t, ctx, 1)
	after := check(mined)
	if after.Tree == emptyTree || len(after.Tree) <= len(emptyTree) {
		t.Fatalf("tree state at %d after shielding %s is still the empty tree (%s)", mined, txid, after.Tree)
	}
	if before := check(mined - 1); before.Tree != emptyTree {
		t.Fatalf("tree state at %d (before the note) changed: %s", mined-1, before.Tree)
	}
	if _, err := walletrpc.NewCompactTxStreamerClient(d.fork).GetTreeState(ctx, &walletrpc.BlockID{Height: mined + 1000}); err == nil {
		t.Fatal("GetTreeState past the tip must fail")
	}
	t.Logf("GetTreeState: heights 1,2,%d,%d,%d,%d by height and hash equal z_gettreestate and the baseline; after %s at %d the tree is %d hex chars (empty: %d)",
		tip/3, tip/2, tip-1, tip, txid[:8], mined, len(after.Tree), len(emptyTree))
}

func mempoolTxids(t *testing.T, ctx context.Context, conn *grpc.ClientConn) ([]string, [][]byte) {
	t.Helper()
	s, err := walletrpc.NewCompactTxStreamerClient(conn).GetMempoolTx(ctx, &walletrpc.Exclude{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	var raw [][]byte
	for {
		tx, err := s.Recv()
		if err == io.EOF {
			return ids, raw
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := proto.Marshal(tx)
		raw = append(raw, b)
		ids = append(ids, fmt.Sprintf("%x", reverse(tx.Hash)))
	}
}

func reverse(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

// TestDevnetMempoolStreamsSaplingOnly pins X-F70: GetMempoolTx (stock, 0.4.6) streams a mempool
// transaction only when it has Sapling elements; a purely transparent one — every YED transfer,
// every x402 transparent payment — is never streamed, on the fork exactly as on the baseline.
// A light agent therefore cannot see a pending spend of its own coin through this method.
func TestDevnetMempoolStreamsSaplingOnly(t *testing.T) {
	d := openDevnet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	d.mine(t, ctx, 1) // start from an empty mempool
	var to, ttxid string
	d.rpc(t, &to, "getnewaddress")
	d.rpc(t, &ttxid, "sendtoaddress", to, 1.5)
	d.waitRelayed(t, ttxid)
	ztxid := d.shieldCoinbase(t)
	var pool []string
	d.rpc(t, &pool, "getrawmempool")
	has := func(ids []string, id string) bool {
		for _, x := range ids {
			if x == id {
				return true
			}
		}
		return false
	}
	if !has(pool, ttxid) || !has(pool, ztxid) {
		t.Fatalf("node 0 mempool %v lacks %s or %s", pool, ttxid, ztxid)
	}
	var forkIDs []string
	var forkRaw, baseRaw [][]byte
	deadline := time.Now().Add(30 * time.Second)
	for {
		forkIDs, forkRaw = mempoolTxids(t, ctx, d.fork)
		if has(forkIDs, ztxid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fork GetMempoolTx never streamed the Sapling tx %s (streamed %v)", ztxid, forkIDs)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if has(forkIDs, ttxid) {
		t.Fatalf("GetMempoolTx streamed the transparent tx %s: X-F70 no longer holds, update the docs", ttxid)
	}
	var baseIDs []string
	baseIDs, baseRaw = mempoolTxids(t, ctx, d.baseline)
	sort.Strings(forkIDs)
	sort.Strings(baseIDs)
	if strings.Join(forkIDs, ",") != strings.Join(baseIDs, ",") || len(forkRaw) != len(baseRaw) {
		t.Fatalf("GetMempoolTx differs: fork %v baseline %v", forkIDs, baseIDs)
	}
	d.mine(t, ctx, 1)
	t.Logf("GetMempoolTx: Sapling tx %s streamed, transparent tx %s not streamed, fork and baseline agree (%d txs)", ztxid[:8], ttxid[:8], len(forkIDs))
}
