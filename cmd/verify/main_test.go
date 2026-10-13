package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lechefran/mailban.workers/internal/logchain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testKey = "mb_test"

func ptr[T any](v T) *T { return &v }

var signer = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))

func addr(a string) *string { return ptr(sha256Hex(a)) }

// vote builds an unchained log event for address a by voter v.
func vote(seq int64, action, a, v string, reputable *bool) event {
	return event{Seq: seq, At: 1760270000000000 + seq, Action: action,
		AddressID: addr(a), MailboxID: addr(a), VoterID: ptr(v), Reputable: reputable}
}

// sampleLog is a log where a@x.com ends 1/2, b@x.com 0/1 and c@x.com was
// opted out (its earlier rows redacted).
func sampleLog() []event {
	return []event{
		vote(1, "cast", "a@x.com", "v1", ptr(true)),
		vote(2, "cast", "a@x.com", "v2", ptr(true)),
		vote(3, "cast", "b@x.com", "v1", ptr(true)),
		vote(4, "change", "a@x.com", "v2", ptr(false)),
		vote(5, "cast", "a@x.com", "v3", ptr(true)),
		vote(6, "retract", "a@x.com", "v3", nil),
		vote(7, "cast", "c@x.com", "v1", ptr(false)),
		vote(8, "expire", "b@x.com", "v1", nil),
		vote(9, "cast", "b@x.com", "v4", ptr(false)),
		vote(10, "suspend", "a@x.com", "v9", nil), // voter without a vote here: no-op
		{Seq: 11, At: 1760270000000011, Action: "optout", Redacted: true},
	}
}

var sampleEmails = []email{
	{Address: "a@x.com", PositiveVotes: 1, TotalVotes: 2},
	{Address: "b@x.com", PositiveVotes: 0, TotalVotes: 1},
	{Address: "z@x.com", PositiveVotes: 0, TotalVotes: 0}, // awaiting cleanup: same as absent
}

// chainAll fills leaf and chain for the first n events, as the chain job does.
func chainAll(events []event, n int) {
	prev := logchain.Genesis
	for i := range events[:n] {
		leaf := logchain.Leaf(events[i].row())
		chain := logchain.Chain(prev, leaf)
		events[i].Leaf, events[i].Chain = ptr(leaf), ptr(chain)
		prev = chain
	}
}

// redactOptOut redacts c@x.com's rows as the API's opt-out does.
func redactOptOut(events []event) {
	for i := range events {
		if e := events[i]; e.MailboxID != nil && *e.MailboxID == sha256Hex("c@x.com") {
			events[i].AddressID, events[i].MailboxID, events[i].Redacted = nil, nil, true
		}
	}
}

func sign(key ed25519.PrivateKey, seq int64, chain string) checkpoint {
	sig := ed25519.Sign(key, logchain.CheckpointMessage(seq, chain))
	return checkpoint{Seq: seq, Chain: chain, SignedAt: "2026-10-12T00:00:00Z", Signature: base64.StdEncoding.EncodeToString(sig)}
}

// fakeAPI serves the transparency endpoints and /emails with pages of 3.
type fakeAPI struct {
	pub         ed25519.PublicKey
	checkpoints []checkpoint
	events      []event
	emails      []email
}

func (f *fakeAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	const page = 3
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	after := func(r *http.Request) int64 {
		n, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		return n
	}
	mux.HandleFunc("GET /transparency/key", func(w http.ResponseWriter, r *http.Request) {
		if f.pub == nil {
			http.NotFound(w, r)
			return
		}
		write(w, map[string]string{"algorithm": "ed25519", "publicKey": base64.StdEncoding.EncodeToString(f.pub)})
	})
	mux.HandleFunc("GET /transparency/checkpoints", func(w http.ResponseWriter, r *http.Request) {
		var out []checkpoint
		for _, cp := range f.checkpoints {
			if cp.Seq > after(r) {
				out = append(out, cp)
			}
		}
		hasNext := len(out) > page
		write(w, map[string]any{"checkpoints": out[:min(page, len(out))], "hasNext": hasNext, "status": map[string]any{}})
	})
	mux.HandleFunc("GET /transparency/log", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var out []event
		for _, e := range f.events {
			if e.Seq > after(r) {
				out = append(out, e)
			}
		}
		write(w, map[string]any{"events": out[:min(page, len(out))], "hasNext": len(out) > page, "status": map[string]any{}})
	})
	mux.HandleFunc("GET /emails", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		assert.Equal(t, "all", r.URL.Query().Get("reputation"))
		start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		end := min(start+page, len(f.emails))
		resp := map[string]any{"emails": f.emails[start:end], "hasNext": end < len(f.emails), "status": map[string]any{}}
		if end < len(f.emails) {
			resp["cursor"] = strconv.Itoa(end)
		}
		write(w, resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func runVerify(t *testing.T, api *fakeAPI) (int, string) {
	t.Helper()
	srv := api.server(t)
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"-api", srv.URL + "/"}, &out, &errOut,
		func(k string) string { return map[string]string{"MAILBAN_API_KEY": testKey}[k] }, srv.Client())
	return code, out.String() + errOut.String()
}

// validAPI returns a consistent API: rows 1-9 chained, two checkpoints,
// row 10 not chained yet, c@x.com opted out after chaining.
func validAPI() *fakeAPI {
	events := sampleLog()
	chainAll(events, 9)
	redactOptOut(events)
	return &fakeAPI{
		pub:         signer.Public().(ed25519.PublicKey),
		checkpoints: []checkpoint{sign(signer, 4, *events[3].Chain), sign(signer, 9, *events[8].Chain)},
		events:      events,
		emails:      sampleEmails,
	}
}

