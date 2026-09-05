package main

import (
	"errors"
	"sort"
)

var ErrNoHealthyBackend = errors.New("no healthy backend available")

// Balancer implements performance-based dynamic backend selection.
//
// Algorithm ("Weighted Least-Load with Threshold Cutover"):
//  1. Only consider backends currently marked healthy by the HealthChecker.
//  2. For every healthy backend compute a live load score:
//     score = activeRequests + (ewmaLatencyMs / 100.0)
//     i.e. every in-flight request counts as "1 unit" of load, and every
//     100ms of recent average latency counts as another unit. This makes
//     the score comparable across backends regardless of absolute request
//     volume, and reacts within milliseconds to real congestion instead of
//     waiting for a periodic health-check tick.
//  3. A backend is "over threshold" (overloaded) if its activeRequests
//     exceeds MaxActiveRequests OR its ewmaLatencyMs exceeds MaxLatencyMs.
//     Overloaded backends are only used if EVERY healthy backend is
//     overloaded (graceful degradation instead of hard failure).
//  4. Among the eligible set, pick the backend with the lowest score.
//
// This is deliberately NOT round robin: two consecutive calls can (and
// usually will) return the same backend if it is genuinely the least loaded
// one, and traffic automatically drains away from a backend the instant its
// score crosses the threshold -- no fixed rotation involved.
type Balancer struct {
	cfg      Config
	backends []*Backend
}

func NewBalancer(cfg Config, backends []*Backend) *Balancer {
	return &Balancer{cfg: cfg, backends: backends}
}

func (bl *Balancer) score(b *Backend) float64 {
	active := float64(b.Snapshot().ActiveRequests) // uses the live atomic counter under the hood
	lat := b.getLatency()
	return active + lat/100.0
}

func (bl *Balancer) overloaded(b *Backend) bool {
	active := b.Snapshot().ActiveRequests
	lat := b.getLatency()
	return active > int64(bl.cfg.MaxActiveRequests) || lat > bl.cfg.MaxLatencyMs
}

// Pick returns an ordered candidate list: [0] is the best choice, the rest
// are fallbacks tried in order if [0]'s request fails (see proxy retry
// logic). Ordering by ascending score means retries naturally degrade to
// the next-least-loaded healthy backend rather than a random one.
func (bl *Balancer) Pick() ([]*Backend, error) {
	var healthy []*Backend
	for _, b := range bl.backends {
		if b.IsHealthy() {
			healthy = append(healthy, b)
		}
	}
	if len(healthy) == 0 {
		return nil, ErrNoHealthyBackend
	}

	var underThreshold, overThreshold []*Backend
	for _, b := range healthy {
		if bl.overloaded(b) {
			overThreshold = append(overThreshold, b)
		} else {
			underThreshold = append(underThreshold, b)
		}
	}

	pool := underThreshold
	if len(pool) == 0 {
		// Every healthy backend is over threshold: degrade gracefully by
		// routing to the least-bad one instead of rejecting the request.
		pool = overThreshold
	}

	sort.Slice(pool, func(i, j int) bool { return bl.score(pool[i]) < bl.score(pool[j]) })

	// Append the rest of the healthy set (also score-sorted) as retry fallbacks
	// in case the top pick fails mid-flight (connection error, timeout, 5xx).
	rest := make([]*Backend, 0, len(healthy))
	for _, b := range healthy {
		found := false
		for _, p := range pool {
			if p == b {
				found = true
				break
			}
		}
		if !found {
			rest = append(rest, b)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return bl.score(rest[i]) < bl.score(rest[j]) })

	return append(pool, rest...), nil
}
