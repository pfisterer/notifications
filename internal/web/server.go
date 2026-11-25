// Package web is the HTTP side: the events the services post, the settings and recent notifications of the person signed in at the portal, the unsubscribe links, the admin triggers and the metrics.
package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/logging"
	"github.com/pfisterer/cloud-self-service-golib/oidcauth"
	"github.com/pfisterer/notifications/internal/engine"
	"github.com/pfisterer/notifications/internal/generated_docs"
	"github.com/pfisterer/notifications/internal/store"
	"go.uber.org/zap"
)

// TokenVerifier checks a person's ID token; *oidcauth.Verifier is one.
type TokenVerifier interface {
	Verify(ctx context.Context, rawIDToken string) (*authn.Claims, error)
}

// ServiceToken is the bearer token of one sending service.
type ServiceToken struct {
	Name  string
	Token string
}

// Config are the router's dependencies.
type Config struct {
	DevMode bool
	Log     *zap.SugaredLogger
	Engine  *engine.Engine
	Store   *store.Store
	// ServiceTokens may post events, each under its name.
	ServiceTokens []ServiceToken
	// AdminTokens may trigger a pass or the summaries (/v1/admin).
	AdminTokens []string
	// Verifier checks the ID tokens the portal's proxy forwards; nil (and no DevAuthUser) leaves the person routes out.
	Verifier TokenVerifier
	// DevAuthUser, only in development and without a Verifier: every person request without an Authorization header is this person.
	DevAuthUser    string
	TrustedProxies []string
	Health         *ginweb.Health
}

const maxBody = 1 << 20

// SetupRouter builds the API router.
func SetupRouter(cfg Config) (*gin.Engine, error) {
	if cfg.DevMode {
		gin.SetMode(gin.TestMode)
	} else {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		return nil, err
	}
	w := &logging.Writer{Logger: cfg.Log, Level: cfg.Log.Level()}
	gin.DefaultWriter, gin.DefaultErrorWriter = w, w
	r.Use(ginzap.RecoveryWithZap(cfg.Log.Desugar(), true), ginweb.Language(), ginweb.APISecurityHeaders())
	if cfg.DevMode {
		r.Use(ginweb.DisableCaching())
	}

	r.GET("/", func(c *gin.Context) {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, strings.ReplaceAll(indexHTML, "__VERSION__", generated_docs.Version))
	})
	r.GET("/swagger.json", func(c *gin.Context) {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.String(http.StatusOK, generated_docs.SwaggerJSON)
	})
	if cfg.Health != nil {
		cfg.Health.Mount(r)
	}

	h := &handlers{cfg: cfg}
	r.GET("/unsubscribe", h.unsubscribePage)
	r.POST("/unsubscribe", ginweb.LimitRequestBody(4096), h.unsubscribe)

	v1 := r.Group("/v1", ginweb.LimitRequestBody(maxBody), accessLog(cfg.Log))
	v1.POST("/events", serviceAuth(cfg.ServiceTokens, cfg.Log), h.postEvent)

	admin := v1.Group("/admin", adminAuth(cfg.AdminTokens, cfg.Log))
	admin.POST("/pass", h.adminPass)
	admin.POST("/digests", h.adminDigests)

	identify, err := personIdentity(cfg)
	if err != nil {
		return nil, err
	}
	if identify != nil {
		// Reached through the portal's proxy, same-origin; writes a page from elsewhere makes the browser send are refused.
		person := v1.Group("", ginweb.RejectCrossOriginWrites(ginweb.CrossOriginOptions{DevMode: cfg.DevMode, Log: cfg.Log}), personAuth(identify))
		person.GET("/preferences", h.getPreferences)
		person.PUT("/preferences", h.putPreferences)
		person.GET("/types", h.getTypes)
		person.GET("/notifications", h.getNotifications)
	}
	return r, nil
}

// MetricsRouter serves /metrics on its own port, which no ingress exposes.
func MetricsRouter(m *engine.Metrics, s *store.Store) *gin.Engine {
	r := gin.New()
	r.GET("/metrics", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
		defer cancel()
		b, err := m.Render(ctx, s)
		if err != nil {
			c.String(http.StatusInternalServerError, "metrics: %v", err)
			return
		}
		c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", b)
	})
	return r
}

const (
	senderKey = "notifySender"
	personKey = "notifyPerson"
)

func abort(c *gin.Context, status int, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"error": msg})
}

