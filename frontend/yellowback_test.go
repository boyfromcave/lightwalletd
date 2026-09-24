// Offline tests for the Yellowback service: a fake node answering every yed_* RPC from the
// node's contract (testdata/yellowback/contract.json, kept equal to
// ycash-dd/doc/yellowback-rpc-contract.json by the workspace's `make spec`), the same trick as
// YecWallet's offline QTest. Plan section 6.2.
package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/zcash/lightwalletd/common"
	"github.com/zcash/lightwalletd/walletrpc"
)

// contract is the node's RPC contract: method -> {"args": ..., "returns": ...}.
type contract map[string]json.RawMessage

func loadContract(t *testing.T) contract {
	t.Helper()
	data, err := ioutil.ReadFile("../testdata/yellowback/contract.json")
	if err != nil {
		t.Fatalf("contract fixture: %v", err)
	}
	var c contract
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("contract fixture: %v", err)
	}
	return c
}

func (c contract) returns(t *testing.T, method string) json.RawMessage {
	t.Helper()
	var entry struct {
		Returns json.RawMessage `json:"returns"`
	}
	raw, ok := c[method]
	if !ok {
		t.Fatalf("%s is not in the contract", method)
	}
	if err := json.Unmarshal(raw, &entry); err != nil || len(entry.Returns) == 0 {
		t.Fatalf("%s: no returns in the contract", method)
	}
	return entry.Returns
}

// fakeNode answers RawRequest from the contract's example values, records the calls, and can
// be told to fail a method with a node-style error. It is installed as common.RawRequest, the
// tree's own stub pattern (common_test.go, frontend_test.go).
type fakeNode struct {
	t        *testing.T
	contract contract
	calls    []string
	params   map[string][]json.RawMessage
	fail     map[string]error
	features []string
}

func newFakeNode(t *testing.T) *fakeNode {
	return &fakeNode{t: t, contract: loadContract(t), params: map[string][]json.RawMessage{}, fail: map[string]error{},
		features: []string{"yellowback"}}
}

func (f *fakeNode) RawRequest(method string, params []json.RawMessage) (json.RawMessage, error) {
	f.calls = append(f.calls, method)
	f.params[method] = params
	if err, ok := f.fail[method]; ok {
		return nil, err
	}
	if method == "getexperimentalfeatures" {
		b, _ := json.Marshal(f.features)
		return b, nil
	}
	return f.contract.returns(f.t, method), nil
}

// install makes node the process's RawRequest for the test and restores the previous one after.
func install(t *testing.T, node *fakeNode) {
	t.Helper()
	previous := common.RawRequest
	common.RawRequest = node.RawRequest
	t.Cleanup(func() { common.RawRequest = previous })
}

func newService(t *testing.T) (*YellowbackStreamer, *fakeNode) {
	node := newFakeNode(t)
	install(t, node)
	capability := common.YellowbackCapability{Enabled: true, RPCVersion: common.KnownRPCVersion, Network: "regtest"}
	logger := logrus.New()
	logger.SetOutput(ioutil.Discard)
	return NewYellowbackStreamer(capability, logger.WithField("test", t.Name())), node
}

// fakeStream collects what a streaming handler sends.
type fakeStream struct {
	grpc.ServerStream
	rows []interface{}
}

func (s *fakeStream) Send(m interface{}) error { s.rows = append(s.rows, m); return nil }
func (s *fakeStream) Context() context.Context { return context.Background() }

type vaultStream struct{ fakeStream }

func (s *vaultStream) Send(m *walletrpc.YedVault) error { return s.fakeStream.Send(m) }

type claimableStream struct{ fakeStream }

func (s *claimableStream) Send(m *walletrpc.YedClaimable) error { return s.fakeStream.Send(m) }

type attestationStream struct{ fakeStream }

func (s *attestationStream) Send(m *walletrpc.YedAttestation) error { return s.fakeStream.Send(m) }

type attestorStream struct{ fakeStream }

func (s *attestorStream) Send(m *walletrpc.YedAttestor) error { return s.fakeStream.Send(m) }

type tokenStream struct{ fakeStream }

func (s *tokenStream) Send(m *walletrpc.YedToken) error { return s.fakeStream.Send(m) }

