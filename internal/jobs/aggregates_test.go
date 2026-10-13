package jobs

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lechefran/mailban.workers/internal/dbtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckAggregatesConsistent(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	mock.ExpectQuery(dbtest.ExactSQL(countMismatchesSQL)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	n, err := CheckAggregates(false)(context.Background(), db, now)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestCheckAggregatesReportsMismatches(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	mock.ExpectQuery(dbtest.ExactSQL(countMismatchesSQL)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	n, err := CheckAggregates(false)(context.Background(), db, now)
	assert.Equal(t, int64(3), n)
	assert.ErrorContains(t, err, "3 addresses")

	// Both one-sided addresses and differing counts are mismatches.
	assert.Contains(t, countMismatchesSQL, "FULL JOIN actual a ON a.email_address = r.email_address")
	assert.Contains(t, countMismatchesSQL, "COALESCE(r.total_votes, 0) <> COALESCE(a.total, 0)")
}

func TestCheckAggregatesRepairs(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery(dbtest.ExactSQL(countMismatchesSQL)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectExec(dbtest.ExactSQL(repairAggregatesSQL)).WithArgs(now).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	n, err := CheckAggregates(true)(context.Background(), db, now)
	assert.Equal(t, int64(2), n)
	assert.ErrorContains(t, err, "repaired 2")
}

func TestCheckAggregatesRepairNothingToDo(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery(dbtest.ExactSQL(countMismatchesSQL)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectRollback()

	n, err := CheckAggregates(true)(context.Background(), db, now)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestCheckAggregatesRepairFailureRollsBack(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery(dbtest.ExactSQL(countMismatchesSQL)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(dbtest.ExactSQL(repairAggregatesSQL)).WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	n, err := CheckAggregates(true)(context.Background(), db, now)
	assert.Equal(t, int64(1), n)
	assert.ErrorContains(t, err, "repair failed")
}
