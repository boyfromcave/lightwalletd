// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

// YellowbackStreamer: the Yellowback (YED) light-client service, every method a proxy of one
// read-only yed_* node RPC through common.CallYed (docs/plans/yellowback-lightwalletd-plan.md
// section 4.1). Each handler validates its input, calls, unmarshals the JSON result straight into
// the generated message (field names are the contract's JSON names), and returns. No Yellowback
// logic lives here.
package frontend

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zcash/lightwalletd/common"
	"github.com/zcash/lightwalletd/walletrpc"
)

// Input bounds checked at the edge so the node never sees a malformed request from here
// (plan Phase L3 item; the cheap ones land now).
const (
	maxHexLen        = 2 * 200000 // a raw transaction is at most 100 kB on Ycash's MAX_TX_SIZE ... generous
	maxSelectorLen   = 2 * 36     // a serialised COutPoint
	maxListCount     = 1000
	defaultListCount = 100 // yed_listvaults' own default (audit E-5)
	maxAddresses     = 100 // yed_listtokens' own bound
	maxAddressLen    = 64
)

var txidPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
var hexPattern = regexp.MustCompile(`^([0-9a-fA-F]{2})*$`)

// vaultStatuses is yed_listvaults' status filter enum (the contract; yellowback.cpp "status
// must be ACTIVE, VOID, CLOSED or CLAIMED"), checked at the edge (audit E-5).
var vaultStatuses = map[string]bool{"": true, "ACTIVE": true, "VOID": true, "CLOSED": true, "CLAIMED": true}

// yedCacheTTL: the per-tip, parameterless answers (GetYellowbackInfo, GetPrice at the tip,
// GetStats, GetActivation, ListClaimable, GetAttestations) are identical bytes for every client
// between blocks, so one node call serves them all for this long (audit E-3). A time bound, not
// a height key: keying on the node's height would cost the very yed_getinfo call being saved.
const yedCacheTTL = 2 * time.Second

type cacheEntry struct {
	raw json.RawMessage
	at  time.Time
}

// YellowbackStreamer implements walletrpc.YellowbackStreamerServer.
type YellowbackStreamer struct {
	capability common.YellowbackCapability
	log        *logrus.Entry
	limiter    *rateLimiter // per-peer bucket, every method (yellowback_ratelimit.go)
	cacheMu    sync.Mutex
	cache      map[string]cacheEntry // method -> last tip answer (callCached)
	now        func() time.Time
	walletrpc.UnimplementedYellowbackStreamerServer
}

// NewYellowbackStreamer builds the service; the node is reached through common.RawRequest, as
// every other handler in this tree does.
func NewYellowbackStreamer(capability common.YellowbackCapability, log *logrus.Entry) *YellowbackStreamer {
	return &YellowbackStreamer{capability: capability, log: log, limiter: newRateLimiter(yedRateBurst, yedRateInterval),
		cache: map[string]cacheEntry{}, now: time.Now}
}

// call forwards and unmarshals into out (a pointer to a generated message or a slice of them).
func (y *YellowbackStreamer) call(ctx context.Context, out interface{}, method string, params ...json.RawMessage) error {
	raw, err := common.CallYed(ctx, method, params...)
	if err != nil {
		return err
	}
	return y.decode(raw, out, method)
}

// callCached is call for a parameterless method whose answer is shared by every client for
// yedCacheTTL.
func (y *YellowbackStreamer) callCached(ctx context.Context, out interface{}, method string) error {
	y.cacheMu.Lock()
	e, ok := y.cache[method]
	y.cacheMu.Unlock()
	if ok && y.now().Sub(e.at) < yedCacheTTL {
		return y.decode(e.raw, out, method)
	}
	raw, err := common.CallYed(ctx, method)
	if err != nil {
		return err
	}
	y.cacheMu.Lock()
	y.cache[method] = cacheEntry{raw: raw, at: y.now()}
	y.cacheMu.Unlock()
	return y.decode(raw, out, method)
}

func (y *YellowbackStreamer) decode(raw json.RawMessage, out interface{}, method string) error {
	if err := json.Unmarshal(raw, out); err != nil {
		y.log.WithFields(logrus.Fields{"method": method, "error": err}).Error("Yellowback: node result did not match the contract")
		return status.Errorf(codes.Internal, "%s: node result did not match the contract", method)
	}
	return nil
}

