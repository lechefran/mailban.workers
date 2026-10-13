package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// voteJobs are the jobs that run every VOTE_JOB_INTERVAL.
var voteJobs = map[string]bool{
	"expire-stale-votes": true, "delete-unvoted-addresses": true, "chain-log": true,
	"checkpoint-log": true, "check-aggregates": true,
}

func names(cfg config) []string {
	var out []string
	for _, j := range buildJobs(cfg) {
		out = append(out, j.Name)
	}
	return out
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{"DB_CONN": "postgres://x"}))
	require.NoError(t, err)
	assert.Equal(t, "8081", cfg.port)
	assert.Nil(t, cfg.signingKey)
	assert.False(t, cfg.repair)
	assert.Equal(t, time.Hour, cfg.interval)
	assert.Equal(t, 10*time.Minute, cfg.voteInterval)

	// Without a signing key or list URL those two jobs are not registered.
	assert.Equal(t, []string{
		"deactivate-expired-tokens", "delete-old-tokens", "expire-stale-votes", "delete-unvoted-addresses",
		"delete-expired-optouts", "purge-usage", "prune-job-runs", "chain-log", "check-aggregates",
	}, names(cfg))
	for _, j := range buildJobs(cfg) {
		if voteJobs[j.Name] {
			assert.Equal(t, 10*time.Minute, j.Interval, j.Name)
		} else {
			assert.Equal(t, time.Hour, j.Interval, j.Name)
		}
	}
}

func TestLoadConfigAllOptions(t *testing.T) {
	seed := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32)))
	cfg, err := loadConfig(env(map[string]string{
		"DB_CONN":             "postgres://x",
		"PORT":                "9000",
		"LOG_SIGNING_KEY":     seed,
		"BLOCKED_DOMAINS_URL": "https://example.com/list.txt",
		"REPAIR_AGGREGATES":   "true",
		"JOB_INTERVAL":        "30m",
		"VOTE_JOB_INTERVAL":   "2m",
	}))
	require.NoError(t, err)
	assert.True(t, cfg.repair)
	assert.NotNil(t, cfg.signingKey)

	jobs := buildJobs(cfg)
	assert.Len(t, jobs, 11)
	for _, j := range jobs {
		if voteJobs[j.Name] {
			assert.Equal(t, 2*time.Minute, j.Interval, j.Name)
		} else {
			assert.Equal(t, 30*time.Minute, j.Interval, j.Name)
		}
		switch j.Name {
		case "chain-log", "expire-stale-votes", "check-aggregates":
			assert.Equal(t, 5*time.Minute, j.Timeout, j.Name)
		default:
			assert.Zero(t, j.Timeout, j.Name)
		}
		assert.NotNil(t, j.Run)
	}
}

func TestLoadConfigRejectsInvalid(t *testing.T) {
	_, err := loadConfig(env(map[string]string{
		"PORT":                "http",
		"LOG_SIGNING_KEY":     "short",
		"BLOCKED_DOMAINS_URL": "ftp://example.com/list",
		"REPAIR_AGGREGATES":   "maybe",
		"JOB_INTERVAL":        "soon",
		"VOTE_JOB_INTERVAL":   "10",
	}))
	require.Error(t, err)
	for _, want := range []string{"DB_CONN", "PORT", "LOG_SIGNING_KEY", "BLOCKED_DOMAINS_URL", "REPAIR_AGGREGATES",
		`JOB_INTERVAL "soon"`, `VOTE_JOB_INTERVAL "10"`} {
		assert.ErrorContains(t, err, want)
	}

	_, err = loadConfig(env(map[string]string{"DB_CONN": "postgres://x", "VOTE_JOB_INTERVAL": "10ms"}))
	assert.EqualError(t, err, `VOTE_JOB_INTERVAL "10ms" must be a Go duration of at least 1s`)
}

type fakeStaler struct {
	stale []string
	err   error
}

func (f fakeStaler) Stale(context.Context, time.Time) ([]string, error) { return f.stale, f.err }

func TestHealth(t *testing.T) {
	for name, tc := range map[string]struct {
		s    fakeStaler
		code int
		body string
	}{
		"ok":    {fakeStaler{}, http.StatusOK, `{"status":"ok"}`},
		"stale": {fakeStaler{stale: []string{"chain-log"}}, http.StatusServiceUnavailable, `{"status":"stale","staleJobs":["chain-log"]}`},
		"error": {fakeStaler{err: errors.New("db down")}, http.StatusServiceUnavailable, `{"status":"error"}`},
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			healthHandler(tc.s).ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
			assert.Equal(t, tc.code, rec.Code)
			assert.JSONEq(t, tc.body, rec.Body.String())
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		})
	}

	rec := httptest.NewRecorder()
	healthHandler(fakeStaler{}).ServeHTTP(rec, httptest.NewRequest("POST", "/health", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
