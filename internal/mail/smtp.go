package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// TLS modes of an SMTP server.
const (
	TLSStartTLS = "starttls"
	TLSImplicit = "tls"
	TLSNone     = "none"
)

// SMTPConfig is an SMTP relay. Plain connections (TLSNone) are only allowed to this machine,
// where a local catcher such as Mailpit listens: codes never cross a network in clear text.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	TLS      string
	// From is the sender, "Prismaport <no-reply@example.com>".
	From string
	// Timeout bounds one delivery (dial to QUIT).
	Timeout time.Duration
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// Check validates the configuration.
func (c SMTPConfig) Check() error {
	if c.Host == "" {
		return errors.New("mail: SMTP host is required")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("mail: SMTP port %d is not valid", c.Port)
	}
	switch c.TLS {
	case TLSStartTLS, TLSImplicit:
	case TLSNone:
		if !loopback(c.Host) {
			return fmt.Errorf("mail: plain SMTP (no TLS) is only allowed to localhost, not %s", c.Host)
		}
	default:
		return fmt.Errorf("mail: SMTP TLS mode must be starttls, tls or none, got %q", c.TLS)
	}
	if (c.Username == "") != (c.Password == "") {
		return errors.New("mail: SMTP username and password are set together")
	}
	if _, err := mail.ParseAddress(c.From); err != nil || strings.ContainsAny(c.From, "\r\n") {
		return fmt.Errorf("mail: sender %q is not a valid address", c.From)
	}
	return nil
}

// SMTP delivers each message over its own connection.
type SMTP struct {
	cfg SMTPConfig
	// from is the envelope sender: cfg.From's address, parsed once by NewSMTP.
	from string
	now  func() time.Time
}

// NewSMTP returns a sender that delivers through an SMTP server.
func NewSMTP(cfg SMTPConfig) (*SMTP, error) {
	if cfg.TLS == "" {
		cfg.TLS = TLSStartTLS
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return nil, fmt.Errorf("mail: sender %q is not a valid address", cfg.From)
	}
	return &SMTP{cfg: cfg, from: from.Address, now: time.Now}, nil
}

// Send builds the message and delivers it through the SMTP server.
func (s *SMTP) Send(ctx context.Context, m Message) error {
	raw, err := Build(s.cfg.From, m, s.now())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mail: dial %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	tlsConf := &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}
	if s.cfg.TLS == TLSImplicit {
		conn = tls.Client(conn, tlsConf)
	}
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mail: greeting: %w", err)
	}
	defer c.Close()
	if err := c.Hello("localhost"); err != nil {
		return fmt.Errorf("mail: EHLO: %w", err)
	}
	if s.cfg.TLS == TLSStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("mail: the SMTP server does not offer STARTTLS")
		}
		if err := c.StartTLS(tlsConf); err != nil {
			return fmt.Errorf("mail: STARTTLS: %w", err)
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return fmt.Errorf("mail: AUTH: %w", err)
		}
	}
	if err := c.Mail(s.from); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(m.To); err != nil {
		return fmt.Errorf("mail: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("mail: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: end of data: %w", err)
	}
	return c.Quit()
}
