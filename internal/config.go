package app

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/pfisterer/cloud-self-service-golib/envconf"
	"github.com/pfisterer/notifications/internal/engine"
	"github.com/pfisterer/notifications/internal/mail"
	"github.com/pfisterer/notifications/internal/web"
)

// Config is the service's configuration, read from the environment (and a .env file in development).
type Config struct {
	DevMode bool
	// Bind is the API's address (API_BIND, default :8087), MetricsBind the one of /metrics (METRICS_BIND, default :8088), which no ingress exposes.
	Bind        string
	MetricsBind string
	// DBConnectionString is the PostgreSQL DSN (DB_CONNECTION_STRING).
	DBConnectionString string
	// ServiceTokens may post events (NOTIFY_SERVICE_TOKENS, "name=token,…"): one per sending service, the name is how the service is recognised.
	ServiceTokens []web.ServiceToken
	// AdminTokens may trigger a pass or the summaries (NOTIFY_ADMIN_TOKENS, comma-separated).
	AdminTokens []string
	// UnsubscribeSecret signs the unsubscribe links (UNSUBSCRIBE_SECRET, at least 32 characters). Changing it invalidates the links in every mail sent so far.
	UnsubscribeSecret string
	// PublicURL is where people reach this service (PUBLIC_URL), for the unsubscribe links.
	PublicURL string
	// SettingsURL is the portal's notification settings page (SETTINGS_URL).
	SettingsURL string
	// PortalURL is the portal (PORTAL_URL), for links to objects sent without a url.
	PortalURL string
	// MailMode is smtp or log (MAIL_MODE): log only writes what would be sent.
	MailMode string
	SMTP     mail.SMTPConfig
	// RoleProviderURL and RoleProviderToken resolve group recipients (ROLE_PROVIDER_URL, ROLE_PROVIDER_TOKEN: a resolve or read token of the role provider). Without a URL an event naming a group waits until one is configured.
	RoleProviderURL   string
	RoleProviderToken string
	OIDC              OIDCConfig
	DevAuthUser       string
	TrustedProxies    []string
	Policy            engine.Policy
	// PassInterval is the time between two delivery passes (PASS_INTERVAL_SECONDS, default 30).
	PassInterval time.Duration
}

// OIDCConfig is the identity provider whose ID tokens the person routes accept: the portal's.
type OIDCConfig struct {
	IssuerURL string
	ClientID  string
	JWKSURL   string
}