func badArg(format string, args ...interface{}) error {
	return status.Errorf(codes.InvalidArgument, format, args...)
}

func checkTxid(txid string) error {
	if !txidPattern.MatchString(txid) {
		return badArg("txid must be 64 hex characters")
	}
	return nil
}

func checkHex(field, value string, max int) error {
	if len(value) == 0 || len(value) > max || !hexPattern.MatchString(value) {
		return badArg("%s must be non-empty hex of at most %d characters", field, max)
	}
	return nil
}

// GetYellowbackInfo proxies yed_getinfo and adds this server's version.
func (y *YellowbackStreamer) GetYellowbackInfo(ctx context.Context, _ *walletrpc.Empty) (*walletrpc.YellowbackInfo, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	out := &walletrpc.YellowbackInfo{}
	if err := y.callCached(ctx, out, "yed_getinfo"); err != nil {
		return nil, err
	}
	out.ServerVersion = common.ServerVersion
	return out, nil
}

// GetPrice proxies yed_getprice [height]; height 0 means the index tip.
func (y *YellowbackStreamer) GetPrice(ctx context.Context, in *walletrpc.HeightFilter) (*walletrpc.YedPrice, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	out := &walletrpc.YedPrice{}
	if in == nil || in.Height == 0 {
		return out, y.callCached(ctx, out, "yed_getprice")
	}
	return out, y.call(ctx, out, "yed_getprice", common.JSONNumber(int64(in.Height)))
}

// GetStats proxies yed_getstats.
func (y *YellowbackStreamer) GetStats(ctx context.Context, _ *walletrpc.Empty) (*walletrpc.YellowbackStats, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	out := &walletrpc.YellowbackStats{}
	return out, y.callCached(ctx, out, "yed_getstats")
}

// GetActivation proxies yed_getactivation.
func (y *YellowbackStreamer) GetActivation(ctx context.Context, _ *walletrpc.Empty) (*walletrpc.YellowbackActivation, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	out := &walletrpc.YellowbackActivation{}
	return out, y.callCached(ctx, out, "yed_getactivation")
}

// GetTxInfo proxies yed_gettxinfo <txid>: the node's verdict on one transaction.
func (y *YellowbackStreamer) GetTxInfo(ctx context.Context, in *walletrpc.YedTxid) (*walletrpc.YedTxInfo, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	if in == nil || checkTxid(in.Txid) != nil {
		return nil, badArg("txid must be 64 hex characters")
	}
	out := &walletrpc.YedTxInfo{}
	return out, y.call(ctx, out, "yed_gettxinfo", common.JSONString(in.Txid))
}

// ValidateRawTransaction proxies yed_validaterawtransaction <hex>: the dry run a client performs
// before SendTransaction (plan section 5 item 4).
func (y *YellowbackStreamer) ValidateRawTransaction(ctx context.Context, in *walletrpc.RawTransaction) (*walletrpc.YedValidation, error) {
	if err := y.limited(ctx, yedCostHeavy); err != nil {
		return nil, err
	}
	if in == nil || len(in.Data) == 0 || len(in.Data) > maxHexLen/2 {
		return nil, badArg("data must be a non-empty raw transaction")
	}
	out := &walletrpc.YedValidation{}
	return out, y.call(ctx, out, "yed_validaterawtransaction", common.JSONString(hex.EncodeToString(in.Data)))
}

// DecodePayload proxies yed_decodepayload <hex>.
func (y *YellowbackStreamer) DecodePayload(ctx context.Context, in *walletrpc.YedHex) (*walletrpc.YedPayload, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	if in == nil {
		return nil, badArg("hex required")
	}
	if err := checkHex("hex", in.Hex, 2*520); err != nil {
		return nil, err
	}
	out := &walletrpc.YedPayload{}
	return out, y.call(ctx, out, "yed_decodepayload", common.JSONString(in.Hex))
}

