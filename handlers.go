package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"
)

type Server struct {
	cfg      Config
	backends []*Backend
	balancer *Balancer
	client   *http.Client
}

func NewServer(cfg Config, backends []*Backend, balancer *Balancer) *Server {
	return &Server{
		cfg:      cfg,
		backends: backends,
		balancer: balancer,
		client: &http.Client{
			Timeout: cfg.requestTimeout(),
		},
	}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/message", s.proxyHandler)
	mux.HandleFunc("/feed", s.proxyHandler)
	mux.HandleFunc("/health", s.lbHealthHandler)
	mux.HandleFunc("/stats", s.statsHandler)
	return mux
}

// proxyHandler forwards /message and /feed to the best backend chosen by
// the Balancer, transparently retrying against the next-best healthy
// backend if the first attempt fails. Because every chat message carries
// a client-generated unique message ID and the backend is required to
// de-duplicate on insert, retried POSTs are safe to replay.
func (s *Server) proxyHandler(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	candidates, err := s.balancer.Pick()
	if err != nil {
		http.Error(w, "all backends unavailable", http.StatusServiceUnavailable)
		return
	}

	maxAttempts := 1 + s.cfg.MaxRetries
	if maxAttempts > len(candidates) {
		maxAttempts = len(candidates)
	}

	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		backend := candidates[i]
		status, respBody, respHeader, err := s.forward(r, backend, bodyBytes)
		if err == nil && status < 500 {
			for k, vv := range respHeader {
				for _, v := range vv {
					w.Header().Add(k, v)
				}
			}
			w.Header().Set("X-Served-By", backend.Name)
			w.WriteHeader(status)
			w.Write(respBody)
			return
		}
		lastErr = err
		log.Printf("[proxy] attempt %d via %s failed: %v (status=%d)", i+1, backend.Name, err, status)
	}

	http.Error(w, "upstream request failed after retries: "+errString(lastErr), http.StatusBadGateway)
}

func errString(err error) string {
	if err == nil {
		return "non-2xx/3xx/4xx response from all candidates"
	}
	return err.Error()
}

// forward performs a single attempt against one backend, tracking
// in-flight count and latency for the load-scoring algorithm regardless
// of outcome.
func (s *Server) forward(r *http.Request, b *Backend, body []byte) (int, []byte, http.Header, error) {
	b.begin()
	start := time.Now()
	defer func() {
		b.end()
	}()

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.requestTimeout())
	defer cancel()

	target := *b.URL
	target.Path = joinPath(target.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(ctx, r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		b.recordResult(time.Since(start), true, s.cfg.EwmaAlpha)
		return 0, nil, nil, err
	}
	req.Header = r.Header.Clone()

	resp, err := s.client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		b.recordResult(elapsed, true, s.cfg.EwmaAlpha)
		return 0, nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		b.recordResult(elapsed, true, s.cfg.EwmaAlpha)
		return resp.StatusCode, nil, nil, err
	}

	b.recordResult(elapsed, resp.StatusCode >= 500, s.cfg.EwmaAlpha)
	return resp.StatusCode, respBody, resp.Header, nil
}

func (s *Server) lbHealthHandler(w http.ResponseWriter, r *http.Request) {
	anyHealthy := false
	for _, b := range s.backends {
		if b.IsHealthy() {
			anyHealthy = true
			break
		}
	}
	if anyHealthy {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("no healthy backends"))
	}
}

func (s *Server) statsHandler(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AdminToken != "" {
		if r.Header.Get("Authorization") != "Bearer "+s.cfg.AdminToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	out := make([]BackendStatus, 0, len(s.backends))
	for _, b := range s.backends {
		out = append(out, b.Snapshot())
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"backends": out,
		"config": map[string]interface{}{
			"max_active_requests": s.cfg.MaxActiveRequests,
			"max_latency_ms":      s.cfg.MaxLatencyMs,
		},
	})
}
