package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/base58"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/webhook"
)

const defaultAPIURL = "http://localhost:8080"

// apiClient calls the merchant API with a test key.
type apiClient struct {
	base string
	key  string
	http *http.Client
}

// apiError is an error answer of the merchant API.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("the API answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (c *apiClient) do(ctx context.Context, method, path string, body any, out any, headers ...string) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("User-Agent", "Prismaport-CLI/1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		e := &apiError{Status: resp.StatusCode}
		var b struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if json.Unmarshal(raw, &b) == nil {
			e.Code, e.Message = b.Error.Code, b.Error.Message
		}
		return e
	}
	return json.Unmarshal(raw, out)
}

// testKey refuses anything but a test-mode secret key, before any request is made.
func testKey(key string) (string, error) {
	if key == "" {
		key = os.Getenv("PRISMAPORT_API_KEY")
	}
	switch {
	case key == "":
		return "", errors.New("--api-key is required (or set PRISMAPORT_API_KEY)")
	case strings.HasPrefix(key, "sk_live_"):
		return "", errors.New("sandbox commands work in test mode only: use an sk_test_ key, never an sk_live_ one")
	case !strings.HasPrefix(key, "sk_test_"):
		return "", errors.New("--api-key must be a test-mode secret key (sk_test_...)")
	}
	return key, nil
}

func newAPIClient(apiURL, key string) (*apiClient, error) {
	key, err := testKey(key)
	if err != nil {
		return nil, err
	}
	if apiURL == "" {
		apiURL = os.Getenv("PRISMAPORT_API_URL")
	}
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	if !strings.HasPrefix(apiURL, "http://") && !strings.HasPrefix(apiURL, "https://") {
		return nil, fmt.Errorf("--api-url must start with http:// or https://, got %q", apiURL)
	}
	return &apiClient{base: strings.TrimRight(apiURL, "/"), key: key, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

// eventFilter parses --events; nil means every event.
func eventFilter(list string) (map[string]bool, error) {
	if strings.TrimSpace(list) == "" {
		return nil, nil
	}
	out := map[string]bool{}
	for _, e := range strings.Split(list, ",") {
		e = strings.TrimSpace(e)
		if !session.EventType(e).Valid() {
			return nil, fmt.Errorf("unknown event type %q in --events", e)
		}
		out[e] = true
	}
	return out, nil
}

type feedPage struct {
	Data       []json.RawMessage `json:"data"`
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor"`
}

func newCLISecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "whsec_" + base58.Encode(b[:]), nil
}

// runListen forwards test-mode events from the feed to a local URL, signed like webhooks with a
// secret of this CLI session, until ctx ends.
func runListen(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("listen", flag.ContinueOnError)
	fs.SetOutput(out)
	key := fs.String("api-key", "", "test-mode secret key (sk_test_...); default $PRISMAPORT_API_KEY")
	forward := fs.String("forward-to", "", "local URL that receives the events, e.g. http://localhost:3000/webhooks")
	events := fs.String("events", "", "comma-separated event types to forward (default: all)")
	apiURL := fs.String("api-url", "", "Prismaport API base URL (default $PRISMAPORT_API_URL or "+defaultAPIURL+")")
	after := fs.String("after", "now", `feed cursor to start after: "now" or the cursor printed when a previous run stopped`)
	secretFlag := fs.String("secret", "", "signing secret to use (whsec_...); default: a new one for this session")
	interval := fs.Duration("interval", 2*time.Second, "how often to poll the feed when it is idle")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, err := newAPIClient(*apiURL, *key)
	if err != nil {
		return err
	}
	if *forward == "" {
		return errors.New("--forward-to is required")
	}
	if !strings.HasPrefix(*forward, "http://") && !strings.HasPrefix(*forward, "https://") {
		return fmt.Errorf("--forward-to must be an http:// or https:// URL, got %q", *forward)
	}
	filter, err := eventFilter(*events)
	if err != nil {
		return err
	}
	*interval = max(*interval, 100*time.Millisecond)
	secret := *secretFlag
	if secret == "" {
		if secret, err = newCLISecret(); err != nil {
			return err
		}
	} else if !strings.HasPrefix(secret, "whsec_") {
		return errors.New("--secret must start with whsec_")
	}
	cursor := *after
	if cursor == "now" {
		var page feedPage
		if err := client.do(ctx, "GET", "/v1/events?after=latest", nil, &page); err != nil {
			return fmt.Errorf("reading the feed: %w", err)
		}
		cursor = page.NextCursor
	} else if n, err := strconv.ParseInt(cursor, 10, 64); err != nil || n < 0 {
		return fmt.Errorf(`--after must be "now" or a cursor number, got %q`, cursor)
	}
	fmt.Fprintf(out, "Ready. Forwarding test-mode events to %s\n", *forward)
	fmt.Fprintf(out, "Signing secret for this session: %s\n", secret)
	fmt.Fprintf(out, "Starting after cursor %s. Press Ctrl+C to stop.\n", cursor)

	local := &http.Client{Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	backoff := time.Duration(0)
	for {
		var page feedPage
		err := client.do(ctx, "GET", "/v1/events?limit=100&after="+cursor, nil, &page)
		if ctx.Err() != nil {
			break
		}
		var apiErr *apiError
		if errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden) {
			return fmt.Errorf("the feed refused this key: %w", err)
		}
		if err != nil {
			backoff = min(max(2*backoff, time.Second), 30*time.Second)
			fmt.Fprintf(out, "%s  feed unavailable (%v); retrying in %s\n", stamp(), err, backoff)
			if !sleep(ctx, backoff) {
				break
			}
			continue
		}
		backoff = 0
		for _, raw := range page.Data {
			var env struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &env); err != nil {
				fmt.Fprintf(out, "%s  skipped an event that is not JSON\n", stamp())
				continue
			}
			if filter != nil && !filter[env.Type] {
				continue
			}
			fmt.Fprintln(out, forwardOne(ctx, local, *forward, env.ID, env.Type, raw, secret))
		}
		cursor = page.NextCursor
		if !page.HasMore && !sleep(ctx, *interval) {
			break
		}
	}
	fmt.Fprintf(out, "Stopped. Resume with --after %s\n", cursor)
	return nil
}

