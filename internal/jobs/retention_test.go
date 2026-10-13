package jobs

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lechefran/mailban.workers/internal/dbtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var now = time.Date(2026, 10, 12, 0, 30, 0, 0, time.UTC)

func TestRetentionJobs(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		run  func(context.Context, *sql.DB, time.Time) (int64, error)
		sql  string
		args []driver.Value
	}{
		{"deactivate expired tokens", DeactivateExpiredTokens, `UPDATE api_tokens SET is_active = FALSE WHERE is_active AND expires_at <= $1`, []driver.Value{now}},
		{"delete old tokens", DeleteOldTokens, `DELETE FROM api_tokens WHERE NOT is_active AND expires_at < $1`, []driver.Value{now.Add(-30 * 24 * time.Hour)}},
		{"delete unvoted addresses", DeleteUnvotedAddresses, `DELETE FROM email_reputation WHERE total_votes = 0`, nil},
		{"delete expired opt-outs", DeleteExpiredOptOuts, `DELETE FROM optout_requests WHERE expires_at <= $1`, []driver.Value{now}},
		{"purge usage", PurgeUsage, `DELETE FROM api_usage WHERE day < ($1::date - 2)`, []driver.Value{"2026-10-12"}},
		{"prune job runs", PruneJobRuns, `DELETE FROM job_runs WHERE started_at < $1`, []driver.Value{now.Add(-30 * 24 * time.Hour)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := dbtest.NewMockDB(t)
			e := mock.ExpectExec(dbtest.ExactSQL(tc.sql))
			if tc.args != nil {
				e.WithArgs(tc.args...)
			} else {
				e.WithoutArgs()
			}
			e.WillReturnResult(sqlmock.NewResult(0, 4))
			n, err := tc.run(ctx, db, now)
			require.NoError(t, err)
			assert.Equal(t, int64(4), n)
		})
	}
}

func TestPurgeUsageUsesUTCDay(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	// 20:30 on the 11th in New York is already the 12th in UTC.
	ny := now.In(time.FixedZone("EDT", -4*3600))
	mock.ExpectExec(dbtest.ExactSQL(purgeUsageSQL)).WithArgs("2026-10-12").WillReturnResult(sqlmock.NewResult(0, 0))
	_, err := PurgeUsage(context.Background(), db, ny)
	require.NoError(t, err)
}

func TestExpireStaleVotes(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	mock.ExpectQuery(dbtest.ExactSQL(expireStaleVotesSQL)).
		WithArgs(now.Add(-365*24*time.Hour), now).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(9))
	n, err := ExpireStaleVotes(context.Background(), db, now)
	require.NoError(t, err)
	assert.Equal(t, int64(9), n)

	// The statement deletes, adjusts grouped counts and logs in one go.
	for _, part := range []string{
		"DELETE FROM email_votes",
		"RETURNING email_address, is_reputable, voter_id",
		"GROUP BY email_address",
		"positive_votes = r.positive_votes - g.positive",
		"total_votes = r.total_votes - g.total",
		"updated_at = $2::timestamptz",
		"INSERT INTO vote_log (action, address_id, mailbox_id, voter_id)",
		"'expire', sha256_hex(email_address), sha256_hex(canonical_email(email_address)), voter_id",
		"ORDER BY email_address, voter_id",
		"SELECT COUNT(*) FROM removed",
	} {
		assert.Contains(t, expireStaleVotesSQL, part)
	}
}

func TestExpireStaleVotesError(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	mock.ExpectQuery(dbtest.ExactSQL(expireStaleVotesSQL)).WillReturnError(sql.ErrConnDone)
	_, err := ExpireStaleVotes(context.Background(), db, now)
	assert.ErrorIs(t, err, sql.ErrConnDone)
}
