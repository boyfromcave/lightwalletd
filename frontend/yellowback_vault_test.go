// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

// Offline tests of the vault primitive's read RPCs on the YellowbackStreamer
// (yellowback_vault.go) and of the rpcversion 5 shapes that come with the vault upgrade. The
// fake node answers set_* / vault_* from testdata/vault/<method>.json, real answers captured from
// a ycash-dd upgrade/vault devnet (a set with one member, a TEST vault unlocked into an intent).
package frontend

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"regexp"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zcash/lightwalletd/common"
	"github.com/zcash/lightwalletd/walletrpc"
)

type setStream struct{ fakeStream }

func (s *setStream) Send(m *walletrpc.VaultSet) error { return s.fakeStream.Send(m) }

type vaultOutputStream struct{ fakeStream }

func (s *vaultOutputStream) Send(m *walletrpc.VaultOutput) error { return s.fakeStream.Send(m) }

// hasKey reports whether an encoded message carries key at any depth.
func hasKey(v interface{}, key string) bool {
	switch x := v.(type) {
	case map[string]interface{}:
		for k, e := range x {
			if k == key || hasKey(e, key) {
				return true
			}
		}
	case []interface{}:
		for _, e := range x {
			if hasKey(e, key) {
				return true
			}
		}
	}
	return false
}

func encodedOf(t *testing.T, msg interface{}) interface{} {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var out interface{}
	_ = json.Unmarshal(b, &out)
	return out
}

