// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

package parser

import "encoding/hex"

// MaxMoney is the consensus bound on any single amount, in zatoshi (21 million YEC; the node's
// MAX_MONEY, amount.h).
const MaxMoney = 21000000 * 100000000

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
func (tx *Transaction) FeeParts() (prevouts []Prevout, valueOut uint64, shieldedIn int64, ok bool) {
	for _, in := range tx.transparentInputs {
		prevouts = append(prevouts, Prevout{TxID: hex.EncodeToString(Reverse(append([]byte(nil), in.PrevTxHash...))), Index: in.PrevTxOutIndex})
	}
	// Consensus bounds every one of these amounts by MAX_MONEY (an output value, vpub_old, vpub_new,
	// |valueBalance|); a transaction that breaks one is invalid, and its sums could wrap here, so it
	// has no fee to report.
	for _, out := range tx.transparentOutputs {
		if out.Value > MaxMoney {
			return nil, 0, 0, false
		}
		valueOut += out.Value
	}
	if tx.valueBalance > MaxMoney || tx.valueBalance < -MaxMoney {
		return nil, 0, 0, false
	}
	shieldedIn = tx.valueBalance
	for _, js := range tx.joinSplits {
		if js.vpubNew > MaxMoney || js.vpubOld > MaxMoney {
			return nil, 0, 0, false
		}
		shieldedIn += int64(js.vpubNew) - int64(js.vpubOld)
	}
	return prevouts, valueOut, shieldedIn, true
}
