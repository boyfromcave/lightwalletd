// YED addresses for GetAddressTxids (plan D-L-4). A YED address is a transparent P2PKH address
// with different base58check version bytes and the same 20-byte key hash
// (ycash-dd/src/yellowback/address.h:14-21; params.cpp:142,156,189). The node's getaddresstxids
// knows only the transparent form, so the server maps ye…/yt…/yr… to s1…/sm… before the
// baseline's regex, which stays the last check. Version bytes are the node's and Ycash's
// (ref/ycash/src/chainparams.cpp:149,409,613, base58Prefixes[PUBKEY_ADDRESS]).
package frontend

import (
	"bytes"
	"crypto/sha256"

	"github.com/btcsuite/btcutil/base58"
)

// yedVersionMap: YED PUBKEY_ADDRESS version bytes -> transparent PUBKEY_ADDRESS version bytes.
var yedVersionMap = map[[2]byte][2]byte{
	{0x1F, 0xE4}: {0x1C, 0x28}, // mainnet  ye… -> s1…
	{0x20, 0x07}: {0x1C, 0x95}, // testnet  yt… -> sm…
	{0x20, 0x02}: {0x1C, 0x95}, // regtest  yr… -> sm…
}

func checksum4(b []byte) []byte {
	h := sha256.Sum256(b)
	h = sha256.Sum256(h[:])
	return h[:4]
}

// yedToTransparent returns the transparent form of a YED address and true, or ("", false) when
// the input is not a well-formed YED P2PKH address (wrong length, bad checksum, unknown version).
// A transparent address is returned unchanged with false, so callers apply their own check.
func yedToTransparent(addr string) (string, bool) {
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