// TestVaultMethodsMatchFixtures: every vault handler's answer, field by field, against the node's
// own answer; the node's "wallet" fields (its wallet, not the client's) never reach a client.
func TestVaultMethodsMatchFixtures(t *testing.T) {
	svc, node := newService(t)
	ctx := context.Background()

	info, err := svc.GetVaultInfo(ctx, &walletrpc.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	assertMatchesContract(t, "vault_getinfo", info, vaultFixture(t, "vault_getinfo"))
	assertNonZeroFieldsPresent(t, "vault_getinfo", info, vaultFixture(t, "vault_getinfo"))
	if info.Branchid != "6d5b7a31" || !info.Active || info.GetDbtip().GetHeight() == 0 || info.Lockedvalue == 0 {
		t.Errorf("vault_getinfo: %+v", info)
	}

	set, err := svc.GetSet(ctx, &walletrpc.VaultSetQuery{Setid: "fc7cca3f6bef557056dd9445ac0074b66c097b9d63ebb871ca732bec467905a2"})
	if err != nil {
		t.Fatal(err)
	}
	assertMatchesContract(t, "set_getinfo", set, vaultFixture(t, "set_getinfo"))
	assertNonZeroFieldsPresent(t, "set_getinfo", set, vaultFixture(t, "set_getinfo"))
	if len(set.Memberlist) != 1 || set.Memberlist[0].Status != "active" || !set.Memberlist[0].Current ||
		set.Epochused != 4 || set.Epochbasis != 10 || set.Unlockavailable != 1 || set.Ratelimitbps != 5000 {
		t.Errorf("set_getinfo: %+v", set)
	}
	if p := node.params["set_getinfo"]; len(p) != 1 {
		t.Errorf("set_getinfo without a height must send the setid only, sent %s", p)
	}

	ss := &setStream{}
	if err := svc.ListSets(&walletrpc.Empty{}, ss); err != nil {
		t.Fatal(err)
	}
	var setRows []json.RawMessage
	if err := json.Unmarshal(vaultFixture(t, "set_list"), &setRows); err != nil {
		t.Fatal(err)
	}
	if len(ss.rows) != len(setRows) || len(ss.rows) == 0 {
		t.Fatalf("set_list: streamed %d rows, node has %d", len(ss.rows), len(setRows))
	}
	for i := range ss.rows {
		assertMatchesContract(t, "set_list", ss.rows[i], setRows[i])
		assertNonZeroFieldsPresent(t, "set_list", ss.rows[i], setRows[i])
	}

	vs := &vaultOutputStream{}
	if err := svc.ListVaultOutputs(&walletrpc.VaultOutputFilter{}, vs); err != nil {
		t.Fatal(err)
	}
	var outRows []json.RawMessage
	if err := json.Unmarshal(vaultFixture(t, "vault_list"), &outRows); err != nil {
		t.Fatal(err)
	}
	if len(vs.rows) != len(outRows) || len(vs.rows) != 2 {
		t.Fatalf("vault_list: streamed %d rows, node has %d", len(vs.rows), len(outRows))
	}
	for i := range vs.rows {
		assertMatchesContract(t, "vault_list", vs.rows[i], outRows[i])
		assertNonZeroFieldsPresent(t, "vault_list", vs.rows[i], outRows[i])
	}
	intent := vs.rows[0].(*walletrpc.VaultOutput)
	if intent.Kind != "intent" || intent.Matureheight != 207 || !intent.Cancellable || intent.Valuezat != 400000000 || intent.Origin == "" {
		t.Errorf("intent row: %+v", intent)
	}
	if p := node.params["vault_list"]; len(p) != 0 {
		t.Errorf("an empty filter must send no params, sent %s", p)
	}

	for _, msg := range []interface{}{set, ss.rows[0], vs.rows[0], vs.rows[1]} {
		if hasKey(encodedOf(t, msg), "wallet") {
			t.Errorf("%T carries the node's wallet field", msg)
		}
	}
}

// TestVaultParamsAndValidation: the filter is a JSON object of the given fields only (never
// "mine"), a set height is a number, and malformed inputs stop at the edge with no node call.
func TestVaultParamsAndValidation(t *testing.T) {
	svc, node := newService(t)
	ctx := context.Background()
	setid := "fc7cca3f6bef557056dd9445ac0074b66c097b9d63ebb871ca732bec467905a2"
	if _, err := svc.GetSet(ctx, &walletrpc.VaultSetQuery{Setid: setid, Height: 300}); err != nil {
		t.Fatal(err)
	}
	if p := node.params["set_getinfo"]; len(p) != 2 || string(p[0]) != `"`+setid+`"` || string(p[1]) != "300" {
		t.Errorf("set_getinfo params = %s", p)
	}
	owner := "0379f92647dcfb38dcfcc3a6f0df13bf2083e0c618d8655dca7b61542f1646f1b5"
	if err := svc.ListVaultOutputs(&walletrpc.VaultOutputFilter{Tag: "YED", Setid: setid, Owner: owner, Kind: "intent"}, &vaultOutputStream{}); err != nil {
		t.Fatal(err)
	}
	p := node.params["vault_list"]
	if len(p) != 1 {
		t.Fatalf("vault_list params = %s", p)
	}
	var filter map[string]interface{}
	if err := json.Unmarshal(p[0], &filter); err != nil {
		t.Fatal(err)
	}
	if len(filter) != 4 || filter["tag"] != "YED" || filter["setid"] != setid || filter["owner"] != owner || filter["kind"] != "intent" {
		t.Errorf("vault_list filter = %v", filter)
	}
	if err := svc.ListVaultOutputs(&walletrpc.VaultOutputFilter{Tag: "59454400"}, &vaultOutputStream{}); err != nil {
		t.Errorf("an 8-hex-digit tag: %v", err)
	}

	before := len(node.calls)
	bad := []func() error{
		func() error { _, err := svc.GetSet(ctx, &walletrpc.VaultSetQuery{Setid: "zz"}); return err },
		func() error { _, err := svc.GetSet(ctx, nil); return err },
		func() error {
			return svc.ListVaultOutputs(&walletrpc.VaultOutputFilter{Tag: "TOOLONG"}, &vaultOutputStream{})
		},
		func() error {
			return svc.ListVaultOutputs(&walletrpc.VaultOutputFilter{Tag: "\u00e9"}, &vaultOutputStream{})
		},
		func() error {
			return svc.ListVaultOutputs(&walletrpc.VaultOutputFilter{Setid: "12"}, &vaultOutputStream{})
		},
		func() error {
			return svc.ListVaultOutputs(&walletrpc.VaultOutputFilter{Owner: "04" + owner[2:]}, &vaultOutputStream{})
		},
		func() error {
			return svc.ListVaultOutputs(&walletrpc.VaultOutputFilter{Kind: "bond"}, &vaultOutputStream{})
		},
	}
	for i, call := range bad {
		if st, _ := status.FromError(call()); st.Code() != codes.InvalidArgument {
			t.Errorf("case %d: want INVALID_ARGUMENT, got %v", i, st)
		}
	}
	if len(node.calls) != before {
		t.Errorf("a refused input reached the node: %v", node.calls[before:])
	}
}

// TestVaultAllowListCoversDoc: every set_* / vault_* command of the node's vault-rpc.md (copied
// to testdata/vault/) is offered or declined with a reason, and nothing else is listed.
func TestVaultAllowListCoversDoc(t *testing.T) {
	doc, err := ioutil.ReadFile("../testdata/vault/vault-rpc.md")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^### `((?:set|vault)_[a-z]+)").FindAllStringSubmatch(string(doc), -1) {
		documented[m[1]] = true
	}
	if len(documented) < 20 {
		t.Fatalf("found only %d commands in vault-rpc.md", len(documented))
	}
	for method := range documented {
		_, offered := common.VaultMethods[method]
		_, declined := common.NotOfferedVault[method]
		if offered == declined {
			t.Errorf("%s: must be in exactly one of VaultMethods and NotOfferedVault", method)
		}
	}
	for method := range common.VaultMethods {
		if !documented[method] {
			t.Errorf("%s is allow-listed but not in vault-rpc.md", method)
		}
		if common.YedMethods[method] || common.StockMethods[method] {
			t.Errorf("%s is in two allow-lists", method)
		}
	}
	for method := range common.NotOfferedVault {
		if !documented[method] {
			t.Errorf("%s is in NotOfferedVault but not in vault-rpc.md", method)
		}
	}
	node := newFakeNode(t)
	install(t, node)
	if _, err := common.CallYed(context.Background(), "vault_lock"); status.Code(err) != codes.Internal || len(node.calls) != 0 {
		t.Errorf("vault_lock must be refused without a node call: %v", err)
	}
}

