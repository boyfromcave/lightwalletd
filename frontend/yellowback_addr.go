// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

// YED addresses for GetAddressTxids (plan D-L-4). A YED address is a transparent P2PKH address
// with different base58check version bytes and the same 20-byte key hash
// (ycash-dd/src/yellowback/address.h:14-21; params.cpp:142,156,189). The node's getaddresstxids
// knows only the transparent form, so the server maps yb…/yt…/yr… to s1…/sm… before the
// baseline's regex, which stays the last check. Version bytes are the node's and Ycash's
// (ref/ycash/src/chainparams.cpp:149,409,613, base58Prefixes[PUBKEY_ADDRESS]).
//
// rpcversion 7 (the post-quantum line, docs/plans/yellowback-quantum-spec.md section 4): a YED
// address may also be post-quantum, Base58Check(pqVersion || scheme || keyHash), 53 characters.
// It has no transparent twin (a TX_PQPKH output is not a P2PKH script, and the node's address
// index does not cover it, quantum spec A-11), so the taddr RPCs refuse it with a pointer to
// GetAddressTokens, which takes either form. Owner decision D-Q-21: the mainnet prefix is yb…
// (P2PKH 0x1FDD, PQ 0x56AB); testnet yt… and regtest yr… are unchanged.
package frontend

import (
	"bytes"
	"crypto/sha256"

	"github.com/btcsuite/btcutil/base58"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errPQYedAddress: a post-quantum YED address (rpcversion 7) has no transparent twin and is not
// address-indexed (taddrOf, service.go); GetAddressTokens takes it.
var errPQYedAddress = status.Error(codes.InvalidArgument, "PQ YED address: use GetAddressTokens (post-quantum outputs are not address-indexed)")

// yedVersionMap: YED PUBKEY_ADDRESS version bytes -> transparent PUBKEY_ADDRESS version bytes.
var yedVersionMap = map[[2]byte][2]byte{
	{0x1F, 0xDD}: {0x1C, 0x28}, // mainnet  yb… -> s1… (D-Q-21; ye… 0x1FE4 before it)
	{0x20, 0x07}: {0x1C, 0x95}, // testnet  yt… -> sm…
	{0x20, 0x02}: {0x1C, 0x95}, // regtest  yr… -> sm…
}

// yedPQVersions: the post-quantum YED address version bytes (quantum spec section 4, D-Q-21).
var yedPQVersions = map[[2]byte]bool{
	{0x56, 0xAB}: true, // mainnet  yb… (53)
	{0x57, 0x1E}: true, // testnet  yt… (53)
	{0x57, 0x10}: true, // regtest  yr… (53)
}

// yedAddressLen is the base58check length of every YED P2PKH address (see yedToTransparent);
// yedPQAddressLen that of every post-quantum one (see isYedPQAddress).
const (
	yedAddressLen   = 35
	yedPQAddressLen = 53
)

func checksum4(b []byte) []byte {
	h := sha256.Sum256(b)
	h = sha256.Sum256(h[:])
	return h[:4]
}

// yedToTransparent returns the transparent form of a YED address and true, or ("", false) when
// the input is not a well-formed YED P2PKH address (wrong length, bad checksum, unknown version).
// A transparent address is returned unchanged with false, so callers apply their own check.
//
// The length and prefix are checked BEFORE decoding (audit E-1): base58.Decode is a big-integer
// loop, quadratic in the input length, and the taddr RPCs hand this function a client string
// before the baseline's 34-character regex. A 26-byte payload under any of the three YED
// version bytes is always exactly 35 characters starting with 'y' (0x1FE4..0x2002 << 192 lie
// between 58^34 and 58^35), so anything else is not a YED address and costs O(1) here.
func yedToTransparent(addr string) (string, bool) {
	if len(addr) != yedAddressLen || addr[0] != 'y' {
		return "", false
	}
	raw := base58.Decode(addr)
	if len(raw) != 2+20+4 { // version(2) + hash160(20) + checksum(4)
		return "", false
	}
	if !bytes.Equal(raw[22:], checksum4(raw[:22])) {
		return "", false
	}
	version := [2]byte{raw[0], raw[1]}
	tversion, ok := yedVersionMap[version]
	if !ok {
		return "", false
	}
	payload := make([]byte, 0, 26)
	payload = append(payload, tversion[0], tversion[1])
	payload = append(payload, raw[2:22]...)
	payload = append(payload, checksum4(payload)...)
	return base58.Encode(payload), true
}

// isYedPQAddress reports whether addr is a well-formed post-quantum YED address of any network:
// 53 characters starting with 'y' (checked first, O(1), as in yedToTransparent: every 37-byte
// payload under the three PQ versions encodes to exactly 53 characters), a valid checksum, a PQ
// version and a registered scheme byte (1 SLH-DSA-SHA2-128s, 2 FN-DSA-512).
func isYedPQAddress(addr string) bool {
	if len(addr) != yedPQAddressLen || addr[0] != 'y' {
		return false
	}
	raw := base58.Decode(addr)
	if len(raw) != 2+1+32+4 { // version(2) + scheme(1) + keyHash(32) + checksum(4)
		return false
	}
	if !bytes.Equal(raw[35:], checksum4(raw[:35])) {
		return false
	}
	return yedPQVersions[[2]byte{raw[0], raw[1]}] && (raw[2] == 1 || raw[2] == 2)
}
