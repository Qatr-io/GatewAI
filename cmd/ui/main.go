package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"gatewai/gateway/internal/auth"
	"gatewai/gateway/internal/config"
	"gatewai/gateway/internal/pgstore"
	"gatewai/gateway/internal/storage"
	"gatewai/gateway/internal/ui"
)

var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfgPath := "config.yaml"
	if v := os.Getenv("CONFIG_PATH"); v != "" {
		cfgPath = v
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// PostgreSQL — fatal when DSN is configured but unreachable.
	var store *pgstore.Store
	if cfg.Postgres.DSN != "" {
		store, err = pgstore.New(ctx, cfg.Postgres.DSN, cfg.Postgres.MaxConns, cfg.Postgres.ConnectTimeout)
		if err != nil {
			slog.Error("failed to connect to postgres", "error", err)
			os.Exit(1)
		}
		defer store.Close()
		slog.Info("postgres connected")
	} else {
		slog.Warn("postgres not configured; historical data unavailable")
	}

	// Redis — read-only usage for live quota display.
	redisClient, err := storage.NewRedis(cfg.Redis, cfg.Lifecycle)
	if err != nil {
		slog.Error("failed to connect to redis", "error", err)
		os.Exit(1)
	}
	defer redisClient.Close()

	// Auth — same mechanism as the gateway.
	authenticator, err := buildAuthenticator(ctx, cfg)
	if err != nil {
		slog.Error("failed to initialise authenticator", "error", err)
		os.Exit(1)
	}
	if cfg.Auth.Mode != "" {
		slog.Info("authentication enabled", "mode", cfg.Auth.Mode)
	}

	basePath := strings.TrimRight(cfg.UI.BasePath, "/")

	// UI handler.
	h, err := ui.New(store, ui.NewRedisReader(redisClient.Raw()), cfg.UI.AdminGroups, cfg.UI.AdminRoles, cfg.RateLimits, basePath)
	if err != nil {
		slog.Error("failed to initialise ui handler", "error", err)
		os.Exit(1)
	}

	// Router.
	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(chimw.Recoverer)

	// /healthz is always at root for k8s probes (no proxy in between).
	r.Get("/healthz", h.Health)

	staticStrip := basePath + "/static/"

	// Auth middleware exempt list covers health + static.
	if authenticator != nil {
		exempt := []string{"/healthz", basePath + "/static"}
		r.Use(auth.Middleware(authenticator, cfg.Auth.Mode, exempt, cfg.Server.ConsumerHeader, cfg.Server.UserTypeHeader))
	}

	// Static assets are registered at the absolute pattern regardless of basePath.
	r.Handle(basePath+"/static/*", http.StripPrefix(staticStrip, ui.StaticHandler()))

	if basePath == "" {
		r.Get("/", h.Dashboard)
		r.Get("/history", h.History)
		r.Get("/partials/quota", h.QuotaPartial)
		r.Get("/admin", h.Admin)
		r.Get("/admin/consumer/{name}", h.AdminConsumer)
	} else {
		r.Route(basePath, func(sub chi.Router) {
			sub.Get("/", h.Dashboard)
			sub.Get("/history", h.History)
			sub.Get("/partials/quota", h.QuotaPartial)
			sub.Get("/admin", h.Admin)
			sub.Get("/admin/consumer/{name}", h.AdminConsumer)
		})
	}

	addr := cfg.UI.Addr
	slog.Info("ui server starting", "addr", addr, "version", version)

	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		slog.Error("server error", "error", err)
	case sig := <-quit:
		slog.Info("shutdown signal received", "signal", sig)
	}

	slog.Info("shutting down…")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server forced to shutdown", "error", err)
	}
	slog.Info("ui server stopped")
}

// buildAuthenticator mirrors the same function in cmd/gateway/main.go.
// Both binaries share the same auth configuration block.
func buildAuthenticator(ctx context.Context, cfg *config.Config) (auth.Authenticator, error) {
	switch cfg.Auth.Mode {
	case "oauth2":
		c := cfg.Auth.OAuth2
		var introspectionCfg *auth.IntrospectionConfig
		if c.Introspection != nil {
			cacheTTL := 60 * time.Second
			if c.Introspection.CacheTTL != "" {
				if d, err := time.ParseDuration(c.Introspection.CacheTTL); err == nil {
					cacheTTL = d
				}
			}
			introspectionCfg = &auth.IntrospectionConfig{
				Endpoint:     c.Introspection.Endpoint,
				ClientID:     c.Introspection.ClientID,
				ClientSecret: c.Introspection.ClientSecret,
				CacheTTL:     cacheTTL,
			}
		}
		a, err := auth.NewOAuth2Authenticator(ctx, auth.OAuth2Config{
			Issuer:    c.Issuer,
			JWKSURL:   c.JWKSURL,
			Audiences: c.Audiences,
			Claims: auth.ClaimMap{
				Subject:  c.Claims.Subject,
				Consumer: c.Claims.Consumer,
				Scopes:   c.Claims.Scopes,
				Groups:   c.Claims.Groups,
				Roles:    c.Claims.Roles,
			},
			Validation:    c.Validation,
			Introspection: introspectionCfg,
		})
		if err != nil {
			return nil, err
		}
		return a, nil

	case "proxy":
		p := cfg.Auth.Proxy
		consumerHeader := p.ConsumerHeader
		if consumerHeader == "" {
			consumerHeader = cfg.Server.ConsumerHeader
		}
		userTypeHeader := p.UserTypeHeader
		if userTypeHeader == "" {
			userTypeHeader = cfg.Server.UserTypeHeader
		}
		return auth.NewHeaderAuthenticator(auth.HeaderConfig{
			ConsumerHeader: consumerHeader,
			UserTypeHeader: userTypeHeader,
			GroupsHeader:   p.GroupsHeader,
			RolesHeader:    p.RolesHeader,
			ScopesHeader:   p.ScopesHeader,
		}), nil

	default: // "" — no auth
		return nil, nil
	}
}