// assertMatchesContract re-encodes the message with encoding/json (its json tags are the
// contract's names) and checks every field the message carries against the contract example.
// A message may carry a subset of the result's fields, never a differently named or typed one.
func assertMatchesContract(t *testing.T, method string, got interface{}, example json.RawMessage) {
	t.Helper()
	var want map[string]interface{}
	if err := json.Unmarshal(example, &want); err != nil {
		t.Fatalf("%s: contract example is not an object", method)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	var have map[string]interface{}
	if err := json.Unmarshal(encoded, &have); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	compareObjects(t, method, have, want)
}

func compareObjects(t *testing.T, path string, have, want map[string]interface{}) {
	t.Helper()
	for key, hv := range have {
		if key == "serverVersion" { // added by this server, not in the node's result
			continue
		}
		wv, ok := want[key]
		if !ok {
			t.Errorf("%s.%s: the message carries a field the contract does not name", path, key)
			continue
		}
		compareValues(t, path+"."+key, hv, wv)
	}
	// omitempty drops zero values; every contract field with a non-zero example that the
	// message declares must therefore be present. Fields the message does not declare are
	// allowed (a subset), so a missing key is only an error when it is a declared field with a
	// non-zero example — detected through the struct's json tags by the caller's type.
}

func compareValues(t *testing.T, path string, have, want interface{}) {
	t.Helper()
	switch hv := have.(type) {
	case map[string]interface{}:
		wv, ok := want.(map[string]interface{})
		if !ok {
			t.Errorf("%s: message has an object, contract has %T", path, want)
			return
		}
		compareObjects(t, path, hv, wv)
	case []interface{}:
		wv, ok := want.([]interface{})
		if !ok {
			t.Errorf("%s: message has an array, contract has %T", path, want)
			return
		}
		if len(hv) != len(wv) {
			t.Errorf("%s: %d elements, contract has %d", path, len(hv), len(wv))
			return
		}
		for i := range hv {
			compareValues(t, path+"[]", hv[i], wv[i])
		}
	default:
		if !reflect.DeepEqual(have, want) {
			t.Errorf("%s: message has %v (%T), contract has %v (%T)", path, have, have, want, want)
		}
	}
}

// declaredFields lists the json names a generated message declares (its non-XXX fields).
func declaredFields(msg interface{}) map[string]bool {
	out := map[string]bool{}
	typ := reflect.TypeOf(msg).Elem()
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		out[strings.Split(tag, ",")[0]] = true
	}
	return out
}

// assertNonZeroFieldsPresent: every declared field whose contract example is non-zero must
// survive the round trip (guards against a type mismatch that silently zeroes a field).
func assertNonZeroFieldsPresent(t *testing.T, method string, msg interface{}, example json.RawMessage) {
	t.Helper()
	var want map[string]interface{}
	if err := json.Unmarshal(example, &want); err != nil {
		return
	}
	encoded, _ := json.Marshal(msg)
	var have map[string]interface{}
	_ = json.Unmarshal(encoded, &have)
	for name := range declaredFields(msg) {
		wv, ok := want[name]
		if !ok || wv == nil {
			continue
		}
		if isZero(wv) {
			continue
		}
		if _, present := have[name]; !present {
			t.Errorf("%s.%s: contract example is %v but the field decoded as zero", method, name, wv)
		}
	}
}

func isZero(v interface{}) bool {
	switch x := v.(type) {
	case float64:
		return x == 0
	case string:
		return x == ""
	case bool:
		return !x
	case []interface{}:
		return len(x) == 0
	case map[string]interface{}:
		return len(x) == 0
	}
	return v == nil
}

const exampleTxid = "6a1f2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8"

