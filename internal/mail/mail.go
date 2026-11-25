// Package mail delivers messages. Deliverer is the interface every channel implements; e-mail over SMTP is the first one, further channels (Matrix, Telegram) are meant to be added beside it.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Message is one message to one person.
type Message struct {
	// To is the recipient's address (for e-mail) or whatever the channel addresses people by.
	To      string
	Subject string
	Text    string
	HTML    string
	// Headers are additional headers (List-Unsubscribe and the like); channels without headers ignore them.
	Headers map[string]string
}

// Deliverer sends messages over one channel.
type Deliverer interface {
	// Channel names the channel ("email") for logs and metrics.
	Channel() string
	// Deliver sends m. An error means it was not sent and may be tried again.
	Deliver(ctx context.Context, m Message) error
}

// SMTPConfig is where and as whom mails are sent.
type SMTPConfig struct {
	Host string
	Port int
	// TLS is "starttls" (use it when the server offers it, required when Username is set), "tls" (implicit TLS, port 465) or "none".
	TLS string
	// Username and Password, if set, log in (PLAIN, only over TLS). The DHBW smarthost lets the cluster's pods relay on port 25 without login.
	Username string
	Password string
	// From is the sender address, FromName its display name.
	From     string
	FromName string
	// ReplyTo, if set, is where answers go.
	ReplyTo string
	// HeloName is the name given in EHLO; empty = "localhost".
	HeloName string
	Timeout  time.Duration
}

// SMTP delivers e-mail over SMTP.
type SMTP struct {
	cfg SMTPConfig
}

// NewSMTP checks cfg and returns the deliverer.
func NewSMTP(cfg SMTPConfig) (*SMTP, error) {
	if cfg.Host == "" || cfg.Port <= 0 {
		return nil, fmt.Errorf("SMTP host and port are required")
	}
	if cfg.From == "" {
		return nil, fmt.Errorf("SMTP sender (from) is required")
	}
	switch cfg.TLS {
	case "", "starttls":
		cfg.TLS = "starttls"
	case "tls", "none":
	default:
		return nil, fmt.Errorf("SMTP TLS must be starttls, tls or none, not %q", cfg.TLS)
	}
	if cfg.Username != "" && cfg.TLS == "none" {
		return nil, fmt.Errorf("SMTP login needs TLS (starttls or tls)")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.HeloName == "" {
		cfg.HeloName = "localhost"
	}
	return &SMTP{cfg: cfg}, nil
}

// Channel is "email".
func (s *SMTP) Channel() string { return "email" }

// Deliver sends m.
func (s *SMTP) Deliver(ctx context.Context, m Message) error {
	raw, err := Compose(s.cfg, m, time.Now())
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	dialer := &net.Dialer{Timeout: s.cfg.Timeout}
	var conn net.Conn
	if s.cfg.TLS == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp connect %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(s.cfg.Timeout))
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp greeting: %w", err)
	}
	defer c.Close()
	if err := c.Hello(s.cfg.HeloName); err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}
	if s.cfg.TLS == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
				return fmt.Errorf("smtp starttls: %w", err)
			}
		} else if s.cfg.Username != "" {
			return fmt.Errorf("smtp: server offers no STARTTLS, refusing to log in in clear text")
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	if err := c.Rcpt(m.To); err != nil {
		return fmt.Errorf("smtp RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp end of data: %w", err)
	}
	return c.Quit()
}

// Compose writes m as a MIME message (multipart/alternative, text and HTML, quoted-printable).
func Compose(cfg SMTPConfig, m Message, now time.Time) ([]byte, error) {
	if strings.ContainsAny(m.To, "\r\n") || strings.ContainsAny(cfg.From, "\r\n") {
		return nil, fmt.Errorf("address contains a line break")
	}
	boundary := randomHex(12)
	var b bytes.Buffer
	h := func(k, v string) {
		// Header values come from our own texts and configuration; line breaks are removed anyway, so nothing can add a header.
		v = strings.NewReplacer("\r", " ", "\n", " ").Replace(v)
		b.WriteString(k + ": " + v + "\r\n")
	}
	from := cfg.From
	if cfg.FromName != "" {
		from = mime.QEncoding.Encode("utf-8", cfg.FromName) + " <" + cfg.From + ">"
	}
	h("From", from)
	h("To", m.To)
	if cfg.ReplyTo != "" {
		h("Reply-To", cfg.ReplyTo)
	}
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", now.Format(time.RFC1123Z))
	domain := "localhost"
	if at := strings.LastIndex(cfg.From, "@"); at >= 0 {
		domain = cfg.From[at+1:]
	}
	h("Message-ID", "<"+randomHex(16)+"@"+domain+">")
	h("MIME-Version", "1.0")
	// Mark the mail as automatic, so out-of-office replies and similar stay away (RFC 3834, Exchange).
	h("Auto-Submitted", "auto-generated")
	h("X-Auto-Response-Suppress", "All")
	for k, v := range m.Headers {
		h(k, v)
	}
	h("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	part := func(ctype, body string) error {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: " + ctype + "; charset=utf-8\r\n")
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		qp := quotedprintable.NewWriter(&b)
		if _, err := qp.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n"))); err != nil {
			return err
		}
		if err := qp.Close(); err != nil {
			return err
		}
		b.WriteString("\r\n")
		return nil
	}
	if err := part("text/plain", m.Text); err != nil {
		return nil, err
	}
	if m.HTML != "" {
		if err := part("text/html", m.HTML); err != nil {
			return nil, err
		}
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), nil
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// Log only writes what it would send into the log: for development, and for an environment that must not mail anybody.
type Log struct {
	Log *zap.SugaredLogger
}

// Channel is "log".
func (l Log) Channel() string { return "log" }

// Deliver logs m's recipient and subject.
func (l Log) Deliver(_ context.Context, m Message) error {
	l.Log.Infow("mail (log only, not sent)", "to", m.To, "subject", m.Subject)
	return nil
}

// Recorder keeps every message, for tests. Fail, while set, makes Deliver fail.
type Recorder struct {
	mu   sync.Mutex
	Sent []Message
	Fail error
}

// Channel is "test".
func (r *Recorder) Channel() string { return "test" }

// Deliver records m.
func (r *Recorder) Deliver(_ context.Context, m Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Fail != nil {
		return r.Fail
	}
	r.Sent = append(r.Sent, m)
	return nil
}

// Messages is a copy of what was sent.
func (r *Recorder) Messages() []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Message(nil), r.Sent...)
}

// Reset forgets what was sent.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Sent = nil
}
