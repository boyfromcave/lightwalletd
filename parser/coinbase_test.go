// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

package parser

import (
	"bufio"
	"encoding/hex"
	"os"
	"testing"
)

// TestIsCoinbase: in every block of testdata/blocks the first transaction is the coinbase and
// no other is; a truncated prefix is an error, not a verdict.
func TestIsCoinbase(t *testing.T) {
	f, err := os.Open("../testdata/blocks")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 1<<20), 1<<24)
	blocks, txs := 0, 0
	for scan.Scan() {
		data, err := hex.DecodeString(scan.Text())
		if err != nil {
			t.Fatal(err)
		}
		block := NewBlock()
		if _, err := block.ParseFromSlice(data); err != nil {
			t.Fatal(err)
		}
		for i, tx := range block.Transactions() {
			got, err := IsCoinbase(tx.Bytes())
			if err != nil || got != (i == 0) {
				t.Fatalf("block %d tx %d: IsCoinbase = %v, %v", block.GetHeight(), i, got, err)
			}
			txs++
		}
		blocks++
	}
	if blocks == 0 || txs <= blocks {
		t.Fatalf("too little test data: %d blocks, %d transactions", blocks, txs)
	}
	// A spend of output 0xffffffff of a non-null txid, and a null txid with another index, are
	// not coinbase (v4 header, one input).
	v4 := "0400008085202f8901"
	for _, in := range []string{
		"01" + "00000000000000000000000000000000000000000000000000000000000000" + "ffffffff",
		"0000000000000000000000000000000000000000000000000000000000000000" + "00000000",
	} {
		data, _ := hex.DecodeString(v4 + in + "00ffffffff")
		if got, err := IsCoinbase(data); err != nil || got {
			t.Fatalf("%s: IsCoinbase = %v, %v", in, got, err)
		}
	}
	for _, short := range []string{"", "04000080", "0400008085202f89", "0400008085202f8901" + "00"} {
		data, _ := hex.DecodeString(short)
		if _, err := IsCoinbase(data); err == nil {
			t.Fatalf("%q: a truncated prefix must be an error", short)
		}
	}
}
