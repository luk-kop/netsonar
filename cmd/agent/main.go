// Package main is the entry point for the netsonar binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"netsonar/internal/config"
	"netsonar/internal/metrics"
	"netsonar/internal/probe"
	"netsonar/internal/scheduler"
)

// version is injected at build time via -ldflags.
var version = "dev"

// agentLogLevel is the dynamic level shared by the default slog handler.
// setupLogger wires it into the handler once at startup; configureLogger
// updates it atomically on config reload so log_level changes via SIGHUP
// take effect without swapping handlers.
var agentLogLevel = new(slog.LevelVar)

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "/etc/netsonar/config.yaml", "Path to YAML configuration file")
	listenAddr := flag.String("listen-addr", "", "Override agent.listen_addr from config (e.g. :9275)")
	flag.Parse()

	// Load and validate configuration.
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		return 1
	}

	// CLI flag overrides config value.
	if *listenAddr != "" {
		cfg.Agent.ListenAddr = *listenAddr
	}
	if cfg.Agent.ListenAddr == "" {
		cfg.Agent.ListenAddr = ":9275"
	}
	if cfg.Agent.MetricsPath == "" {
		cfg.Agent.MetricsPath = "/metrics"
	}

	setupLogger(cfg.Agent.LogLevel)

	slog.Info("starting netsonar",
		"version", version,
		"listen_addr", cfg.Agent.ListenAddr,
		"targets", len(cfg.Targets),
		"config_hash", mustConfigHash(cfg),
	)

	// Initialize metrics exporter with dynamic tag keys from config.
	tagKeys := config.CollectTagKeys(cfg)
	exporter := metrics.NewMetricsExporter(tagKeys)

	// Initialize scheduler with prober factory.
	sched := scheduler.New(exporter, newProber)

	// Start all probe goroutines.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sched.Start(ctx, *cfg)

	// Set initial agent metadata metrics.
	updateAgentMetrics(exporter, cfg)

	// Start HTTP server for /metrics and health endpoints.
	srv := newHTTPServer(cfg.Agent.ListenAddr, cfg.Agent.MetricsPath, exporter, true)

	// Run HTTP server in background.
	srvErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- err
		}
		close(srvErr)
	}()

	// Signal handling: SIGHUP → reload, SIGTERM/SIGINT → graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)

	for {
		select {
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGHUP:
				slog.Info("received SIGHUP, reloading configuration")
				handleReload(*configPath, ctx, sched, exporter, tagKeys)

			case syscall.SIGTERM, syscall.SIGINT:
				slog.Info("received shutdown signal, initiating graceful shutdown", "signal", sig)
				return shutdown(sched, srv, cancel)
			}

		case err := <-srvErr:
			if err != nil {
				slog.Error("HTTP server error", "error", err)
				sched.Stop()
				cancel()
				return 1
			}
		}
	}
}

// handleReload re-reads the configuration file and applies changes via
// the scheduler's diff-based reload. If the new config is invalid or the
// effective tag key set changed (which requires a restart), the agent
// continues with the previous configuration.
func handleReload(configPath string, ctx context.Context, sched *scheduler.Scheduler, exporter *metrics.MetricsExporter, startupTagKeys []string) {
	newCfg, err := config.LoadConfig(configPath)
	if err != nil {
		slog.Error("config reload failed, keeping previous configuration", "error", err)
		return
	}

	newTagKeys := config.CollectTagKeys(newCfg)
	if !slices.Equal(startupTagKeys, newTagKeys) {
		slog.Error("config reload rejected: tag key set changed; restart required",
			"old_tag_keys", startupTagKeys,
			"new_tag_keys", newTagKeys,
		)
		return
	}

	sched.Reload(ctx, *newCfg)
	configureLogger(newCfg.Agent.LogLevel)
	updateAgentMetrics(exporter, newCfg)

	slog.Info("configuration reloaded successfully",
		"targets", len(newCfg.Targets),
		"config_hash", mustConfigHash(newCfg),
	)
}

