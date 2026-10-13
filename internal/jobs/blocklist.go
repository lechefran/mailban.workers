package jobs

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	// BlocklistTimeout bounds fetching the blocked domain list.
	BlocklistTimeout = 30 * time.Second
	// BlocklistMaxBytes is the largest list accepted.
	BlocklistMaxBytes = 5 << 20
)

// The blocklist statements; tests match them exactly.
const (
	// Domains already blocked from another source keep that source, so a
	// manual entry is not deleted when it drops off the published list.
	upsertDomainsSQL = `INSERT INTO blocked_domains (domain, source)
SELECT d, $2 FROM unnest($1::text[]) AS d
ON CONFLICT (domain) DO NOTHING`
	deleteDroppedDomainsSQL = `DELETE FROM blocked_domains WHERE source = $1 AND domain <> ALL($2::text[])`
)

// RefreshBlockedDomains returns the blocklist job: it downloads the
// newline-separated domain list at url and, in one transaction, adds every
// listed domain with source = url and removes rows from that source that are
// no longer listed. It returns the number of domains in the list. An empty
// list is an error, so a broken download never clears the blocklist.
func RefreshBlockedDomains(client *http.Client, url string) func(context.Context, *sql.DB, time.Time) (int64, error) {
	return func(ctx context.Context, db *sql.DB, _ time.Time) (int64, error) {
		body, err := fetchList(ctx, client, url)
		if err != nil {
			return 0, err
		}
		domains, invalid := ParseDomains(body)
		if invalid > 0 {
			log.Printf("refresh-blocked-domains: skipped %d invalid lines", invalid)
		}
		if len(domains) == 0 {
			return 0, errors.New("blocked domain list is empty; keeping the current blocklist")
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		defer tx.Rollback()

		added, err := tx.ExecContext(ctx, upsertDomainsSQL, domains, url)
		if err != nil {
			return 0, fmt.Errorf("add blocked domains: %w", err)
		}
		removed, err := tx.ExecContext(ctx, deleteDroppedDomainsSQL, url, domains)
		if err != nil {
			return 0, fmt.Errorf("remove dropped domains: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		a, _ := added.RowsAffected()
		r, _ := removed.RowsAffected()
		log.Printf("refresh-blocked-domains: %d listed, %d added, %d removed", len(domains), a, r)
		return int64(len(domains)), nil
	}
}

// fetchList GETs url within BlocklistTimeout and returns at most
// BlocklistMaxBytes of body; a larger body or a non-200 status is an error.
func fetchList(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, BlocklistTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch blocked domain list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch blocked domain list: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, BlocklistMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read blocked domain list: %w", err)
	}
	if len(body) > BlocklistMaxBytes {
		return nil, fmt.Errorf("blocked domain list exceeds %d bytes", BlocklistMaxBytes)
	}
	return body, nil
}

// ParseDomains reads one domain per line, skipping blank lines and lines
// starting with '#'. Domains are lowercased and deduplicated in first-seen
// order; lines that are not plausible domain names are counted as invalid.
func ParseDomains(body []byte) (domains []string, invalid int) {
	seen := make(map[string]bool)
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 4096), BlocklistMaxBytes)
	for sc.Scan() {
		line := strings.ToLower(strings.TrimSpace(sc.Text()))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !validDomain(line) {
			invalid++
			continue
		}
		if !seen[line] {
			seen[line] = true
			domains = append(domains, line)
		}
	}
	return domains, invalid
}

// validDomain accepts lowercase names of at least two dot-separated labels,
// each 1-63 letters, digits or inner hyphens, at most 253 bytes in total.
func validDomain(d string) bool {
	if len(d) > 253 || !strings.Contains(d, ".") {
		return false
	}
	for label := range strings.SplitSeq(d, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