// TestUnaryInfoUpgrade: rpcversion 5's yed_getinfo.upgrade and the attestor set reach the client,
// and GetActivation is the upgrade object.
func TestUnaryInfoUpgrade(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	info, err := svc.GetYellowbackInfo(ctx, &walletrpc.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	u := info.GetUpgrade()
	if u.GetName() != "Vault" || u.GetBranchId() != "6d5b7a31" || u.GetStatus() != "active" || u.GetActivationHeight() != 103 ||
		u.GetAttestorSetId() == "" || u.GetAttestorSetId() != info.GetParams().GetAttestorSetId() || info.GetParams().GetClaimDelay() != 10 {
		t.Errorf("upgrade: %+v params %+v", u, info.GetParams())
	}
	act, err := svc.GetActivation(ctx, &walletrpc.Empty{})
	if err != nil || act.GetBranchId() != u.GetBranchId() || act.GetClaimDelay() != u.GetClaimDelay() || act.GetHeight() != u.GetHeight() {
		t.Errorf("GetActivation %+v (err %v) differs from yed_getinfo.upgrade %+v", act, err, u)
	}
	vs := &vaultStream{}
	if err := svc.ListVaults(&walletrpc.YedVaultFilter{Status: "CLAIMING"}, vs); err != nil {
		t.Fatalf("CLAIMING is a status filter from rpcversion 5: %v", err)
	}
	if v := vs.rows[0].(*walletrpc.YedVault); v.ScriptPubKey == "" {
		t.Errorf("vault row without scriptPubKey: %+v", v)
	}
}
