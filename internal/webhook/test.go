package webhook

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prismaport/prismaport/internal/service"
)

// TestBodyLimit is how much of an endpoint's response body a test send shows.
const TestBodyLimit = 2 << 10

// Header is one HTTP header of a test delivery.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// TestRequest is the request a test delivery sent.
type TestRequest struct {
	Method  string   `json:"method"`
	URL     string   `json:"url"`
	Headers []Header `json:"headers"`
	Body    string   `json:"body"`
}

// TestResponse is what the endpoint answered to a test delivery: status, latency and the start of
// the body.
type TestResponse struct {
	Status    int   `json:"status"`
	LatencyMS int64 `json:"latency_ms"`
	// Body is the first TestBodyLimit bytes of the response body.
	Body          string `json:"body"`
	BodyTruncated bool   `json:"body_truncated"`
}

// TestEventRef names the event a test delivery carried.
type TestEventRef struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// TestResult is what a test send did: the exact request, and the response or the error.
type TestResult struct {
	Event    TestEventRef         `json:"event"`
	Delivery service.DeliveryView `json:"delivery"`
	Request  TestRequest          `json:"request"`
	Response *TestResponse        `json:"response"`
	Error    *string              `json:"error"`
}

// SendTest builds a sample event of eventType, signs it like a real delivery to the endpoint,
// sends it once with the dispatcher's client (same network rules, no redirects, same timeout)
// and records it in the delivery log as a test. Test mode only.
func (d *Dispatcher) SendTest(ctx context.Context, a service.Auth, endpointID, eventType string) (TestResult, error) {
	te, err := d.svc.PrepareTestEvent(ctx, a, endpointID, eventType)
	if err != nil {
		return TestResult{}, err
	}
	if err := d.svc.DeliverableURL(te.Endpoint); err != nil {
		return TestResult{}, err
	}
	req, err := NewDeliveryRequest(ctx, te.Endpoint.URL, te.ID, te.Payload, te.At, te.Secrets...)
	if err != nil {
		return TestResult{}, err
	}
	res := TestResult{Event: TestEventRef{ID: te.ID, Type: te.Type},
		Request: TestRequest{Method: req.Method, URL: te.Endpoint.URL, Body: string(te.Payload)}}
	for _, h := range DeliveryHeaders {
		res.Request.Headers = append(res.Request.Headers, Header{Name: h, Value: req.Header.Get(h)})
	}
	start := time.Now()
	out := service.TestOutcome{At: te.At}
	resp, sendErr := d.client.Do(req)
	if sendErr == nil {
		latency := time.Since(start)
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, TestBodyLimit+1))
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		r := &TestResponse{Status: resp.StatusCode, LatencyMS: latency.Milliseconds()}
		if len(raw) > TestBodyLimit {
			raw, r.BodyTruncated = raw[:TestBodyLimit], true
		}
		r.Body = validUTF8Prefix(raw)
		res.Response = r
		out.StatusCode, out.StatusText = resp.StatusCode, http.StatusText(resp.StatusCode)
		if readErr != nil && !errors.Is(readErr, io.EOF) && out.StatusCode >= 200 && out.StatusCode < 300 {
			out.Err = "reading the response: " + readErr.Error()
		}
	} else {
		out.Err = truncate(errorText(sendErr), 500)
	}
	if out.Err != "" {
		msg := out.Err
		res.Error = &msg
	}
	if res.Delivery, err = d.svc.RecordTestDelivery(ctx, a, te, out); err != nil {
		return TestResult{}, err
	}
	return res, nil
}

// validUTF8Prefix drops a trailing character cut in half by the size limit and replaces invalid
// bytes, so the body is a valid JSON string.
func validUTF8Prefix(b []byte) string {
	for n := 1; n < utf8.UTFMax && n <= len(b); n++ {
		if utf8.RuneStart(b[len(b)-n]) {
			if !utf8.FullRune(b[len(b)-n:]) {
				b = b[:len(b)-n]
			}
			break
		}
	}
	return strings.ToValidUTF8(string(b), string(utf8.RuneError))
}