func TestVerifyOK(t *testing.T) {
	code, out := runVerify(t, validAPI())
	assert.Equal(t, exitOK, code, out)
	assert.Contains(t, out, "log rows:              11 (9 chained, 2 not chained yet)")
	assert.Contains(t, out, "checkpoints verified:  2 of 2")
	assert.Contains(t, out, "RESULT: OK")
}

func TestVerifyAcceptsRedactedRows(t *testing.T) {
	api := validAPI()
	// Account erasure re-pseudonymizes the voter after chaining.
	for i := range api.events {
		if e := api.events[i]; e.VoterID != nil && *e.VoterID == "v4" {
			api.events[i].VoterID, api.events[i].Redacted = ptr("erased-abc"), true
		}
	}
	code, out := runVerify(t, api)
	assert.Equal(t, exitOK, code, out)
	res := verifyChain(api.events, api.checkpoints, api.pub)
	assert.Empty(t, res.LeafFailures)
}

func TestVerifyDetectsTamperedRow(t *testing.T) {
	api := validAPI()
	api.events[1].Reputable = ptr(false) // not redacted: leaf no longer matches

	code, out := runVerify(t, api)
	assert.Equal(t, exitFailed, code, out)
	assert.Contains(t, out, "leaf failures:         1 (seq 2)")
	assert.Contains(t, out, "chain failures:        0")
	assert.Contains(t, out, "RESULT: FAILED")
}

func TestVerifyDetectsBrokenChain(t *testing.T) {
	api := validAPI()
	api.events[5].Chain = ptr(strings.Repeat("f", 64))
	res := verifyChain(api.events, api.checkpoints, api.pub)
	// The altered row fails, and so does its successor, which was chained from
	// the original value; later rows verify again.
	assert.Equal(t, []int64{6, 7}, res.ChainFailures)
	assert.Empty(t, res.LeafFailures)

	api = validAPI()
	api.checkpoints[0] = sign(signer, 4, *api.events[2].Chain) // signed, but not the log's chain at 4
	res = verifyChain(api.events, api.checkpoints, api.pub)
	assert.Equal(t, []int64{4}, res.CheckpointMismatches)
	assert.True(t, res.failed())
}

func TestVerifyDetectsBadSignature(t *testing.T) {
	api := validAPI()
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, ed25519.SeedSize))
	api.checkpoints[1] = sign(other, 9, *api.events[8].Chain)

	code, out := runVerify(t, api)
	assert.Equal(t, exitFailed, code, out)
	assert.Contains(t, out, "signature failures:    1 (seq 9)")
	assert.Contains(t, out, "checkpoints verified:  1 of 2")
}

func TestVerifyCheckpointsWithoutKeyFail(t *testing.T) {
	api := validAPI()
	api.pub = nil
	code, out := runVerify(t, api)
	assert.Equal(t, exitFailed, code, out)
	assert.Contains(t, out, "none published")

	api.checkpoints = nil
	code, out = runVerify(t, api)
	assert.Equal(t, exitOK, code, out)
}

func TestVerifyScoreMismatch(t *testing.T) {
	api := validAPI()
	api.emails = []email{{Address: "a@x.com", PositiveVotes: 2, TotalVotes: 2}}

	code, out := runVerify(t, api)
	assert.Equal(t, exitScores, code, out)
	assert.Contains(t, out, "score mismatches:      2")
	assert.Contains(t, out, "a@x.com: log 1/2, API 2/2")
	assert.Contains(t, out, "id:"+sha256Hex("b@x.com")+": log 0/1, API 0/0")
	assert.Contains(t, out, "re-run")
}

func TestReplay(t *testing.T) {
	got := replay(sampleLog())
	assert.Equal(t, map[string]counts{
		sha256Hex("a@x.com"): {Positive: 1, Total: 2},
		sha256Hex("b@x.com"): {Positive: 0, Total: 1},
		sha256Hex("c@x.com"): {Positive: 0, Total: 1},
	}, got)

	events := sampleLog()
	redactOptOut(events)
	_, ok := replay(events)[sha256Hex("c@x.com")]
	assert.False(t, ok, "redacted rows are skipped")
}

func TestRunRequiresAPIAndKey(t *testing.T) {
	var out bytes.Buffer
	none := func(string) string { return "" }
	assert.Equal(t, exitCannotRun, run(context.Background(), nil, &out, &out, none, http.DefaultClient))
	assert.Equal(t, exitCannotRun, run(context.Background(), []string{"-api", "http://x"}, &out, &out, none, http.DefaultClient))
	assert.Equal(t, exitCannotRun, run(context.Background(), []string{"-bogus"}, &out, &out, none, http.DefaultClient))
}

func TestClientRetriesRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"checkpoints":[],"hasNext":false}`))
	}))
	defer srv.Close()

	c := newClient(srv.URL, testKey, srv.Client())
	var waited time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { waited += d; return nil }
	cps, err := c.fetchCheckpoints(context.Background())
	require.NoError(t, err)
	assert.Empty(t, cps)
	assert.Equal(t, 2*time.Second, waited)
	assert.Equal(t, int32(2), calls.Load())
}

func TestClientReportsAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthorized","title":"Unauthorized","message":"Invalid API key"}`))
	}))
	defer srv.Close()
	_, err := newClient(srv.URL, "bad", srv.Client()).fetchLog(context.Background())
	assert.ErrorContains(t, err, "status 401 Invalid API key")
}
