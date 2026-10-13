// Package scheduler runs each job on its own ticker. Runs are serialized
// across worker replicas with a PostgreSQL session advisory lock per job and
// recorded in job_runs, which the watchdog reads.
package scheduler

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log"
	"sync"
	"time"
)

// DefaultTimeout bounds one run of a job that sets no Timeout.
const DefaultTimeout = time.Minute

// bookkeepingTimeout bounds the lock, unlock and job_runs statements. They
// use a context detached from shutdown so a cancelled run is still recorded
// and its lock released.
const bookkeepingTimeout = 5 * time.Second

// maxErrorLen caps the error text stored in job_runs.
const maxErrorLen = 2000

// Func is one job: it does its work as of now and returns how many rows it
// changed.
type Func func(ctx context.Context, db *sql.DB, now time.Time) (int64, error)

// Job is a named Func run every Interval.
type Job struct {
	Name     string
	Interval time.Duration
	Timeout  time.Duration // 0 means DefaultTimeout
	Run      Func
}

// The statements the scheduler issues; tests match them exactly.
const (
	lockQuery     = `SELECT pg_try_advisory_lock(hashtext('mailban.workers:' || $1))`
	unlockQuery   = `SELECT pg_advisory_unlock(hashtext('mailban.workers:' || $1))`
	startRunQuery = `INSERT INTO job_runs (job, started_at) VALUES ($1, $2) RETURNING id`
	endRunQuery   = `UPDATE job_runs SET finished_at = $2, ok = $3, rows = $4, error = $5 WHERE id = $1`
	// lastSuccessQuery reads each job's latest successful run through the
	// (job, started_at DESC) index, so it stays cheap as job_runs grows.
	lastSuccessQuery = `SELECT j.name, r.finished_at
FROM unnest($1::text[]) AS j(name)
LEFT JOIN LATERAL (
    SELECT finished_at FROM job_runs
    WHERE job = j.name AND ok
    ORDER BY started_at DESC
    LIMIT 1
) r ON TRUE`
)

// Scheduler runs a fixed set of jobs against one database.
type Scheduler struct {
	db      *sql.DB
	jobs    []Job
	started time.Time
	// Now is the clock; tests replace it.
	Now func() time.Time
}

// New returns a Scheduler for jobs. The watchdog counts from now for jobs
// that have never succeeded.
func New(db *sql.DB, jobs []Job) *Scheduler {
	return &Scheduler{db: db, jobs: jobs, started: time.Now().UTC(), Now: func() time.Time { return time.Now().UTC() }}
}

// Jobs returns the registered jobs.
func (s *Scheduler) Jobs() []Job { return s.jobs }

// Run starts every job (once immediately, then on each interval) and blocks
// until ctx is cancelled and every in-flight run has returned. A failing job
// is logged and recorded and does not affect the others.
func (s *Scheduler) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, job := range s.jobs {
		wg.Go(func() {
			ticker := time.NewTicker(job.Interval)
			defer ticker.Stop()
			for ctx.Err() == nil {
				if _, err := s.RunOnce(ctx, job); err != nil && ctx.Err() == nil {
					log.Printf("job %s: %v", job.Name, err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		})
	}
	wg.Wait()
}

// RunOnce runs job once if this process wins its advisory lock. ran is false
// when another session holds the lock; the run is then skipped silently and
// nothing is recorded. Otherwise the run is recorded in job_runs and err is
// the job's error (or a bookkeeping error).
func (s *Scheduler) RunOnce(ctx context.Context, job Job) (ran bool, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("get connection: %w", err)
	}
	defer conn.Close()

	bookCtx, cancelBook := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancelBook()

	var locked bool
	if err := conn.QueryRowContext(ctx, lockQuery, job.Name).Scan(&locked); err != nil {
		return false, fmt.Errorf("try advisory lock: %w", err)
	}
	if !locked {
		return false, nil
	}
	defer func() {
		var released bool
		if uerr := conn.QueryRowContext(bookCtx, unlockQuery, job.Name).Scan(&released); uerr != nil || !released {
			log.Printf("job %s: release advisory lock: %v (dropping connection)", job.Name, uerr)
			// Discard the session so the lock cannot outlive this run.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()

	now := s.Now()
	var id int64
	if err := conn.QueryRowContext(bookCtx, startRunQuery, job.Name, now).Scan(&id); err != nil {
		return true, fmt.Errorf("record job start: %w", err)
	}

	timeout := job.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	rows, runErr := job.Run(runCtx, s.db, now)
	cancel()

	var errText any // NULL on success
	if runErr != nil {
		msg := runErr.Error()
		if len(msg) > maxErrorLen {
			msg = msg[:maxErrorLen]
		}
		errText = msg
	} else if rows > 0 {
		log.Printf("job %s: %d rows", job.Name, rows)
	}
	if _, err := conn.ExecContext(bookCtx, endRunQuery, id, s.Now(), runErr == nil, rows, errText); err != nil {
		if runErr != nil {
			return true, fmt.Errorf("%w (and record job end: %v)", runErr, err)
		}
		return true, fmt.Errorf("record job end: %w", err)
	}
	return true, runErr
}

// Stale returns the names of jobs whose last successful run finished more
// than twice their interval before now, or that have never succeeded although
// twice their interval has passed since the scheduler was created. It reads
// job_runs, so runs by other replicas count.
func (s *Scheduler) Stale(ctx context.Context, now time.Time) ([]string, error) {
	names := make([]string, len(s.jobs))
	for i, j := range s.jobs {
		names[i] = j.Name
	}
	rows, err := s.db.QueryContext(ctx, lastSuccessQuery, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	last := make(map[string]time.Time, len(names))
	for rows.Next() {
		var name string
		var finished sql.NullTime
		if err := rows.Scan(&name, &finished); err != nil {
			return nil, err
		}
		if finished.Valid {
			last[name] = finished.Time
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	stale := []string{}
	for _, j := range s.jobs {
		since, ok := last[j.Name]
		if !ok {
			since = s.started
		}
		if now.Sub(since) > 2*j.Interval {
			stale = append(stale, j.Name)
		}
	}
	return stale, nil
}
