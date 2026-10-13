package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// mismatchCTE lists every address whose email_reputation counts differ from
// its votes in email_votes, including addresses present on only one side. A
// reputation row with zero votes and no votes behind it is consistent (it
// waits for delete-unvoted-addresses).
const mismatchCTE = `WITH actual AS (
    SELECT email_address, SUM(is_reputable::int)::int AS positive, COUNT(*)::int AS total
    FROM email_votes
    GROUP BY email_address
), diff AS (
    SELECT COALESCE(r.email_address, a.email_address) AS email_address,
           COALESCE(a.positive, 0) AS positive,
           COALESCE(a.total, 0) AS total
    FROM email_reputation r
    FULL JOIN actual a ON a.email_address = r.email_address
    WHERE COALESCE(r.positive_votes, 0) <> COALESCE(a.positive, 0)
       OR COALESCE(r.total_votes, 0) <> COALESCE(a.total, 0)
)
`

// The aggregate statements; tests match them exactly.
const (
	countMismatchesSQL = mismatchCTE + `SELECT COUNT(*) FROM diff`
	// repairAggregatesSQL rewrites the mismatched rows from the votes, the
	// source of truth. An address left without votes gets 0/0 and is removed
	// by delete-unvoted-addresses.
	repairAggregatesSQL = mismatchCTE + `INSERT INTO email_reputation (email_address, positive_votes, total_votes, updated_at)
SELECT email_address, positive, total, $1::timestamptz FROM diff
ON CONFLICT (email_address) DO UPDATE
SET positive_votes = EXCLUDED.positive_votes, total_votes = EXCLUDED.total_votes, updated_at = EXCLUDED.updated_at`
)

// CheckAggregates returns the aggregate check job. It counts addresses whose
// email_reputation counts differ from COUNT/SUM over email_votes and returns
// that count, with an error when it is not zero so job_runs records the
// failure. With repair, the mismatched rows are rebuilt from the votes in
// the same repeatable-read transaction; a concurrent vote on a repaired row
// makes the transaction fail instead of writing stale counts, and the next
// run retries.
func CheckAggregates(repair bool) func(context.Context, *sql.DB, time.Time) (int64, error) {
	return func(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
		if !repair {
			var n int64
			if err := db.QueryRowContext(ctx, countMismatchesSQL).Scan(&n); err != nil {
				return 0, err
			}
			if n > 0 {
				return n, fmt.Errorf("%d addresses have aggregates that differ from their votes", n)
			}
			return 0, nil
		}

		tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
		if err != nil {
			return 0, err
		}
		defer tx.Rollback()

		var n int64
		if err := tx.QueryRowContext(ctx, countMismatchesSQL).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, nil
		}
		res, err := tx.ExecContext(ctx, repairAggregatesSQL, now)
		if err != nil {
			return n, fmt.Errorf("%d addresses have aggregates that differ from their votes; repair failed: %w", n, err)
		}
		if err := tx.Commit(); err != nil {
			return n, fmt.Errorf("%d addresses have aggregates that differ from their votes; repair failed: %w", n, err)
		}
		repaired, _ := res.RowsAffected()
		log.Printf("check-aggregates: repaired %d addresses from email_votes", repaired)
		return n, fmt.Errorf("%d addresses had aggregates that differed from their votes; repaired %d", n, repaired)
	}
}
