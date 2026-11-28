package parser

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func cs(n int) []byte {
	switch {
	case n < 253:
		return []byte{byte(n)}
	case n <= 0xffff:
		return []byte{253, byte(n), byte(n >> 8)}
	default:
		b := make([]byte, 5)
		b[0] = 254
		binary.LittleEndian.PutUint32(b[1:], uint32(n))
		return b
	}
}

// yugenTx builds a v4 transaction under the Yugen version group with one transparent
// output and the Yugen field (transfer, proof_a, proof_b).
func yugenTx(transfer, pa, pb int) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(0x80000004))
	binary.Write(&b, binary.LittleEndian, uint32(YugenVersionGroupID))
	b.Write(cs(0))                                  // vin
	b.Write(cs(1))                                  // vout
	binary.Write(&b, binary.LittleEndian, int64(5)) // value
	b.Write(cs(1))
	b.WriteByte(0x51)                                 // script OP_1
	binary.Write(&b, binary.LittleEndian, uint32(0))  // nLockTime
	binary.Write(&b, binary.LittleEndian, uint32(10)) // nExpiryHeight
	binary.Write(&b, binary.LittleEndian, int64(0))   // valueBalance
	b.Write(cs(0))                                    // spends
	b.Write(cs(0))                                    // outputs
	b.Write(cs(0))                                    // joinsplits
	for _, n := range []int{transfer, pa, pb} {
		b.Write(cs(n))
		b.Write(bytes.Repeat([]byte{0xab}, n))
	}
	return b.Bytes()
}

func TestYugenTxParse(t *testing.T) {
	for _, c := range [][3]int{{300, 70000, 760000}, {5000, 0, 0}} {
		raw := yugenTx(c[0], c[1], c[2])
		trailer := []byte{1, 2, 3}
		tx := NewTransaction()
		rest, err := tx.ParseFromSlice(append(append([]byte{}, raw...), trailer...))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !bytes.Equal(rest, trailer) {
			t.Fatalf("rest %d bytes, want the 3-byte trailer", len(rest))
		}
		if !tx.IsYugen() || tx.HasSaplingElements() {
			t.Fatal("want Yugen, no Sapling elements")
		}
		if !bytes.Equal(tx.Bytes(), raw) {
			t.Fatal("raw bytes differ")
		}
	}
	// Truncated Yugen field is an error, not a silent skip.
	raw := yugenTx(10, 0, 0)
	if _, err := NewTransaction().ParseFromSlice(raw[:len(raw)-3]); err == nil {
		t.Fatal("truncated Yugen field parsed")
	}
}

func TestYugenBlockSidecar(t *testing.T) {
	hdr := make([]byte, 4+32+32+32+4+4+32+3+1344) // version..nonce, compactsize(1344) solution
	hdr[0] = 4
	copy(hdr[140:], []byte{0xfd, 0x40, 0x05})
	tx := yugenTx(40, 0, 0)
	var b bytes.Buffer
	b.Write(hdr)
	b.Write(cs(1))
	b.Write(tx)
	b.Write(cs(1))                       // one sidecar entry
	b.Write(bytes.Repeat([]byte{7}, 32)) // txid
	b.Write(cs(2))
	b.Write(cs(300))
	b.Write(bytes.Repeat([]byte{1}, 300))
	b.Write(cs(0))
	blk := NewBlock()
	rest, err := blk.ParseFromSlice(b.Bytes())
	if err != nil || len(rest) != 0 || blk.yugenSidecar != 1 {
		t.Fatalf("err %v rest %d sidecar %d", err, len(rest), blk.yugenSidecar)
	}
	if !blk.hasYugen() {
		t.Fatal("no Yugen tx seen")
	}
	// A sidecar on a block without a Yugen transaction stays "overlong" (refused by the caller).
}
