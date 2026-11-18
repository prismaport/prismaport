// Package pwned asks the Have I Been Pwned range API whether a password is in a known data breach.
// The API is k-anonymous: only the first 5 hex characters of the password's SHA-1 leave this
// process, and the comparison with the suffixes it answers happens here.
package pwned

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// BaseURL is the public range API. It needs no key.
	BaseURL = "https://api.pwnedpasswords.com"
	// Timeout bounds a whole check; the caller goes on without the answer after it.
	Timeout = 2 * time.Second
	// maxBody is far above a padded answer (about 2000 lines of 40 bytes).
	maxBody = 1 << 20
)

// Client checks passwords against the range API.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client for the public API, with Timeout and without following redirects.
func New() *Client {
	return &Client{BaseURL: BaseURL, HTTP: &http.Client{
		Timeout:       Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Breached reports whether the password is in a known breach. The error never holds the password
// or any part of its hash, so a caller may log it.
func (c *Client) Breached(ctx context.Context, password string) (bool, error) {
	sum := sha1.Sum([]byte(password))
	digest := strings.ToUpper(hex.EncodeToString(sum[:]))
	prefix, suffix := digest[:5], digest[5:]

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/range/"+prefix, nil)
	if err != nil {
		return false, errors.New("pwned: cannot build the request")
	}
	req.Header.Set("Add-Padding", "true")
	req.Header.Set("User-Agent", "prismaport")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// A *url.Error quotes the URL, and the URL holds the hash prefix.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return false, fmt.Errorf("pwned: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("pwned: range answered %d", resp.StatusCode)
	}

	lines := bufio.NewScanner(io.LimitReader(resp.Body, maxBody))
	for lines.Scan() {
		got, count, ok := strings.Cut(strings.TrimSpace(lines.Text()), ":")
		if !ok || !strings.EqualFold(got, suffix) {
			continue
		}
		n, err := strconv.Atoi(count)
		if err != nil {
			return false, errors.New("pwned: malformed count in the answer")
		}
		// Add-Padding mixes in made-up suffixes with a count of 0.
		return n > 0, nil
	}
	if err := lines.Err(); err != nil {
		return false, fmt.Errorf("pwned: reading the answer: %w", err)
	}
	return false, nil
}
