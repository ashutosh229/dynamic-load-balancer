package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBackend simulates a chat backend whose /message handler takes an
// artificial delay (to simulate load) and can be toggled healthy/unhealthy.
func newFakeBackend(t *testing.T, delay func() time.Duration, healthy *int32, hitCounter *int64) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(healthy) == 1 {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(hitCounter, 1)
		time.Sleep(delay())
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	})
	return httptest.NewServer(mux)
}

func TestDynamicRoutingAvoidsSlowBackend(t *testing.T) {
	slowHealthy := int32(1)
	fastHealthy := int32(1)
	var slowHits, fastHits int64

	slow := newFakeBackend(t, func() time.Duration { return 150 * time.Millisecond }, &slowHealthy, &slowHits)
	defer slow.Close()
	fast := newFakeBackend(t, func() time.Duration { return 2 * time.Millisecond }, &fastHealthy, &fastHits)
	defer fast.Close()

	cfg := defaultConfig()
	cfg.HealthCheckIntervalMs = 100
	cfg.HealthCheckTimeoutMs = 100
	cfg.MaxActiveRequests = 5
	cfg.MaxLatencyMs = 50 // slow backend's 150ms latency will exceed this
	cfg.Backends = []BackendConfig{
		{Name: "slow", URL: slow.URL},
		{Name: "fast", URL: fast.URL},
	}

	backends := mustBackends(t, cfg)
	hc := NewHealthChecker(cfg, backends)
	go hc.Run(context.Background())
	time.Sleep(300 * time.Millisecond) // let health checker warm up + latency samples settle

	balancer := NewBalancer(cfg, backends)
	srv := NewServer(cfg, backends, balancer)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// Fire enough requests that the scoring algorithm has time to react.
	for i := 0; i < 20; i++ {
		resp, err := http.Post(ts.URL+"/message", "application/json", nil)
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		resp.Body.Close()
	}

	t.Logf("slow backend hits=%d fast backend hits=%d", atomic.LoadInt64(&slowHits), atomic.LoadInt64(&fastHits))
	if atomic.LoadInt64(&fastHits) <= atomic.LoadInt64(&slowHits) {
		t.Fatalf("expected fast backend to receive materially more traffic than slow backend")
	}
}

func TestUnhealthyBackendIsExcluded(t *testing.T) {
	h1 := int32(1)
	h2 := int32(0) // starts down
	var hits1, hits2 int64

	b1 := newFakeBackend(t, func() time.Duration { return time.Millisecond }, &h1, &hits1)
	defer b1.Close()
	b2 := newFakeBackend(t, func() time.Duration { return time.Millisecond }, &h2, &hits2)
	defer b2.Close()

	cfg := defaultConfig()
	cfg.HealthCheckIntervalMs = 50
	cfg.HealthCheckTimeoutMs = 100
	cfg.UnhealthyThreshold = 1
	cfg.Backends = []BackendConfig{
		{Name: "b1", URL: b1.URL},
		{Name: "b2", URL: b2.URL},
	}

	backends := mustBackends(t, cfg)
	hc := NewHealthChecker(cfg, backends)
	go hc.Run(context.Background())
	time.Sleep(200 * time.Millisecond) // let it mark b2 unhealthy

	balancer := NewBalancer(cfg, backends)
	srv := NewServer(cfg, backends, balancer)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	for i := 0; i < 10; i++ {
		resp, err := http.Post(ts.URL+"/message", "application/json", nil)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		resp.Body.Close()
	}

	if atomic.LoadInt64(&hits2) != 0 {
		t.Fatalf("expected 0 requests to unhealthy backend, got %d", hits2)
	}
	if atomic.LoadInt64(&hits1) != 10 {
		t.Fatalf("expected all 10 requests on healthy backend, got %d", hits1)
	}
}

func mustBackends(t *testing.T, cfg Config) []*Backend {
	var backends []*Backend
	for _, bc := range cfg.Backends {
		b, err := NewBackend(bc.Name, bc.URL)
		if err != nil {
			t.Fatalf("bad backend: %v", err)
		}
		backends = append(backends, b)
	}
	return backends
}