// GetVault proxies yed_getvault <txid>.
func (y *YellowbackStreamer) GetVault(ctx context.Context, in *walletrpc.YedTxid) (*walletrpc.YedVault, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	if in == nil || checkTxid(in.Txid) != nil {
		return nil, badArg("txid must be 64 hex characters")
	}
	out := &walletrpc.YedVault{}
	return out, y.call(ctx, out, "yed_getvault", common.JSONString(in.Txid))
}

// ListVaults proxies yed_listvaults [status] [count] [skip] as a stream.
func (y *YellowbackStreamer) ListVaults(in *walletrpc.YedVaultFilter, stream walletrpc.YellowbackStreamer_ListVaultsServer) error {
	if err := y.limited(stream.Context(), yedCostHeavy); err != nil {
		return err
	}
	if in == nil {
		in = &walletrpc.YedVaultFilter{}
	}
	if in.Count > maxListCount {
		return badArg("count must be at most %d", maxListCount)
	}
	if !vaultStatuses[in.Status] {
		return badArg("status must be ACTIVE, VOID, CLOSED or CLAIMED")
	}
	// The RPC's positionals: status defaults to "" (all) when count or skip is given.
	var params []json.RawMessage
	if in.Status != "" || in.Count != 0 || in.Skip != 0 {
		params = append(params, common.JSONString(in.Status))
	}
	if in.Count != 0 || in.Skip != 0 {
		count := int64(in.Count)
		if count == 0 {
			count = defaultListCount
		}
		params = append(params, common.JSONNumber(count))
	}
	if in.Skip != 0 {
		params = append(params, common.JSONNumber(int64(in.Skip)))
	}
	var rows []*walletrpc.YedVault
	if err := y.call(stream.Context(), &rows, "yed_listvaults", params...); err != nil {
		return err
	}
	for _, row := range rows {
		if err := stream.Send(row); err != nil {
			return err
		}
	}
	return nil
}

// ListClaimable proxies yed_listclaimable as a stream.
func (y *YellowbackStreamer) ListClaimable(_ *walletrpc.Empty, stream walletrpc.YellowbackStreamer_ListClaimableServer) error {
	if err := y.limited(stream.Context(), yedCostHeavy); err != nil {
		return err
	}
	var rows []*walletrpc.YedClaimable
	if err := y.callCached(stream.Context(), &rows, "yed_listclaimable"); err != nil {
		return err
	}
	for _, row := range rows {
		if err := stream.Send(row); err != nil {
			return err
		}
	}
	return nil
}

// GetNotice proxies yed_getnotice <vaultTxid>; found=false is an answer, not an error.
func (y *YellowbackStreamer) GetNotice(ctx context.Context, in *walletrpc.YedTxid) (*walletrpc.YedNotice, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	if in == nil || checkTxid(in.Txid) != nil {
		return nil, badArg("txid must be 64 hex characters")
	}
	out := &walletrpc.YedNotice{}
	return out, y.call(ctx, out, "yed_getnotice", common.JSONString(in.Txid))
}

// EstimateCollateral proxies yed_estimatecollateral <cents> <lockBlocks> [priceMicroUsd].
func (y *YellowbackStreamer) EstimateCollateral(ctx context.Context, in *walletrpc.YedMintQuery) (*walletrpc.YedCollateralEstimate, error) {
	if err := y.limited(ctx, yedCostHeavy); err != nil {
		return nil, err
	}
	if in == nil || in.Cents == 0 || in.LockBlocks == 0 {
		return nil, badArg("cents and lockBlocks must be positive")
	}
	params := []json.RawMessage{common.JSONNumber(int64(in.Cents)), common.JSONNumber(int64(in.LockBlocks))}
	if in.PriceMicroUsd != 0 {
		params = append(params, common.JSONNumber(int64(in.PriceMicroUsd)))
	}
	out := &walletrpc.YedCollateralEstimate{}
	return out, y.call(ctx, out, "yed_estimatecollateral", params...)
}

// EstimateFee proxies yed_estimatefee <collateralZat>.
func (y *YellowbackStreamer) EstimateFee(ctx context.Context, in *walletrpc.YedFeeQuery) (*walletrpc.YedFeeEstimate, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	if in == nil || in.CollateralZat <= 0 {
		return nil, badArg("collateralZat must be positive")
	}
	out := &walletrpc.YedFeeEstimate{}
	return out, y.call(ctx, out, "yed_estimatefee", common.JSONNumber(in.CollateralZat))
}