// serviceAuth accepts the bearer token of a sending service and remembers its name.
func serviceAuth(tokens []ServiceToken, log *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := authn.CutBearerPrefix(c.GetHeader("Authorization"))
		if !ok {
			abort(c, http.StatusUnauthorized, "missing or invalid Authorization Bearer header")
			return
		}
		// Every token is compared, in constant time, so the answer time says nothing about which one nearly matched.
		name := ""
		for _, t := range tokens {
			if authn.SecretEqual(raw, t.Token) {
				name = t.Name
			}
		}
		if name == "" {
			log.Warnw("events: unknown service token", "client", c.ClientIP())
			abort(c, http.StatusUnauthorized, "unknown service token")
			return
		}
		c.Set(senderKey, name)
		c.Next()
	}
}

func adminAuth(tokens []string, log *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := authn.CutBearerPrefix(c.GetHeader("Authorization"))
		match := false
		for _, t := range tokens {
			if ok && authn.SecretEqual(raw, t) {
				match = true
			}
		}
		if !match {
			log.Warnw("admin: refused", "client", c.ClientIP())
			abort(c, http.StatusUnauthorized, "admin token required")
			return
		}
		c.Next()
	}
}

type identifyFunc func(c *gin.Context) (string, bool)

func personIdentity(cfg Config) (identifyFunc, error) {
	dev := strings.ToLower(strings.TrimSpace(cfg.DevAuthUser))
	switch {
	case dev == "" && cfg.Verifier == nil:
		return nil, nil
	case dev == "":
		v := cfg.Verifier
		return func(c *gin.Context) (string, bool) {
			raw, ok := authn.CutBearerPrefix(c.GetHeader("Authorization"))
			if !ok {
				abort(c, http.StatusUnauthorized, "missing or invalid Authorization Bearer header")
				return "", false
			}
			claims, err := v.Verify(c.Request.Context(), raw)
			if err != nil {
				if errors.Is(err, oidcauth.ErrKeysUnavailable) {
					abort(c, http.StatusServiceUnavailable, "sign-in is temporarily unavailable: the identity provider cannot be reached")
					return "", false
				}
				abort(c, http.StatusUnauthorized, "invalid token")
				return "", false
			}
			id := claims.Identity()
			if id == "" {
				abort(c, http.StatusUnauthorized, "the token names nobody")
				return "", false
			}
			return id, true
		}, nil
	case !cfg.DevMode:
		return nil, errors.New("DEV_AUTH_USER is only allowed in development mode (API_MODE=development)")
	case cfg.Verifier != nil:
		return nil, errors.New("DEV_AUTH_USER and OIDC exclude each other")
	}
	cfg.Log.Warnw("DEVELOPMENT SIGN-IN ACTIVE: every person request without an Authorization header is this person", "dev_auth_user", dev)
	return func(c *gin.Context) (string, bool) {
		if c.GetHeader("Authorization") != "" {
			abort(c, http.StatusUnauthorized, "development sign-in: send no Authorization header")
			return "", false
		}
		return dev, true
	}, nil
}

// personAuth names the person a request comes from. Settings are always one's own: acting on behalf of somebody (X-Act-As) is not offered here.
func personAuth(identify identifyFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := identify(c)
		if !ok {
			return
		}
		if c.GetHeader("X-Act-As") != "" {
			abort(c, http.StatusForbidden, "notification settings are personal: they cannot be read or changed on behalf of somebody else")
			return
		}
		c.Set(personKey, id)
		c.Next()
	}
}

// accessLog writes one line per request, without bodies (addresses of people).
func accessLog(log *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.Infow("request", "method", c.Request.Method, "path", c.FullPath(), "status", c.Writer.Status(), "duration_ms", time.Since(start).Milliseconds(), "client", c.ClientIP())
	}
}

const indexHTML = `<!DOCTYPE html>
<html>
<head>
  <title>notifications</title>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1"/>
  <style>
    body { font: 15px/1.5 system-ui, sans-serif; margin: 3rem auto; max-width: 42rem; padding: 0 1rem; color: #222; }
    code { background: #f2f2f2; padding: .1rem .3rem; border-radius: 3px; }
    footer { margin-top: 3rem; color: #888; font-size: 12px; }
    @media (prefers-color-scheme: dark) { body { background: #111; color: #eee; } code { background: #222; } a { color: #7ab7ff; } }
  </style>
</head>
<body>
  <h1>notifications</h1>
  <p>Notification service of the DHBW Cloud self-service platform. Services post events to <code>POST /v1/events</code> with their service token; people manage their settings in the portal.</p>
  <p>API specification: <a href="swagger.json">swagger.json</a> (OpenAPI 2.0).</p>
  <footer>v__VERSION__</footer>
</body>
</html>`
