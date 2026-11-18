package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

// ResendBaseURL is Resend's HTTPS API.
const ResendBaseURL = "https://api.resend.com"

// ResendConfig sends through Resend's HTTPS API, for hosts that block outbound SMTP ports
// (Railway's Hobby plan drops 25, 465, 587 and 2525).
type ResendConfig struct {
	APIKey string
	// From is the sender, "Prismaport <no-reply@example.com>", on a domain verified in Resend.
	From string
	// BaseURL defaults to ResendBaseURL; plain http is only allowed to this machine (tests).
	BaseURL string
	// Timeout bounds one request.
	Timeout time.Duration
}

// Check validates the configuration.
func (c ResendConfig) Check() error {
	if c.APIKey == "" || strings.ContainsAny(c.APIKey, "\r\n ") {
		return errors.New("mail: Resend API key is required and must be a single token")
	}
	if _, err := mail.ParseAddress(c.From); err != nil || strings.ContainsAny(c.From, "\r\n") {
		return fmt.Errorf("mail: sender %q is not a valid address", c.From)
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("mail: Resend base URL %q is not valid", c.BaseURL)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback(u.Hostname())) {
		return fmt.Errorf("mail: Resend base URL must be https, not %s", u.Scheme)
	}
	return nil
}

// Resend delivers each message with one API request.
type Resend struct {
	cfg    ResendConfig
	client *http.Client
}

// NewResend returns a sender that delivers through the Resend HTTP API.
func NewResend(cfg ResendConfig) (*Resend, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = ResendBaseURL
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Resend{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}, nil
}

type resendAttachment struct {
	Content     string `json:"content"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type,omitempty"`
	ContentID   string `json:"content_id"`
}

type resendEmail struct {
	From        string             `json:"from"`
	To          []string           `json:"to"`
	Subject     string             `json:"subject"`
	Text        string             `json:"text"`
	HTML        string             `json:"html,omitempty"`
	Attachments []resendAttachment `json:"attachments,omitempty"`
}

// Send validates the message and delivers it through the Resend API.
func (r *Resend) Send(ctx context.Context, m Message) error {
	if err := m.Validate(); err != nil {
		return err
	}
	body := resendEmail{From: r.cfg.From, To: []string{m.To}, Subject: m.Subject, Text: m.Text, HTML: m.HTML}
	for _, in := range m.Inline {
		body.Attachments = append(body.Attachments, resendAttachment{Content: base64.StdEncoding.EncodeToString(in.Data),
			Filename: in.Filename, ContentType: in.ContentType, ContentID: in.ContentID})
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("mail: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.BaseURL+"/emails", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("mail: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "prismaport")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("mail: Resend: %w", err)
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Name    string `json:"name"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(reply, &e)
		return fmt.Errorf("mail: Resend answered %d %s: %s", resp.StatusCode, e.Name, e.Message)
	}
	return nil
}
