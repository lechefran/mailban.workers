package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lechefran/mailban.workers/internal/dbtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func fixedClock(s *Scheduler) { s.Now = func() time.Time { return t0 } }

func expectLock(mock sqlmock.Sqlmock, job string, got bool) {
	mock.ExpectQuery(dbtest.ExactSQL(lockQuery)).WithArgs(job).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(got))
}

func expectUnlock(mock sqlmock.Sqlmock, job string) {
	mock.ExpectQuery(dbtest.ExactSQL(unlockQuery)).WithArgs(job).
		WillReturnRows(sqlmock.NewRows([]string{"pg_advisory_unlock"}).AddRow(true))
}

func TestRunOnceSkipsWhenLockHeld(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	expectLock(mock, "j", false)

	called := false
	s := New(db, nil)
	ran, err := s.RunOnce(context.Background(), Job{Name: "j", Interval: time.Hour, Run: func(context.Context, *sql.DB, time.Time) (int64, error) {
		called = true
		return 0, nil
	}})
	require.NoError(t, err)
	assert.False(t, ran)
	assert.False(t, called, "job must not run without the lock")
}

func TestRunOnceRecordsSuccess(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	expectLock(mock, "j", true)
	mock.ExpectQuery(dbtest.ExactSQL(startRunQuery)).WithArgs("j", t0).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(11))
	mock.ExpectExec(dbtest.ExactSQL(endRunQuery)).WithArgs(int64(11), t0, true, int64(5), nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectUnlock(mock, "j")

	s := New(db, nil)
	fixedClock(s)
	var gotNow time.Time
	var hasDeadline bool
	ran, err := s.RunOnce(context.Background(), Job{Name: "j", Interval: time.Hour, Run: func(ctx context.Context, _ *sql.DB, now time.Time) (int64, error) {
		gotNow = now
		_, hasDeadline = ctx.Deadline()
		return 5, nil
	}})
	require.NoError(t, err)
	assert.True(t, ran)
	assert.Equal(t, t0, gotNow)
	assert.True(t, hasDeadline, "runs are bounded by a timeout")
}

func TestRunOnceRecordsFailure(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	expectLock(mock, "j", true)
	mock.ExpectQuery(dbtest.ExactSQL(startRunQuery)).WithArgs("j", t0).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(12))
	mock.ExpectExec(dbtest.ExactSQL(endRunQuery)).WithArgs(int64(12), t0, false, int64(2), "boom").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectUnlock(mock, "j")

	s := New(db, nil)
	fixedClock(s)
	ran, err := s.RunOnce(context.Background(), Job{Name: "j", Interval: time.Hour, Run: func(context.Context, *sql.DB, time.Time) (int64, error) {
		return 2, errors.New("boom")
	}})
	assert.True(t, ran)
	assert.EqualError(t, err, "boom")
}

func TestRunRunsAtStartAndOnInterval(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	mock.MatchExpectationsInOrder(true)
	for i := range 2 {
		expectLock(mock, "j", true)
		mock.ExpectQuery(dbtest.ExactSQL(startRunQuery)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(i + 1))
		mock.ExpectExec(dbtest.ExactSQL(endRunQuery)).WillReturnResult(sqlmock.NewResult(0, 1))
		expectUnlock(mock, "j")
	}

	var runs atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s := New(db, []Job{{Name: "j", Interval: 20 * time.Millisecond, Run: func(context.Context, *sql.DB, time.Time) (int64, error) {
		if runs.Add(1) == 2 {
			cancel()
		}
		return 0, nil
	}}})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("scheduler did not run twice and stop")
	}
	assert.Equal(t, int32(2), runs.Load())
}

func TestStale(t *testing.T) {
	jobs := []Job{
		{Name: "fresh", Interval: time.Hour},
		{Name: "old", Interval: time.Hour},
		{Name: "never", Interval: time.Minute},
		{Name: "never-young", Interval: time.Hour},
	}
	db, mock := dbtest.NewMockDB(t)
	mock.ExpectQuery(dbtest.ExactSQL(lastSuccessQuery)).
		WithArgs([]string{"fresh", "old", "never", "never-young"}).
		WillReturnRows(sqlmock.NewRows([]string{"name", "finished_at"}).
			AddRow("fresh", t0.Add(-90*time.Minute)).
			AddRow("old", t0.Add(-121*time.Minute)).
			AddRow("never", nil).
			AddRow("never-young", nil))

	s := New(db, jobs)
	s.started = t0.Add(-10 * time.Minute)
	stale, err := s.Stale(context.Background(), t0)
	require.NoError(t, err)
	assert.Equal(t, []string{"old", "never"}, stale)
}

func TestStaleQueryError(t *testing.T) {
	db, mock := dbtest.NewMockDB(t)
	mock.ExpectQuery(dbtest.ExactSQL(lastSuccessQuery)).WillReturnError(sql.ErrConnDone)
	_, err := New(db, []Job{{Name: "j", Interval: time.Hour}}).Stale(context.Background(), t0)
	assert.Error(t, err)
}