func loadConfig() (Config, error) {
	if _, err := os.Stat(".env"); err == nil {
		if err := godotenv.Overload(".env"); err != nil {
			return Config{}, fmt.Errorf("failed to load .env: %w", err)
		}
	}
	cfg := Config{
		DevMode:            envconf.String("API_MODE", "production") == "development",
		Bind:               envconf.String("API_BIND", ":8087"),
		MetricsBind:        envconf.String("METRICS_BIND", ":8088"),
		DBConnectionString: envconf.String("DB_CONNECTION_STRING", ""),
		AdminTokens:        envconf.StringSlice("NOTIFY_ADMIN_TOKENS", nil),
		UnsubscribeSecret:  envconf.String("UNSUBSCRIBE_SECRET", ""),
		PublicURL:          strings.TrimRight(envconf.String("PUBLIC_URL", ""), "/"),
		SettingsURL:        envconf.String("SETTINGS_URL", ""),
		PortalURL:          strings.TrimRight(envconf.String("PORTAL_URL", ""), "/"),
		MailMode:           envconf.String("MAIL_MODE", "smtp"),
		SMTP: mail.SMTPConfig{
			Host:     envconf.String("SMTP_HOST", ""),
			Port:     envconf.Int("SMTP_PORT", 25),
			TLS:      envconf.String("SMTP_TLS", "starttls"),
			Username: envconf.String("SMTP_USERNAME", ""),
			Password: envconf.String("SMTP_PASSWORD", ""),
			From:     envconf.String("SMTP_FROM", ""),
			FromName: envconf.String("SMTP_FROM_NAME", "DHBW Cloud"),
			ReplyTo:  envconf.String("SMTP_REPLY_TO", ""),
			HeloName: envconf.String("SMTP_HELO_NAME", ""),
		},
		RoleProviderURL:   envconf.String("ROLE_PROVIDER_URL", ""),
		RoleProviderToken: envconf.String("ROLE_PROVIDER_TOKEN", ""),
		OIDC: OIDCConfig{
			IssuerURL: envconf.String("OIDC_ISSUER_URL", ""),
			ClientID:  envconf.String("OIDC_CLIENT_ID", ""),
			JWKSURL:   envconf.String("OIDC_JWKS_URL", ""),
		},
		DevAuthUser:    strings.ToLower(envconf.String("DEV_AUTH_USER", "")),
		TrustedProxies: envconf.StringSlice("TRUSTED_PROXIES", nil),
		PassInterval:   time.Duration(envconf.Int("PASS_INTERVAL_SECONDS", 30)) * time.Second,
	}
	tokens, err := parseServiceTokens(envconf.StringSlice("NOTIFY_SERVICE_TOKENS", nil))
	if err != nil {
		return Config{}, fmt.Errorf("NOTIFY_SERVICE_TOKENS: %w", err)
	}
	cfg.ServiceTokens = tokens
	for _, a := range cfg.AdminTokens {
		for _, s := range tokens {
			if a == s.Token {
				return Config{}, fmt.Errorf("NOTIFY_ADMIN_TOKENS: an admin token is also the service token of %q", s.Name)
			}
		}
	}

	p := engine.DefaultPolicy()
	if tz := envconf.String("TIMEZONE", "Europe/Berlin"); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return Config{}, fmt.Errorf("TIMEZONE: %w", err)
		}
		p.Location = loc
	}
	p.BundleCollect = time.Duration(envconf.Int("BUNDLE_COLLECT_SECONDS", int(p.BundleCollect/time.Second))) * time.Second
	p.BundleWindow = time.Duration(envconf.Int("BUNDLE_WINDOW_MINUTES", int(p.BundleWindow/time.Minute))) * time.Minute
	p.DailyLimit = envconf.Int("DAILY_LIMIT", p.DailyLimit)
	p.DigestHour = envconf.Int("DIGEST_HOUR", p.DigestHour)
	p.Retention = time.Duration(envconf.Int("RETENTION_DAYS", int(p.Retention/(24*time.Hour)))) * 24 * time.Hour
	if days := envconf.StringSlice("REMINDER_DAYS", nil); len(days) > 0 {
		p.ReminderAfter = nil
		for _, d := range days {
			var n int
			if _, err := fmt.Sscanf(d, "%d", &n); err != nil || n <= 0 {
				return Config{}, fmt.Errorf("REMINDER_DAYS: %q is not a positive number of days", d)
			}
			p.ReminderAfter = append(p.ReminderAfter, time.Duration(n)*24*time.Hour)
		}
	}
	cfg.Policy = p

	if err := cfg.check(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) check() error {
	if c.DBConnectionString == "" {
		return fmt.Errorf("DB_CONNECTION_STRING must be set")
	}
	if len(c.ServiceTokens) == 0 {
		return fmt.Errorf("NOTIFY_SERVICE_TOKENS must name at least one service (name=token,…)")
	}
	if len(c.UnsubscribeSecret) < 32 {
		return fmt.Errorf("UNSUBSCRIBE_SECRET must be at least 32 characters")
	}
	if c.PublicURL == "" {
		return fmt.Errorf("PUBLIC_URL must be set (the unsubscribe links point there)")
	}
	switch c.MailMode {
	case "smtp":
		if c.SMTP.Host == "" || c.SMTP.From == "" {
			return fmt.Errorf("MAIL_MODE=smtp needs SMTP_HOST and SMTP_FROM")
		}
	case "log":
	default:
		return fmt.Errorf("MAIL_MODE must be smtp or log, got %q", c.MailMode)
	}
	if c.OIDC.IssuerURL != "" && c.OIDC.ClientID == "" {
		return fmt.Errorf("OIDC_CLIENT_ID must be set with OIDC_ISSUER_URL")
	}
	if c.DevAuthUser != "" && !c.DevMode {
		return fmt.Errorf("DEV_AUTH_USER is only allowed with API_MODE=development")
	}
	if c.DevAuthUser != "" && c.OIDC.IssuerURL != "" {
		return fmt.Errorf("DEV_AUTH_USER and OIDC_ISSUER_URL exclude each other")
	}
	if c.PassInterval < time.Second {
		return fmt.Errorf("PASS_INTERVAL_SECONDS must be at least 1")
	}
	return nil
}

// parseServiceTokens reads "name=token" entries; a token shorter than 32 characters or used twice is refused.
func parseServiceTokens(entries []string) ([]web.ServiceToken, error) {
	seenName, seenToken := map[string]bool{}, map[string]bool{}
	var out []web.ServiceToken
	for i, e := range entries {
		name, token, ok := strings.Cut(e, "=")
		name, token = strings.TrimSpace(name), strings.TrimSpace(token)
		if !ok || name == "" || token == "" {
			return nil, fmt.Errorf("entry %d is not name=token", i+1)
		}
		if len(token) < 32 {
			return nil, fmt.Errorf("the token of %q is shorter than 32 characters", name)
		}
		if seenName[name] {
			return nil, fmt.Errorf("%q is named twice", name)
		}
		if seenToken[token] {
			return nil, fmt.Errorf("the token of %q is used for another service too", name)
		}
		seenName[name], seenToken[token] = true, true
		out = append(out, web.ServiceToken{Name: name, Token: token})
	}
	return out, nil
}
