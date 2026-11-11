// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

package frontend

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/ioutil"
	"testing"
)

// pqVectorsSHA256 is the sha256 of testdata/yellowback/pq_address_vectors.json, a byte-identical
// copy of the nodes' shared post-quantum address vectors (ycash6 / ycash-dd
// src/test/data/pq_address_vectors.json on up/b2-pq-prefix, decision D-9). A change to the copy
// fails here: take the new file from the node line and update the hash with it.
const pqVectorsSHA256 = "dbbd1da05d995d659e1e9f8a671197e141c272998722d6cca85cdbdb52b61797"

type pqVectorFile struct {
	Format string `json:"format"`
	Valid  []struct {
		Network string `json:"network"`
		Coin    string `json:"coin"`
		Scheme  byte   `json:"scheme"`
		Version string `json:"version"`
		KeyHash string `json:"keyhash"`
		Address string `json:"address"`
	} `json:"valid"`
	Invalid []struct {
		Network string `json:"network"`
		Coin    string `json:"coin"`
		Reason  string `json:"reason"`
		Address string `json:"address"`
	} `json:"invalid"`
}

// TestPQAddressSharedVectors: every valid vector decodes to its own network, coin, scheme and key
// hash, re-encodes to itself, is recognised only as its own coin's form, and a YED one maps to the
// plain address of the same network, scheme and key hash; every invalid vector (the pre-D-9 form,
// truncated, extended, bad checksum, unknown version) is refused by both recognisers and by
// taddrOf/checkTaddress.
func TestPQAddressSharedVectors(t *testing.T) {
	data, err := ioutil.ReadFile("../testdata/yellowback/pq_address_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != pqVectorsSHA256 {
		t.Fatalf("pq_address_vectors.json sha256 %x, want %s: the copy drifted from the node lines' file", sum, pqVectorsSHA256)
	}
	var f pqVectorFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.Format != "ycash-pq-address-vectors-1" || len(f.Valid) != 72 || len(f.Invalid) != 60 {
		t.Fatalf("format %q, %d valid, %d invalid; want ycash-pq-address-vectors-1, 72, 60", f.Format, len(f.Valid), len(f.Invalid))
	}
	YellowbackAddresses = true
	defer func() { YellowbackAddresses = false }()

	plainOf := map[string]string{} // network/scheme/keyhash -> the plain address
	for _, v := range f.Valid {
		if v.Coin == "yec" {
			plainOf[v.Network+string(rune('0'+v.Scheme))+v.KeyHash] = v.Address
		}
	}
	for _, v := range f.Valid {
		yed := v.Coin == "yed"
		k, keyHash, ok := decodePQ(v.Address, v.Address[0])
		if !ok || k.network != v.Network || k.yed != yed || k.scheme != v.Scheme || hex.EncodeToString(keyHash) != v.KeyHash {
			t.Errorf("%s: decoded %+v %x %v, want %s %s scheme %d %s", v.Address, k, keyHash, ok, v.Network, v.Coin, v.Scheme, v.KeyHash)
			continue
		}
		if ver := pqVersionOf[k]; hex.EncodeToString(ver[:]) != v.Version {
			t.Errorf("%s: version %x, want %s", v.Address, ver, v.Version)
		}
		if again, ok := encodePQ(k, keyHash); !ok || again != v.Address {
			t.Errorf("%s: re-encodes to %q", v.Address, again)
		}
		if isYedPQAddress(v.Address) != yed || isPlainPQAddress(v.Address) == yed {
			t.Errorf("%s (%s): recognised as the wrong coin's form", v.Address, v.Coin)
		}
		if yed {
			want := plainOf[v.Network+string(rune('0'+v.Scheme))+v.KeyHash]
			if got, ok := yedPQToPlain(v.Address); !ok || got != want {
				t.Errorf("yedPQToPlain(%s) = %q %v, want %s", v.Address, got, ok, want)
			}
			if err := checkTaddress(v.Address); err == nil {
				t.Errorf("checkTaddress(%s): an unmapped YED PQ address must not reach the node", v.Address)
			}
		} else if err := checkTaddress(v.Address); err != nil {
			t.Errorf("checkTaddress(%s): %v", v.Address, err)
		}
	}
	for _, v := range f.Invalid {
		if isYedPQAddress(v.Address) || isPlainPQAddress(v.Address) {
			t.Errorf("%s (%s %s, %s) accepted", v.Address, v.Network, v.Coin, v.Reason)
		}
		if mapped, err := taddrOf(v.Address); err == nil && checkTaddress(mapped) == nil {
			t.Errorf("%s (%s): passed checkTaddress as %s", v.Address, v.Reason, mapped)
		}
	}
}
