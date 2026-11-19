// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

// The light attestor's calls (beta4 item 3d, B-T1; workspace docs/plans/beta4/price-feed.md
// section 2.2): SubmitAttestation, GetTag, GetTxOut and GetMempoolTxids. A light seat runs no
// node: it signs its price statements itself and hands them to the node behind this server, and
// its cancel watcher reads outputs and the mempool through here.
//
// SubmitAttestation is the one call of the YellowbackStreamer service that changes node state:
// it adds a statement to the node's in-memory attestation pool (yed_addattestation), the pool
// BuildBundle reads. It never creates a transaction, touches a wallet or changes a rule; the node
// verifies the statement (signature under the cited block's hash, a member's key, the freshness
// window, the price range) exactly as for a statement that arrives over gossip. The server's
// guards only keep the node's verification work bounded:
//
//	size  exactly 74 bytes (seq u16, priceMicroUsd u32, citedHeight u32, signature 64, all LE;
//	      ycash6 src/yellowback/attest.cpp DecodeAttestation)                    INVALID_ARGUMENT
//	seq   a seq listed by yed_listattestors (cached; refreshed when the tip moves or after
//	      attestMembersTTL), the subscribe agent's test (contrib/yellowback/attest/src/subscribe.rs
//	      refresh_attestors): the node itself refuses a listed seq that is not eligible  PERMISSION_DENIED
//	rate  at most one new statement per seq per attestSeqBlocks blocks of citedHeight
//	      (--yellowback-attest-seq-blocks, default 5); a byte-identical resubmit of the last
//	      accepted statement is answered from memory and does not count            RESOURCE_EXHAUSTED
//	peer  the per-IP token bucket every method takes from (yellowback_ratelimit.go) RESOURCE_EXHAUSTED
//
// The window is measured in citedHeight, not in the tip at arrival, and only an accepted statement
// moves it: a statement is signed by its seat, so a third party can only replay genuine ones, and
// neither a refused frame nor an older statement replayed by someone else can use up the seat's
// window (an older one cites a lower height and is refused by the window before the node sees it).
package frontend

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/zcash/lightwalletd/common"
	"github.com/zcash/lightwalletd/walletrpc"
)

const (
	// attestationSize is ATTESTATION_SIZE (ycash6 src/yellowback/attest.h; kogany the same).
	attestationSize = 74
	// DefaultAttestSeqBlocks is --yellowback-attest-seq-blocks' default: a seat signs every
	// ATTEST_INTERVAL blocks (10 on mainnet), so 5 leaves room for a re-sign after a short reorg.
	DefaultAttestSeqBlocks = 5
	// attestMembersTTL bounds the member cache's age when the tip does not move.
	attestMembersTTL = 60 * time.Second
)

// attestSeqState is what the server remembers per seq: the last statement the node accepted
// through this server and the node's answer to it.
type attestSeqState struct {
	frame    []byte
	cited    int64
	result   *walletrpc.YedAttestationAccepted
	inFlight bool
}

// attestGuard holds the member cache and the per-seq windows.
type attestGuard struct {
	mu         sync.Mutex
	seqBlocks  int64
	members    map[uint16]bool
	membersTip []byte
	membersAt  time.Time
	seqs       map[uint16]*attestSeqState
}

func newAttestGuard() *attestGuard {
	return &attestGuard{seqBlocks: DefaultAttestSeqBlocks, seqs: map[uint16]*attestSeqState{}}
}

// SetAttestSeqBlocks sets the per-seq window (cmd/root.go --yellowback-attest-seq-blocks); n < 1
// is taken as 1 (one new statement per seq per cited block).
func (y *YellowbackStreamer) SetAttestSeqBlocks(n int) {
	if n < 1 {
		n = 1
	}
	y.attest.mu.Lock()
	y.attest.seqBlocks = int64(n)
	y.attest.mu.Unlock()
}