// TestUnaryMethodsMatchContract calls every unary method with a valid input and checks the
// answer field by field against the contract's example.
func TestUnaryMethodsMatchContract(t *testing.T) {
	svc, node := newService(t)
	ctx := context.Background()
	cases := []struct {
		method string
		call   func() (interface{}, error)
	}{
		{"yed_getinfo", func() (interface{}, error) { return svc.GetYellowbackInfo(ctx, &walletrpc.Empty{}) }},
		{"yed_getprice", func() (interface{}, error) { return svc.GetPrice(ctx, &walletrpc.HeightFilter{Height: 331}) }},
		{"yed_getstats", func() (interface{}, error) { return svc.GetStats(ctx, &walletrpc.Empty{}) }},
		{"yed_getactivation", func() (interface{}, error) { return svc.GetActivation(ctx, &walletrpc.Empty{}) }},
		{"yed_gettxinfo", func() (interface{}, error) { return svc.GetTxInfo(ctx, &walletrpc.YedTxid{Txid: exampleTxid}) }},
		{"yed_validaterawtransaction", func() (interface{}, error) {
			return svc.ValidateRawTransaction(ctx, &walletrpc.RawTransaction{Data: []byte{4, 0, 0, 0x80}})
		}},
		{"yed_decodepayload", func() (interface{}, error) { return svc.DecodePayload(ctx, &walletrpc.YedHex{Hex: "59420301"}) }},
		{"yed_getvault", func() (interface{}, error) { return svc.GetVault(ctx, &walletrpc.YedTxid{Txid: exampleTxid}) }},
		{"yed_getnotice", func() (interface{}, error) { return svc.GetNotice(ctx, &walletrpc.YedTxid{Txid: exampleTxid}) }},
		{"yed_estimatecollateral", func() (interface{}, error) {
			return svc.EstimateCollateral(ctx, &walletrpc.YedMintQuery{Cents: 100000, LockBlocks: 48})
		}},
		{"yed_estimatefee", func() (interface{}, error) {
			return svc.EstimateFee(ctx, &walletrpc.YedFeeQuery{CollateralZat: 25125628141})
		}},
		{"yed_getfeepayee", func() (interface{}, error) {
			return svc.GetFeePayee(ctx, &walletrpc.YedPayeeQuery{RefHeight: 329, CollateralZat: 25125628141})
		}},
		{"yed_buildbundle", func() (interface{}, error) {
			return svc.BuildBundle(ctx, &walletrpc.YedBundleQuery{RefHeight: 329, SelectorHex: ""})
		}},
		{"yed_getselection", func() (interface{}, error) {
			return svc.GetSelection(ctx, &walletrpc.YedBundleQuery{RefHeight: 329, SelectorHex: "ab"})
		}},
	}
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			got, err := c.call()
			if err != nil {
				t.Fatalf("%v", err)
			}
			example := node.contract.returns(t, c.method)
			assertMatchesContract(t, c.method, got, example)
			assertNonZeroFieldsPresent(t, c.method, got, example)
			if last := node.calls[len(node.calls)-1]; last != c.method {
				t.Errorf("called %s, want %s", last, c.method)
			}
		})
	}
	if info, err := svc.GetYellowbackInfo(ctx, &walletrpc.Empty{}); err != nil || info.ServerVersion != common.ServerVersion {
		t.Errorf("serverVersion = %q, want %q", info.GetServerVersion(), common.ServerVersion)
	}
}

// TestStreamingMethodsMatchContract does the same for the four streams.
func TestStreamingMethodsMatchContract(t *testing.T) {
	svc, node := newService(t)
	check := func(method string, rows []interface{}) {
		t.Helper()
		var examples []json.RawMessage
		if err := json.Unmarshal(node.contract.returns(t, method), &examples); err != nil {
			t.Fatalf("%s: contract example is not an array", method)
		}
		if len(rows) != len(examples) {
			t.Fatalf("%s: streamed %d rows, contract has %d", method, len(rows), len(examples))
		}
		for i := range rows {
			assertMatchesContract(t, method, rows[i], examples[i])
			assertNonZeroFieldsPresent(t, method, rows[i], examples[i])
		}
	}
	vs := &vaultStream{}
	if err := svc.ListVaults(&walletrpc.YedVaultFilter{Status: "ACTIVE", Count: 10}, vs); err != nil {
		t.Fatal(err)
	}
	check("yed_listvaults", vs.rows)
	if p := node.params["yed_listvaults"]; len(p) != 2 || string(p[0]) != `"ACTIVE"` || string(p[1]) != "10" {
		t.Errorf("yed_listvaults params = %s", p)
	}
	cs := &claimableStream{}
	if err := svc.ListClaimable(&walletrpc.Empty{}, cs); err != nil {
		t.Fatal(err)
	}
	check("yed_listclaimable", cs.rows)
	as := &attestationStream{}
	if err := svc.GetAttestations(&walletrpc.Empty{}, as); err != nil {
		t.Fatal(err)
	}
	check("yed_getattestations", as.rows)
	ls := &attestorStream{}
	if err := svc.ListAttestors(&walletrpc.HeightFilter{}, ls); err != nil {
		t.Fatal(err)
	}
	check("yed_listattestors", ls.rows)
	if p := node.params["yed_listattestors"]; len(p) != 0 {
		t.Errorf("yed_listattestors with height 0 must send no params, sent %s", p)
	}
	ts := &tokenStream{}
	if err := svc.GetAddressTokens(&walletrpc.YedAddressList{Addresses: []string{"yr9L2hEfNDjpRnbHcY14f9b3coWbKdBD9ij", "smFv3B1FhYXS2ZU1e54x9sdLfq7e46YhxVT"}, MinHeight: 300}, ts); err != nil {
		t.Fatal(err)
	}
	check("yed_listtokens", ts.rows)
	if p := node.params["yed_listtokens"]; len(p) != 2 || string(p[0]) != `["yr9L2hEfNDjpRnbHcY14f9b3coWbKdBD9ij","smFv3B1FhYXS2ZU1e54x9sdLfq7e46YhxVT"]` || string(p[1]) != "300" {
		t.Errorf("yed_listtokens params = %s", p)
	}
}

