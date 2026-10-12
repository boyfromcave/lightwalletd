// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

package parser

import "encoding/hex"

// Prevout names the transparent output a transaction input spends: the txid in display order
// (hex, as the node's RPCs take it) and the output index.
type Prevout struct {
	TxID  string
	Index uint32
}

// FeeParts returns what a transaction's fee is computed from without the values of its transparent
// inputs, which only the node's UTXO set knows: the prevouts those inputs spend, the transparent
// output total, and the net value the shielded side moves into the transparent pool (Sapling
// valueBalance plus every JoinSplit's vpub_new - vpub_old). The fee is then
// sum(prevout values) + shieldedIn - valueOut. Used by SendTransaction's fee ceiling (rpcversion 8).
func (tx *Transaction) FeeParts() (prevouts []Prevout, valueOut uint64, shieldedIn int64) {
	for _, in := range tx.transparentInputs {
		prevouts = append(prevouts, Prevout{TxID: hex.EncodeToString(Reverse(append([]byte(nil), in.PrevTxHash...))), Index: in.PrevTxOutIndex})
	}
	for _, out := range tx.transparentOutputs {
		valueOut += out.Value
	}
	shieldedIn = tx.valueBalance
	for _, js := range tx.joinSplits {
		shieldedIn += int64(js.vpubNew) - int64(js.vpubOld)
	}
	return prevouts, valueOut, shieldedIn
}
