// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

package frontend

import (
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/pkg/errors"
	"github.com/zcash/lightwalletd/common"
	"github.com/zcash/lightwalletd/parser"
)

// GetAddressUtxosReply.coinbase (beta4). The node's getaddressutxos (ycashd and koganyd alike)
// does not say whether an output is a coinbase, so the server asks for the transaction once
// (getrawtransaction <txid> 1, the call GetTransaction already makes; both backends answer it
// for a confirmed transaction) and reads its first input. Whether a txid is a coinbase never
// changes (the txid commits to its inputs), so the verdict is cached by txid, both ways, and a
// reorg cannot make it stale. The cache is bounded: when full it is dropped and refilled.

const coinbaseCacheMax = 200000

var coinbaseCache = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

// txIsCoinbase answers for a txid in the node's display (big-endian) hex form.
func txIsCoinbase(txid string) (bool, error) {
	coinbaseCache.Lock()
	v, ok := coinbaseCache.m[txid]
	coinbaseCache.Unlock()
	if ok {
		return v, nil
	}
	param, err := json.Marshal(txid)
	if err != nil {
		return false, err
	}
	result, rpcErr := common.RawRequest("getrawtransaction", []json.RawMessage{param, json.RawMessage("1")})
	if rpcErr != nil && common.DarksideEnabled {
		// Darkside's staged UTXOs need not name a staged transaction: report them as before.
		return false, nil
	}
	if rpcErr != nil {
		return false, errors.Wrap(rpcErr, "coinbase lookup: getrawtransaction "+txid)
	}
	var txinfo common.ZcashdRpcReplyGetrawtransaction
	if err := json.Unmarshal(result, &txinfo); err != nil {
		return false, errors.Wrap(err, "coinbase lookup")
	}
	raw, err := hex.DecodeString(txinfo.Hex)
	if err != nil {
		return false, errors.Wrap(err, "coinbase lookup")
	}
	v, err = parser.IsCoinbase(raw)
	if err != nil {
		return false, errors.Wrap(err, "coinbase lookup: "+txid)
	}
	coinbaseCache.Lock()
	if len(coinbaseCache.m) >= coinbaseCacheMax {
		coinbaseCache.m = map[string]bool{}
	}
	coinbaseCache.m[txid] = v
	coinbaseCache.Unlock()
	return v, nil
}
