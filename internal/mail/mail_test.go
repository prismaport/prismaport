package mail

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func sample() Message {
	return Message{To: "owner@example.com", Subject: "Confirm your email for Prismaport", Kind: KindVerifyEmail,
		Text: "Your code:\n\n    482193\n", HTML: `<p>Your code: <b>482193</b></p><img src="cid:prismaport-icon">`,
		Inline: []Inline{{ContentID: IconContentID, ContentType: "image/png", Filename: "prismaport.png", Data: []byte("\x89PNG fake")}}}
}

func TestValidateRefusesHeaderInjection(t *testing.T) {
	for name, m := range map[string]Message{
		"display name":     {To: "Owner <owner@example.com>", Subject: "s", Text: "t"},
		"two recipients":   {To: "a@example.com, b@example.com", Subject: "s", Text: "t"},
		"CRLF in subject":  {To: "owner@example.com", Subject: "s\r\nBcc: x@example.com", Text: "t"},
		"CRLF in address":  {To: "owner@example.com\r\nBcc: x@example.com", Subject: "s", Text: "t"},
		"no text body":     {To: "owner@example.com", Subject: "s"},
		"bad inline id":    {To: "owner@example.com", Subject: "s", Text: "t", Inline: []Inline{{ContentID: "a>\r\nX: y"}}},
		"not an address":   {To: "owner", Subject: "s", Text: "t"},
		"empty subject":    {To: "owner@example.com", Text: "t"},
		"empty recipient":  {Subject: "s", Text: "t"},
		"quoted recipient": {To: `"owner"@example.com`, Subject: "s", Text: "t"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := m.Validate(); !errors.Is(err, ErrInvalidMessage) {
				t.Errorf("%s: %v", name, err)
			}
		})
	}
	if err := sample().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildIsAlternativeWithInlineIcon(t *testing.T) {
	raw, err := Build("Prismaport <no-reply@prismaport.test>", sample(), time.Date(2026, 10, 26, 18, 4, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	h := msg.Header
	if h.Get("From") != `"Prismaport" <no-reply@prismaport.test>` || h.Get("To") != "owner@example.com" ||
		h.Get("Date") != "Mon, 26 Oct 2026 18:04:00 +0000" || !strings.HasSuffix(h.Get("Message-Id"), "@prismaport.test>") ||
		h.Get("Auto-Submitted") != "auto-generated" {
		t.Fatalf("headers: %v", h)
	}
	if subj, err := new(mime.WordDecoder).DecodeHeader(h.Get("Subject")); err != nil || subj != "Confirm your email for Prismaport" {
		t.Fatalf("subject %q %v", subj, err)
	}
	mt, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content type %s %v", mt, err)
	}
	alt := multipart.NewReader(msg.Body, params["boundary"])
	text, err := alt.NextPart()
	if err != nil || text.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("first part: %v %v", text, err)
	}
	if b, _ := io.ReadAll(text); !strings.Contains(string(b), "482193") || !strings.Contains(string(b), "\r\n") {
		t.Fatalf("text part %q", b)
	}
	relPart, err := alt.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	mt, params, _ = mime.ParseMediaType(relPart.Header.Get("Content-Type"))
	if mt != "multipart/related" {
		t.Fatalf("second part %s", mt)
	}
	rel := multipart.NewReader(relPart, params["boundary"])
	html, err := rel.NextPart()
	if err != nil || html.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("html part: %v", err)
	}
	if b, _ := io.ReadAll(html); !strings.Contains(string(b), "cid:prismaport-icon") {
		t.Fatalf("html %q", b)
	}
	img, err := rel.NextPart()
	if err != nil || img.Header.Get("Content-Id") != "<prismaport-icon>" || img.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("image part: %v %v", img, err)
	}
	if _, err := rel.NextPart(); err != io.EOF {
		t.Fatalf("extra related part: %v", err)
	}
	if _, err := alt.NextPart(); err != io.EOF {
		t.Fatalf("extra alternative part: %v", err)
	}
}

func TestBuildTextOnly(t *testing.T) {
	m := sample()
	m.HTML, m.Inline = "", nil
	raw, err := Build("no-reply@prismaport.test", m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := mail.ReadMessage(bytes.NewReader(raw))
	if msg.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("content type %s", msg.Header.Get("Content-Type"))
	}
}