func stamp() string { return time.Now().UTC().Format("2006-01-02 15:04:05") }

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// forwardOne posts one event exactly as a webhook delivery is made and describes the outcome.
func forwardOne(ctx context.Context, c *http.Client, url, id, typ string, body []byte, secret string) string {
	line := fmt.Sprintf("%s  %-26s %s  ->", stamp(), typ, id)
	req, err := webhook.NewDeliveryRequest(ctx, url, id, body, time.Now(), secret)
	if err != nil {
		return line + " error: " + err.Error()
	}
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return line + " error: " + err.Error()
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return fmt.Sprintf("%s %d %s (%d ms)", line, resp.StatusCode, http.StatusText(resp.StatusCode), time.Since(start).Milliseconds())
}

// triggerPlans are the simulator actions that make a new test session emit each event type, as
// the same scenario would in production.
var triggerPlans = map[string][]string{
	string(session.EventPaymentDetected): {"pay_exact"},
	string(session.EventPartiallyPaid):   {"pay_partial"},
	string(session.EventPaid):            {"pay_exact"},
	string(session.EventOverpaid):        {"overpay"},
	string(session.EventExpired):         {"expire"},
	string(session.EventLatePayment):     {"late_payment"},
	string(session.EventUnderReview):     {"under_review"},
	string(session.EventPaymentReversed): {"pay_exact", "reverse_last_payment"},
}

