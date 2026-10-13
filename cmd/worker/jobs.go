package main

import (
	"log"
	"net/http"
	"time"

	"github.com/lechefran/mailban.workers/internal/jobs"
	"github.com/lechefran/mailban.workers/internal/scheduler"
)

// jobDef is a job's name, run timeout (0: the scheduler default) and whether
// it is a vote job. Vote jobs run every VOTE_JOB_INTERVAL, the rest every
// JOB_INTERVAL.
type jobDef struct {
	name    string
	timeout time.Duration
	vote    bool
}

// jobDefs lists every job the worker knows, in registration order.
var jobDefs = []jobDef{
	{"deactivate-expired-tokens", 0, false},
	{"delete-old-tokens", 0, false},
	{"expire-stale-votes", 5 * time.Minute, true},
	{"delete-unvoted-addresses", 0, true},
	{"delete-expired-optouts", 0, false},
	{"purge-usage", 0, false},
	{"prune-job-runs", 0, false},
	{"chain-log", 5 * time.Minute, true},
	{"checkpoint-log", 0, true},
	{"check-aggregates", 5 * time.Minute, true},
	{"refresh-blocked-domains", 0, false},
}

// buildJobs returns the jobs to schedule for cfg. checkpoint-log needs
// LOG_SIGNING_KEY and refresh-blocked-domains needs BLOCKED_DOMAINS_URL;
// without them those jobs are left out.
func buildJobs(cfg config) []scheduler.Job {
	runs := map[string]scheduler.Func{
		"deactivate-expired-tokens": jobs.DeactivateExpiredTokens,
		"delete-old-tokens":         jobs.DeleteOldTokens,
		"expire-stale-votes":        jobs.ExpireStaleVotes,
		"delete-unvoted-addresses":  jobs.DeleteUnvotedAddresses,
		"delete-expired-optouts":    jobs.DeleteExpiredOptOuts,
		"purge-usage":               jobs.PurgeUsage,
		"prune-job-runs":            jobs.PruneJobRuns,
		"chain-log":                 jobs.ChainLog,
		"check-aggregates":          jobs.CheckAggregates(cfg.repair),
	}
	if cfg.signingKey != nil {
		runs["checkpoint-log"] = jobs.CheckpointLog(cfg.signingKey)
	} else {
		log.Println("warning: LOG_SIGNING_KEY is not set; checkpoint-log is disabled and the vote log is not signed")
	}
	if cfg.blockedURL != "" {
		runs["refresh-blocked-domains"] = jobs.RefreshBlockedDomains(&http.Client{Timeout: jobs.BlocklistTimeout}, cfg.blockedURL)
	}

	var out []scheduler.Job
	for _, d := range jobDefs {
		run, ok := runs[d.name]
		if !ok {
			continue
		}
		interval := cfg.interval
		if d.vote {
			interval = cfg.voteInterval
		}
		out = append(out, scheduler.Job{Name: d.name, Interval: interval, Timeout: d.timeout, Run: run})
	}
	return out
}
