// Package jobs holds Mailban's background jobs. Each job has the signature
// func(ctx, *sql.DB, now) (int64, error): it does its work as of now and
// returns how many rows it changed. Scheduling, locking and run history live
// in internal/scheduler.
package jobs

import (
	"context"
	"database/sql"
	"time"
)

const (
	// TokenRetention is how long an inactive token is kept after it expires.
	TokenRetention = 30 * 24 * time.Hour
	// VoteRetention is how long a vote is kept after its last change.
	VoteRetention = 365 * 24 * time.Hour
	// JobRunRetention is how long job history is kept. It must stay well above
	// twice the longest job interval, or the watchdog would see jobs as stale.
	JobRunRetention = 30 * 24 * time.Hour
)

// The retention statements; tests match them exactly.
const (
	deactivateExpiredTokensSQL = `UPDATE api_tokens SET is_active = FALSE WHERE is_active AND expires_at <= $1`
	deleteOldTokensSQL         = `DELETE FROM api_tokens WHERE NOT is_active AND expires_at < $1`
	deleteUnvotedAddressesSQL  = `DELETE FROM email_reputation WHERE total_votes = 0`
	deleteExpiredOptOutsSQL    = `DELETE FROM optout_requests WHERE expires_at <= $1`
	purgeUsageSQL              = `DELETE FROM api_usage WHERE day < ($1::date - 2)`
	pruneJobRunsSQL            = `DELETE FROM job_runs WHERE started_at < $1`

	// expireStaleVotesSQL removes votes unchanged for VoteRetention, takes
	// them out of the aggregates and logs one 'expire' event per vote, all in
	// one statement (one transaction). Several voters can expire on one
	// address, so counts are grouped before the update. Events are inserted in
	// (address, voter) order so their seq order is deterministic.
	expireStaleVotesSQL = `WITH removed AS (
    DELETE FROM email_votes
    WHERE updated_at < $1::timestamptz
    RETURNING email_address, is_reputable, voter_id
), grouped AS (
    SELECT email_address, SUM(is_reputable::int)::int AS positive, COUNT(*)::int AS total
    FROM removed
    GROUP BY email_address
), counted AS (
    UPDATE email_reputation AS r
    SET positive_votes = r.positive_votes - g.positive,
        total_votes = r.total_votes - g.total,
        updated_at = $2::timestamptz
    FROM grouped g
    WHERE r.email_address = g.email_address
), logged AS (
    INSERT INTO vote_log (action, address_id, mailbox_id, voter_id)
    SELECT 'expire', sha256_hex(email_address), sha256_hex(canonical_email(email_address)), voter_id
    FROM removed
    ORDER BY email_address, voter_id
)
SELECT COUNT(*) FROM removed`
)

// DeactivateExpiredTokens flags active tokens past their expiry as inactive.
func DeactivateExpiredTokens(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	return execCount(ctx, db, deactivateExpiredTokensSQL, now)
}

// DeleteOldTokens removes inactive tokens that expired over TokenRetention ago.
func DeleteOldTokens(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	return execCount(ctx, db, deleteOldTokensSQL, now.Add(-TokenRetention))
}

// ExpireStaleVotes removes votes unchanged for VoteRetention, adjusts the
// aggregates and logs an 'expire' event per vote. It returns the number of
// votes removed.
func ExpireStaleVotes(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, expireStaleVotesSQL, now.Add(-VoteRetention), now).Scan(&n)
	return n, err
}

// DeleteUnvotedAddresses removes addresses whose every vote was retracted or
// expired, so no aggregate outlives its votes.
func DeleteUnvotedAddresses(ctx context.Context, db *sql.DB, _ time.Time) (int64, error) {
	return execCount(ctx, db, deleteUnvotedAddressesSQL)
}

// DeleteExpiredOptOuts removes opt-out requests whose link has expired.
func DeleteExpiredOptOuts(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	return execCount(ctx, db, deleteExpiredOptOutsSQL, now)
}

// PurgeUsage removes quota counters from before the day before yesterday
// (UTC), keeping today and the two previous days. The day is passed as a date string so the session time
// zone cannot shift it.
func PurgeUsage(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	return execCount(ctx, db, purgeUsageSQL, now.UTC().Format(time.DateOnly))
}

// PruneJobRuns removes job history older than JobRunRetention, so job_runs
// (about 1,500 rows a day, mostly chain-log) doesn't grow forever.
func PruneJobRuns(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	return execCount(ctx, db, pruneJobRunsSQL, now.Add(-JobRunRetention))
}

func execCount(ctx context.Context, db *sql.DB, query string, args ...any) (int64, error) {
	result, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
