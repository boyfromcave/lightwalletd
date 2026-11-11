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
// address may also be post-quantum, naming a TX_PQPKH output `0x20 <keyHash:32> OP_1|OP_2
// OP_CHECKPQSIG`; the node's address index covers TX_PQPKH outputs (src/rpc/misc.cpp getIndexKey:
// CScript::P2PQPKH, keyed by Hash160(scheme || keyHash)), so the taddr RPCs take a plain PQ address
// as is and map a PQ YED address to the plain one of the same script, the same mapping ycash-infra's
// yindex makes (yindex/internal/addr). The node names a PQ address in its answers as the request
// named it, so a client sees the plain form back, as it sees s…/sm… for a YED P2PKH address.
// rpcversion 9 (decision D-9, quantum spec section 4.1): the address is Base58Check(version(3) ||
// keyHash(32)), 53 characters, one 3-byte version per (network, coin, scheme), so the prefix shows
// the scheme: plain YEC sqf…/sqs… (main), tqf…/tqs… (test), rqf…/rqs… (regtest); YED ybf…/ybs…,
// ytf…/yts…, yrf…/yrs… (f FN-DSA-512, s SLH-DSA-SHA2-128s). The pre-D-9 form (2-byte version ||
// scheme byte) is neither produced nor accepted. The ECC YED address is unchanged (D-Q-21: mainnet
// yb…, P2PKH 0x1FDD; testnet yt… and regtest yr…).
package frontend

import (
	"bytes"
	"crypto/sha256"

	"github.com/btcsuite/btcutil/base58"
)

// yedVersionMap: YED PUBKEY_ADDRESS version bytes -> transparent PUBKEY_ADDRESS version bytes.
var yedVersionMap = map[[2]byte][2]byte{
	{0x1F, 0xDD}: {0x1C, 0x28}, // mainnet  yb… -> s1… (D-Q-21; ye… 0x1FE4 before it)
	{0x20, 0x07}: {0x1C, 0x95}, // testnet  yt… -> sm…
	{0x20, 0x02}: {0x1C, 0x95}, // regtest  yr… -> sm…
}

// pqVersion is what one post-quantum address version names (quantum spec section 4.1, D-9): the
// network, whether it is the YED holder form or the plain-YEC form, and the signature scheme
// (1 SLH-DSA-SHA2-128s, 2 FN-DSA-512). Since D-9 the scheme is part of the 3-byte version, so the
// prefix shows it: Base58Check(version(3) || keyHash(32)), 53 characters.
type pqVersion struct {
	network string // "main", "test", "regtest"
	yed     bool   // the YED holder form (yb…/yt…/yr…) rather than plain YEC (sq…/tq…/rq…)
	scheme  byte
}

// pqVersions: every post-quantum address version, the one table both directions read (the node's
// key_io PlainPQAddressVersions and yellowback Params::pqAddressVersions, ycash6/ycash-dd
// up/b2-pq-prefix). Any other version, the pre-D-9 2-byte forms 4DD9/4F61/4C51 and
// 56AB/571E/5710 among them, is not a post-quantum address (clean cut, no old-form acceptance).
var pqVersions = map[[3]byte]pqVersion{
	{0x4D, 0xDD, 0x04}: {"main", false, 2},    // sqf… FN-DSA-512
	{0x4D, 0xDE, 0x6A}: {"main", false, 1},    // sqs… SLH-DSA-SHA2-128s
	{0x4F, 0x65, 0x19}: {"test", false, 2},    // tqf…
	{0x4F, 0x66, 0x7F}: {"test", false, 1},    // tqs…
	{0x4C, 0x54, 0xEF}: {"regtest", false, 2}, // rqf…
	{0x4C, 0x56, 0x55}: {"regtest", false, 1}, // rqs…
	{0x56, 0xAE, 0xDE}: {"main", true, 2},     // ybf…
	{0x56, 0xB0, 0x44}: {"main", true, 1},     // ybs…
	{0x57, 0x21, 0xC9}: {"test", true, 2},     // ytf…
	{0x57, 0x23, 0x30}: {"test", true, 1},     // yts…
	{0x57, 0x14, 0x44}: {"regtest", true, 2},  // yrf…
	{0x57, 0x15, 0xAA}: {"regtest", true, 1},  // yrs…
}

// pqVersionOf is pqVersions read the other way: the 3-byte version of a (network, form, scheme).
var pqVersionOf = func() map[pqVersion][3]byte {
	m := make(map[pqVersion][3]byte, len(pqVersions))
	for v, k := range pqVersions {
		m[k] = v
	}
	return m
}()

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

// decodePQ returns what a well-formed 53-character post-quantum address whose first character is
// first names (its version's entry and the 32-byte key hash), and ok: the length and first
// character are checked before decoding (audit E-1, as in yedToTransparent: every 35-byte payload
// under the twelve PQ versions encodes to exactly 53 characters), then the checksum and the
// version, which must be one of pqVersions (it carries the scheme, D-9).
func decodePQ(addr string, first byte) (pqVersion, []byte, bool) {
	if len(addr) != yedPQAddressLen || addr[0] != first {
		return pqVersion{}, nil, false
	}
	raw := base58.Decode(addr)
	if len(raw) != 3+32+4 { // version(3) + keyHash(32) + checksum(4)
		return pqVersion{}, nil, false
	}
	if !bytes.Equal(raw[35:], checksum4(raw[:35])) {
		return pqVersion{}, nil, false
	}
	k, ok := pqVersions[[3]byte{raw[0], raw[1], raw[2]}]
	if !ok {
		return pqVersion{}, nil, false
	}
	return k, raw[3:35], true
}

// encodePQ is decodePQ's inverse: Base58Check(version(k) || keyHash).
func encodePQ(k pqVersion, keyHash []byte) (string, bool) {
	v, ok := pqVersionOf[k]
	if !ok || len(keyHash) != 32 {
		return "", false
	}
	payload := make([]byte, 0, 39)
	payload = append(payload, v[:]...)
	payload = append(payload, keyHash...)
	payload = append(payload, checksum4(payload)...)
	return base58.Encode(payload), true
}

// isYedPQAddress reports whether addr is a well-formed post-quantum YED address of any network.
func isYedPQAddress(addr string) bool {
	_, ok := yedPQToPlain(addr)
	return ok
}

// yedPQToPlain returns the plain-YEC post-quantum address (sqf…/sqs…, tqf…/tqs…, rqf…/rqs…) of the
// TX_PQPKH script a post-quantum YED address (ybf…/ybs…, ytf…/yts…, yrf…/yrs…, 53 characters)
// names: the same network, scheme and key hash. Else ("", false).
func yedPQToPlain(addr string) (string, bool) {
	k, keyHash, ok := decodePQ(addr, 'y')
	if !ok || !k.yed {
		return "", false
	}
	k.yed = false
	return encodePQ(k, keyHash)
}

// isPlainPQAddress reports whether addr is a well-formed plain-YEC post-quantum address
// (sqf…/sqs…, tqf…/tqs…, rqf…/rqs…, 53 characters, D-Q-20, D-9) of any network.
func isPlainPQAddress(addr string) bool {
	if len(addr) != yedPQAddressLen {
		return false
	}
	switch addr[0] {
	case 's', 't', 'r':
	default:
		return false
	}
	k, _, ok := decodePQ(addr, addr[0])
	return ok && !k.yed
}