// shutdown performs graceful shutdown: cancel probe goroutines, drain the
// HTTP server with a 5-second grace period, then exit cleanly.
func shutdown(sched *scheduler.Scheduler, srv *http.Server, cancel context.CancelFunc) int {
	// Stop all probe goroutines.
	sched.Stop()
	cancel()

	// Give in-flight HTTP scrapes 5 seconds to complete.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
		return 1
	}

	slog.Info("graceful shutdown complete")
	return 0
}

// newHTTPServer builds the agent HTTP server with defensive timeouts.
func newHTTPServer(listenAddr, metricsPath string, exporter *metrics.MetricsExporter, ready bool) *http.Server {
	return &http.Server{
		Addr:              listenAddr,
		Handler:           newHTTPMux(metricsPath, exporter, ready),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// newHTTPMux registers metrics and health/readiness endpoints.
func newHTTPMux(metricsPath string, exporter *metrics.MetricsExporter, ready bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(metricsPath, exporter.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writePlainText(w, http.StatusOK, "ok\n")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready {
			writePlainText(w, http.StatusOK, "ok\n")
			return
		}
		writePlainText(w, http.StatusServiceUnavailable, "not ready\n")
	})
	return mux
}

func writePlainText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// newProber returns the appropriate Prober implementation for a target's
// probe type.
func newProber(target config.TargetConfig) probe.Prober {
	switch target.ProbeType {
	case config.ProbeTypeTCP:
		return &probe.TCPProber{}
	case config.ProbeTypeHTTP:
		return probe.NewHTTPProber(target.ProbeOpts.TLSSkipVerify, target.ProbeOpts.FollowRedirects, target.ProbeOpts.ProxyURL)
	case config.ProbeTypeICMP:
		return &probe.ICMPProber{}
	case config.ProbeTypeMTU:
		return &probe.MTUProber{}
	case config.ProbeTypeDNS:
		return &probe.DNSProber{}
	case config.ProbeTypeTLSCert:
		return &probe.TLSCertProber{}
	case config.ProbeTypeHTTPBody:
		return probe.NewHTTPBodyProber(target.ProbeOpts.TLSSkipVerify, target.ProbeOpts.FollowRedirects, target.ProbeOpts.ProxyURL, target.ProbeOpts.BodyMatchRegex)
	case config.ProbeTypeProxy:
		return &probe.ProxyProber{}
	default:
		// Should never happen after config validation.
		slog.Error("unknown probe type", "probe_type", target.ProbeType, "target", target.Name)
		return &probe.TCPProber{}
	}
}

// updateAgentMetrics sets the agent metadata gauges after startup or reload.
func updateAgentMetrics(exporter *metrics.MetricsExporter, cfg *config.Config) {
	exporter.SetAgentInfo(version)
	exporter.SetConfigInfo(mustConfigHash(cfg))
	exporter.SetTargetsTotal(len(cfg.Targets))
	exporter.SetConfigReloadTimestamp(time.Now())
}

// mustConfigHash returns the short config hash, falling back to "unknown"
// if canonical marshaling ever fails. The fallback exists so a logging or
// metric call site never has to handle an error for an operation that
// should always succeed for a validated Config.
func mustConfigHash(cfg *config.Config) string {
	h, err := config.ComputeHash(cfg)
	if err != nil {
		slog.Error("failed to compute config hash", "error", err)
		return "unknown"
	}
	return h
}

// setupLogger installs the default slog logger backed by agentLogLevel.
// It is called exactly once, at agent startup. Subsequent log_level changes
// on reload go through configureLogger and do not swap the handler.
func setupLogger(level string) {
	configureLogger(level)
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: agentLogLevel})
	slog.SetDefault(slog.New(handler))
}

// configureLogger updates the agent's dynamic log level. The level string
// must have already been validated by config.LoadConfig; unknown values
// fall back to info so a misuse at an internal call site never panics.
func configureLogger(level string) {
	agentLogLevel.Set(parseLogLevel(level))
}

// parseLogLevel maps a validated log_level string to a slog.Level.
// Unknown values default to slog.LevelInfo; config validation is the
// source of truth for accepted values.
func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
