// Command cfos-adapter is the CFOS/Sandstorm execution bridge (ticket
// #17 U1): a thin HTTP service that runs CFOS executeCode requests in
// forkd microVMs via the lease API.
//
// Plain Go binary (homelab preference), no Docker. Env:
//
//	LEASE_URL      https://sandbox.lacy.casa:8890 (or direct vm2 addr)
//	LEASE_TOKEN    consumer token for the lease API
//	ADAPTER_ADDR   listen address (default :8893)
//	ADAPTER_TOKEN  bearer token CFOS presents to this adapter
//	DEFAULT_IMAGE  image for Workers-style JS code (default js-base)
package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jrimmer/forkd-service/cfos"
	"github.com/jrimmer/forkd-service/runner"
)

func main() {
	leaseURL := envOr("LEASE_URL", "https://127.0.0.1:8890")
	leaseToken := os.Getenv("LEASE_TOKEN")
	if leaseToken == "" {
		log.Fatal("LEASE_TOKEN is required")
	}
	adapterToken := envOr("ADAPTER_TOKEN", leaseToken)
	addr := envOr("ADAPTER_ADDR", ":8893")
	defaultImage := envOr("DEFAULT_IMAGE", "js-base")

	lease := runner.NewHTTPLeaseClient(leaseURL, leaseToken)
	srv := cfos.New(cfos.Config{
		Sandbox:      lease,
		Tokens:       map[string]string{adapterToken: "cfos"},
		MaxTimeout:   300,
		DefaultTTL:   300,
		DefaultImage: defaultImage,
	})

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("cfos-adapter listening on %s (lease %s, default image %s)", addr, leaseURL, defaultImage)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
