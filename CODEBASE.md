# Codebase Structure

## Overview
mailban.workers is a Go module (`github.com/lechefran/mailban.workers`, Go 1.27) with two commands: `cmd/worker`, which runs Mailban's background jobs against the API's PostgreSQL database, and `cmd/verify`, a public CLI that checks a deployment's vote log. The contract shared with the API (mailban2) is mailban2's `docs/reputation-spec.md`; the schema is mailban2's `migrations/` (version 7 or later required).

Dependencies: `github.com/jackc/pgx/v5/stdlib` (database/sql driver), testify and go-sqlmock (tests). Ed25519 and SHA-256 come from the standard library.

## Layout

### `cmd/worker`
- `main.go` — entrypoint: loads config, opens the database, checks the schema version, starts the scheduler and the health server, and shuts both down gracefully on SIGINT/SIGTERM (waits for in-flight runs to record their end).
- `config.go` — `loadConfig` reads and validates `DB_CONN`, `PORT` (default 8081), `LOG_SIGNING_KEY`, `BLOCKED_DOMAINS_URL`, `REPAIR_AGGREGATES`, `JOB_INTERVAL` (default 1h) and `VOTE_JOB_INTERVAL` (default 10m). All errors are joined and reported together.
- `jobs.go` — `jobDefs` (name, timeout and vote flag of every job) and `buildJobs`, which wires each name to its `internal/jobs` function and gives vote jobs `cfg.voteInterval`, the rest `cfg.interval`. `checkpoint-log` is left out without a signing key (warning logged), `refresh-blocked-domains` without a list URL.
- `health.go` — `GET /health`: 200 `{"status":"ok"}`, 503 `{"status":"stale","staleJobs":[…]}` from `Scheduler.Stale`, or 503 `{"status":"error"}` if job history cannot be read.
- `main_test.go` — config parsing/validation, job registration and overrides, health responses.

### `cmd/verify`
- `main.go` — flags `-api` (required) and `-key` (or `MAILBAN_API_KEY`); `run` fetches, verifies, prints the summary and returns the exit status (0 ok, 1 integrity failure, 2 score mismatch only, 3 could not run). `run` takes the `*http.Client`, so tests drive it against httptest.
- `client.go` — API client: `fetchKey` (nil on 404), `fetchCheckpoints` and `fetchLog` (page with `after`/`limit`, require ascending seq), `fetchEmails` (`reputation=all&limit=100`, follows `cursor`). Retries 429 after `Retry-After`; `sleep` is injectable.
- `verify.go` — pure checks: `verifyChain` (spec step 2: leaf recomputed for non-redacted chained rows, chain recomputed from the previous stored chain, checkpoint signatures and checkpoint chain vs. log; counts late unchained rows), `replay` (step 3: cast/change set, retract/expire/suspend delete, null address skipped), `compareScores` (step 4: maps API addresses to `sha256_hex(address)`, zero-vote API rows count as absent).
- `main_test.go` — fake API over httptest with paging: valid log, redacted rows (opt-out and erasure) accepted, tampered row, broken chain, checkpoint mismatch, bad signature, missing key, score mismatch, replay counts, 429 retry, API errors.

### `internal/db`
- `db.go` — `Open` (pgx via database/sql, pool of 25 so every job can hold a lock connection and a work connection, ping) and `CheckSchema` (`SELECT MAX(version) FROM schema_migrations` must be ≥ `MinSchemaVersion` = 7).
- `db_test.go` — schema version accepted/rejected.

### `internal/scheduler`
- `scheduler.go` — `Job{Name, Interval, Timeout, Run}`; `Scheduler.Run` starts one goroutine per job (run at start, then on a ticker, until ctx is cancelled). `RunOnce` takes `pg_try_advisory_lock(hashtext('mailban.workers:' || name))` on a dedicated `*sql.Conn` (skip silently if not acquired), inserts a `job_runs` row, runs the job with its timeout (default 1 min), records `finished_at/ok/rows/error`, and unlocks (dropping the connection if unlock fails). Bookkeeping uses a context detached from shutdown. `Stale(ctx, now)` reads each job's last successful run with a LATERAL query on the `(job, started_at DESC)` index and reports jobs older than 2× interval (or never successful 2× interval after start).
- `scheduler_test.go` — lock skip, success/failure recording, ticker loop and stop, staleness.

### `internal/jobs`
Every job is `func(ctx, *sql.DB, now time.Time) (int64, error)`; SQL lives in package constants that tests pin exactly.
- `retention.go` — `DeactivateExpiredTokens`, `DeleteOldTokens` (30 d), `ExpireStaleVotes` (one CTE statement: delete votes older than 365 d, subtract grouped counts from `email_reputation`, insert `expire` events ordered by address and voter, return the count), `DeleteUnvotedAddresses`, `DeleteExpiredOptOuts`, `PurgeUsage` (UTC day as a date string), `PruneJobRuns` (30 d).
- `chain.go` — `ChainLog`: in one transaction reads the last chained row (or genesis), counts late rows (unchained below the head), reads up to 5000 unchained rows above the head in seq order and stops at the first not older than 1 minute, computes leaves/chains with `internal/logchain`, stores them with one `unnest` UPDATE, commits, then returns an error if late rows exist.
- `checkpoint.go` — `ParseSigningKey` (base64 32-byte seed) and `CheckpointLog(key)`: signs the chain head when it is newer than the last checkpoint and inserts it.
- `aggregates.go` — `CheckAggregates(repair)`: FULL JOIN of `email_reputation` against grouped `email_votes`; mismatches fail the run; with repair, an upsert from the votes in a repeatable-read transaction.
- `blocklist.go` — `RefreshBlockedDomains(client, url)`: GET with 30 s / 5 MiB limits, `ParseDomains` (skip blanks/comments, lowercase, validate, dedupe), then insert-missing and delete-dropped for that source in one transaction. An empty list is an error.
- `*_test.go` — SQL and arguments of every job, chain genesis/continuation/young-row stop/late commit, checkpoint signature verified with the public key, aggregate check and repair, blocklist parsing and download failures.

### `internal/logchain`
- `logchain.go` — `Row`, `LeafBytes` (the spec's exact JSON bytes, fixed key order, `null` for NULL, `at` in microseconds), `Leaf`, `Chain`, `Genesis` and `CheckpointMessage`. Shared by the jobs and the verifier.
- `logchain_test.go` — byte format and a known vector computed outside Go with `sha256sum`.

### `internal/dbtest`
- `dbtest.go` — `NewMockDB` (sqlmock with a passthrough value converter for array arguments; expectations checked at cleanup) and `ExactSQL`.

## Other files
- `Dockerfile` — multi-stage build of `cmd/worker` onto distroless static (nonroot), port 8081.
- `README.md` — purpose, configuration, jobs, health, verifier usage, Railway deployment, least-privilege role.
- `AGENTS.md` — rules for automated contributors.