func TestMemoryAndLog(t *testing.T) {
	var mem Memory
	if err := mem.Send(t.Context(), sample()); err != nil {
		t.Fatal(err)
	}
	if got := mem.To("OWNER@example.com"); len(got) != 1 || got[0].Kind != KindVerifyEmail {
		t.Fatalf("memory: %v", got)
	}
	mem.Fail = errors.New("down")
	if err := mem.Send(t.Context(), sample()); err == nil || len(mem.Sent()) != 1 {
		t.Fatal("a failing memory sender recorded the message")
	}
	var logs bytes.Buffer
	l := Log{Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	if err := l.Send(t.Context(), sample()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "482193") || strings.Contains(logs.String(), "owner@") || !strings.Contains(logs.String(), "to_domain=example.com") {
		t.Fatalf("log sender leaked the body or the address: %s", logs.String())
	}
}

type flaky struct {
	fails atomic.Int32
	sent  chan Message
}

func (f *flaky) Send(_ context.Context, m Message) error {
	if f.fails.Add(-1) >= 0 {
		return errors.New("temporary")
	}
	f.sent <- m
	return nil
}

// The queue retries on its real schedule (2 s, then 10 s), run on synctest's fake clock.
func TestQueueRetriesInTheBackground(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &flaky{sent: make(chan Message, 1)}
		f.fails.Store(2)
		q := NewQueue(f, nil, 4)
		start := time.Now()
		if err := q.Send(t.Context(), sample()); err != nil {
			t.Fatal(err)
		}
		select {
		case m := <-f.sent:
			if m.To != "owner@example.com" {
				t.Fatalf("delivered %v", m)
			}
		case <-time.After(time.Minute):
			t.Fatal("not delivered after retries")
		}
		if took := time.Since(start); took != queueRetryDelays[0]+queueRetryDelays[1] {
			t.Fatalf("delivered after %s, want the first two retry delays", took)
		}
		if err := q.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
	if err := (Message{}).Validate(); err == nil {
		t.Fatal("empty message valid")
	}
}

// panicky panics on its first message and delivers the ones after it.
type panicky struct {
	calls atomic.Int32
	sent  chan Message
}

func (p *panicky) Send(_ context.Context, m Message) error {
	if p.calls.Add(1) == 1 {
		panic("bad message")
	}
	p.sent <- m
	return nil
}

// A sender that panics on one message fails that message only: the queue goes on delivering.
func TestQueueSurvivesASenderPanic(t *testing.T) {
	p := &panicky{sent: make(chan Message, 1)}
	q := NewQueue(p, nil, 4)
	q.retries = nil
	if err := q.Send(t.Context(), sample()); err != nil {
		t.Fatal(err)
	}
	if err := q.Send(t.Context(), sample()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the message after the panic was not delivered")
	}
	if err := q.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// SendNow skips the queue: the caller gets the transport's answer, with no retry, and a panic
// is an error.
func TestQueueSendNowAnswersTheTransport(t *testing.T) {
	f := &flaky{sent: make(chan Message, 1)}
	f.fails.Store(1)
	q := NewQueue(f, nil, 4)
	defer func() { _ = q.Close(t.Context()) }()
	if err := SendNow(t.Context(), q, sample()); err == nil || err.Error() != "temporary" {
		t.Fatalf("a failing transport: %v", err)
	}
	if err := SendNow(t.Context(), q, sample()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.sent:
	default:
		t.Fatal("SendNow returned before the transport took the message")
	}
	pq := NewQueue(&panicky{sent: make(chan Message, 1)}, nil, 1)
	defer func() { _ = pq.Close(t.Context()) }()
	if err := SendNow(t.Context(), pq, sample()); err == nil {
		t.Fatal("a panicking transport answered nil")
	}
	m := &Memory{Fail: errors.New("down")}
	if err := SendNow(t.Context(), m, sample()); err == nil || len(m.Sent()) != 0 {
		t.Fatalf("a sender without a queue: %v", err)
	}
}

// fakeSMTP accepts one connection, speaks just enough SMTP and hands over the DATA it received.
func fakeSMTP(t *testing.T, startTLS bool) (string, <-chan string, <-chan []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	data := make(chan string, 1)
	cmds := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		write := func(s string) { _, _ = conn.Write([]byte(s)) }
		write("220 fake ESMTP\r\n")
		var seen []string
		defer func() { cmds <- seen }()
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			seen = append(seen, line)
			switch up := strings.ToUpper(line); {
			case strings.HasPrefix(up, "EHLO"):
				if startTLS {
					write("250-fake\r\n250-STARTTLS\r\n250 8BITMIME\r\n")
				} else {
					write("250-fake\r\n250 8BITMIME\r\n")
				}
			case strings.HasPrefix(up, "MAIL FROM"), strings.HasPrefix(up, "RCPT TO"):
				write("250 OK\r\n")
			case up == "DATA":
				write("354 go on\r\n")
				var b strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				data <- b.String()
				write("250 queued\r\n")
			case up == "QUIT":
				write("221 bye\r\n")
				return
			default:
				write("502 no\r\n")
			}
		}
	}()
	return ln.Addr().String(), data, cmds
}

