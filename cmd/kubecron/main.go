package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	// Embed the IANA time-zone database. The distroless runtime image ships no
	// /usr/share/zoneinfo, so without this time.LoadLocation fails and every
	// CronJob spec.timeZone would be unresolvable (DOM-1).
	_ "time/tzdata"

	"github.com/caarlos0/env/v11"

	"github.com/kubecron/kubecron/internal/api"
	"github.com/kubecron/kubecron/internal/auth"
	"github.com/kubecron/kubecron/internal/cluster"
	"github.com/kubecron/kubecron/internal/metrics"
	"github.com/kubecron/kubecron/internal/sampler"
	"github.com/kubecron/kubecron/internal/storage"
	"github.com/kubecron/kubecron/internal/streamer"
	"github.com/kubecron/kubecron/internal/version"
	"github.com/kubecron/kubecron/internal/watcher"
)

// Config holds all runtime configuration sourced from environment variables.
type Config struct {
	// Mode selects the HTTP surface: "ui" (default) serves the dashboard and
	// the CronJob controls; "server" serves only the read-only /api/v1
	// collector contract. The collection machinery below — informers, sampler,
	// log streamer, retention — is identical in both.
	Mode                  string `env:"KUBECRON_MODE"           envDefault:"ui"`
	KubeconfigDir         string `env:"KUBECONFIG_DIR"          envDefault:"/etc/kubecron/kubeconfigs"`
	DBPath                string `env:"DB_PATH"                 envDefault:"/data/kubecron.db"`
	Port                  int    `env:"PORT"                    envDefault:"8080"`
	RetentionDays         int    `env:"RETENTION_DAYS"          envDefault:"180"`
	LogRetentionDays      int    `env:"LOG_RETENTION_DAYS"      envDefault:"14"`
	MetricsSampleInterval int    `env:"METRICS_SAMPLE_INTERVAL" envDefault:"15"` // seconds
	// ClusterID names the cluster when KubeCron runs on its own ServiceAccount
	// with no kubeconfig directory — the one-collector-per-cluster shape. It is
	// the ID a consumer sees in /api/v1/clusters and the key every stored row
	// hangs off, so changing it on an existing deployment orphans its history.
	ClusterID string `env:"CLUSTER_ID" envDefault:"local"`
	// APIToken, when set, is required as `Authorization: Bearer <token>` on
	// every request in server mode. Ignored in UI mode, which uses OIDC.
	APIToken string `env:"API_TOKEN"`
	OIDC     auth.Config
}