// TestParamsEncoding: strings are JSON-quoted (never spliced), numbers are numbers, optionals
// are omitted when zero.
func TestParamsEncoding(t *testing.T) {
	svc, node := newService(t)
	ctx := context.Background()
	_, _ = svc.GetTxInfo(ctx, &walletrpc.YedTxid{Txid: exampleTxid})
	if p := node.params["yed_gettxinfo"]; len(p) != 1 || string(p[0]) != `"`+exampleTxid+`"` {
		t.Errorf("yed_gettxinfo params = %s", p)
	}
	_, _ = svc.GetPrice(ctx, &walletrpc.HeightFilter{})
	if p := node.params["yed_getprice"]; len(p) != 0 {
		t.Errorf("yed_getprice at the tip must send no params, sent %s", p)
	}
	_, _ = svc.EstimateCollateral(ctx, &walletrpc.YedMintQuery{Cents: 5, LockBlocks: 6, PriceMicroUsd: 7})
	if p := node.params["yed_estimatecollateral"]; len(p) != 3 || string(p[0]) != "5" || string(p[2]) != "7" {
		t.Errorf("yed_estimatecollateral params = %s", p)
	}
	_, _ = svc.ValidateRawTransaction(ctx, &walletrpc.RawTransaction{Data: []byte{0xde, 0xad}})
	if p := node.params["yed_validaterawtransaction"]; len(p) != 1 || string(p[0]) != `"dead"` {
		t.Errorf("yed_validaterawtransaction params = %s", p)
	}
}

// TestNodeErrorsAreFailedPrecondition: the node's error identifier reaches the client verbatim.
func TestNodeErrorsAreFailedPrecondition(t *testing.T) {
	svc, node := newService(t)
	node.fail["yed_buildbundle"] = errors.New("-8: bundle-insufficient: 2 of 3 selected attestors fresh") // btcjson.RPCError.Error()'s form
	_, err := svc.BuildBundle(context.Background(), &walletrpc.YedBundleQuery{RefHeight: 329})
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition || !strings.HasPrefix(st.Message(), "bundle-insufficient") {
		t.Fatalf("got %v, want FAILED_PRECONDITION bundle-insufficient…", err)
	}
	node.fail["yed_getvault"] = errors.New("-5: vault-not-found")
	_, err = svc.GetVault(context.Background(), &walletrpc.YedTxid{Txid: exampleTxid})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != "vault-not-found" {
		t.Fatalf("got %v, want FAILED_PRECONDITION vault-not-found", err)
	}
	// A transport failure is UNAVAILABLE.
	node.fail["yed_getstats"] = errors.New("connection refused")
	_, err = svc.GetStats(context.Background(), &walletrpc.Empty{})
	if st, _ := status.FromError(err); st.Code() != codes.Unavailable {
		t.Fatalf("got %v, want UNAVAILABLE", err)
	}
}

