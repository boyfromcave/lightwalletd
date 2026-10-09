// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

// The tip-hash-keyed cache for Yellowback reads (ycash-infra plan K3b; docs/yellowback.md
// "The read cache"). CallYed remembers what the node answered for (method, arguments, chain
// tip) so that repeated reads between blocks never reach the node; it never computes an answer
// (plan D-L-2 "proxy, not indexer", contract rule 3: the verdict is the node's). A new block
// changes the key, so every old entry goes cold and the LRU evicts it; nothing is invalidated
// by hand. The tip is the ingestor's (BlockCache.GetLatestHash), learned for free — never a
// getbestblockhash per request — so an entry can be at most one ingestor poll (two seconds at
// the tip) staler than the node, the same bound the frontend's two-second answers have.
package common

import (
	"bytes"
	"container/list"
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// YedCacheable is the allow-list of methods whose answer is a pure function of (arguments,
// chain tip): the index, the vault primitive's state and the chain itself, nothing node-local.
// A method whose answer also depends on this node's mempool, attestation pool, wallet or clock
// is left out and reaches the node every time; the reasons are tabled in docs/yellowback.md.
// Only successful answers are remembered: an error (vault-not-found, tx-not-found, the index
// unhealthy) is the node's to repeat.
var YedCacheable = map[string]bool{
	"yed_getinfo":            true,
	"yed_getprice":           true,
	"yed_getstats":           true,
	"yed_getactivation":      true,
	"yed_gettxinfo":          true,
	"yed_decodepayload":      true,
	"yed_getvault":           true,
	"yed_listvaults":         true,
	"yed_listclaimable":      true,
	"yed_getnotice":          true,
	"yed_estimatecollateral": true,
	"yed_estimatefee":        true,
	"yed_getfeepayee":        true,
	"yed_listtokens":         true,
	"getblockchaininfo":      true,
	"vault_getinfo":          true,
	"set_list":               true,
	"set_getinfo":            true,
	"vault_list":             true,
}

// YedTipHash returns the hash of the newest block this server knows (the ingestor's), or nil
// when it knows none yet; main assigns it from the BlockCache. With no tip there is no key, and
// CallYed forwards to the node as if the cache were off.
var YedTipHash = func() []byte { return nil }

// DefaultYedCacheEntries / DefaultYedCacheBytes bound the in-process cache
// (--yellowback-cache-entries, --yellowback-cache-bytes). 10 000 entries is a block's worth of
// distinct reads on a busy server; 64 MiB holds that many yed_listtokens pages of a few kB.
const (
	DefaultYedCacheEntries = 10000
	DefaultYedCacheBytes   = 64 << 20
)

// yedCacheStore is the backend behind CallYed: in-process (yedLRU) today.
//
// TODO(K3b, Redis): a second implementation over the chart's Redis would share entries across
// replicas (the same key, a TTL of a few block intervals in place of the LRU). It is not written
// because no Go Redis client is vendored (vendor/modules.txt) and the lightwalletd plan's D-L-6
// says no dependency changes; adding one is `go get github.com/redis/go-redis/v9 && go mod
// vendor` (about 1.5 MB of vendored code) and a plan decision first. The flag would be
// --yellowback-cache-redis addr.
type yedCacheStore interface {
	get(key string) (json.RawMessage, bool)
	put(key string, raw json.RawMessage)
}

// yedLRU is a bounded (entries and bytes) least-recently-used map. Bytes count the stored
// answers only; the keys are small.
type yedLRU struct {
	mu         sync.Mutex
	maxEntries int
	maxBytes   int64
	bytes      int64
	order      *list.List // front = most recently used
	items      map[string]*list.Element
}

type yedLRUEntry struct {
	key string
	raw json.RawMessage
}

func newYedLRU(maxEntries int, maxBytes int64) *yedLRU {
	return &yedLRU{maxEntries: maxEntries, maxBytes: maxBytes, order: list.New(), items: map[string]*list.Element{}}
}

func (c *yedLRU) get(key string) (json.RawMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(e)
	return e.Value.(*yedLRUEntry).raw, true
}

func (c *yedLRU) put(key string, raw json.RawMessage) {
	if int64(len(raw)) > c.maxBytes {
		return // larger than the whole budget: not worth evicting everything for
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.bytes -= int64(len(e.Value.(*yedLRUEntry).raw))
		e.Value.(*yedLRUEntry).raw = raw
		c.bytes += int64(len(raw))
		c.order.MoveToFront(e)
	} else {
		c.items[key] = c.order.PushFront(&yedLRUEntry{key: key, raw: raw})
		c.bytes += int64(len(raw))
	}
	for c.order.Len() > c.maxEntries || c.bytes > c.maxBytes {
		oldest := c.order.Back()
		ent := oldest.Value.(*yedLRUEntry)
		c.order.Remove(oldest)
		delete(c.items, ent.key)
		c.bytes -= int64(len(ent.raw))
	}
}

func (c *yedLRU) stats() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len(), c.bytes
}

