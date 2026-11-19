// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

package parser

import (
	"github.com/pkg/errors"
	"github.com/zcash/lightwalletd/parser/internal/bytestring"
)

// IsCoinbase reports whether the serialized transaction is a coinbase: exactly one transparent
// input whose prevout is null (all-zero hash, index 0xffffffff), as CTransaction::IsCoinBase.
// Only the prefix up to the first input is read, so it works for every transaction version
// (v1..v4, and v5's header order) without parsing the shielded parts.
func IsCoinbase(data []byte) (bool, error) {
	s := bytestring.String(data)
	var header uint32
	if !s.ReadUint32(&header) {
		return false, errors.New("could not read header")
	}
	overwintered := header>>31 == 1
	version := header & 0x7FFFFFFF
	if overwintered && version >= 3 {
		var groupID uint32
		if !s.ReadUint32(&groupID) {
			return false, errors.New("could not read nVersionGroupId")
		}
		if version >= 5 {
			// nConsensusBranchId, nLockTime, nExpiryHeight precede the transparent bundle.
			if !s.Skip(12) {
				return false, errors.New("could not read the v5 header")
			}
		}
	}
	var inCount int
	if !s.ReadCompactSize(&inCount) {
		return false, errors.New("could not read tx_in_count")
	}
	if inCount != 1 {
		return false, nil
	}
	var prevHash []byte
	var prevIndex uint32
	if !s.ReadBytes(&prevHash, 32) || !s.ReadUint32(&prevIndex) {
		return false, errors.New("could not read the first input's prevout")
	}
	if prevIndex != 0xffffffff {
		return false, nil
	}
	for _, b := range prevHash {
		if b != 0 {
			return false, nil
		}
	}
	return true, nil
}
