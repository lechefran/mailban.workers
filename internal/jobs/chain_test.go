package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lechefran/mailban.workers/internal/dbtest"
	"github.com/lechefran/mailban.workers/internal/logchain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptr[T any](v T) *T { return &v }

var logColumns = []string{"seq", "at", "action", "address_id", "mailbox_id", "voter_id", "reputable"}

func TestChainLogGenesis(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	old := now.Add(-2 * time.Minute)
	mock.ExpectBegin()
	mock.ExpectQuery(dbtest.ExactSQL(lastChainedSQL)).WillReturnRows(sqlmock.NewRows([]string{"seq", "chain"}))
	mock.ExpectQuery(dbtest.ExactSQL(unchainedSQL)).WithArgs(int64(0), ChainBatch).
		WillReturnRows(sqlmock.NewRows(logColumns).
			AddRow(1, old, "cast", "a1", "m1", "v1", true).
			AddRow(2, old, "optout", nil, nil, nil, nil))

	r1 := logchain.Row{Seq: 1, At: old.UnixMicro(), Action: "cast", AddressID: ptr("a1"), MailboxID: ptr("m1"), VoterID: ptr("v1"), Reputable: ptr(true)}
	r2 := logchain.Row{Seq: 2, At: old.UnixMicro(), Action: "optout"}
	l1, l2 := logchain.Leaf(r1), logchain.Leaf(r2)
	c1 := logchain.Chain(logchain.Genesis, l1)
	c2 := logchain.Chain(c1, l2)
	mock.ExpectExec(dbtest.ExactSQL(setChainSQL)).
		WithArgs([]int64{1, 2}, []string{l1, l2}, []string{c1, c2}).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	n, err := ChainLog(context.Background(), db, now)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
}

func TestChainLogContinuesAndStopsAtYoungRow(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	prev := logchain.Chain(logchain.Genesis, "ab")
	old := now.Add(-90 * time.Second)
	mock.ExpectBegin()
	mock.ExpectQuery(dbtest.ExactSQL(lastChainedSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"seq", "chain"}).AddRow(10, prev))
	mock.ExpectQuery(dbtest.ExactSQL(lateRowsSQL)).WithArgs(int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(dbtest.ExactSQL(unchainedSQL)).WithArgs(int64(10), ChainBatch).
		WillReturnRows(sqlmock.NewRows(logColumns).
			AddRow(12, old, "expire", "a", "m", "v", nil).
			AddRow(13, now.Add(-30*time.Second), "cast", "a", "m", "v", false). // too young: stop here
			AddRow(14, old, "cast", "a", "m", "v", false))                      // older, but after a young row

	r := logchain.Row{Seq: 12, At: old.UnixMicro(), Action: "expire", AddressID: ptr("a"), MailboxID: ptr("m"), VoterID: ptr("v")}
	leaf := logchain.Leaf(r)
	mock.ExpectExec(dbtest.ExactSQL(setChainSQL)).
		WithArgs([]int64{12}, []string{leaf}, []string{logchain.Chain(prev, leaf)}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	n, err := ChainLog(context.Background(), db, now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
}

func TestChainLogNothingToChain(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery(dbtest.ExactSQL(lastChainedSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"seq", "chain"}).AddRow(5, "c"))
	mock.ExpectQuery(dbtest.ExactSQL(lateRowsSQL)).WithArgs(int64(5)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(dbtest.ExactSQL(unchainedSQL)).WillReturnRows(sqlmock.NewRows(logColumns))
	mock.ExpectRollback()

	n, err := ChainLog(context.Background(), db, now)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestChainLogReportsLateCommitAfterChaining(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	old := now.Add(-5 * time.Minute)
	mock.ExpectBegin()
	mock.ExpectQuery(dbtest.ExactSQL(lastChainedSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"seq", "chain"}).AddRow(20, "prev"))
	mock.ExpectQuery(dbtest.ExactSQL(lateRowsSQL)).WithArgs(int64(20)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(dbtest.ExactSQL(unchainedSQL)).WithArgs(int64(20), ChainBatch).
		WillReturnRows(sqlmock.NewRows(logColumns).AddRow(21, old, "retract", "a", "m", "v", nil))
	mock.ExpectExec(dbtest.ExactSQL(setChainSQL)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	n, err := ChainLog(context.Background(), db, now)
	assert.Equal(t, int64(1), n, "eligible rows are still chained")
	assert.ErrorContains(t, err, "1 late-committed vote_log rows below seq 20")
}
