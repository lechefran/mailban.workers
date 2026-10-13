// Command verify independently checks a Mailban API's public vote log: it
// recomputes every leaf and chain, verifies the signed checkpoints and
// replays the log to compare the published scores (docs/reputation-spec.md
// in mailban2, "Verifying").
//
// Usage:
//
//	verify -api https://api.example.com [-key mb_…]
//
// The key may also be given in MAILBAN_API_KEY. Exit status: 0 when
// everything matches, 1 on any leaf, chain, signature or checkpoint failure,
// 2 when only scores differ (votes cast during the fetch can race; re-run),
// 3 when the verification could not run.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"time"
)

// Exit statuses.
const (
	exitOK        = 0
	exitFailed    = 1
	exitScores    = 2
	exitCannotRun = 3
)

// maxListed caps how many failing seqs or mismatched addresses are printed.
const maxListed = 20

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv, &http.Client{Timeout: time.Minute}))
}

// run parses args, fetches everything from the API through hc, verifies it
// and prints a summary to stdout. It returns the exit status.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string, hc *http.Client) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	api := fs.String("api", "", "Mailban API base URL (required)")
	key := fs.String("key", "", "API key (default $MAILBAN_API_KEY)")
	if err := fs.Parse(args); err != nil {
		return exitCannotRun
	}
	if *key == "" {
		*key = getenv("MAILBAN_API_KEY")
	}
	if *api == "" || *key == "" {
		fmt.Fprintln(stderr, "verify: -api and an API key (-key or MAILBAN_API_KEY) are required")
		fs.Usage()
		return exitCannotRun
	}

	c := newClient(*api, *key, hc)
	pub, err := c.fetchKey(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "verify: fetch key: %v\n", err)
		return exitCannotRun
	}
	cps, err := c.fetchCheckpoints(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "verify: fetch checkpoints: %v\n", err)
		return exitCannotRun
	}
	events, err := c.fetchLog(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "verify: fetch log: %v\n", err)
		return exitCannotRun
	}
	emails, err := c.fetchEmails(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "verify: fetch emails: %v\n", err)
		return exitCannotRun
	}

	res := verifyChain(events, cps, pub)
	mismatches := compareScores(replay(events), emails)
	printSummary(stdout, *api, res, len(cps), len(emails), mismatches)

	switch {
	case res.failed():
		return exitFailed
	case len(mismatches) > 0:
		return exitScores
	default:
		return exitOK
	}
}

func printSummary(w io.Writer, api string, res chainResult, checkpoints, addresses int, mismatches []scoreMismatch) {
	fmt.Fprintf(w, "Mailban vote log verification: %s\n", api)
	fmt.Fprintf(w, "log rows:              %d (%d chained, %d not chained yet)\n", res.Rows, res.Chained, res.Unchained)
	if res.LateUnchained > 0 {
		fmt.Fprintf(w, "  note: %d unchained rows lie below the chain head (late commits; not covered by any checkpoint)\n", res.LateUnchained)
	}
	if res.NoKey {
		fmt.Fprintln(w, "signing key:           none published")
	}
	fmt.Fprintf(w, "checkpoints verified:  %d of %d\n", res.CheckpointsVerified, checkpoints)
	printSeqs(w, "leaf failures:", res.LeafFailures)
	printSeqs(w, "chain failures:", res.ChainFailures)
	printSeqs(w, "signature failures:", res.SignatureFailures)
	printSeqs(w, "checkpoint mismatches:", res.CheckpointMismatches)
	fmt.Fprintf(w, "score mismatches:      %d (API lists %d addresses)\n", len(mismatches), addresses)
	for i, m := range mismatches {
		if i == maxListed {
			fmt.Fprintf(w, "  … and %d more\n", len(mismatches)-maxListed)
			break
		}
		fmt.Fprintf(w, "  %s: log %d/%d, API %d/%d (positive/total)\n", m.Address, m.Log.Positive, m.Log.Total, m.API.Positive, m.API.Total)
	}

	switch {
	case res.failed():
		fmt.Fprintln(w, "RESULT: FAILED — the log does not match its hashes or signatures")
	case len(mismatches) > 0:
		fmt.Fprintln(w, "RESULT: SCORES DIFFER — votes cast while fetching can race the log; re-run to confirm")
	default:
		fmt.Fprintln(w, "RESULT: OK")
	}
}

func printSeqs(w io.Writer, label string, seqs []int64) {
	fmt.Fprintf(w, "%-22s %d", label, len(seqs))
	if len(seqs) > 0 {
		fmt.Fprint(w, " (seq")
		for i, s := range seqs {
			if i == maxListed {
				fmt.Fprint(w, " …")
				break
			}
			fmt.Fprintf(w, " %d", s)
		}
		fmt.Fprint(w, ")")
	}
	fmt.Fprintln(w)
}
