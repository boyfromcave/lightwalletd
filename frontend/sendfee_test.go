// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

package frontend

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zcash/lightwalletd/common"
	"github.com/zcash/lightwalletd/walletrpc"
)

// v4Tx builds a minimal Sapling-version (v4) transaction: one transparent input spending
// prevTxid:0 and one transparent output of outZat, no shielded parts.
func v4Tx(prevTxidDisplay string, outZat uint64) []byte {
	b := make([]byte, 0, 128)
	b = binary.LittleEndian.AppendUint32(b, 0x80000004)
	b = binary.LittleEndian.AppendUint32(b, 0x892f2085)
	b = append(b, 1) // vin
	h, _ := hex.DecodeString(prevTxidDisplay)
	for i := len(h) - 1; i >= 0; i-- { // the wire order is the reverse of the display order
		b = append(b, h[i])
	}
	b = binary.LittleEndian.AppendUint32(b, 0)
	b = append(b, 0) // empty scriptSig
	b = binary.LittleEndian.AppendUint32(b, 0xffffffff)
	b = append(b, 1) // vout
	b = binary.LittleEndian.AppendUint64(b, outZat)
	b = append(b, 0)                           // empty script
	b = binary.LittleEndian.AppendUint32(b, 0) // nLockTime
	b = binary.LittleEndian.AppendUint32(b, 0) // nExpiryHeight
	b = binary.LittleEndian.AppendUint64(b, 0) // valueBalance
	b = append(b, 0, 0, 0)                     // no spends, outputs, joinsplits
	return b
}

// highFeeNode answers sendrawtransaction (refusing as absurdly high unless allowhighfees is true),
// yed_getinfo (the rpcversion-8 contract example, or "Method not found") and gettxout.
type highFeeNode struct {
	refusal    string // the node's absurd-fee error
	yellowback bool
	prevValue  string // gettxout value, "" = null
	sends      [][]json.RawMessage
	calls      []string
	contract   contract
}

func (n *highFeeNode) RawRequest(method string, params []json.RawMessage) (json.RawMessage, error) {
	n.calls = append(n.calls, method)
	switch method {
	case "sendrawtransaction":
		n.sends = append(n.sends, params)
		if len(params) > 1 && string(params[1]) == "true" {
			return json.RawMessage(`"` + exampleTxid + `"`), nil
		}
		return nil, errors.New(n.refusal)
	case "yed_getinfo":
		if !n.yellowback {
			return nil, errors.New("-32601: Method not found")
		}
		return n.contract["yed_getinfo"], nil
	case "gettxout":
		if n.prevValue == "" {
			return json.RawMessage("null"), nil
		}
		return json.RawMessage(`{"bestblock":"00","confirmations":3,"value":` + n.prevValue + `}`), nil
	}
	return nil, errors.New("-32601: Method not found")
}

// TestSendHighFee (review M-2): a transaction refused as absurdly high is resubmitted with
// allowhighfees = true when its fee is within params.fees.network.yellowback.maxTxFeeCeilingZat (the
// contract example: 1,000,000,000 zat), refused with the ceiling named above it, and left to the
// node's refusal when the node has no Yellowback service or the fee cannot be established.
func TestSendHighFee(t *testing.T) {
	c := loadContract(t)
	var gi struct {
		Returns json.RawMessage `json:"returns"`
	}
	if err := json.Unmarshal(c["yed_getinfo"], &gi); err != nil {
		t.Fatal(err)
	}
	ycashdd := func(fee string) string {
		return "-26: AcceptToMemoryPool: absurdly high fees " + exampleTxid + ", " + fee + " > 10000000"
	}
	tx := v4Tx(exampleTxid, 100000000)
	cases := []struct {
		name       string
		refusal    string
		yellowback bool
		prevValue  string
		wantResend bool
		wantCode   int32
		wantMsg    string
	}{
		{"ycash-dd, under the ceiling", ycashdd("50000000"), true, "", true, 0, exampleTxid},
		{"ycash-dd, at the ceiling", ycashdd("1000000000"), true, "", true, 0, exampleTxid},
		{"ycash-dd, over the ceiling", ycashdd("1000000001"), true, "", false, -26, "absurdly-high-fee: fee 1000000001 zat exceeds"},
		{"no Yellowback service", ycashdd("50000000"), false, "", false, -26, "AcceptToMemoryPool: absurdly high fees"},
		// ycash6 states no amount: the fee is computed, prevout 1.5 YEC - output 1 YEC = 0.5 YEC.
		{"ycash6, computed under", "-26: 256: absurdly-high-fee", true, "1.5", true, 0, exampleTxid},
		// prevout 11.00000001 YEC - 1 YEC = 1,000,000,001 zat.
		{"ycash6, computed over", "-26: 256: absurdly-high-fee", true, "11.00000001", false, -26, "absurdly-high-fee: fee 1000000001 zat"},
		{"ycash6, prevout unknown", "-26: 256: absurdly-high-fee", true, "", false, -26, "256: absurdly-high-fee"},
		{"another refusal", "-26: 18: bad-txns-in-belowout", true, "1.5", false, -26, "18: bad-txns-in-belowout"},
	}
	saved := common.RawRequest
	defer func() { common.RawRequest = saved }()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &highFeeNode{refusal: tc.refusal, yellowback: tc.yellowback, prevValue: tc.prevValue,
				contract: contract{"yed_getinfo": gi.Returns}}
			common.RawRequest = node.RawRequest
			lwd := &lwdStreamer{}
			resp, err := lwd.SendTransaction(context.Background(), &walletrpc.RawTransaction{Data: tx})
			if err != nil {
				t.Fatal(err)
			}
			resent := len(node.sends) == 2
			if resent != tc.wantResend || len(node.sends) > 2 {
				t.Fatalf("sendrawtransaction calls %d (%v), want resend %v", len(node.sends), node.calls, tc.wantResend)
			}
			if resent && (len(node.sends[1]) != 2 || string(node.sends[1][1]) != "true") {
				t.Errorf("resubmission params %s, want allowhighfees true", node.sends[1])
			}
			if resp.ErrorCode != tc.wantCode || !strings.Contains(resp.ErrorMessage, tc.wantMsg) {
				t.Errorf("response %d %q, want %d containing %q", resp.ErrorCode, resp.ErrorMessage, tc.wantCode, tc.wantMsg)
			}
			if strings.Contains(tc.refusal, "bad-txns") && len(node.calls) != 1 {
				t.Errorf("a refusal that is not about the fee must cost no further call: %v", node.calls)
			}
		})
	}
}

func TestSendHighFeeZatFromDecimal(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "1": 100000000, "1.5": 150000000, "0.00000001": 1, "21000000.00000000": 2100000000000000} {
		if got, ok := zatFromDecimal(json.Number(in)); !ok || got != want {
			t.Errorf("zatFromDecimal(%s) = %d %v, want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "-1", "1e3", "0.000000001", ".5"} {
		if _, ok := zatFromDecimal(json.Number(bad)); ok {
			t.Errorf("zatFromDecimal(%q) accepted", bad)
		}
	}
}
