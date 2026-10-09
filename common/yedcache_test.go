// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

package common

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// cacheStub is a RawRequest that counts its calls and answers with the call number, so a
// served-from-cache answer is told apart from a fresh one by its bytes.
type cacheStub struct {
	calls int
	fail  error
}

func (s *cacheStub) RawRequest(method string, params []json.RawMessage) (json.RawMessage, error) {
	s.calls++
	if s.fail != nil {
		return nil, s.fail
	}
	return json.RawMessage(fmt.Sprintf(`{"method":%q,"call":%d}`, method, s.calls)), nil
}

func withCache(t *testing.T, entries int, bytes int64) *cacheStub {
	t.Helper()
	stub := &cacheStub{}
	previous, previousTip := RawRequest, YedTipHash
	RawRequest = stub.RawRequest
	YedTipHash = func() []byte { return []byte{0xaa} }
	SetYedCache(entries, bytes)
	t.Cleanup(func() { RawRequest, YedTipHash = previous, previousTip; SetYedCache(0, 0) })
	return stub
}

func TestYedCacheServesRepeatAtTheSameTip(t *testing.T) {
	stub := withCache(t, 100, 1<<20)
	ctx := context.Background()
	first, err := CallYed(ctx, "yed_listtokens", JSONString("ye1"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := CallYed(ctx, "yed_listtokens", JSONString("ye1"))
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 || string(first) != string(second) {
		t.Fatalf("a repeat at the same tip must be served from the cache: calls=%d first=%s second=%s", stub.calls, first, second)
	}
	// Different arguments are a different question.
	if _, err := CallYed(ctx, "yed_listtokens", JSONString("ye2")); err != nil || stub.calls != 2 {
		t.Fatalf("other arguments must reach the node: calls=%d err=%v", stub.calls, err)
	}
	// Equal arguments spelled with whitespace are the same question (canonical JSON).
	if _, err := CallYed(ctx, "yed_listtokens", json.RawMessage(`  "ye1" `)); err != nil || stub.calls != 2 {
		t.Fatalf("whitespace must not change the key: calls=%d err=%v", stub.calls, err)
	}
	// A result handed out is a copy: mutating it must not poison the cache.
	second[0] = 'X'
	third, _ := CallYed(ctx, "yed_listtokens", JSONString("ye1"))
	if string(third) != string(first) {
		t.Fatalf("the cache must hand out copies: %s", third)
	}
	if on, entries, bytes := YedCacheStats(); !on || entries != 2 || bytes != int64(len(first)*2) {
		t.Fatalf("stats: on=%v entries=%d bytes=%d", on, entries, bytes)
	}
}

func TestYedCacheMissesOnANewTip(t *testing.T) {
	stub := withCache(t, 100, 1<<20)
	ctx := context.Background()
	tip := []byte{1}
	YedTipHash = func() []byte { return tip }
	for i := 0; i < 3; i++ {
		if _, err := CallYed(ctx, "yed_getprice"); err != nil {
			t.Fatal(err)
		}
	}
	if stub.calls != 1 {
		t.Fatalf("three reads at one tip are one node call, got %d", stub.calls)
	}
	tip = []byte{2}
	raw, err := CallYed(ctx, "yed_getprice")
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 2 || !strings.Contains(string(raw), `"call":2`) {
		t.Fatalf("a new tip must reach the node: calls=%d raw=%s", stub.calls, raw)
	}
	// No tip known (the ingestor has not started): every call reaches the node.
	tip = nil
	CallYed(ctx, "yed_getprice")
	CallYed(ctx, "yed_getprice")
	if stub.calls != 4 {
		t.Fatalf("without a tip there is no key: calls=%d", stub.calls)
	}
}

func TestYedCacheBypassesNonCacheableMethods(t *testing.T) {
	stub := withCache(t, 100, 1<<20)
	ctx := context.Background()
	for _, method := range []string{"yed_validaterawtransaction", "yed_buildbundle", "yed_getselection", "yed_getattestations", "yed_listattestors"} {
		if YedCacheable[method] {
			t.Fatalf("%s must not be cacheable (mempool / attestation pool)", method)
		}
		before := stub.calls
		CallYed(ctx, method, JSONString("x"))
		CallYed(ctx, method, JSONString("x"))
		if stub.calls != before+2 {
			t.Fatalf("%s must reach the node every time", method)
		}
	}
}

func TestYedCacheAllowListIsWithinTheOffered(t *testing.T) {
	for method := range YedCacheable {
		if !YedMethods[method] && !StockMethods[method] && !VaultMethods[method] {
			t.Fatalf("%s is cacheable but not an offered method", method)
		}
	}
}

func TestYedCacheOffIsTheOldBehaviour(t *testing.T) {
	stub := withCache(t, 0, 0)
	ctx := context.Background()
	if on, _, _ := YedCacheStats(); on {
		t.Fatal("0 entries must mean off")
	}
	CallYed(ctx, "yed_getprice")
	CallYed(ctx, "yed_getprice")
	if stub.calls != 2 {
		t.Fatalf("with the cache off every call reaches the node: %d", stub.calls)
	}
}

func TestYedCacheDoesNotRememberErrors(t *testing.T) {
	stub := withCache(t, 100, 1<<20)
	ctx := context.Background()
	stub.fail = errors.New("-8: vault-not-found: x")
	if _, err := CallYed(ctx, "yed_getvault", JSONString("t")); err == nil {
		t.Fatal("expected the node's error")
	}
	stub.fail = nil
	if _, err := CallYed(ctx, "yed_getvault", JSONString("t")); err != nil || stub.calls != 2 {
		t.Fatalf("an error is never cached: calls=%d err=%v", stub.calls, err)
	}
}

func TestYedCacheEvictsAtTheBounds(t *testing.T) {
	stub := withCache(t, 3, 1<<20)
	ctx := context.Background()
	for _, a := range []string{"a", "b", "c", "d"} {
		CallYed(ctx, "yed_getvault", JSONString(a))
	}
	if _, entries, _ := YedCacheStats(); entries != 3 {
		t.Fatalf("entries bound: %d", entries)
	}
	before := stub.calls
	CallYed(ctx, "yed_getvault", JSONString("a")) // the oldest, evicted
	CallYed(ctx, "yed_getvault", JSONString("d")) // still there
	if stub.calls != before+1 {
		t.Fatalf("the oldest entry must be evicted and the newest kept: calls=%d", stub.calls-before)
	}
	// The byte budget: answers are ~35 bytes; a 100-byte budget holds two.
	SetYedCache(100, 100)
	for _, a := range []string{"a", "b", "c"} {
		CallYed(ctx, "yed_getvault", JSONString(a))
	}
	if _, entries, bytes := YedCacheStats(); entries != 2 || bytes > 100 {
		t.Fatalf("byte bound: entries=%d bytes=%d", entries, bytes)
	}
	// An answer larger than the whole budget is passed through, not stored.
	SetYedCache(100, 10)
	CallYed(ctx, "yed_getvault", JSONString("big"))
	if _, entries, _ := YedCacheStats(); entries != 0 {
		t.Fatalf("an oversized answer is not stored: entries=%d", entries)
	}
}