// TestInputValidation: malformed input never reaches the node.
func TestInputValidation(t *testing.T) {
	svc, node := newService(t)
	ctx := context.Background()
	bad := []func() error{
		func() error { _, err := svc.GetTxInfo(ctx, &walletrpc.YedTxid{Txid: "not-hex"}); return err },
		func() error { _, err := svc.GetVault(ctx, nil); return err },
		func() error { _, err := svc.DecodePayload(ctx, &walletrpc.YedHex{Hex: "abc"}); return err },
		func() error {
			_, err := svc.EstimateCollateral(ctx, &walletrpc.YedMintQuery{Cents: 0, LockBlocks: 1})
			return err
		},
		func() error { _, err := svc.EstimateFee(ctx, &walletrpc.YedFeeQuery{CollateralZat: -1}); return err },
		func() error {
			_, err := svc.BuildBundle(ctx, &walletrpc.YedBundleQuery{RefHeight: 1, SelectorHex: strings.Repeat("ab", 37)})
			return err
		},
		func() error { _, err := svc.ValidateRawTransaction(ctx, &walletrpc.RawTransaction{}); return err },
		func() error {
			return svc.ListVaults(&walletrpc.YedVaultFilter{Count: maxListCount + 1}, &vaultStream{})
		},
		func() error { return svc.GetAddressTokens(&walletrpc.YedAddressList{}, &tokenStream{}) },
		func() error {
			return svc.GetAddressTokens(&walletrpc.YedAddressList{Addresses: []string{""}}, &tokenStream{})
		},
		func() error {
			return svc.GetAddressTokens(&walletrpc.YedAddressList{Addresses: make([]string, maxAddresses+1)}, &tokenStream{})
		},
	}
	for i, f := range bad {
		if st, _ := status.FromError(f()); st.Code() != codes.InvalidArgument {
			t.Errorf("case %d: want INVALID_ARGUMENT, got %v", i, st)
		}
	}
	if len(node.calls) != 0 {
		t.Errorf("the node was called for malformed input: %v", node.calls)
	}
}

// TestAllowListCoversContract: every yed_* method of the contract is either proxied or listed
// as not offered with a reason, and every proxied method exists in the contract.
func TestAllowListCoversContract(t *testing.T) {
	c := loadContract(t)
	for method := range c {
		if !strings.HasPrefix(method, "yed_") {
			continue
		}
		_, offered := common.YedMethods[method]
		_, declined := common.NotOffered[method]
		if offered == declined {
			t.Errorf("%s: must be in exactly one of YedMethods and NotOffered (offered=%v, declined=%v)", method, offered, declined)
		}
	}
	for method := range common.YedMethods {
		if _, ok := c[method]; !ok {
			t.Errorf("%s is allow-listed but not in the contract", method)
		}
	}
	for method := range common.NotOffered {
		if _, ok := c[method]; !ok {
			t.Errorf("%s is in NotOffered but not in the contract", method)
		}
	}
}

// TestAllowListIsTheOnlyPath: a method outside the list is refused before any node call.
func TestAllowListIsTheOnlyPath(t *testing.T) {
	node := newFakeNode(t)
	install(t, node)
	_, err := common.CallYed("yed_mint")
	if st, _ := status.FromError(err); st.Code() != codes.Internal || len(node.calls) != 0 {
		t.Fatalf("yed_mint must be refused without a node call: %v %v", err, node.calls)
	}
}

// TestProbe: stock node, wrong rpcversion, and the good case.
func TestProbe(t *testing.T) {
	node := newFakeNode(t)
	install(t, node)
	node.features = []string{"insightexplorer"}
	capability, err := common.ProbeYellowback()
	if err != nil || capability.Enabled {
		t.Fatalf("stock node: got enabled=%v err=%v", capability.Enabled, err)
	}
	node = newFakeNode(t)
	install(t, node)
	capability, err = common.ProbeYellowback()
	if err != nil || !capability.Enabled || capability.RPCVersion != common.KnownRPCVersion || capability.Network != "regtest" {
		t.Fatalf("yellowback node: got %+v err=%v", capability, err)
	}
	node = newFakeNode(t)
	install(t, node)
	node.contract["yed_getinfo"] = json.RawMessage(`{"returns": {"rpcversion": 4, "network": "regtest"}}`)
	if _, err := common.ProbeYellowback(); err == nil {
		t.Fatal("rpcversion 4 must be refused")
	}
	node = newFakeNode(t)
	install(t, node)
	node.fail["getexperimentalfeatures"] = errors.New("connection refused")
	if _, err := common.ProbeYellowback(); err == nil {
		t.Fatal("a transport failure must be an error")
	}
}

