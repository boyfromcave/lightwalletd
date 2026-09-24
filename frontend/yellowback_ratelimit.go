// Per-peer rate limiting for the Yellowback methods that make the node do work (plan Phase L3).
// A plain token bucket per peer IP, no new dependency: the vendored tree has no
// golang.org/x/time/rate and D-L-6 forbids adding one. The baseline limits nothing but
// GetBlockRange (a per-IP latency cache, service.go); this covers the four methods that reach
// into the node's index or attestation pool per call. A refused call is RESOURCE_EXHAUSTED and
// never reaches the node.
package frontend

import (
	"context"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Defaults measured on the regtest devnet (docs/yellowback.md, L3): a phone's mint flow makes
// at most a handful of these calls per block.
const (
	yedRateBurst     = 20          // calls a peer may make at once
	yedRateInterval  = time.Second // one token refilled per interval
	yedRateIdleSweep = 10 * time.Minute
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
	now      func() time.Time
}

func newRateLimiter(burst int, interval time.Duration) *rateLimiter {
	return &rateLimiter{burst: float64(burst), interval: interval, peers: map[string]*tokenBucket{}, now: time.Now}
}

// allow takes one token for key; false when the bucket is empty.
func (r *rateLimiter) allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b, ok := r.peers[key]
	if !ok {
		b = &tokenBucket{tokens: r.burst, lastSeen: now}
		r.peers[key] = b
	} else {
		b.tokens += float64(now.Sub(b.lastSeen)) / float64(r.interval)
		if b.tokens > r.burst {
			b.tokens = r.burst
		}
		b.lastSeen = now
	}
	if now.Sub(r.lastSwep) > yedRateIdleSweep { // forget peers idle for a sweep interval
		for k, v := range r.peers {
			if now.Sub(v.lastSeen) > yedRateIdleSweep {
				delete(r.peers, k)
			}
		}
		r.lastSwep = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// peerKey mirrors the baseline's peer identification (service.go peerIPFromContext): the
// x-real-ip metadata a reverse proxy sets, else the connection's address.
func peerKey(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("x-real-ip"); len(v) > 0 && v[0] != "" {
			return v[0]
		}
	}
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		if host, _, err := net.SplitHostPort(p.Addr.String()); err == nil {
			return host
		}
		return p.Addr.String()
	}
	return "unknown"
}

// limited is the gate the node-work handlers call first.
func (y *YellowbackStreamer) limited(ctx context.Context) error {
	if y.limiter == nil || y.limiter.allow(peerKey(ctx)) {
		return nil
	}
	return status.Errorf(codes.ResourceExhausted, "rate limited: at most %d calls per burst, one more per %s", yedRateBurst, yedRateInterval)
}