func TestSMTPDeliversToALocalCatcher(t *testing.T) {
	addr, data, cmds := fakeSMTP(t, false)
	host, port, _ := net.SplitHostPort(addr)
	s, err := NewSMTP(SMTPConfig{Host: host, Port: atoiT(t, port), TLS: TLSNone, From: "Prismaport <no-reply@prismaport.test>"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(t.Context(), sample()); err != nil {
		t.Fatal(err)
	}
	got := <-data
	if !strings.Contains(got, "To: owner@example.com\r\n") || !strings.Contains(got, "multipart/alternative") {
		t.Fatalf("data: %q", got)
	}
	seen := strings.Join(<-cmds, "\n")
	if !strings.Contains(seen, "MAIL FROM:<no-reply@prismaport.test>") || !strings.Contains(seen, "RCPT TO:<owner@example.com>") {
		t.Fatalf("commands: %s", seen)
	}
}

func TestSMTPNeverSendsInClearWhenTLSIsExpected(t *testing.T) {
	addr, data, _ := fakeSMTP(t, false)
	host, port, _ := net.SplitHostPort(addr)
	s, err := NewSMTP(SMTPConfig{Host: host, Port: atoiT(t, port), From: "no-reply@prismaport.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(t.Context(), sample()); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("sent without STARTTLS: %v", err)
	}
	select {
	case d := <-data:
		t.Fatalf("data reached the server in clear text: %q", d)
	default:
	}
}

func TestSMTPConfigCheck(t *testing.T) {
	ok := SMTPConfig{Host: "smtp.example.com", Port: 587, TLS: TLSStartTLS, From: "Prismaport <no-reply@example.com>"}
	if err := ok.Check(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*SMTPConfig){
		"plain to a remote host": func(c *SMTPConfig) { c.TLS = TLSNone },
		"unknown TLS mode":       func(c *SMTPConfig) { c.TLS = "ssl" },
		"no host":                func(c *SMTPConfig) { c.Host = "" },
		"bad port":               func(c *SMTPConfig) { c.Port = 70000 },
		"user without password":  func(c *SMTPConfig) { c.Username = "u" },
		"bad sender":             func(c *SMTPConfig) { c.From = "not an address" },
		"sender with CRLF":       func(c *SMTPConfig) { c.From = "a@example.com\r\nBcc: x@example.com" },
	} {
		t.Run(name, func(t *testing.T) {
			c := ok
			mutate(&c)
			if err := c.Check(); err == nil {
				t.Errorf("%s: accepted", name)
			}
		})
	}
	for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
		c := ok
		c.Host, c.TLS = host, TLSNone
		if err := c.Check(); err != nil {
			t.Errorf("plain to %s: %v", host, err)
		}
	}
}

func atoiT(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// down fails every send until its context ends.
type down struct{ calls atomic.Int32 }

func (d *down) Send(ctx context.Context, _ Message) error {
	d.calls.Add(1)
	return errors.New("provider down")
}

// Close is bounded by its context: retries waiting on their timer stop at once, and a Send after
// Close is an error, not a send on a closed channel.
func TestQueueCloseIsBoundedAndSendAfterCloseFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &down{}
		q := NewQueue(d, nil, 4)
		q.retries = []time.Duration{time.Hour}
		if err := q.Send(t.Context(), sample()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // the first send failed and the retry waits on its hour-long timer
		if d.calls.Load() != 1 {
			t.Fatalf("%d sends before close", d.calls.Load())
		}
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := q.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("close: %v", err)
		}
		if took := time.Since(start); took != 50*time.Millisecond {
			t.Fatalf("close took %s, want its 50ms deadline", took)
		}
		q.wg.Wait() // the retry's wait ended with the deadline, not after an hour
		if took := time.Since(start); took != 50*time.Millisecond {
			t.Fatalf("the retry stopped after %s", took)
		}
		if d.calls.Load() != 1 {
			t.Fatalf("%d sends", d.calls.Load())
		}
		if err := q.Send(t.Context(), sample()); !errors.Is(err, ErrQueueClosed) {
			t.Fatalf("send after close: %v", err)
		}
		if err := q.Close(t.Context()); err != nil {
			t.Fatalf("second close: %v", err)
		}
	})
}
