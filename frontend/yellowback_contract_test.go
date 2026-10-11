// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

// The reverse contract checks (quantum clients survey, lightwalletd (c) "gap"): the forward checks
// (TestUnaryMethodsMatchContract, TestVaultMethodsMatchFixtures) compare what a message CARRIES
// against the node's example, so a field the node adds or renames is silently dropped by the JSON
// unmarshal and nothing fails. These tests walk the node's side instead: every field of every
// offered method's result must be declared in the proto message, or be on an explicit list below
// with the reason it is not carried. A renamed field (rpcversion 7's ownerPubKey -> ownerHash)
// then fails here until the proto follows.
package frontend

import (
	"encoding/json"
	"io/ioutil"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/zcash/lightwalletd/walletrpc"
)

// notCarried: contract fields (method.path, rows of a list as []) the proto deliberately omits.
// A message may carry a subset of a result (yellowback.proto header); each omission is named here.
var notCarried = map[string]string{
	// yed_getinfo: operator and attestor tuning that no light client reads (the light-client plan
	// section 4.1 carries what a wallet needs to mint, redeem and judge health).
	"yed_getinfo.params.attest.ageCap":             "attestor tuning",
	"yed_getinfo.params.attest.armDelay":           "attestor tuning",
	"yed_getinfo.params.attest.armMin":             "attestor tuning",
	"yed_getinfo.params.attest.divergeBpsAttest":   "attestor tuning",
	"yed_getinfo.params.attest.dormancyBlocks":     "attestor tuning",
	"yed_getinfo.params.attest.dormancyCheck":      "attestor tuning",
	"yed_getinfo.params.attest.dormancyMinBundles": "attestor tuning",
	"yed_getinfo.params.attest.emergencyNoticeTtl": "attestor tuning",
	"yed_getinfo.params.attest.emergencyPersist":   "attestor tuning",
	"yed_getinfo.params.attest.foundingWindow":     "attestor tuning",
	"yed_getinfo.params.attest.kSlack":             "attestor tuning",
	"yed_getinfo.params.attest.nSlots":             "attestor tuning",
	"yed_getinfo.params.attest.pinDeltaBps":        "attestor tuning",
	"yed_getinfo.params.attest.pinMinBundles":      "attestor tuning",
	"yed_getinfo.params.attest.pinMinTags":         "attestor tuning",
	"yed_getinfo.params.attest.pinWindow":          "attestor tuning",
	"yed_getinfo.params.attest.qHighBps":           "attestor tuning",
	"yed_getinfo.params.attest.qLowBps":            "attestor tuning",
	"yed_getinfo.params.minFill":                   "GetPrice carries the fills (YedPrice.fill)",
	"yed_getinfo.params.windows":                   "GetPrice carries the windows (YedPrice.fill)",
	"yed_getinfo.params.payeeWindow":               "miner tuning; GetFeePayee answers the payee",
	"yed_getinfo.params.policy":                    "miner-local policy (preferred payee/attestor)",
	"yed_getinfo.supplyCapReached":                 "GetStats carries the supply and its cap",
	"yed_getinfo.lockedOutputs":                    "the node's wallet layer (H10), not the client's",
	"yed_getinfo.miner":                            "the node's own miner state (quote, payout address)",
	// The decimal YEC twin of collateralZat (yellowback-rpc.md: amounts are zatoshi, a decimal twin
	// only where the prototype had one); the proto carries the exact integer.
	"yed_getvault.collateral":     "decimal twin of collateralZat",
	"yed_listvaults[].collateral": "decimal twin of collateralZat",
	// Quantum line (rpcversion 7, the node's Y-B sweep). Not carried yet: carrying them changes
	// yellowback.proto and so YEW's proto/PIN; a light wallet derives both from what it has.
	"yed_estimatecollateral.requiredZatFloored": "max(requiredZat, 4 x FEE_MIN) rounded up to 1,000 zat; the wallet floors requiredZat with its own FEE_MIN",
	"yed_getinfo.params.addressVersion":         "address version bytes: a light wallet takes them from its network parameters",
	"yed_getinfo.params.pqAddressVersion":       "address version bytes: a light wallet takes them from its network parameters",
	"yed_getinfo.params.pq.plainAddressVersion": "address version bytes: a light wallet takes them from its network parameters",
}

