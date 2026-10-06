// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

// The vault primitive's read RPCs on the YellowbackStreamer (docs/plans/yellowback-upgrade-plan.md
// section 15.8): vault_getinfo, set_list, set_getinfo and vault_list, each a proxy through
// common.CallYed exactly like the yed_* handlers in yellowback.go. From rpcversion 5 YED runs on
// the primitive (its vaults are V templates, its claims intents, its attestors a set), so these are
// what a light client reads to see the attestor set and the intents of a claim. No rule here.
package frontend

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/zcash/lightwalletd/common"
	"github.com/zcash/lightwalletd/walletrpc"
)

var pubkeyPattern = regexp.MustCompile(`^0[23][0-9a-fA-F]{64}$`)
var tagHexPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}$`)
var tagTextPattern = regexp.MustCompile(`^[\x21-\x7e]{1,4}$`)

// GetVaultInfo proxies vault_getinfo: activation, branch id, counts and the state hash.
func (y *YellowbackStreamer) GetVaultInfo(ctx context.Context, _ *walletrpc.Empty) (*walletrpc.VaultInfo, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	out := &walletrpc.VaultInfo{}
	return out, y.callCached(ctx, out, "vault_getinfo")
}

// ListSets proxies set_list as a stream (every set, without its member list).
func (y *YellowbackStreamer) ListSets(_ *walletrpc.Empty, stream walletrpc.YellowbackStreamer_ListSetsServer) error {
	if err := y.limited(stream.Context(), yedCostHeavy); err != nil {
		return err
	}
	var rows []*walletrpc.VaultSet
	if err := y.callCached(stream.Context(), &rows, "set_list"); err != nil {
		return err
	}
	for _, row := range rows {
		if err := stream.Send(row); err != nil {
			return err
		}
	}
	return nil
}

// GetSet proxies set_getinfo <setid> [height]: parameters, members, the dormant/released
// predicates and the rate epoch; height 0 means the node's default (the next block).
func (y *YellowbackStreamer) GetSet(ctx context.Context, in *walletrpc.VaultSetQuery) (*walletrpc.VaultSet, error) {
	if err := y.limited(ctx, yedCostLight); err != nil {
		return nil, err
	}
	if in == nil || checkTxid(in.Setid) != nil {
		return nil, badArg("setid must be 64 hex characters")
	}
	params := []json.RawMessage{common.JSONString(in.Setid)}
	if in.Height != 0 {
		params = append(params, common.JSONNumber(int64(in.Height)))
	}
	out := &walletrpc.VaultSet{}
	return out, y.call(ctx, out, "set_getinfo", params...)
}

// ListVaultOutputs proxies vault_list [{tag, setid, owner, kind}] as a stream: the unspent vault
// and intent outputs. The node's "mine" filter is wallet context and has no field here.
func (y *YellowbackStreamer) ListVaultOutputs(in *walletrpc.VaultOutputFilter, stream walletrpc.YellowbackStreamer_ListVaultOutputsServer) error {
	if err := y.limited(stream.Context(), yedCostHeavy); err != nil {
		return err
	}
	if in == nil {
		in = &walletrpc.VaultOutputFilter{}
	}
	filter := map[string]string{}
	if in.Tag != "" {
		if !tagHexPattern.MatchString(in.Tag) && !tagTextPattern.MatchString(in.Tag) {
			return badArg("tag must be 1-4 printable ASCII characters or 8 hex digits")
		}
		filter["tag"] = in.Tag
	}
	if in.Setid != "" {
		if checkTxid(in.Setid) != nil {
			return badArg("setid must be 64 hex characters")
		}
		filter["setid"] = in.Setid
	}
	if in.Owner != "" {
		if !pubkeyPattern.MatchString(in.Owner) {
			return badArg("owner must be a compressed public key (66 hex characters)")
		}
		filter["owner"] = in.Owner
	}
	switch in.Kind {
	case "":
	case "vault", "intent":
		filter["kind"] = in.Kind
	default:
		return badArg("kind must be vault or intent")
	}
	var params []json.RawMessage
	if len(filter) > 0 {
		encoded, err := json.Marshal(filter)
		if err != nil {
			return badArg("filter: %v", err)
		}
		params = append(params, json.RawMessage(encoded))
	}
	var rows []*walletrpc.VaultOutput
	if err := y.call(stream.Context(), &rows, "vault_list", params...); err != nil {
		return err
	}
	for _, row := range rows {
		if err := stream.Send(row); err != nil {
			return err
		}
	}
	return nil
}
