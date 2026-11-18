// Package mail sends Prismaport's transactional email (sign-up codes, password resets, security
// notices): an SMTP sender for real delivery, a log-only sender and an in-memory recorder for
// tests, and the branded templates.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prismaport/prismaport/internal/safe"
)

// Message is one email to one recipient. HTML may reference Inline parts as cid:<ContentID>.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
	Inline  []Inline
	// Kind names the template ("verify_email", ...) for logs and tests; it is not sent.
	Kind string
}

// Inline is an image carried in the message and shown by the HTML part.
type Inline struct {
	ContentID   string
	ContentType string
	Filename    string
	Data        []byte
}

// Sender delivers a message. Implementations must be safe for concurrent use.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// ErrInvalidMessage is returned, wrapped with the reason, when a message has no plain recipient, a
// multi-line subject or no plain-text body.
var ErrInvalidMessage = errors.New("mail: invalid message")

// Validate refuses a message that cannot be sent safely: one plain recipient address, a subject
// and a body without header-breaking characters.
func (m Message) Validate() error {
	a, err := mail.ParseAddress(m.To)
	if err != nil || a.Name != "" || a.Address != m.To {
		return fmt.Errorf("%w: recipient must be a plain address", ErrInvalidMessage)
	}
	if m.Subject == "" || strings.ContainsAny(m.Subject+m.To, "\r\n") {
		return fmt.Errorf("%w: subject and recipient must be single lines", ErrInvalidMessage)
	}
	if m.Text == "" {
		return fmt.Errorf("%w: a plain-text body is required", ErrInvalidMessage)
	}
	for _, in := range m.Inline {
		if in.ContentID == "" || strings.ContainsAny(in.ContentID+in.ContentType+in.Filename, "\r\n<>\"") {
			return fmt.Errorf("%w: inline part %q", ErrInvalidMessage, in.ContentID)
		}
	}
	return nil
}

// Build renders the message as RFC 5322 bytes: multipart/alternative with the plain text first,
// then the HTML with its inline images in multipart/related.
func Build(from string, m Message, now time.Time) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	fa, err := mail.ParseAddress(from)
	if err != nil {
		return nil, fmt.Errorf("mail: from address %q: %w", from, err)
	}
	domain := "prismaport.local"
	if at := strings.LastIndexByte(fa.Address, '@'); at >= 0 {
		domain = fa.Address[at+1:]
	}
	var id [12]byte
	_, _ = rand.Read(id[:])
	header := []string{
		"From: " + fa.String(),
		"To: " + m.To,
		"Subject: " + mime.QEncoding.Encode("utf-8", m.Subject),
		"Date: " + now.UTC().Format(time.RFC1123Z),
		"Message-ID: <" + hex.EncodeToString(id[:]) + "@" + domain + ">",
		"MIME-Version: 1.0",
		"Auto-Submitted: auto-generated",
	}
	var body bytes.Buffer
	if m.HTML == "" {
		header = append(header, "Content-Type: text/plain; charset=utf-8", "Content-Transfer-Encoding: quoted-printable")
		if err := writeQP(&body, m.Text); err != nil {
			return nil, err
		}
	} else {
		alt := multipart.NewWriter(&body)
		textPart, err := alt.CreatePart(textproto.MIMEHeader{"Content-Type": {"text/plain; charset=utf-8"}, "Content-Transfer-Encoding": {"quoted-printable"}})
		if err != nil {
			return nil, err
		}
		if err := writeQP(textPart, m.Text); err != nil {
			return nil, err
		}
		var related bytes.Buffer
		rel := multipart.NewWriter(&related)
		htmlPart, err := rel.CreatePart(textproto.MIMEHeader{"Content-Type": {"text/html; charset=utf-8"}, "Content-Transfer-Encoding": {"quoted-printable"}})
		if err != nil {
			return nil, err
		}
		if err := writeQP(htmlPart, m.HTML); err != nil {
			return nil, err
		}
		for _, in := range m.Inline {
			p, err := rel.CreatePart(textproto.MIMEHeader{
				"Content-Type":              {in.ContentType},
				"Content-Transfer-Encoding": {"base64"},
				"Content-ID":                {"<" + in.ContentID + ">"},
				"Content-Disposition":       {`inline; filename="` + in.Filename + `"`},
			})
			if err != nil {
				return nil, err
			}
			if err := writeBase64(p, in.Data); err != nil {
				return nil, err
			}
		}
		if err := rel.Close(); err != nil {
			return nil, err
		}
		relPart, err := alt.CreatePart(textproto.MIMEHeader{"Content-Type": {`multipart/related; boundary="` + rel.Boundary() + `"`}})
		if err != nil {
			return nil, err
		}
		if _, err := relPart.Write(related.Bytes()); err != nil {
			return nil, err
		}
		if err := alt.Close(); err != nil {
			return nil, err
		}
		header = append(header, `Content-Type: multipart/alternative; boundary="`+alt.Boundary()+`"`)
	}
	out := []byte(strings.Join(header, "\r\n") + "\r\n\r\n")
	return append(out, body.Bytes()...), nil
}

func writeQP(w io.Writer, s string) error {
	q := quotedprintable.NewWriter(w)
	if _, err := q.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n"))); err != nil {
		return err
	}
	return q.Close()
}

func writeBase64(w io.Writer, data []byte) error {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		if _, err := io.WriteString(w, enc[:76]+"\r\n"); err != nil {
			return err
		}
		enc = enc[76:]
	}
	_, err := io.WriteString(w, enc+"\r\n")
	return err
}

// Memory records messages instead of sending them (tests).
type Memory struct {
	mu   sync.Mutex
	msgs []Message
	// Fail, when set, is returned by Send and nothing is recorded.
	Fail error
}