// fieldByJSON finds a message's field by its JSON name or its proto name.
func fieldByJSON(md protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if f.JSONName() == name || string(f.Name()) == name {
			return f
		}
	}
	return nil
}

// undeclared walks a contract example against a message descriptor and returns every path whose
// key the message does not declare (and is not in skip), descending into message fields.
func undeclared(path string, example interface{}, md protoreflect.MessageDescriptor, skip map[string]string) []string {
	var out []string
	switch v := example.(type) {
	case []interface{}:
		for _, e := range v {
			out = append(out, undeclared(path+"[]", e, md, skip)...)
		}
	case map[string]interface{}:
		for key, e := range v {
			p := path + "." + key
			if _, ok := skip[p]; ok {
				continue
			}
			f := fieldByJSON(md, key)
			if f == nil {
				if md.ReservedNames().Has(protoreflect.Name(key)) {
					out = append(out, p+" (a RESERVED name: the node still sends a field this proto retired)")
				} else {
					out = append(out, p)
				}
				continue
			}
			if f.Kind() == protoreflect.MessageKind && e != nil {
				out = append(out, undeclared(p, e, f.Message(), skip)...)
			}
		}
	}
	return out
}

// TestContractFieldsAreDeclared: every field of every offered yed_* method's contract example is a
// field of the method's message (or a notCarried entry), and every notCarried entry still exists.
func TestContractFieldsAreDeclared(t *testing.T) {
	c := loadContract(t)
	messages := map[string]proto.Message{
		"yed_getinfo":                &walletrpc.YellowbackInfo{},
		"yed_getprice":               &walletrpc.YedPrice{},
		"yed_getstats":               &walletrpc.YellowbackStats{},
		"yed_getactivation":          &walletrpc.YellowbackActivation{},
		"yed_gettxinfo":              &walletrpc.YedTxInfo{},
		"yed_validaterawtransaction": &walletrpc.YedValidation{},
		"yed_decodepayload":          &walletrpc.YedPayload{},
		"yed_getvault":               &walletrpc.YedVault{},
		"yed_listvaults":             &walletrpc.YedVault{},
		"yed_listclaimable":          &walletrpc.YedClaimable{},
		"yed_getnotice":              &walletrpc.YedNotice{},
		"yed_estimatecollateral":     &walletrpc.YedCollateralEstimate{},
		"yed_estimatefee":            &walletrpc.YedFeeEstimate{},
		"yed_getfeepayee":            &walletrpc.YedPayee{},
		"yed_buildbundle":            &walletrpc.YedBundle{},
		"yed_getselection":           &walletrpc.YedSelection{},
		"yed_getattestations":        &walletrpc.YedAttestation{},
		"yed_listattestors":          &walletrpc.YedAttestor{},
		"yed_listtokens":             &walletrpc.YedToken{},
	}
	var missing []string
	for method, msg := range messages {
		var example interface{}
		if err := json.Unmarshal(c.returns(t, method), &example); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		missing = append(missing, undeclared(method, example, msg.ProtoReflect().Descriptor(), notCarried)...)
	}
	sort.Strings(missing)
	for _, p := range missing {
		t.Errorf("%s: in the node's contract but not declared in yellowback.proto (add it, or list it in notCarried with a reason)", p)
	}
	// A stale notCarried entry hides nothing but misleads: each must still be a contract path.
	for p := range notCarried {
		if !contractHasPath(t, c, p) {
			t.Errorf("notCarried %s: the contract no longer has this field", p)
		}
	}
}

func contractHasPath(t *testing.T, c contract, path string) bool {
	var cur interface{}
	parts := splitPath(path)
	if err := json.Unmarshal(c.returns(t, parts[0]), &cur); err != nil {
		return false
	}
	for _, key := range parts[1:] {
		if rows, ok := cur.([]interface{}); ok && len(rows) > 0 {
			cur = rows[0]
		}
		obj, ok := cur.(map[string]interface{})
		if !ok {
			return false
		}
		if cur, ok = obj[key]; !ok {
			return false
		}
	}
	return true
}

// splitPath splits method.a[].b into [method a b]; a row marker is implied by the value.
func splitPath(p string) []string {
	parts := strings.Split(p, ".")
	for i := range parts {
		parts[i] = strings.TrimSuffix(parts[i], "[]")
	}
	return parts
}