// yedCache is the process's store; nil means off (the default until main configures it, so
// the tests see the old behaviour unless they ask for the cache).
var (
	yedCacheMu sync.RWMutex
	yedCache   *yedLRU
)

// SetYedCache sizes the in-process cache; called once at startup, before any request.
// entries <= 0 turns the cache off. Any earlier contents are dropped.
func SetYedCache(entries int, bytes int64) {
	yedCacheMu.Lock()
	defer yedCacheMu.Unlock()
	if entries <= 0 || bytes <= 0 {
		yedCache = nil
		return
	}
	yedCache = newYedLRU(entries, bytes)
}

// YedCacheStats reports the cache's fill (entries, bytes) and whether it is on; for the tests
// and the probe tool.
func YedCacheStats() (on bool, entries int, bytes int64) {
	yedCacheMu.RLock()
	defer yedCacheMu.RUnlock()
	if yedCache == nil {
		return false, 0, 0
	}
	entries, bytes = yedCache.stats()
	return true, entries, bytes
}

// yedCacheRequests counts every CallYed by outcome: hit (served from the cache), miss (the
// node answered and the answer was stored) or bypass (the cache is off, the method is not
// cacheable, or no tip is known yet). Registered on the default registry promhttp serves.
var yedCacheRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "lightwalletd_yellowback_cache_requests_total",
	Help: "Yellowback node reads by cache outcome (hit, miss, bypass) per node RPC method.",
}, []string{"method", "result"})

func init() {
	prometheus.MustRegister(yedCacheRequests)
}

// yedCacheKey is method, tip and the canonical (compacted) arguments, or "" when the call is
// not cacheable now. The arguments are this server's own encodings (JSONString, JSONNumber,
// json.Marshal), so compacting makes equal values equal keys.
func yedCacheKey(method string, params []json.RawMessage) string {
	if !YedCacheable[method] {
		return ""
	}
	tip := YedTipHash()
	if len(tip) == 0 {
		return ""
	}
	key := method + "\x00" + hex.EncodeToString(tip)
	for _, p := range params {
		var buf []byte
		if compact, err := compactJSON(p); err == nil {
			buf = compact
		} else {
			buf = p
		}
		key += "\x00" + string(buf)
	}
	return key
}

func compactJSON(raw json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// yedCacheLookup returns the cached answer for key, counting the outcome; key "" is a bypass.
func yedCacheLookup(method, key string) (json.RawMessage, bool) {
	yedCacheMu.RLock()
	c := yedCache
	yedCacheMu.RUnlock()
	if c == nil || key == "" {
		yedCacheRequests.WithLabelValues(method, "bypass").Inc()
		return nil, false
	}
	if raw, ok := c.get(key); ok {
		yedCacheRequests.WithLabelValues(method, "hit").Inc()
		out := make(json.RawMessage, len(raw))
		copy(out, raw)
		return out, true
	}
	yedCacheRequests.WithLabelValues(method, "miss").Inc()
	return nil, false
}

// yedCacheRemember remembers a successful answer under key (no-op for "" or when off).
func yedCacheRemember(key string, raw json.RawMessage) {
	if key == "" {
		return
	}
	yedCacheMu.RLock()
	c := yedCache
	yedCacheMu.RUnlock()
	if c == nil {
		return
	}
	kept := make(json.RawMessage, len(raw))
	copy(kept, raw)
	c.put(key, kept)
}