// isMember answers from the cached yed_listattestors, refreshing it first when the ingestor's
// tip moved or the cache is older than attestMembersTTL. A failed refresh keeps the old list
// (the subscribe agent's rule); with no list at all the call fails UNAVAILABLE.
func (y *YellowbackStreamer) isMember(ctx context.Context, seq uint16) (bool, error) {
	tip := common.YedTipHash()
	y.attest.mu.Lock()
	fresh := y.attest.members != nil && y.now().Sub(y.attest.membersAt) < attestMembersTTL &&
		(tip == nil || bytes.Equal(tip, y.attest.membersTip))
	if fresh {
		ok := y.attest.members[seq]
		y.attest.mu.Unlock()
		return ok, nil
	}
	y.attest.mu.Unlock()
	var rows []struct {
		Seq int64 `json:"seq"`
	}
	err := y.call(ctx, &rows, "yed_listattestors")
	y.attest.mu.Lock()
	defer y.attest.mu.Unlock()
	if err != nil {
		if y.attest.members == nil {
			return false, status.Errorf(codes.Unavailable, "seq: the member list is not available: %v", err)
		}
		return y.attest.members[seq], nil
	}
	members := make(map[uint16]bool, len(rows))
	for _, r := range rows {
		if r.Seq >= 0 && r.Seq <= math.MaxUint16 {
			members[uint16(r.Seq)] = true
		}
	}
	y.attest.members, y.attest.membersTip, y.attest.membersAt = members, tip, y.now()
	return members[seq], nil
}

// SubmitAttestation hands one 74-byte statement to the node's pool through yed_addattestation,
// behind the guards described at the top of this file. The node's refusals (attest-stale,
// attest-invalid, attest-not-eligible, ...) come back as FAILED_PRECONDITION with its message.
func (y *YellowbackStreamer) SubmitAttestation(ctx context.Context, in *walletrpc.YedAttestationFrame) (*walletrpc.YedAttestationAccepted, error) {
	if err := y.limited(ctx, yedCostHeavy); err != nil {
		return nil, err
	}
	if in == nil || len(in.Frame) != attestationSize {
		n := 0
		if in != nil {
			n = len(in.Frame)
		}
		return nil, badArg("size: an attestation is exactly %d bytes, got %d", attestationSize, n)
	}
	frame := append([]byte(nil), in.Frame...)
	seq := binary.LittleEndian.Uint16(frame[0:2])
	cited := int64(binary.LittleEndian.Uint32(frame[6:10]))
	member, err := y.isMember(ctx, seq)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, status.Errorf(codes.PermissionDenied, "seq: %d is not a member of the attestor set (yed_listattestors)", seq)
	}

	y.attest.mu.Lock()
	st := y.attest.seqs[seq]
	if st == nil {
		st = &attestSeqState{}
		y.attest.seqs[seq] = st
	}
	if st.result != nil && bytes.Equal(st.frame, frame) {
		cached := proto.Clone(st.result).(*walletrpc.YedAttestationAccepted)
		y.attest.mu.Unlock()
		return cached, nil
	}
	if st.inFlight {
		y.attest.mu.Unlock()
		return nil, status.Errorf(codes.ResourceExhausted, "rate: a statement of seq %d is being submitted", seq)
	}
	if st.result != nil && cited < st.cited+y.attest.seqBlocks {
		next := st.cited + y.attest.seqBlocks
		y.attest.mu.Unlock()
		return nil, status.Errorf(codes.ResourceExhausted, "rate: seq %d already has a statement citing %d here; the next may cite %d or later",
			seq, st.cited, next)
	}
	st.inFlight = true
	y.attest.mu.Unlock()

	out := &walletrpc.YedAttestationAccepted{}
	err = y.call(ctx, out, "yed_addattestation", common.JSONString(hex.EncodeToString(frame)))

	y.attest.mu.Lock()
	st.inFlight = false
	if err == nil && out.Accepted {
		st.frame, st.cited = frame, cited
		st.result = proto.Clone(out).(*walletrpc.YedAttestationAccepted)
	}
	y.attest.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// GetAttestations' shared answer (callCached) is stale now.
	y.cacheMu.Lock()
	delete(y.cache, "yed_getattestations")
	y.cacheMu.Unlock()
	return out, nil
}

