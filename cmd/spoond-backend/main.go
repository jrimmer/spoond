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
//	LLM_UPSTREAM_URL OpenAI-compatible LLM API base for the per-lease
//	                 LLM gateway (e.g. https://openrouter.ai/api/v1)
//	LLM_UPSTREAM_KEY server-side key for that upstream (never sent to
//	                 sandboxes; empty disables the gateway)
package spoondbackend

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

	"github.com/jrimmer/spoond/api"
	"github.com/jrimmer/spoond/forkd"
	"github.com/jrimmer/spoond/identity"
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

func Main(args []string) int {
	forkdURL := envOr("FORKD_URL", "http://127.0.0.1:8889")
	forkdToken := os.Getenv("FORKD_TOKEN")
	bindAddr := envOr("BIND_ADDR", "127.0.0.1:8890")
	proxyAddr := envOr("PROXY_ADDR", "") // e.g. 0.0.0.0:8891 (Caddy wildcard front)
	tlsCert := os.Getenv("TLS_CERT")
	tlsKey := os.Getenv("TLS_KEY")
	poolSize := envIntOr("POOL_SIZE", 0)
	idleTimeoutSecs := envIntOr("IDLE_TIMEOUT_SECS", 0) // persistent-lease auto-suspend
	idleTimeout := time.Duration(idleTimeoutSecs) * time.Second
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
	// knownTags surfaces baked images even when the controller's list
	// endpoint is empty; add tags here as you bake them. Also seeds the
	// warm pool so every image pre-forks at startup.
	knownTags := []string{}
	if v := os.Getenv("KNOWN_IMAGES"); v != "" {
		knownTags = strings.Split(v, ",")
	}
	svc := api.NewServiceWithIdle(fc, tokens, poolSize, defaultTTL, maxTTL, idleTimeout, knownTags...)
	// Egress policy enforcement (ticket #13): install iptables FORWARD
	// rules in each lease's child netns. NETPOL_DNS lists resolvers the
	// restricted policy always permits so guests can resolve allowlisted
	// names; empty NETPOL_DNS disables enforcement (no root/netns access).
	if dns := os.Getenv("NETPOL_DNS"); dns != "" {
		svc.SetNetpol(&api.NetnsPolicyApplier{}, strings.Split(dns, ","))
	}
	reg := api.NewImageRegistry(fc, knownTags...)
	// LLM gateway model map: "exe.dev-id=upstream-id,exe.dev-id2=upstream2".
	// Shelley sends exe.dev catalog ids; the gateway rewrites them to the
	// configured upstream's models. Unmapped ids fall back to defaultModel.
	llmModelMap := map[string]string{}
	if v := os.Getenv("LLM_MODEL_MAP"); v != "" {
		for _, pair := range strings.Split(v, ",") {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 {
				llmModelMap[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}
	// Identity store (epic #26 T1): users + key/token resolution. The
	// store file is optional; when absent the backend runs in legacy
	// single-user mode (consumer tokens only) until a user is created.
	// The first user created becomes the admin (KTD-2).
	if usersFile := os.Getenv("USERS_FILE"); usersFile != "" {
		ids, err := identity.NewStore(usersFile)
		if err != nil {
			log.Fatalf("identity store: %v", err)
		}
		svc.SetIdentities(ids)
		log.Printf("identity store: %s (%d user(s))", usersFile, ids.Count())
	}
	// Trusted gateway impersonation (U6/T5): the SSH gateway's service
	// token, used to act as the SSH-authenticated user on ctl calls.
	if gt := os.Getenv("GATEWAY_TOKEN"); gt != "" {
		svc.SetGatewayToken(gt)
		log.Printf("gateway token: trusted impersonation enabled")
	}

	srv := api.NewServerWithLLM(svc, reg, os.Getenv("LLM_UPSTREAM_URL"), os.Getenv("LLM_UPSTREAM_KEY"), os.Getenv("LLM_DEFAULT_MODEL"), llmModelMap)
	// Static assets (shelley binary etc.) served to guests on the proxy
	// listener at /assets/<file> (default off; set ASSETS_DIR to enable).
	if d := os.Getenv("ASSETS_DIR"); d != "" {
		srv.SetAssetsDir(d)
	}

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

	// Optional second listener: the public HTTP proxy (wildcard
	// *.sandbox.lacy.casa via Caddy). Plain HTTP — Caddy terminates TLS.
	var proxySrv *http.Server
	if proxyAddr != "" {
		proxySrv = &http.Server{Addr: proxyAddr, Handler: srv.ProxyHandler()}
		go func() {
			log.Printf("forkd proxy listening on %s (wildcard sandbox hostnames)", proxyAddr)
			if err := proxySrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("proxy: %v", err)
			}
		}()
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
		if proxySrv != nil {
			_ = proxySrv.Shutdown(shutdownCtx)
		}
		cancel()
	}()

	log.Printf("spoond-backend listening on %s (forkd at %s, %d consumer(s), pool=%d)", bindAddr, forkdURL, len(tokens), poolSize)
	var err error
	if tlsCert != "" && tlsKey != "" {
		err = httpSrv.ListenAndServeTLS(tlsCert, tlsKey)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
	return 0
}