// Send validates the message and keeps it in memory.
func (m *Memory) Send(_ context.Context, msg Message) error {
	if err := msg.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.msgs = append(m.msgs, msg)
	return nil
}

// Sent lists every recorded message, oldest first.
func (m *Memory) Sent() []Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Message(nil), m.msgs...)
}

// To lists the messages sent to one address, oldest first.
func (m *Memory) To(addr string) []Message {
	var out []Message
	for _, msg := range m.Sent() {
		if strings.EqualFold(msg.To, addr) {
			out = append(out, msg)
		}
	}
	return out
}

// Log only logs that a message would have been sent: its kind and the recipient's domain, never
// the body, so codes never reach the logs.
type Log struct {
	Logger *slog.Logger
}

// Send validates the message and logs that it was not sent, with its kind and the recipient's
// domain.
func (l Log) Send(_ context.Context, m Message) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if l.Logger != nil {
		l.Logger.Warn("mail not sent: no SMTP server is configured", "kind", m.Kind, "to_domain", domainOf(m.To))
	}
	return nil
}

func domainOf(addr string) string {
	if at := strings.LastIndexByte(addr, '@'); at >= 0 {
		return addr[at+1:]
	}
	return ""
}

// Queue sends in the background so a request never waits on the mail server, and its timing
// does not depend on whether a message was sent. A message that still fails after the retries
// is logged and dropped; every flow that sends one can ask for it again.
type Queue struct {
	next    Sender
	log     *slog.Logger
	ch      chan Message
	retries []time.Duration
	wg      sync.WaitGroup
	// mu guards closed and every send on ch, so nothing is sent on ch once Close closed it.
	mu     sync.RWMutex
	closed bool
	// ctx is cancelled when Close's deadline passes: the send in flight is cut, retries stop and
	// what is left is dropped.
	ctx    context.Context
	cancel context.CancelFunc
}

// queueRetryDelays are the waits before each retry of a message that failed to send.
var queueRetryDelays = []time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second}

// NewQueue starts one delivery goroutine; Close stops it after the queue drains.
func NewQueue(next Sender, log *slog.Logger, size int) *Queue {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{next: next, log: log, ch: make(chan Message, size), retries: slices.Clone(queueRetryDelays),
		ctx: ctx, cancel: cancel}
	q.wg.Add(1)
	go q.run()
	return q
}

// Errors returned when the queue cannot accept a message.
var (
	ErrQueueFull   = errors.New("mail: queue full")
	ErrQueueClosed = errors.New("mail: queue closed")
)

// Send validates the message and queues it for the background sender without waiting.
func (q *Queue) Send(_ context.Context, m Message) error {
	if err := m.Validate(); err != nil {
		return err
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		// A request still finishing during shutdown: an error, never a send on a closed channel.
		q.log.Error("mail dropped: shutting down", "kind", m.Kind, "to_domain", domainOf(m.To))
		return ErrQueueClosed
	}
	select {
	case q.ch <- m:
		return nil
	default:
		// The caller logs the error: logging it here too would report one drop twice.
		return ErrQueueFull
	}
}

func (q *Queue) run() {
	defer q.wg.Done()
	for m := range q.ch {
		err := q.ctx.Err()
		for attempt := 0; err == nil; attempt++ {
			ctx, cancel := context.WithTimeout(q.ctx, time.Minute)
			// A sender that panics on one message fails that message, not the queue.
			err = safe.Call(func() error { return q.next.Send(ctx, m) })
			cancel()
			if err == nil || attempt >= len(q.retries) {
				break
			}
			t := time.NewTimer(q.retries[attempt])
			select {
			case <-t.C:
				err = nil // try again
			case <-q.ctx.Done():
				t.Stop()
			}
		}
		if err != nil {
			q.log.Error("mail not delivered", "kind", m.Kind, "to_domain", domainOf(m.To), "err", err)
		} else {
			q.log.Info("mail sent", "kind", m.Kind, "to_domain", domainOf(m.To))
		}
	}
}

// sendNowTimeout bounds a message sent while the request waits.
const sendNowTimeout = 15 * time.Second

// SendNow delivers m through the transport while the caller waits, bypassing the queue, so the
// caller learns whether the provider took it: for a message the request answers about (a
// confirmation code). One attempt; the user asks again.
func (q *Queue) SendNow(ctx context.Context, m Message) error {
	if err := m.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sendNowTimeout)
	defer cancel()
	err := safe.Call(func() error { return q.next.Send(ctx, m) })
	if err != nil {
		q.log.Error("mail not delivered", "kind", m.Kind, "to_domain", domainOf(m.To), "err", err)
	} else {
		q.log.Info("mail sent", "kind", m.Kind, "to_domain", domainOf(m.To))
	}
	return err
}

// SendNow delivers m while the caller waits: through the transport of a Queue, or with Send for
// a sender that has no queue (the transports themselves, tests).
func SendNow(ctx context.Context, s Sender, m Message) error {
	if q, ok := s.(interface {
		SendNow(context.Context, Message) error
	}); ok {
		return q.SendNow(ctx, m)
	}
	return s.Send(ctx, m)
}

// Close stops accepting messages and waits for the queued ones until ctx ends. Then the send in
// flight is cut, retries stop and the messages left are dropped (logged); Close returns ctx's
// error without waiting further, so a shutdown stays within its grace period.
func (q *Queue) Close(ctx context.Context) error {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.ch)
	}
	q.mu.Unlock()
	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		q.cancel()
		return nil
	case <-ctx.Done():
		q.cancel()
		return ctx.Err()
	}
}