func main() {
	// 1. Parse config from environment.
	cfg := Config{}
	if err := env.Parse(&cfg); err != nil {
		slog.Error("config parse failed", "err", err)
		os.Exit(1)
	}

	mode, err := api.ParseMode(cfg.Mode)
	if err != nil {
		slog.Error("config parse failed", "err", err)
		os.Exit(1)
	}

	// 2. Structured JSON logging.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("kubecron starting", "mode", string(mode), "port", cfg.Port, "db", cfg.DBPath)

	// Root context — cancelled on SIGTERM / SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// 3. Open SQLite and run migrations.
	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		slog.Error("database open failed", "err", err)
		os.Exit(1)
	}

	// 4. Background retention goroutine.
	go storage.StartRetention(ctx, store, cfg.RetentionDays, cfg.LogRetentionDays)

	// 5. Load kubeconfigs → one ClusterClient per file.
	mgr := cluster.NewManager(store, cfg.KubeconfigDir, cfg.ClusterID)
	if err := mgr.Load(ctx); err != nil {
		// Non-fatal: some clusters may have failed; others are still loaded.
		slog.Warn("cluster loading completed with errors", "err", err)
	}

	// 6. Restart recovery: mark any run still 'running' in the DB as failed
	// before informers start — this avoids a race where OnAdd fires before cleanup.
	if runs, err := store.GetRunningRuns(ctx); err == nil && len(runs) > 0 {
		for _, r := range runs {
			if err := store.MarkRunFailed(ctx, r.ID); err != nil {
				slog.Warn("failed to mark stale run as failed", "run_id", r.ID, "err", err)
			}
		}
		slog.Info("marked stale runs as failed on startup", "count", len(runs))
	}

	// 7. Shared broadcaster and streamer (single instance, cross-cluster).
	broadcaster := streamer.NewBroadcaster()
	logStreamer := streamer.NewStreamer(store, broadcaster)

	sampleInterval := time.Duration(cfg.MetricsSampleInterval) * time.Second

	// 8. Per-cluster: probe Metrics API + start informer controller.
	var controllers []*watcher.Controller

	for _, cc := range mgr.Registry().All() {
		// Probe the Metrics API availability (background re-probe every 5 min).
		// On success, flip the in-memory flag the pod watcher reads to gate
		// resource sampling.
		sampler.StartProbe(ctx, cc.ID, cc.MetricsClient, store, func() { cc.SetMetricsEnabled(true) })

		s := sampler.NewSampler(store, cc.MetricsClient, sampleInterval)

		ctrl := watcher.NewController(cc, store, logStreamer, s)
		if err := ctrl.Start(ctx); err != nil {
			slog.Error("watcher start failed", "cluster", cc.ID, "err", err)
			continue
		}
		controllers = append(controllers, ctrl)
		slog.Info("watcher started", "cluster", cc.ID)
	}

	// cacheSynced is true once all informer caches have completed their initial
	// list — used by /readyz to gate traffic.
	cacheSynced := func() bool {
		for _, ctrl := range controllers {
			if !ctrl.CacheSynced() {
				return false
			}
		}
		return true
	}

	// 9. The front door, which differs by mode.
	//
	// UI mode is browsed by a person, so it uses OIDC: an unauthenticated
	// request is redirected to an identity provider. Server mode is called by a
	// program, which cannot complete that redirect, so it uses a bearer token
	// instead. Configuring the wrong one for the mode is a silent way to end up
	// with no front door at all, so each mode says what it found.
	var authenticator *auth.Authenticator
	switch {
	case mode.ServesUI() && cfg.OIDC.Enabled:
		authenticator, err = auth.NewAuthenticator(ctx, cfg.OIDC)
		if err != nil {
			slog.Error("OIDC initialisation failed", "err", err)
			os.Exit(1)
		}
		slog.Info("OIDC authentication enabled", "issuer", cfg.OIDC.IssuerURL)

	case mode.ServesUI():
		// The Helm chart refuses to install an externally-reachable release with
		// OIDC off (SEC-28), but nothing stops a raw manifest, a Compose file or
		// a `go run` from doing it. Say so once, loudly, at Warn: without OIDC
		// the operator gate in api.Server is a pass-through and the auth
		// middleware is never installed, so suspend/resume/trigger are anonymous
		// on every cluster whose kubeconfig is mounted.
		slog.Warn("OIDC is disabled — all endpoints are UNAUTHENTICATED, including suspend/resume/trigger on every connected cluster; do not expose this service outside a trusted network",
			"remediation", "set OIDC_ENABLED=true")

	case cfg.APIToken != "":
		slog.Info("collector API requires a bearer token", "mode", string(mode))
		if cfg.OIDC.Enabled {
			// Not fatal — the token is a working front door and refusing to
			// start would be a worse outcome than the misconfiguration.
			slog.Warn("OIDC_ENABLED is set but has no effect in server mode: there is no browser flow to run. API_TOKEN is what guards this instance")
		}

	default:
		// Milder than the UI-mode warning, and deliberately so: a collector
		// registers no mutating route, so the exposure is disclosure of the
		// CronJob inventory, schedules, run outcomes and log bodies — not
		// anonymous control of the fleet.
		slog.Warn("no API_TOKEN set — the collector API is UNAUTHENTICATED and discloses every CronJob, run outcome and captured log body it holds; keep it on a ClusterIP Service reached by port-forward",
			"remediation", "set API_TOKEN")
		if cfg.OIDC.Enabled {
			slog.Warn("OIDC_ENABLED is set but has no effect in server mode: there is no browser flow to run", "remediation", "set API_TOKEN instead")
		}
	}

	// 10. Prometheus state collector. Republishes the gauge-valued metrics from
	// stored state on a ticker so that they survive a restart: the watcher
	// wiring alone only reacts to live events, leaving run-outcome gauges with
	// no series at all until each CronJob next fires.
	metrics.SetBuildInfo(version.Version)
	go metrics.NewStateCollector(store, metrics.DefaultCollectInterval).Run(ctx)

	// 11. HTTP server.
	info := api.CollectorInfo{
		Mode:                  mode,
		RetentionDays:         cfg.RetentionDays,
		LogRetentionDays:      cfg.LogRetentionDays,
		SampleIntervalSeconds: cfg.MetricsSampleInterval,
	}
	srv := api.NewServer(store, mgr.Registry(), broadcaster, cacheSynced, authenticator, info, cfg.APIToken)

	go func() {
		slog.Info("http server listening", "port", cfg.Port, "mode", string(mode))
		if err := srv.Start(cfg.Port); err != nil {
			slog.Error("http server exited", "err", err)
		}
	}()

	// 12. Block until shutdown signal, then drain gracefully.
	<-ctx.Done()
	slog.Info("shutdown signal received, draining (timeout 30s)…")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown error", "err", err)
	}

	// Close after the HTTP server has drained: SQLite checkpoints the WAL into
	// the main file on the last connection close.
	if err := store.Close(); err != nil {
		slog.Error("database close error", "err", err)
	}

	slog.Info("kubecron stopped")
}