// GetFeePayee proxies yed_getfeepayee <refHeight> <collateralZat> [selectorHex].
func (y *YellowbackStreamer) GetFeePayee(ctx context.Context, in *walletrpc.YedPayeeQuery) (*walletrpc.YedPayee, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	if in == nil || in.CollateralZat <= 0 {
		return nil, badArg("refHeight and a positive collateralZat are required")
	}
	params := []json.RawMessage{common.JSONNumber(int64(in.RefHeight)), common.JSONNumber(in.CollateralZat)}
	if in.SelectorHex != "" {
		if err := checkHex("selectorHex", in.SelectorHex, maxSelectorLen); err != nil {
			return nil, err
		}
		params = append(params, common.JSONString(in.SelectorHex))
	}
	out := &walletrpc.YedPayee{}
	return out, y.call(ctx, out, "yed_getfeepayee", params...)
}

func bundleParams(in *walletrpc.YedBundleQuery) ([]json.RawMessage, error) {
	if in == nil {
		return nil, badArg("refHeight required")
	}
	if in.SelectorHex != "" {
		if err := checkHex("selectorHex", in.SelectorHex, maxSelectorLen); err != nil {
			return nil, err
		}
	}
	return []json.RawMessage{common.JSONNumber(int64(in.RefHeight)), common.JSONString(in.SelectorHex)}, nil
}

// BuildBundle proxies yed_buildbundle <refHeight> <selectorHex>: the carrier step of a mint or
// claim. bundle-insufficient comes back as FAILED_PRECONDITION with the node's message.
func (y *YellowbackStreamer) BuildBundle(ctx context.Context, in *walletrpc.YedBundleQuery) (*walletrpc.YedBundle, error) {
	if err := y.limited(ctx, yedCostHeavy); err != nil {
		return nil, err
	}
	params, err := bundleParams(in)
	if err != nil {
		return nil, err
	}
	out := &walletrpc.YedBundle{}
	return out, y.call(ctx, out, "yed_buildbundle", params...)
}

// GetSelection proxies yed_getselection <refHeight> <selectorHex>.
func (y *YellowbackStreamer) GetSelection(ctx context.Context, in *walletrpc.YedBundleQuery) (*walletrpc.YedSelection, error) {
	if err := y.limited(ctx, yedCostHeavy); err != nil {
		return nil, err
	}
	params, err := bundleParams(in)
	if err != nil {
		return nil, err
	}
	out := &walletrpc.YedSelection{}
	return out, y.call(ctx, out, "yed_getselection", params...)
}

// GetAttestations proxies yed_getattestations (the node's pool) as a stream.
func (y *YellowbackStreamer) GetAttestations(_ *walletrpc.Empty, stream walletrpc.YellowbackStreamer_GetAttestationsServer) error {
	if err := y.limited(stream.Context(), yedCostHeavy); err != nil {
		return err
	}
	var rows []*walletrpc.YedAttestation
	if err := y.callCached(stream.Context(), &rows, "yed_getattestations"); err != nil {
		return err
	}
	for _, row := range rows {
		if err := stream.Send(row); err != nil {
			return err
		}
	}
	return nil
}

// ListAttestors proxies yed_listattestors [height] as a stream.
func (y *YellowbackStreamer) ListAttestors(in *walletrpc.HeightFilter, stream walletrpc.YellowbackStreamer_ListAttestorsServer) error {
	if err := y.limited(stream.Context(), yedCostLight); err != nil {
		return err
	}
	var params []json.RawMessage
	if in != nil && in.Height != 0 {
		params = append(params, common.JSONNumber(int64(in.Height)))
	}
	var rows []*walletrpc.YedAttestor
	if err := y.call(stream.Context(), &rows, "yed_listattestors", params...); err != nil {
		return err
	}
	for _, row := range rows {
		if err := stream.Send(row); err != nil {
			return err
		}
	}
	return nil
}

