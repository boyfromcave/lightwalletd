// Copyright (c) 2026 The Ycash developers
// Distributed under the MIT software license, see the accompanying
// file COPYING or https://www.opensource.org/licenses/mit-license.php .

// Per-peer rate limiting for every Yellowback method (plan Phase L3; audit E-2, E-3).
// A plain token bucket per peer IP, no new dependency: the vendored tree has no
// golang.org/x/time/rate and D-L-6 forbids adding one. The baseline limits nothing but
// GetBlockRange (a per-IP latency cache, service.go). Every YellowbackStreamer method takes
// tokens from its peer's bucket before the node is called: the methods that make the node walk
// its index or attestation pool cost a whole token, the cheap index reads a fraction. A refused
// call is RESOURCE_EXHAUSTED and never reaches the node.
package frontend

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Defaults measured on the regtest devnet (docs/yellowback.md, L3): a phone's mint flow makes
// at most a handful of the node-work calls per block.
const (
	yedRateBurst     = 20          // whole-token calls a peer may make at once
	yedRateInterval  = time.Second // one token refilled per interval
	yedRateIdleSweep = 10 * time.Minute
	// yedRateMaxPeers bounds the limiter's map (audit E-2). A bucket that has been idle for
	// burst*interval is full again and carries no information, so dropping it is lossless;
	// past the bound such buckets are swept first and, if the map is still full, the least
	// recently seen peer is evicted.
	yedRateMaxPeers = 10000
)

// Costs in tokens. A light call is an index read the node answers from memory under one lock;
// a heavy call walks the index, the attestation pool or the address scan.
const (
	yedCostLight = 0.2
	yedCostHeavy = 1
)

type tokenBucket struct {
	tokens   float64
	lastSeen time.Time
}

type rateLimiter struct {
	mu       sync.Mutex
	burst    float64
	interval time.Duration
	peers    map[string]*tokenBucket
	lastSwep time.Time
	maxPeers int
	trusted  []*net.IPNet // reverse proxies whose x-real-ip / x-forwarded-for is believed
	now      func() time.Time
}

func newRateLimiter(burst int, interval time.Duration) *rateLimiter {
	return &rateLimiter{burst: float64(burst), interval: interval, peers: map[string]*tokenBucket{},
		maxPeers: yedRateMaxPeers, now: time.Now}
}

// allow takes cost tokens for key; false when the bucket holds fewer.
func (r *rateLimiter) allow(key string, cost float64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if now.Sub(r.lastSwep) > yedRateIdleSweep { // forget peers idle for a sweep interval
		r.sweep(now, yedRateIdleSweep)
		r.lastSwep = now
	}
	b, ok := r.peers[key]
	if !ok {
		if len(r.peers) >= r.maxPeers {
			r.evict(now)
		}
		b = &tokenBucket{tokens: r.burst, lastSeen: now}
		r.peers[key] = b
	} else {
		b.tokens += float64(now.Sub(b.lastSeen)) / float64(r.interval)
		if b.tokens > r.burst {
			b.tokens = r.burst
		}
		b.lastSeen = now
	}
	if b.tokens < cost {
		return false
	}
	b.tokens -= cost
	return true
}

// sweep drops every bucket idle for longer than idle. Caller holds mu.
func (r *rateLimiter) sweep(now time.Time, idle time.Duration) {
	for k, v := range r.peers {
		if now.Sub(v.lastSeen) > idle {
			delete(r.peers, k)
		}
	}
}

// evict makes room for one more peer: first the buckets that have refilled completely (lossless),
// then, if every peer is live, the least recently seen one. Caller holds mu.
func (r *rateLimiter) evict(now time.Time) {
	full := time.Duration(r.burst) * r.interval
	r.sweep(now, full)
	if len(r.peers) < r.maxPeers {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, v := range r.peers {
		if oldestKey == "" || v.lastSeen.Before(oldest) {
			oldestKey, oldest = k, v.lastSeen
		}
	}
	delete(r.peers, oldestKey)
}

// peerKey identifies the caller for the limiter: the connection's address, unless that address
// is one of the trusted reverse proxies (--trusted-proxy-cidr), in which case the proxy's
// x-real-ip, else the first x-forwarded-for entry, is believed. gRPC metadata is set by the
// client, so without a trusted proxy a forged header must not create a fresh bucket (audit E-2).
func (r *rateLimiter) peerKey(ctx context.Context) string {
	addr := "unknown"
	var ip net.IP
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		addr = p.Addr.String()
		if host, _, err := net.SplitHostPort(addr); err == nil {
			addr = host
		}
		ip = net.ParseIP(addr)
	}
	if ip == nil || !r.trustedProxy(ip) {
		return addr
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return addr
	}
	if v := md.Get("x-real-ip"); len(v) > 0 && v[0] != "" {
		return v[0]
	}
	if v := md.Get("x-forwarded-for"); len(v) > 0 {
		first := v[0]
		if i := strings.IndexByte(first, ','); i >= 0 {
			first = first[:i]
		}
		if first = strings.TrimSpace(first); first != "" {
			return first
		}
	}
	return addr
}

func (r *rateLimiter) trustedProxy(ip net.IP) bool {
	for _, n := range r.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// TrustProxies sets the reverse-proxy networks whose forwarded-IP headers the limiter believes
// (cmd/root.go --trusted-proxy-cidr). Empty means none: the connection address is the peer.
func (y *YellowbackStreamer) TrustProxies(cidrs []string) error {
	var nets []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return err
		}
		nets = append(nets, n)
	}
	y.limiter.trusted = nets
	return nil
}

// limited is the gate every handler calls first; cost is yedCostLight or yedCostHeavy.
func (y *YellowbackStreamer) limited(ctx context.Context, cost float64) error {
	if y.limiter == nil || y.limiter.allow(y.limiter.peerKey(ctx), cost) {
		return nil
	}
	return status.Errorf(codes.ResourceExhausted, "rate limited: at most %d calls per burst, one more per %s", yedRateBurst, yedRateInterval)
}
