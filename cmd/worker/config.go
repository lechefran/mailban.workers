package main

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lechefran/mailban.workers/internal/jobs"
)

const (
	// defaultPort is the health server's port when PORT is unset.
	defaultPort = "8081"
	// defaultInterval is how often non-vote jobs run when JOB_INTERVAL is unset.
	defaultInterval = time.Hour
	// defaultVoteInterval is how often vote jobs run when VOTE_JOB_INTERVAL
	// is unset.
	defaultVoteInterval = 10 * time.Minute
)

// config is the validated environment.
type config struct {
	dbConn       string
	port         string
	signingKey   ed25519.PrivateKey // nil: checkpoint-log is not registered
	blockedURL   string             // "": refresh-blocked-domains is not registered
	repair       bool
	interval     time.Duration // non-vote jobs (JOB_INTERVAL)
	voteInterval time.Duration // vote jobs (VOTE_JOB_INTERVAL)
}

// loadConfig reads and validates the environment through getenv.
func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		dbConn:       getenv("DB_CONN"),
		port:         getenv("PORT"),
		blockedURL:   strings.TrimSpace(getenv("BLOCKED_DOMAINS_URL")),
		interval:     defaultInterval,
		voteInterval: defaultVoteInterval,
	}
	var errs []error
	if cfg.dbConn == "" {
		errs = append(errs, errors.New("DB_CONN is required"))
	}
	if cfg.port == "" {
		cfg.port = defaultPort
	} else if p, err := strconv.Atoi(cfg.port); err != nil || p < 1 || p > 65535 {
		errs = append(errs, fmt.Errorf("PORT %q is not a valid port", cfg.port))
	}
	if s := getenv("LOG_SIGNING_KEY"); s != "" {
		key, err := jobs.ParseSigningKey(s)
		if err != nil {
			errs = append(errs, err)
		}
		cfg.signingKey = key
	}
	if cfg.blockedURL != "" {
		u, err := url.Parse(cfg.blockedURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, fmt.Errorf("BLOCKED_DOMAINS_URL %q must be an http(s) URL", cfg.blockedURL))
		}
	}
	if s := getenv("REPAIR_AGGREGATES"); s != "" {
		b, err := strconv.ParseBool(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("REPAIR_AGGREGATES %q is not a boolean", s))
		}
		cfg.repair = b
	}
	for name, dst := range map[string]*time.Duration{"JOB_INTERVAL": &cfg.interval, "VOTE_JOB_INTERVAL": &cfg.voteInterval} {
		s := getenv(name)
		if s == "" {
			continue
		}
		d, err := time.ParseDuration(s)
		if err != nil || d < time.Second {
			errs = append(errs, fmt.Errorf("%s %q must be a Go duration of at least 1s", name, s))
		}
		*dst = d
	}
	return cfg, errors.Join(errs...)
}
