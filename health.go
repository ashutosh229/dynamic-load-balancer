package main

import (
	"context"
	"log"
	"net/http"
	"time"
)

// HealthChecker polls every backend's health endpoint on a fixed cadence,
// independent of live traffic. This is what lets the LB detect a backend
// that is down (process crashed, container stopped, network partition)
// even if no client happens to be sending requests to it right now.
type HealthChecker struct {
	cfg      Config
	backends []*Backend
	client   *http.Client
}

func NewHealthChecker(cfg Config, backends []*Backend) *HealthChecker {
	return &HealthChecker{
		cfg:      cfg,
		backends: backends,
		client:   &http.Client{Timeout: cfg.healthTimeout()},
	}
}

func (h *HealthChecker) Run(ctx context.Context) {
	ticker := time.NewTicker(h.cfg.healthInterval())
	defer ticker.Stop()

	// Check once immediately at startup so we don't route traffic blind.
	h.checkAll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.checkAll(ctx)
		}
	}
}

func (h *HealthChecker) checkAll(ctx context.Context) {
	for _, b := range h.backends {
		go h.checkOne(ctx, b)
	}
}

func (h *HealthChecker) checkOne(ctx context.Context, b *Backend) {
	reqCtx, cancel := context.WithTimeout(ctx, h.cfg.healthTimeout())
	defer cancel()

	req, err := b.healthCheckRequest(reqCtx, h.cfg.HealthCheckPath)
	if err != nil {
		b.recordHealthCheck(false, err.Error(), h.cfg.UnhealthyThreshold, h.cfg.HealthyThreshold)
		return
	}

	start := time.Now()
	resp, err := h.client.Do(req)
	elapsed := time.Since(start)

	wasHealthy := b.IsHealthy()

	if err != nil {
		b.recordHealthCheck(false, err.Error(), h.cfg.UnhealthyThreshold, h.cfg.HealthyThreshold)
	} else {
		resp.Body.Close()
		ok := resp.StatusCode >= 200 && resp.StatusCode < 500 // 5xx = app unhealthy even if TCP is fine
		b.recordHealthCheck(ok, "", h.cfg.UnhealthyThreshold, h.cfg.HealthyThreshold)
		// A passive health probe also gives us a latency sample for free,
		// which feeds straight into the load-based routing decision.
		if ok {
			b.updateLatency(float64(elapsed.Milliseconds()), h.cfg.EwmaAlpha)
		}
	}

	if isNowHealthy := b.IsHealthy(); isNowHealthy != wasHealthy {
		if isNowHealthy {
			log.Printf("[health] backend %s (%s) is now HEALTHY", b.Name, b.URL)
		} else {
			log.Printf("[health] backend %s (%s) is now UNHEALTHY", b.Name, b.URL)
		}
	}
}
