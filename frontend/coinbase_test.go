// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zcash/lightwalletd/common"
	"github.com/zcash/lightwalletd/walletrpc"
)

// coinbaseNode: getaddressutxos lists a coinbase output (txid ...c0) and an ordinary one
// (txid ...0d); getrawtransaction answers the two prefixes and counts its calls.
type coinbaseNode struct {
	rawCalls map[string]int
	fail     bool
}

func (n *coinbaseNode) RawRequest(method string, params []json.RawMessage) (json.RawMessage, error) {
	switch method {
	case "getaddressutxos":
		return json.RawMessage(fmt.Sprintf(`[
{"address":"smFv3B1FhYXS2ZU1e54x9sdLfq7e46YhxVT","txid":"%064x","outputIndex":0,"script":"76a914%040x88ac","satoshis":1250000000,"height":300},
{"address":"smFv3B1FhYXS2ZU1e54x9sdLfq7e46YhxVT","txid":"%064x","outputIndex":1,"script":"76a914%040x88ac","satoshis":5000,"height":301}]`,
			0xc0, 1, 0x0d, 1)), nil
	case "getrawtransaction":
		var txid string
		_ = json.Unmarshal(params[0], &txid)
		n.rawCalls[txid]++
		if n.fail {
			return nil, errors.New("-5: No such mempool or blockchain transaction")
		}
		if string(params[1]) != "1" {
			return nil, fmt.Errorf("verbosity %s", params[1])
		}
		in := fmt.Sprintf("%064x00000000", 0xab) // spends output 0 of a real txid
		if strings.HasSuffix(txid, "c0") {
			in = strings.Repeat("0", 64) + "ffffffff" // the null prevout
		}
		return json.RawMessage(`{"hex":"0400008085202f8901` + in + `0affffffff","height":300}`), nil
	}
	return nil, fmt.Errorf("unexpected %s", method)
}

// TestGetAddressUtxosCoinbase: the reply marks the coinbase output and only it, on both the
// unary and the streaming method; each txid is looked up once (the verdict is cached); a lookup
// the node cannot answer fails the call rather than report a coinbase as an ordinary coin.
func TestGetAddressUtxosCoinbase(t *testing.T) {
	node := &coinbaseNode{rawCalls: map[string]int{}}
	previous := common.RawRequest
	common.RawRequest = node.RawRequest
	defer func() { common.RawRequest = previous }()
	coinbaseCache.Lock()
	coinbaseCache.m = map[string]bool{}
	coinbaseCache.Unlock()
	s := &lwdStreamer{}
	arg := func() *walletrpc.GetAddressUtxosArg {
		return &walletrpc.GetAddressUtxosArg{Addresses: []string{"smFv3B1FhYXS2ZU1e54x9sdLfq7e46YhxVT"}}
	}
	check := func(name string, rows []*walletrpc.GetAddressUtxosReply) {
		if len(rows) != 2 || !rows[0].Coinbase || rows[1].Coinbase || rows[0].ValueZat != 1250000000 {
			t.Fatalf("%s: %v", name, rows)
		}
	}
	r, err := s.GetAddressUtxos(context.Background(), arg())
	if err != nil {
		t.Fatal(err)
	}
	check("GetAddressUtxos", r.AddressUtxos)
	us := &utxoStream{}
	if err := s.GetAddressUtxosStream(arg(), us); err != nil {
		t.Fatal(err)
	}
	var rows []*walletrpc.GetAddressUtxosReply
	for _, m := range us.rows {
		rows = append(rows, m.(*walletrpc.GetAddressUtxosReply))
	}
	check("GetAddressUtxosStream", rows)
	if len(node.rawCalls) != 2 {
		t.Fatalf("lookups: %v", node.rawCalls)
	}
	for txid, c := range node.rawCalls {
		if c != 1 {
			t.Fatalf("%s looked up %d times, want once (cached)", txid, c)
		}
	}

	coinbaseCache.Lock()
	coinbaseCache.m = map[string]bool{}
	coinbaseCache.Unlock()
	node.fail = true
	if _, err := s.GetAddressUtxos(context.Background(), arg()); err == nil || !strings.Contains(err.Error(), "coinbase lookup") {
		t.Fatalf("a failed lookup must fail the call, got %v", err)
	}
	common.DarksideEnabled = true
	defer func() { common.DarksideEnabled = false }()
	r, err = s.GetAddressUtxos(context.Background(), arg())
	if err != nil || len(r.AddressUtxos) != 2 || r.AddressUtxos[0].Coinbase {
		t.Fatalf("darkside: staged UTXOs without a staged transaction are reported as before: %v %v", r, err)
	}
}
