// Package app wires the notification service together.
package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/logging"
	"github.com/pfisterer/cloud-self-service-golib/oidcauth"
	"github.com/pfisterer/notifications/internal/engine"
	"github.com/pfisterer/notifications/internal/mail"
	"github.com/pfisterer/notifications/internal/roleprovider"
	"github.com/pfisterer/notifications/internal/store"
	"github.com/pfisterer/notifications/internal/unsub"
	"github.com/pfisterer/notifications/internal/web"
)

// Run starts the service and blocks until it is stopped.
func Run() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
		os.Exit(1)
	}
	_, log := logging.Init(cfg.DevMode)
	defer log.Sync() //nolint:errcheck
	log.Infow("Starting notifications", "mail_mode", cfg.MailMode, "senders", len(cfg.ServiceTokens))

	db, err := openDB(context.Background(), cfg.DBConnectionString, log)
	if err != nil {
		log.Fatalw("database", "error", err)
	}
	st, err := store.New(db)
	if err != nil {
		log.Fatalw("database schema", "error", err)
	}

	var deliverer mail.Deliverer
	if cfg.MailMode == "log" {
		deliverer = mail.Log{Log: log}
	} else {
		smtp, err := mail.NewSMTP(cfg.SMTP)
		if err != nil {
			log.Fatalw("smtp", "error", err)
		}
		deliverer = smtp
		log.Infow("mail over SMTP", "host", cfg.SMTP.Host, "port", cfg.SMTP.Port, "tls", cfg.SMTP.TLS, "from", cfg.SMTP.From)
	}

	var resolver roleprovider.Resolver = unresolvable{}
	if cfg.RoleProviderURL != "" {
		r, err := roleprovider.NewHTTP(cfg.RoleProviderURL, cfg.RoleProviderToken, 15*time.Second)
		if err != nil {
			log.Fatalw("role provider", "error", err)
		}
		resolver = r
	} else {
		log.Warnw("ROLE_PROVIDER_URL is not set: events naming a group wait until it is")
	}

	signer, err := unsub.New(cfg.UnsubscribeSecret)
	if err != nil {
		log.Fatalw("unsubscribe secret", "error", err)
	}

	eng := &engine.Engine{
		Store: st, Deliverer: deliverer, Resolver: resolver, Signer: signer, Policy: cfg.Policy,
		Links:   engine.Links{SettingsURL: cfg.SettingsURL, PublicURL: cfg.PublicURL, PortalURL: cfg.PortalURL},
		Metrics: engine.NewMetrics(), Log: log,
	}

	var verifier web.TokenVerifier
	if cfg.OIDC.IssuerURL != "" {
		v, err := oidcauth.New(oidcauth.Config{IssuerURL: cfg.OIDC.IssuerURL, ClientID: cfg.OIDC.ClientID, JWKSURL: cfg.OIDC.JWKSURL}, log)
		if err != nil {
			log.Fatalw("oidc verifier", "error", err)
		}
		verifier = v
	}

	health := &ginweb.Health{}
	health.AddCheck("database", st.Ping)

	router, err := web.SetupRouter(web.Config{
		DevMode: cfg.DevMode, Log: log, Engine: eng, Store: st,
		ServiceTokens: cfg.ServiceTokens, AdminTokens: cfg.AdminTokens,
		Verifier: verifier, DevAuthUser: cfg.DevAuthUser, TrustedProxies: cfg.TrustedProxies, Health: health,
	})
	if err != nil {
		log.Fatalw("router", "error", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go eng.Run(ctx, cfg.PassInterval)

	metricsSrv := &http.Server{Addr: cfg.MetricsBind, Handler: web.MetricsRouter(eng.Metrics, st), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Errorw("metrics server stopped", "error", err)
		}
	}()
	// The API drains on SIGTERM (ginweb.ServeUntil, up to ginweb.ShutdownGrace): events a service posts during a rollout are answered, not refused. The metrics server goes after it.
	srv := ginweb.NewServer(cfg.Bind, router)
	log.Infow("Listening", "bind", cfg.Bind, "metrics", cfg.MetricsBind)
	if err := ginweb.ServeUntil(ctx, srv, ginweb.ShutdownGrace); err != nil {
		log.Fatalw("server stopped", "error", err)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = metricsSrv.Shutdown(shutdown)
}

// unresolvable is the resolver without a role provider: groups cannot be resolved, so such events wait.
type unresolvable struct{}

func (unresolvable) Members(context.Context, string) ([]string, error) {
	return nil, fmt.Errorf("no role provider configured (ROLE_PROVIDER_URL)")
}
