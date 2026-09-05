package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config.json")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	var backends []*Backend
	for _, bc := range cfg.Backends {
		b, err := NewBackend(bc.Name, bc.URL)
		if err != nil {
			log.Fatalf("invalid backend %s (%s): %v", bc.Name, bc.URL, err)
		}
		backends = append(backends, b)
		log.Printf("registered backend %s -> %s", b.Name, b.URL)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hc := NewHealthChecker(cfg, backends)
	go hc.Run(ctx)

	balancer := NewBalancer(cfg, backends)
	srv := NewServer(cfg, backends, balancer)

	httpServer := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      srv.Routes(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("load balancer listening on %s", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("shutting down...")
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}