// tipHeight is the node's best height, from the shared getblockchaininfo answer.
func (y *YellowbackStreamer) tipHeight(ctx context.Context) (uint64, error) {
	var reply chainInfoReply
	if err := y.callCached(ctx, &reply, "getblockchaininfo"); err != nil {
		return 0, err
	}
	return reply.Blocks, nil
}

// GetTag proxies yed_gettag <height> (the RPC takes the height as a string: "height|blockhash");
// height 0 is the node's tip. found = false (kind "none") is an answer, not an error.
func (y *YellowbackStreamer) GetTag(ctx context.Context, in *walletrpc.HeightFilter) (*walletrpc.YedTag, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	height := uint64(0)
	if in != nil {
		height = uint64(in.Height)
	}
	if height == 0 {
		tip, err := y.tipHeight(ctx)
		if err != nil {
			return nil, err
		}
		height = tip
	}
	out := &walletrpc.YedTag{}
	if err := y.call(ctx, out, "yed_gettag", common.JSONString(strconv.FormatUint(height, 10))); err != nil {
		return nil, err
	}
	out.Height = int64(height)
	return out, nil
}

// txOutReply is gettxout's answer: value in coins (a JSON number, ycashd prints 8 decimals,
// kogany a float), scriptPubKey an object.
type txOutReply struct {
	Bestblock     string      `json:"bestblock"`
	Confirmations int64       `json:"confirmations"`
	Value         json.Number `json:"value"`
	ScriptPubKey  struct {
		Hex string `json:"hex"`
	} `json:"scriptPubKey"`
	Coinbase bool `json:"coinbase"`
}

// coinsToZat converts gettxout's value (coins) to zatoshi. Every amount below 21e6 coins is
// exact in a float64 to well under half a zatoshi, so rounding the product is exact.
func coinsToZat(v json.Number) (int64, error) {
	f, err := v.Float64()
	if err != nil || f < 0 || f > 21e6 {
		return 0, status.Errorf(codes.Internal, "gettxout: value %q is not an amount", v.String())
	}
	return int64(math.Round(f * 1e8)), nil
}

// GetTxOut proxies gettxout <txid> <vout> <includeMempool>. A spent or unknown output (the RPC's
// null) is found = false.
func (y *YellowbackStreamer) GetTxOut(ctx context.Context, in *walletrpc.YedTxOutQuery) (*walletrpc.YedTxOut, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	if in == nil || checkTxid(in.Txid) != nil {
		return nil, badArg("txid must be 64 hex characters")
	}
	raw, err := common.CallYed(ctx, "gettxout", common.JSONString(in.Txid), common.JSONNumber(int64(in.Vout)),
		json.RawMessage(strconv.FormatBool(in.IncludeMempool)))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return &walletrpc.YedTxOut{Found: false}, nil
	}
	var reply txOutReply
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&reply); err != nil {
		y.log.WithField("method", "gettxout").Error("Yellowback: node result did not match the contract")
		return nil, status.Errorf(codes.Internal, "gettxout: node result did not match the contract")
	}
	zat, err := coinsToZat(reply.Value)
	if err != nil {
		return nil, err
	}
	return &walletrpc.YedTxOut{Found: true, Bestblock: reply.Bestblock, Confirmations: reply.Confirmations,
		ValueZat: zat, ScriptPubKeyHex: reply.ScriptPubKey.Hex, Coinbase: reply.Coinbase}, nil
}

// GetMempoolTxids proxies getrawmempool (txids, display order). It reaches the node every time
// (the mempool is not a function of the tip; common.YedCacheable leaves it out).
func (y *YellowbackStreamer) GetMempoolTxids(ctx context.Context, _ *walletrpc.Empty) (*walletrpc.YedTxidList, error) {
	if err := y.limited(ctx, yedCostHeavy); err != nil {
		return nil, err
	}
	var txids []string
	if err := y.call(ctx, &txids, "getrawmempool"); err != nil {
		return nil, err
	}
	for _, id := range txids {
		if !txidPattern.MatchString(id) {
			return nil, status.Errorf(codes.Internal, "getrawmempool: node result did not match the contract")
		}
	}
	return &walletrpc.YedTxidList{Txids: txids}, nil
}
