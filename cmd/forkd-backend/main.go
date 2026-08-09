// Command forkd-backend runs the forkd ephemeral-backend lease API.
//
// Configuration is read from the environment:
//
//	FORKD_URL        forkd-controller base URL (default http://127.0.0.1:8889)
//	FORKD_TOKEN      bearer token for forkd-controller (optional)
//	BIND_ADDR        listen address (default 127.0.0.1:8890)
//	TLS_CERT, TLS_KEY  serve HTTPS when both are set
//	CONSUMER_TOKENS  comma-separated token=consumer pairs (e.g. "abc=forgejo,def=pi")
//	POOL_SIZE        warm-pool size per image (default 0 = disabled)
//	DEFAULT_TTL_SECS default lease TTL (default 300)
//	MAX_TTL_SECS     max lease TTL (default 3600)
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jrimmer/forkd-service/api"
	"github.com/jrimmer/forkd-service/forkd"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	forkdURL := envOr("FORKD_URL", "http://127.0.0.1:8889")
	forkdToken := os.Getenv("FORKD_TOKEN")
	bindAddr := envOr("BIND_ADDR", "127.0.0.1:8890")
	tlsCert := os.Getenv("TLS_CERT")
	tlsKey := os.Getenv("TLS_KEY")
	poolSize := envIntOr("POOL_SIZE", 0)
	defaultTTL := time.Duration(envIntOr("DEFAULT_TTL_SECS", 300)) * time.Second
	maxTTL := time.Duration(envIntOr("MAX_TTL_SECS", 3600)) * time.Second

	// Parse consumer tokens: "abc=forgejo,def=pi"
	tokens := map[string]string{}
	for _, pair := range strings.Split(os.Getenv("CONSUMER_TOKENS"), ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			tokens[parts[0]] = parts[1]
		}
	}
	if len(tokens) == 0 {
		log.Fatal("CONSUMER_TOKENS is required (token=consumer,comma-separated)")
	}

	fc := forkd.NewClient(forkdURL, forkdToken)
	svc := api.NewService(fc, tokens, poolSize, defaultTTL, maxTTL)
	// knownTags surfaces baked images even when the controller's list
	// endpoint is empty; add tags here as you bake them.
	knownTags := []string{}
	if v := os.Getenv("KNOWN_IMAGES"); v != "" {
		knownTags = strings.Split(v, ",")
	}
	reg := api.NewImageRegistry(fc, knownTags...)
	srv := api.NewServer(svc, reg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)

	// Kill any sandboxes left by a previous backend incarnation before
	// warming the pool, so netns slots are never double-booked.
	svc.ReconcileOrphans(ctx)

	httpSrv := &http.Server{
		Addr:    bindAddr,
		Handler: srv.Handler(),
	}

	// Graceful shutdown: on SIGTERM/SIGINT kill every lease and pooled
	// sandbox before exiting. Without this, a backend restart orphans
	// its warm VMs in the controller (which has no client-liveness), and
	// they hold netns slots until manually reaped.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("received %v, shutting down (releasing %d leases + warm pool)", sig, len(svc.LiveLeases()))
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		svc.Shutdown(shutdownCtx)
		_ = httpSrv.Shutdown(shutdownCtx)
		cancel()
	}()

	log.Printf("forkd-backend listening on %s (forkd at %s, %d consumer(s), pool=%d)", bindAddr, forkdURL, len(tokens), poolSize)
	var err error
	if tlsCert != "" && tlsKey != "" {
		err = httpSrv.ListenAndServeTLS(tlsCert, tlsKey)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}
