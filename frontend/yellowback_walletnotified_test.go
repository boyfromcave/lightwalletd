// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

// Offline tests of the devnet suite's wallet-notification barrier (yellowback_devnet_sync_test.go).
package frontend

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"
)

// fakeNotifyNode answers getblockchaininfo/getmempoolinfo with fullyNotified false for the first
// chainLag/poolLag calls, then true; a nil lag drops the field (a non-regtest node).
type fakeNotifyNode struct {
	chainLag, poolLag *int
	calls             map[string]int
}

func (f *fakeNotifyNode) call(method string, params []json.RawMessage) (json.RawMessage, error) {
	f.calls[method]++
	var lag *int
	switch method {
	case "getblockchaininfo":
		lag = f.chainLag
	case "getmempoolinfo":
		lag = f.poolLag
	default:
		return nil, errors.New("unexpected method " + method)
	}
	if lag == nil {
		return json.RawMessage(`{"blocks":5}`), nil
	}
	if f.calls[method] <= *lag {
		return json.RawMessage(`{"fullyNotified":false}`), nil
	}
	return json.RawMessage(`{"fullyNotified":true}`), nil
}

func intp(n int) *int { return &n }

// The barrier returns only once BOTH the chain and the mempool notifications have caught up: the
// nightly's reject came from a block the wallet had not been told about, the mempool flag alone
// would not have caught it.
func TestWalletNotifiedWaitsForChainAndMempool(t *testing.T) {
	for _, c := range []struct{ chain, pool int }{{0, 0}, {3, 0}, {0, 3}, {2, 4}} {
		f := &fakeNotifyNode{chainLag: intp(c.chain), poolLag: intp(c.pool), calls: map[string]int{}}
		if err := waitWalletNotified(f.call, 5*time.Second, time.Millisecond); err != nil {
			t.Fatalf("lag chain %d pool %d: %v", c.chain, c.pool, err)
		}
		want := c.chain
		if c.pool > want {
			want = c.pool
		}
		if f.calls["getblockchaininfo"] != want+1 || f.calls["getmempoolinfo"] != want+1 {
			t.Fatalf("lag chain %d pool %d: returned after %v, want %d polls of each", c.chain, c.pool, f.calls, want+1)
		}
	}
}

func TestWalletNotifiedTimesOut(t *testing.T) {
	f := &fakeNotifyNode{chainLag: intp(1 << 30), poolLag: intp(0), calls: map[string]int{}}
	err := waitWalletNotified(f.call, 20*time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not notified") {
		t.Fatalf("a wallet that never catches up: err = %v", err)
	}
}

func TestWalletNotifiedNeedsTheRegtestField(t *testing.T) {
	f := &fakeNotifyNode{chainLag: nil, poolLag: intp(0), calls: map[string]int{}}
	err := waitWalletNotified(f.call, time.Second, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "fullyNotified") {
		t.Fatalf("a node without fullyNotified: err = %v", err)
	}
	failing := func(string, []json.RawMessage) (json.RawMessage, error) { return nil, errors.New("connection refused") }
	if err := waitWalletNotified(failing, time.Second, time.Millisecond); err == nil {
		t.Fatal("an RPC error must surface")
	}
}

// The push-tier guard on the nightly suite itself: the devnet's mine helper (built only with
// -tags devnet) must keep the barrier, or a test that sends right after a block races the
// wallet notifier again (nightly 38340276753).
func TestWalletNotifiedDevnetMineKeepsTheBarrier(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "yellowback_devnet_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "mine" || fn.Recv == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "waitWalletNotified" {
					found = true
				}
			}
			return true
		})
	}
	if !found {
		t.Fatal("(*devnet).mine in yellowback_devnet_test.go no longer calls waitWalletNotified")
	}
}
