// Command opencode2api starts the API gateway and its management interface.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	adminui "opencode2api/internal/admin"
	"opencode2api/internal/buildinfo"
	"opencode2api/internal/config"
	"opencode2api/internal/gateway"
	"opencode2api/internal/surfshark"
	"opencode2api/internal/telemetry"
)

// version remains the linker injection point used by release builds.
var version = "dev"

// main loads the configuration, starts the gateway runtime and the API and
// WebUI servers, and shuts them down on SIGINT or SIGTERM.
func main() {
	buildinfo.Version = version
	configPath := flag.String("config", "config.json", "path to config.json")
	listen := flag.String("listen", "", "override the configured API listen address")
	webListen := flag.String("web-listen", "", "override the configured WebUI listen address")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *webListen != "" {
		cfg.WebUI.Listen = *webListen
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	level := new(slog.LevelVar)
	telemetry.SetLogLevel(level, cfg.Logging.Level)
	hub := telemetry.LogHub(cfg.Logging.RingSize)
	redactor := config.SecretRedactor()
	redactor.Replace(cfg)
	logger := telemetry.StructuredLogger(level, hub, redactor)
	monitor := telemetry.Monitor()
	if limit := cfg.Performance.MemoryLimitBytes; limit > 0 {
		// A soft limit makes the GC work harder as the heap approaches the
		// ceiling instead of letting the process be OOM-killed. 0 keeps the Go
		// runtime default.
		debug.SetMemoryLimit(limit)
		logger.Info("soft memory limit applied", "component", "gateway", "event", "memory_limit_applied", "limit_bytes", limit)
	}

	// Listen before the WARP pool is ready. A cold start spends tens of seconds
	// registering or probing exits, and until this process was listening every
	// request from the router (chat and WebUI alike) failed with "connection
	// refused". Requests that arrive during startup now wait for the runtime,
	// which the router's timeout comfortably covers.
	apiHandler := newReadyHandler(ctx)
	apiServer := &http.Server{
		Addr: cfg.Listen, Handler: apiHandler, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 120 * time.Second,
	}
	servers := []*http.Server{apiServer}
	go serveHTTP(cancel, logger, apiServer, "api")

	var webHandler *readyHandler
	if cfg.WebUI.Enabled {
		webHandler = newReadyHandler(ctx)
		webServer := &http.Server{
			Addr: cfg.WebUI.Listen, Handler: webHandler, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 120 * time.Second,
		}
		servers = append(servers, webServer)
		go serveHTTP(cancel, logger, webServer, "webui")
	}

	startWatchdog()

	manager, err := gateway.RuntimeManager(ctx, *configPath, cfg, logger, monitor, hub, redactor, level)
	if err != nil {
		logger.Error("failed to initialize runtime", "component", "runtime", "event", "runtime_initialization_failed", "error", err)
		os.Exit(1)
	}
	defer manager.Shutdown()

	apiHandler.set(manager.Handler())
	var console *adminui.Server
	if webHandler != nil {
		console = adminui.New(manager, monitor, hub, logger)
		console.SetSurfshark(surfshark.New(surfshark.Config{
			ConfigDir: filepath.Dir(*configPath),
			ProxyFile: cfg.ProxyFile,
			BasePort:  13101,
			Logger:    logger,
			ReloadConfig: func() error {
				_, err := manager.Reload()
				return err
			},
		}))
		console.SetSessionPath(*configPath + ".admin-sessions.json")
		webHandler.set(console.Handler())
	}

	<-ctx.Done()
	// End server-sent event streams first: a handler still writing to one
	// would keep http.Server.Shutdown waiting until its deadline.
	if console != nil {
		console.Close()
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "component", "server", "event", "shutdown_failed", "address", server.Addr, "error", err)
		}
	}
}

// serveHTTP serves server until it is shut down. An unexpected stop is logged
// and cancels the root context, which begins the shutdown of every server.
func serveHTTP(cancel context.CancelFunc, logger *slog.Logger, server *http.Server, component string) {
	logger.Info("server listening", "component", component, "event", "server_started", "address", server.Addr, "version", buildinfo.Version)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped unexpectedly", "component", component, "event", "server_failed", "address", server.Addr, "error", err)
		cancel()
	}
}

// readyHandler holds a request until the gateway runtime is ready, then hands
// it to the real handler. It exists so the listener is up during WARP startup:
// waiting is recoverable, refusing the connection is not.
type readyHandler struct {
	ctx   context.Context
	ready chan struct{}
	mu    sync.RWMutex
	next  http.Handler
}

func newReadyHandler(ctx context.Context) *readyHandler {
	return &readyHandler{ctx: ctx, ready: make(chan struct{})}
}

func (h *readyHandler) set(next http.Handler) {
	h.mu.Lock()
	h.next = next
	h.mu.Unlock()
	close(h.ready)
}

func (h *readyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case <-h.ready:
	case <-h.ctx.Done():
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	h.mu.RLock()
	next := h.next
	h.mu.RUnlock()
	if next == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	next.ServeHTTP(w, r)
}