// runTrigger creates a test session and runs the simulator so it emits the given event.
func runTrigger(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: prismaport trigger <event type> --api-key sk_test_... [--api-url URL] [--product prod_...] [--amount 25.00] [--settle]")
	}
	event := args[0]
	plan, ok := triggerPlans[event]
	if !ok {
		return fmt.Errorf("unknown event type %q", event)
	}
	fs := flag.NewFlagSet("trigger", flag.ContinueOnError)
	fs.SetOutput(out)
	key := fs.String("api-key", "", "test-mode secret key (sk_test_...); default $PRISMAPORT_API_KEY")
	apiURL := fs.String("api-url", "", "Prismaport API base URL (default $PRISMAPORT_API_URL or "+defaultAPIURL+")")
	amount := fs.String("amount", "25.00", "session amount in USD (for an open-price product)")
	productID := fs.String("product", "", "product the session is opened from (default: an open-price test product the CLI creates once)")
	settle := fs.Bool("settle", false, "then settle the session as the flusher would and print the settlement (fee and payouts)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	client, err := newAPIClient(*apiURL, *key)
	if err != nil {
		return err
	}
	var idem [16]byte
	if _, err := rand.Read(idem[:]); err != nil {
		return err
	}
	prod, err := triggerProduct(ctx, client, *productID)
	if err != nil {
		return err
	}
	body := map[string]any{"product_id": prod.ID, "metadata": map[string]string{"source": "prismaport trigger"}}
	if prod.PriceType == "open" {
		body["amount"] = *amount
	}
	var ses struct {
		ID     string `json:"id"`
		Amount string `json:"amount"`
	}
	if err := client.do(ctx, "POST", "/v1/sessions", body, &ses, "Idempotency-Key", "trigger-"+base58.Encode(idem[:])); err != nil {
		return fmt.Errorf("creating a test session: %w", err)
	}
	fmt.Fprintf(out, "Created test session %s (%s USD)\n", ses.ID, ses.Amount)
	for _, action := range plan {
		var res struct {
			Events []struct {
				ID             string `json:"id"`
				Type           string `json:"type"`
				SessionVersion int64  `json:"session_version"`
			} `json:"events"`
			Session struct {
				Status string `json:"status"`
			} `json:"session"`
		}
		if err := client.do(ctx, "POST", "/v1/test_helpers/sessions/"+ses.ID+"/simulate", map[string]any{"action": action}, &res); err != nil {
			return fmt.Errorf("simulating %s: %w", action, err)
		}
		fmt.Fprintf(out, "  %-22s -> %s\n", action, res.Session.Status)
		for _, e := range res.Events {
			fmt.Fprintf(out, "    %-26s %s  (version %d)\n", e.Type, e.ID, e.SessionVersion)
		}
	}
	fmt.Fprintf(out, "Triggered %s. Webhook endpoints and `prismaport listen` receive it once the worker sequences it.\n", event)
	if !*settle {
		return nil
	}
	var res struct {
		Session struct {
			Version    int64           `json:"version"`
			Settlement json.RawMessage `json:"settlement"`
		} `json:"session"`
	}
	if err := client.do(ctx, "POST", "/v1/test_helpers/sessions/"+ses.ID+"/simulate", map[string]any{"action": "settle"}, &res); err != nil {
		return fmt.Errorf("simulating settle: %w", err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, res.Session.Settlement, "  ", "  "); err != nil {
		return err
	}
	fmt.Fprintf(out, "  %-22s -> version %d, no event (read the session)\n  settlement %s\n", "settle", res.Session.Version, pretty.String())
	return nil
}

// triggerProductName names the open-price product `prismaport trigger` opens its sessions from
// when no --product is given; it is created once and found again by its metadata.
const triggerProductName = "Test events (prismaport trigger)"

type triggerProductRef struct {
	ID        string            `json:"id"`
	PriceType string            `json:"price_type"`
	Active    bool              `json:"active"`
	Metadata  map[string]string `json:"metadata"`
}

// triggerProduct answers the product a trigger session is opened from: the one named, else the
// CLI's own open-price test product, created on first use (every session comes from a product).
func triggerProduct(ctx context.Context, client *apiClient, id string) (triggerProductRef, error) {
	var p triggerProductRef
	if id != "" {
		if err := client.do(ctx, "GET", "/v1/products/"+id, nil, &p); err != nil {
			return p, fmt.Errorf("reading product %s: %w", id, err)
		}
		return p, nil
	}
	after := ""
	for range 20 {
		var page struct {
			Data    []triggerProductRef `json:"data"`
			HasMore bool                `json:"has_more"`
		}
		path := "/v1/products?limit=100"
		if after != "" {
			path += "&starting_after=" + after
		}
		if err := client.do(ctx, "GET", path, nil, &page); err != nil {
			return p, fmt.Errorf("listing products: %w", err)
		}
		for _, x := range page.Data {
			if x.Active && x.PriceType == "open" && x.Metadata["source"] == "prismaport trigger" {
				return x, nil
			}
		}
		if !page.HasMore || len(page.Data) == 0 {
			break
		}
		after = page.Data[len(page.Data)-1].ID
	}
	if err := client.do(ctx, "POST", "/v1/products", map[string]any{"name": triggerProductName, "price_type": "open", "min_amount": "1.00",
		"metadata": map[string]string{"source": "prismaport trigger"}}, &p); err != nil {
		return p, fmt.Errorf("creating the test product: %w", err)
	}
	return p, nil
}
