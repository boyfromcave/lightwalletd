// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

package parser

import "testing"

// TestFeePartsAmountBounds: an amount past MAX_MONEY (which consensus refuses) yields no fee
// parts, so a uint64 that would wrap negative in the int64 sum never reaches the fee check.
func TestFeePartsAmountBounds(t *testing.T) {
	ok := func(raw *rawTransaction) bool {
		_, _, _, ok := (&Transaction{rawTransaction: raw}).FeeParts()
		return ok
	}
	good := &rawTransaction{
		transparentOutputs: []*txOut{{Value: MaxMoney}},
		valueBalance:       -MaxMoney,
		joinSplits:         []*joinSplit{{vpubNew: MaxMoney, vpubOld: MaxMoney}},
	}
	if !ok(good) {
		t.Fatal("amounts at MAX_MONEY must be accepted")
	}
	if _, out, in, _ := (&Transaction{rawTransaction: good}).FeeParts(); out != MaxMoney || in != -MaxMoney {
		t.Fatalf("FeeParts = %d out, %d in", out, in)
	}
	for i, bad := range []*rawTransaction{
		{transparentOutputs: []*txOut{{Value: MaxMoney + 1}}},
		{valueBalance: MaxMoney + 1},
		{valueBalance: -MaxMoney - 1},
		{joinSplits: []*joinSplit{{vpubNew: 1 << 63}}},
		{joinSplits: []*joinSplit{{vpubOld: 1 << 63}}},
	} {
		if ok(bad) {
			t.Errorf("case %d: an amount past MAX_MONEY was accepted", i)
		}
	}
}
