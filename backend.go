package main

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Backend represents one upstream chat-server instance.
// All fields that are mutated concurrently (by the health-checker
// goroutine and by request-handling goroutines at the same time)
// are either atomic or guarded by mu.
type Backend struct {
	Name string
	URL  *url.URL

	// --- health state (guarded by mu) ---
	mu              sync.RWMutex
	healthy         bool
	consecutiveOK   int
	consecutiveFail int
	lastCheckedAt   time.Time
	lastErr         string

	// --- live performance metrics (atomic / lock-free) ---
	activeRequests int64  // in-flight requests routed to this backend right now
	ewmaLatencyMs  uint64 // math.Float64bits-encoded EWMA of response latency
	totalRequests  uint64
	totalErrors    uint64
}

func NewBackend(name, rawURL string) (*Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	b := &Backend{Name: name, URL: u, healthy: true} // optimistic start; health checker corrects fast
	b.setLatency(0)
	return b, nil
}

// ---- health state ----

func (b *Backend) IsHealthy() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.healthy
}

func (b *Backend) recordHealthCheck(ok bool, errMsg string, unhealthyThreshold, healthyThreshold int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastCheckedAt = time.Now()
	b.lastErr = errMsg
	if ok {
		b.consecutiveOK++
		b.consecutiveFail = 0
		if !b.healthy && b.consecutiveOK >= healthyThreshold {
			b.healthy = true
		}
	} else {
		b.consecutiveFail++
		b.consecutiveOK = 0
		if b.healthy && b.consecutiveFail >= unhealthyThreshold {
			b.healthy = false
		}
	}
}

func (b *Backend) Snapshot() BackendStatus {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return BackendStatus{
		Name:            b.Name,
		URL:             b.URL.String(),
		Healthy:         b.healthy,
		ActiveRequests:  atomic.LoadInt64(&b.activeRequests),
		EwmaLatencyMs:   b.getLatency(),
		TotalRequests:   atomic.LoadUint64(&b.totalRequests),
		TotalErrors:     atomic.LoadUint64(&b.totalErrors),
		LastCheckedAt:   b.lastCheckedAt,
		LastError:       b.lastErr,
		ConsecutiveOK:   b.consecutiveOK,
		ConsecutiveFail: b.consecutiveFail,
	}
}

type BackendStatus struct {
	Name            string    `json:"name"`
	URL             string    `json:"url"`
	Healthy         bool      `json:"healthy"`
	ActiveRequests  int64     `json:"active_requests"`
	EwmaLatencyMs   float64   `json:"ewma_latency_ms"`
	TotalRequests   uint64    `json:"total_requests"`
	TotalErrors     uint64    `json:"total_errors"`
	LastCheckedAt   time.Time `json:"last_checked_at"`
	LastError       string    `json:"last_error,omitempty"`
	ConsecutiveOK   int       `json:"consecutive_ok"`
	ConsecutiveFail int       `json:"consecutive_fail"`
}

// ---- live metrics ----

func (b *Backend) begin() int64 { return atomic.AddInt64(&b.activeRequests, 1) }
func (b *Backend) end()         { atomic.AddInt64(&b.activeRequests, -1) }

func (b *Backend) recordResult(latency time.Duration, isErr bool, alpha float64) {
	atomic.AddUint64(&b.totalRequests, 1)
	if isErr {
		atomic.AddUint64(&b.totalErrors, 1)
	}
	b.updateLatency(float64(latency.Milliseconds()), alpha)
}

// EWMA latency stored as bits behind an atomic uint64 so reads/writes
// never need a mutex (float64 has no native atomic type in Go).
func (b *Backend) getLatency() float64 {
	return float64FromBits(atomic.LoadUint64(&b.ewmaLatencyMs))
}
func (b *Backend) setLatency(v float64) {
	atomic.StoreUint64(&b.ewmaLatencyMs, float64ToBits(v))
}
func (b *Backend) updateLatency(sampleMs, alpha float64) {
	for {
		old := atomic.LoadUint64(&b.ewmaLatencyMs)
		oldV := float64FromBits(old)
		var newV float64
		if oldV == 0 {
			newV = sampleMs
		} else {
			newV = alpha*sampleMs + (1-alpha)*oldV
		}
		if atomic.CompareAndSwapUint64(&b.ewmaLatencyMs, old, float64ToBits(newV)) {
			return
		}
	}
}

func (b *Backend) healthCheckRequest(ctx context.Context, path string) (*http.Request, error) {
	u := *b.URL
	u.Path = joinPath(u.Path, path)
	return http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
}