// GetAddressTokens proxies yed_listtokens <addresses> [minHeight] as a stream: the authoritative
// YED UTXO set of the client's addresses (plan section 4.4, D-L-7). Addresses may be the YED or
// the transparent form; the node maps both to one script.
func (y *YellowbackStreamer) GetAddressTokens(in *walletrpc.YedAddressList, stream walletrpc.YellowbackStreamer_GetAddressTokensServer) error {
	if err := y.limited(stream.Context(), yedCostHeavy); err != nil {
		return err
	}
	if in == nil || len(in.Addresses) == 0 || len(in.Addresses) > maxAddresses {
		return badArg("addresses must hold 1..%d entries", maxAddresses)
	}
	for _, a := range in.Addresses {
		if a == "" || len(a) > maxAddressLen {
			return badArg("addresses must be non-empty strings of at most %d characters", maxAddressLen)
		}
	}
	list, err := json.Marshal(in.Addresses)
	if err != nil {
		return badArg("addresses: %v", err)
	}
	params := []json.RawMessage{json.RawMessage(list)}
	if in.MinHeight != 0 {
		params = append(params, common.JSONNumber(int64(in.MinHeight)))
	}
	var rows []*walletrpc.YedToken
	if err := y.call(stream.Context(), &rows, "yed_listtokens", params...); err != nil {
		return err
	}
	for _, row := range rows {
		if err := stream.Send(row); err != nil {
			return err
		}
	}
	return nil
}

// chainInfoReply is the subset of getblockchaininfo GetChainInfo reads (common.ZcashdRpcReplyGetblockchaininfo
// is the frozen baseline's; this adds the upgrade names and the best block hash without touching it).
type chainInfoReply struct {
	Chain         string `json:"chain"`
	Blocks        uint64 `json:"blocks"`
	BestBlockHash string `json:"bestblockhash"`
	Consensus     struct {
		Chaintip  string `json:"chaintip"`
		Nextblock string `json:"nextblock"`
	} `json:"consensus"`
	Upgrades map[string]struct {
		Name             string `json:"name"`
		ActivationHeight int64  `json:"activationheight"`
		Status           string `json:"status"`
	} `json:"upgrades"`
}

// saplingBranchID is the Sapling network upgrade's consensus branch id (ZIP 205), the key
// GetLightdInfo (common.go) reads the activation height from; Ycash kept it.
const saplingBranchID = "76b809bb"

// GetChainInfo proxies the stock getblockchaininfo for the two consensus branch ids. The frozen
// GetLightdInfo reports consensus.chaintip only; a transaction meant for the NEXT block is signed
// under consensus.nextblock (ZIP 243 binds the sighash to it), which differs on the one block
// before a network upgrade (x402 X-F71). One node call serves every client for yedCacheTTL.
func (y *YellowbackStreamer) GetChainInfo(ctx context.Context, _ *walletrpc.Empty) (*walletrpc.YedChainInfo, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	var reply chainInfoReply
	if err := y.callCached(ctx, &reply, "getblockchaininfo"); err != nil {
		return nil, err
	}
	out := &walletrpc.YedChainInfo{ChainName: reply.Chain, BlockHeight: reply.Blocks, BestBlockHash: reply.BestBlockHash,
		ConsensusBranchId: reply.Consensus.Chaintip, NextBlockBranchId: reply.Consensus.Nextblock}
	if reply.Consensus.Chaintip == "" || reply.Consensus.Nextblock == "" {
		y.log.WithField("method", "getblockchaininfo").Error("Yellowback: node result carries no consensus branch ids")
		return nil, status.Errorf(codes.Internal, "getblockchaininfo: node result carries no consensus branch ids")
	}
	for id, u := range reply.Upgrades {
		if id == saplingBranchID {
			out.SaplingActivationHeight = uint64(u.ActivationHeight)
		}
		out.Upgrades = append(out.Upgrades, &walletrpc.YedUpgrade{BranchId: id, Name: u.Name, ActivationHeight: u.ActivationHeight, Status: u.Status})
	}
	sort.Slice(out.Upgrades, func(i, j int) bool {
		a, b := out.Upgrades[i], out.Upgrades[j]
		if a.ActivationHeight != b.ActivationHeight {
			return a.ActivationHeight < b.ActivationHeight
		}
		return a.BranchId < b.BranchId
	})
	return out, nil
}