// TestYedToTransparent: known-answer vectors. The regtest pair is from a ycash-dd regtest node
// (yed_getnewaddress / yed_validateaddress, 2026-09-23); the mainnet and testnet pairs from an
// independent base58check implementation over the key hash 1f2e…d3c4 with the version bytes of
// ycash-dd/src/yellowback/params.cpp and ref/ycash/src/chainparams.cpp.
func TestYedToTransparent(t *testing.T) {
	vectors := map[string]string{
		"yr9L2hEfNDjpRnbHcY14f9b3coWbKdBD9ij": "smFv3B1FhYXS2ZU1e54x9sdLfq7e46YhxVT", // regtest, from the node
		"ye5AFsECVAfVbGza52vizHE2p1wpsg7hdWq": "s1Q3cZRF4QCVySdHFdrJJhMpiKhX62RnGoD", // mainnet
		"yt9zNL2da1yiFuAT6tcP9FcZM3bqtc53TR7": "smFtMtFjTns1UasUhJac3Z2VTvgbubcg77f", // testnet
		"yr9JMQV98U5PsozkfmWiYpzCQu5ZB2quPEn": "smFtMtFjTns1UasUhJac3Z2VTvgbubcg77f", // regtest, same hash
	}
	for yed, want := range vectors {
		got, ok := yedToTransparent(yed)
		if !ok || got != want {
			t.Errorf("%s -> %q (%v), want %s", yed, got, ok, want)
		}
	}
	for _, notYed := range []string{
		"smFv3B1FhYXS2ZU1e54x9sdLfq7e46YhxVT",                                    // already transparent: unchanged, false
		"yr9L2hEfNDjpRnbHcY14f9b3coWbKdBD9ii",                                    // bad checksum
		"ys1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq", // shielded
		"", "yr",
	} {
		if got, ok := yedToTransparent(notYed); ok {
			t.Errorf("%q must not convert, got %q", notYed, got)
		}
	}
}

// TestTaddrOfMapsYedAddresses: with the switch on, taddrOf hands the taddr RPCs the transparent
// form of a YED address and leaves transparent input alone; off, it changes nothing — so the
// baseline's checkTaddress sees exactly what it always saw.
func TestTaddrOfMapsYedAddresses(t *testing.T) {
	YellowbackAddresses = false
	if got := taddrOf("yr9L2hEfNDjpRnbHcY14f9b3coWbKdBD9ij"); got != "yr9L2hEfNDjpRnbHcY14f9b3coWbKdBD9ij" {
		t.Fatalf("switch off must not map: %q", got)
	}
	YellowbackAddresses = true
	defer func() { YellowbackAddresses = false }()
	if got := taddrOf("yr9L2hEfNDjpRnbHcY14f9b3coWbKdBD9ij"); got != "smFv3B1FhYXS2ZU1e54x9sdLfq7e46YhxVT" {
		t.Fatalf("mapping: %q", got)
	}
	if got := taddrOf("smFv3B1FhYXS2ZU1e54x9sdLfq7e46YhxVT"); got != "smFv3B1FhYXS2ZU1e54x9sdLfq7e46YhxVT" {
		t.Fatalf("transparent input must pass unchanged: %q", got)
	}
	if err := checkTaddress(taddrOf("yr9L2hEfNDjpRnbHcY14f9b3coWbKdBD9ij")); err != nil {
		t.Fatalf("the mapped address must satisfy the baseline regex: %v", err)
	}
}

// TestRateLimit: the node-work methods refuse a peer past its burst with RESOURCE_EXHAUSTED,
// before any node call; tokens come back with time; peers are independent.
func TestRateLimit(t *testing.T) {
	svc, node := newService(t)
	now := time.Unix(1700000000, 0)
	svc.limiter = newRateLimiter(3, time.Second)
	svc.limiter.now = func() time.Time { return now }
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5}})
	for i := 0; i < 3; i++ {
		if _, err := svc.EstimateCollateral(ctx, &walletrpc.YedMintQuery{Cents: 100000, LockBlocks: 48}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	calls := len(node.calls)
	_, err := svc.BuildBundle(ctx, &walletrpc.YedBundleQuery{RefHeight: 1})
	if st, _ := status.FromError(err); st.Code() != codes.ResourceExhausted || len(node.calls) != calls {
		t.Fatalf("4th call: want RESOURCE_EXHAUSTED without a node call, got %v (%d calls)", err, len(node.calls)-calls)
	}
	other := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-real-ip", "10.0.0.2"))
	if _, err := svc.EstimateCollateral(other, &walletrpc.YedMintQuery{Cents: 100000, LockBlocks: 48}); err != nil {
		t.Fatalf("another peer must not be limited: %v", err)
	}
	now = now.Add(time.Second)
	if _, err := svc.BuildBundle(ctx, &walletrpc.YedBundleQuery{RefHeight: 1}); err != nil {
		t.Fatalf("one second later one token is back: %v", err)
	}
	if _, err := svc.GetPrice(ctx, &walletrpc.HeightFilter{}); err != nil {
		t.Fatalf("read-only methods are not limited: %v", err)
	}
}
