package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Page sizes: the maximum each endpoint allows.
const (
	checkpointPageSize = 1000
	logPageSize        = 1000
	emailPageSize      = 100
	// maxRetries is how many times a rate-limited (429) request is retried.
	maxRetries = 8
	// maxBodyBytes caps one response body.
	maxBodyBytes = 32 << 20
)

// event is one GET /transparency/log row.
type event struct {
	Seq       int64   `json:"seq"`
	At        int64   `json:"at"`
	Action    string  `json:"action"`
	AddressID *string `json:"addressId"`
	MailboxID *string `json:"mailboxId"`
	VoterID   *string `json:"voterId"`
	Reputable *bool   `json:"reputable"`
	Redacted  bool    `json:"redacted"`
	Leaf      *string `json:"leaf"`
	Chain     *string `json:"chain"`
}

// checkpoint is one GET /transparency/checkpoints entry.
type checkpoint struct {
	Seq       int64  `json:"seq"`
	Chain     string `json:"chain"`
	SignedAt  string `json:"signedAt"`
	Signature string `json:"signature"`
}

// email is the part of a GET /emails object the verifier compares.
type email struct {
	Address       string `json:"address"`
	PositiveVotes int    `json:"positiveVotes"`
	TotalVotes    int    `json:"totalVotes"`
}

// errNotFound is returned for a 404, e.g. when no signing key is configured.
var errNotFound = errors.New("not found")

// client fetches the public transparency data and scores from a Mailban API.
// Its HTTP client and sleep are injectable so tests can use httptest.
type client struct {
	base  string
	key   string
	http  *http.Client
	sleep func(context.Context, time.Duration) error
}

func newClient(base, key string, hc *http.Client) *client {
	return &client{base: strings.TrimRight(base, "/"), key: key, http: hc, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// get GETs path?query into out, sending the API key when auth is set and
// retrying 429 responses after Retry-After (default 1s).
func (c *client) get(ctx context.Context, path string, query url.Values, auth bool, out any) error {
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if auth {
			req.Header.Set("Authorization", "Bearer "+c.key)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("GET %s: %w", path, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("GET %s: %w", path, err)
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("GET %s: decode response: %w", path, err)
			}
			return nil
		case resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries:
			wait := time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				wait = time.Duration(s) * time.Second
			}
			if err := c.sleep(ctx, wait); err != nil {
				return err
			}
		case resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("GET %s: %w", path, errNotFound)
		default:
			var apiErr struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(body, &apiErr)
			return fmt.Errorf("GET %s: status %d %s", path, resp.StatusCode, apiErr.Message)
		}
	}
}

// fetchKey returns the log signing public key, or nil when the API has none.
func (c *client) fetchKey(ctx context.Context) (ed25519.PublicKey, error) {
	var resp struct {
		Algorithm string `json:"algorithm"`
		PublicKey string `json:"publicKey"`
	}
	err := c.get(ctx, "/transparency/key", nil, false, &resp)
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if resp.Algorithm != "ed25519" {
		return nil, fmt.Errorf("unsupported signing algorithm %q", resp.Algorithm)
	}
	key, err := base64.StdEncoding.DecodeString(resp.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("public key is not 32 bytes of standard base64")
	}
	return ed25519.PublicKey(key), nil
}

// fetchCheckpoints pages through every checkpoint in seq order.
func (c *client) fetchCheckpoints(ctx context.Context) ([]checkpoint, error) {
	var all []checkpoint
	var after int64
	for {
		q := url.Values{"limit": {strconv.Itoa(checkpointPageSize)}}
		if after > 0 {
			q.Set("after", strconv.FormatInt(after, 10))
		}
		var page struct {
			Checkpoints []checkpoint `json:"checkpoints"`
			HasNext     bool         `json:"hasNext"`
		}
		if err := c.get(ctx, "/transparency/checkpoints", q, false, &page); err != nil {
			return nil, err
		}
		for _, cp := range page.Checkpoints {
			if cp.Seq <= after {
				return nil, fmt.Errorf("checkpoints out of order at seq %d", cp.Seq)
			}
			after = cp.Seq
		}
		all = append(all, page.Checkpoints...)
		if !page.HasNext {
			return all, nil
		}
		if len(page.Checkpoints) == 0 {
			return nil, errors.New("checkpoint page is empty but hasNext is true")
		}
	}
}

// fetchLog pages through the whole vote log in seq order.
func (c *client) fetchLog(ctx context.Context) ([]event, error) {
	var all []event
	var after int64
	for {
		q := url.Values{"limit": {strconv.Itoa(logPageSize)}}
		if after > 0 {
			q.Set("after", strconv.FormatInt(after, 10))
		}
		var page struct {
			Events  []event `json:"events"`
			HasNext bool    `json:"hasNext"`
		}
		if err := c.get(ctx, "/transparency/log", q, true, &page); err != nil {
			return nil, err
		}
		for _, e := range page.Events {
			if e.Seq <= after {
				return nil, fmt.Errorf("log out of order at seq %d", e.Seq)
			}
			after = e.Seq
		}
		all = append(all, page.Events...)
		if !page.HasNext {
			return all, nil
		}
		if len(page.Events) == 0 {
			return nil, errors.New("log page is empty but hasNext is true")
		}
	}
}

// fetchEmails pages through GET /emails?reputation=all, following cursor.
func (c *client) fetchEmails(ctx context.Context) ([]email, error) {
	var all []email
	cursor := ""
	for {
		q := url.Values{"reputation": {"all"}, "limit": {strconv.Itoa(emailPageSize)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Emails  []email `json:"emails"`
			Cursor  string  `json:"cursor"`
			HasNext bool    `json:"hasNext"`
		}
		if err := c.get(ctx, "/emails", q, true, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Emails...)
		if !page.HasNext {
			return all, nil
		}
		if page.Cursor == "" || page.Cursor == cursor {
			return nil, errors.New("emails page has hasNext but no new cursor")
		}
		cursor = page.Cursor
	}
}