// vaultContract is the node's doc/vault-rpc-contract.json (generated from doc/vault-rpc.md by
// ycash-dd qa/vault-rpc-contract.py), copied to testdata/vault/ beside the document.
type vaultContract struct {
	Shapes  map[string]vaultType `json:"shapes"`
	Methods map[string]struct {
		Result vaultType `json:"result"`
	} `json:"methods"`
}

type vaultType struct {
	Type     string               `json:"type"`
	Ref      string               `json:"ref"`
	Fields   map[string]vaultType `json:"fields"`
	Items    *vaultType           `json:"items"`
	OneOf    []vaultType          `json:"oneOf"`
	AllOf    []vaultType          `json:"allOf"`
	Spread   []string             `json:"spread"`
	Optional []string             `json:"optional"`
}

// fieldsOf flattens a vault contract type into its object field names (a ref resolved, an array
// to its items, a oneOf/allOf merged), each with its own type for descent.
func (vc *vaultContract) fieldsOf(vt vaultType, depth int) map[string]vaultType {
	out := map[string]vaultType{}
	if depth > 8 {
		return out
	}
	if vt.Ref != "" {
		return vc.fieldsOf(vc.Shapes[vt.Ref], depth+1)
	}
	if vt.Items != nil {
		return vc.fieldsOf(*vt.Items, depth+1)
	}
	for _, alt := range append(append([]vaultType{}, vt.OneOf...), vt.AllOf...) {
		for k, v := range vc.fieldsOf(alt, depth+1) {
			out[k] = v
		}
	}
	for _, name := range vt.Spread {
		for k, v := range vc.fieldsOf(vc.Shapes[name], depth+1) {
			out[k] = v
		}
	}
	for k, v := range vt.Fields {
		out[k] = v
	}
	return out
}

// TestVaultContractFieldsAreDeclared: the same reverse check for the four primitive proxies against
// the node's vault RPC contract, and the forward one too (every proto field is a contract field),
// since the vault fixtures are captured answers and cannot show a field the node renamed. The node's
// "wallet" field (its wallet, not the client's) is the one deliberate omission.
func TestVaultContractFieldsAreDeclared(t *testing.T) {
	data, err := ioutil.ReadFile("../testdata/vault/vault-rpc-contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var vc vaultContract
	if err := json.Unmarshal(data, &vc); err != nil {
		t.Fatal(err)
	}
	messages := map[string]proto.Message{
		"vault_getinfo": &walletrpc.VaultInfo{},
		"set_list":      &walletrpc.VaultSet{},
		"set_getinfo":   &walletrpc.VaultSet{},
		"vault_list":    &walletrpc.VaultOutput{},
	}
	// Fields a message declares for another method of the same message (VaultSet serves both
	// set_list and set_getinfo; memberlist is set_getinfo's only).
	otherMethod := map[string]bool{"set_list.memberlist": true}
	var check func(path string, vt vaultType, md protoreflect.MessageDescriptor)
	check = func(path string, vt vaultType, md protoreflect.MessageDescriptor) {
		fields := vc.fieldsOf(vt, 0)
		if len(fields) == 0 {
			t.Errorf("%s: the vault contract has no object fields here", path)
			return
		}
		for name, ft := range fields {
			if name == "wallet" {
				continue
			}
			f := fieldByJSON(md, name)
			if f == nil {
				t.Errorf("%s.%s: in the node's vault contract but not declared in yellowback.proto", path, name)
				continue
			}
			if f.Kind() == protoreflect.MessageKind {
				check(path+"."+name, ft, f.Message())
			}
		}
		declared := md.Fields()
		for i := 0; i < declared.Len(); i++ {
			name := declared.Get(i).JSONName()
			if _, ok := fields[name]; !ok && !otherMethod[path+"."+name] {
				t.Errorf("%s.%s: declared in yellowback.proto but not in the node's vault contract (renamed or removed?)", path, name)
			}
		}
	}
	for method, msg := range messages {
		m, ok := vc.Methods[method]
		if !ok {
			t.Errorf("%s: not in the vault contract", method)
			continue
		}
		check(method, m.Result, msg.ProtoReflect().Descriptor())
	}
}
