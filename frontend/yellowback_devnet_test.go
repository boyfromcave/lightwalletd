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
