// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

// SendTransaction's high-fee path (rpcversion 8, the fee structure; review finding M-2). Post-quantum
// and Yellowback transactions pay a dollar-per-KB network fee at the attested price
// (yed_getinfo.params.fees.network: plain PQ $0.02/KB, vault/Yellowback $0.05/KB), which can exceed the
// node's -maxtxfee: an SLH-DSA vault spend is ~8 KB, so at a low YEC price its correctly priced fee is
// "absurdly high" to sendrawtransaction's default check and a light wallet's transaction would be
// refused. The node's own Yellowback paths submit with that check off and enforce the Yellowback cap
// themselves; this server does the same for client-submitted transactions:
//
//  1. sendrawtransaction "hex" — the stock check; almost every transaction ends here.
//  2. Only when the node refuses it as absurdly high, and only on a node that serves yed_getinfo, the
//     fee is established — from the node's message where it states it (ycash-dd: "absurdly high fees
//     <txid>, <fee> > <maxtxfee>"), else computed here from the transaction (parser.FeeParts) and the
//     values of its transparent prevouts (gettxout, mempool included; ycash6 states only
//     "absurdly-high-fee") — and compared with params.fees.network.yellowback.maxTxFeeZat (the
//     effective cap, min(max(-maxtxfee, $2 at the price), -yellowbackmaxtxfee); maxTxFeeCeilingZat only
//     when the node does not report it). At or below it the transaction is resubmitted with
//     allowhighfees = true; above it the refusal names the fee and the cap.
//
// When the fee cannot be established (an unparsable transaction, a prevout the node does not have)
// or the node has no Yellowback service, the node's original refusal stands: the sanity ceiling is
// never skipped. The extra calls are made only on the refused path, so the common path costs nothing.
package frontend

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/zcash/lightwalletd/common"
	"github.com/zcash/lightwalletd/parser"
)

// absurdFeeMessage: ycash-dd (v4.5.0) "AcceptToMemoryPool: absurdly high fees <txid>, <fee> > <max>";
// ycash6 (6.20.0) "256: absurdly-high-fee" (the amounts are in the debug message only).
var absurdFeeMessage = regexp.MustCompile(`absurdly[ -]high[ -]fee`)
var absurdFeeAmount = regexp.MustCompile(`absurdly high fees [0-9a-fA-F]+, ([0-9]+) > [0-9]+`)

// maxFeeInputs bounds the gettxout calls one refused transaction may cost (YEW's MAX_INPUTS and the
// node's own YED selection bound; a wallet's legitimate high-fee transaction holds a handful).
const maxFeeInputs = 250

// yellowbackFeeCeiling reads the Yellowback fee cap from yed_getinfo: the effective cap
// params.fees.network.yellowback.maxTxFeeZat (what the node's own wallet relays within), or, on a node
// that does not report it, maxTxFeeCeilingZat. False when the node has no Yellowback service or
// reports neither.
func yellowbackFeeCeiling() (int64, bool) {
	raw, err := common.RawRequest("yed_getinfo", nil)
	if err != nil {
		return 0, false
	}
	var info struct {
		RPCVersion int64 `json:"rpcversion"`
		Params     struct {
			Fees struct {
				Network struct {
					Yellowback struct {
						MaxTxFeeCeilingZat int64 `json:"maxTxFeeCeilingZat"`
						MaxTxFeeZat        int64 `json:"maxTxFeeZat"`
					} `json:"yellowback"`
				} `json:"network"`
			} `json:"fees"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &info) != nil || info.RPCVersion < common.KnownRPCVersion {
		return 0, false
	}
	yb := info.Params.Fees.Network.Yellowback
	if yb.MaxTxFeeZat > 0 {
		return yb.MaxTxFeeZat, true
	}
	return yb.MaxTxFeeCeilingZat, yb.MaxTxFeeCeilingZat > 0
}

// zatFromDecimal converts the node's decimal YEC amount (gettxout "value") to zatoshi exactly.
func zatFromDecimal(n json.Number) (int64, bool) {
	s := string(n)
	if s == "" || strings.HasPrefix(s, "-") || strings.ContainsAny(s, "eE") {
		return 0, false
	}
	whole, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		whole, frac = s[:i], s[i+1:]
	}
	if len(frac) > 8 || whole == "" {
		return 0, false
	}
	frac += strings.Repeat("0", 8-len(frac))
	w, err1 := strconv.ParseInt(whole, 10, 64)
	f, err2 := strconv.ParseInt(frac, 10, 64)
	if err1 != nil || err2 != nil || w > (1<<62)/100000000 {
		return 0, false
	}
	return w*100000000 + f, true
}

// computeFee: sum(prevout values) + shielded inflow - transparent outputs, the prevout values from
// gettxout (includemempool = true). False on anything the node cannot answer.
func computeFee(data []byte) (int64, bool) {
	tx := parser.NewTransaction()
	rest, err := tx.ParseFromSlice(data)
	if err != nil || len(rest) != 0 {
		return 0, false
	}
	prevouts, valueOut, shieldedIn, ok := tx.FeeParts()
	if !ok || len(prevouts) > maxFeeInputs {
		return 0, false
	}
	in := shieldedIn
	for _, p := range prevouts {
		txid, _ := json.Marshal(p.TxID)
		raw, err := common.RawRequest("gettxout", []json.RawMessage{txid, json.RawMessage(strconv.FormatUint(uint64(p.Index), 10)), json.RawMessage("true")})
		if err != nil {
			return 0, false
		}
		var out struct {
			Value json.Number `json:"value"`
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if dec.Decode(&out) != nil {
			return 0, false // "null": spent or unknown
		}
		v, ok := zatFromDecimal(out.Value)
		if !ok {
			return 0, false
		}
		in += v
	}
	if valueOut > uint64(1)<<62 {
		return 0, false
	}
	return in - int64(valueOut), true
}

// highFeeVerdict decides a transaction the node refused with rpcErr. allow: resubmit with
// allowhighfees = true. refusal (when not allowed and non-empty): the message replacing the node's.
func highFeeVerdict(data []byte, rpcErr error) (allow bool, refusal string) {
	if rpcErr == nil || !absurdFeeMessage.MatchString(rpcErr.Error()) {
		return false, ""
	}
	ceiling, ok := yellowbackFeeCeiling()
	if !ok {
		return false, ""
	}
	if m := absurdFeeAmount.FindStringSubmatch(rpcErr.Error()); m != nil {
		fee, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return false, ""
		}
		return verdict(fee, ceiling)
	}
	fee, ok := computeFee(data)
	if !ok {
		return false, ""
	}
	return verdict(fee, ceiling)
}

func verdict(fee, ceiling int64) (bool, string) {
	if fee < 0 {
		return false, ""
	}
	if fee > ceiling {
		return false, fmt.Sprintf("absurdly-high-fee: fee %d zat exceeds the Yellowback network-fee cap %d zat (yed_getinfo.params.fees.network.yellowback.maxTxFeeZat)", fee, ceiling)
	}
	return true, ""
}
