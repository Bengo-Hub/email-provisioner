// Package app wires email-provisioner's dependencies and runtime lifecycle.
package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/email-provisioner/internal/config"
	events "github.com/bengobox/email-provisioner/internal/modules/events"
	"github.com/bengobox/email-provisioner/internal/platform/stalwart"
)

// App holds email-provisioner's wired dependencies.
type App struct {
	cfg    *config.Config
	log    *zap.Logger
	nats   *nats.Conn
	server *http.Server
}

// New builds the app: config, logger, Stalwart client, NATS connection +
// subscriptions, and a minimal health-check HTTP server.
func New(ctx context.Context) (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	log, err := zap.NewProduction()
	if err != nil {
		return nil, err
	}

	nc, err := nats.Connect(cfg.EventsNATSURL)
	if err != nil {
		return nil, err
	}

	stalwartClient := stalwart.New(cfg.StalwartAdminURL, cfg.StalwartAdminUser, cfg.StalwartAdminPassword)

	handler := events.New(log, stalwartClient)
	if err := handler.Subscribe(nc); err != nil {
		nc.Close()
		return nil, err
	}

	r := chi.NewRouter()
	r.Get("/healthz/live", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Get("/healthz/ready", func(w http.ResponseWriter, r *http.Request) {
		if nc.IsConnected() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	// Internal-only, unauthenticated (cluster-network-scoped, no Ingress exists
	// for this service — see devops-k8s/apps/email-provisioner/values.yaml's
	// ingress.enabled: false). Consumed by auth-api's lightweight platform
	// monitoring dashboard (internal/clients/k8s/monitor.go), which otherwise
	// has no way to see mail-specific signals like queue depth.
	r.Get("/internal/mail-stats", func(w http.ResponseWriter, r *http.Request) {
		stats, err := stalwartClient.QueueStats(r.Context())
		if err != nil {
			log.Error("mail-stats query failed", zap.Error(err))
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stats)
	})

	return &App{
		cfg:  cfg,
		log:  log,
		nats: nc,
		server: &http.Server{
			Addr:    ":" + strconv.Itoa(cfg.Port),
			Handler: r,
		},
	}, nil
}

// Run starts the health-check HTTP server and blocks until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		if err := a.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		return a.server.Shutdown(context.Background())
	case err := <-errCh:
		return err
	}
}

// Close releases the app's connections.
func (a *App) Close() {
	if a.nats != nil {
		a.nats.Drain()
	}
	if a.log != nil {
		_ = a.log.Sync()
	}
}
