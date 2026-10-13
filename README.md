# mailban.workers

Background jobs for Mailban (the API is mailban2) and a public verifier for its vote log.

- **`cmd/worker`** runs the jobs below against the same PostgreSQL database as the API (mailban2): retention sweeps, the hash chain over the public vote log, signed log checkpoints, an aggregate consistency check and the disposable-domain blocklist. It serves `GET /health` for the platform health check.
- **`cmd/verify`** is a CLI anyone with an API key can run to check a Mailban deployment's public log: every leaf and chain hash, every checkpoint signature, and that replaying the log reproduces the published scores.

The contract shared with the API and website is `docs/reputation-spec.md` in mailban2. The database schema is mailban2's `migrations/`; the worker requires **schema version 7** (`007_reputation.sql`) or later and refuses to start otherwise.

## Jobs

Each job runs once at startup and then on its interval: the vote jobs every `VOTE_JOB_INTERVAL` (default 10 minutes), the rest every `JOB_INTERVAL` (default 1 hour). A run takes a PostgreSQL session advisory lock (`pg_try_advisory_lock(hashtext('mailban.workers:' || job))`), so several worker replicas never run the same job at once; a replica that does not get the lock skips that run silently. Every run that does get the lock is recorded in `job_runs` (start, finish, ok, rows, error). Runs time out after 1 minute unless noted.

| Job | Interval | What it does |
| --- | --- | --- |
| `deactivate-expired-tokens` | `JOB_INTERVAL` | Marks active API tokens past `expires_at` inactive. |
| `delete-old-tokens` | `JOB_INTERVAL` | Deletes inactive tokens that expired more than 30 days ago. |
| `expire-stale-votes` | `VOTE_JOB_INTERVAL` | In one statement: deletes votes unchanged for 365 days, subtracts them from `email_reputation` (grouped per address) and logs one `expire` event per vote in `vote_log`. Timeout 5 min. |
| `delete-unvoted-addresses` | `VOTE_JOB_INTERVAL` | Deletes `email_reputation` rows with `total_votes = 0`. |
| `delete-expired-optouts` | `JOB_INTERVAL` | Deletes expired opt-out requests. |
| `purge-usage` | `JOB_INTERVAL` | Deletes `api_usage` quota rows older than the UTC day before yesterday. |
| `prune-job-runs` | `JOB_INTERVAL` | Deletes `job_runs` history older than 30 days. |
| `chain-log` | `VOTE_JOB_INTERVAL` | Fills `leaf` and `chain` for the next unchained `vote_log` rows (≤ 5000 per run, in `seq` order, only rows older than 1 minute) in one transaction. Late commits (unchained rows below the chain head) are left unchained and fail the run. Timeout 5 min. |
| `checkpoint-log` | `VOTE_JOB_INTERVAL` | Signs `mailban-checkpoint-v1:<seq>:<chain>` for the chain head with Ed25519 and inserts it into `log_checkpoints` when it is newer than the last checkpoint. Only when `LOG_SIGNING_KEY` is set. |
| `check-aggregates` | `VOTE_JOB_INTERVAL` | Counts addresses whose `email_reputation` counts differ from their votes in `email_votes` (including addresses present on only one side). Any mismatch fails the run. With `REPAIR_AGGREGATES=true` the mismatched rows are rebuilt from the votes in the same run. Timeout 5 min. |
| `refresh-blocked-domains` | `JOB_INTERVAL` | Downloads `BLOCKED_DOMAINS_URL` (newline-separated domains, `#` comments; 30 s, 5 MiB max) and, in one transaction, adds the listed domains with `source = <URL>` and removes rows of that source no longer listed. Only when `BLOCKED_DOMAINS_URL` is set. |

## Configuration

| Variable | Required | Meaning |
| --- | --- | --- |
| `DB_CONN` | yes | PostgreSQL connection string (pgx). |
| `PORT` | no | Health server port, default `8081`. |
| `LOG_SIGNING_KEY` | no | Standard base64 of a 32-byte Ed25519 seed. Without it `checkpoint-log` is disabled (a warning is logged). The API publishes the matching public key at `GET /transparency/key`. Generate one with `openssl rand -base64 32`. |
| `BLOCKED_DOMAINS_URL` | no | http(s) URL of the disposable-domain list. Without it `refresh-blocked-domains` is disabled. |
| `REPAIR_AGGREGATES` | no | `true` lets `check-aggregates` rewrite mismatched aggregates from the votes (the source of truth). Default `false`. |
| `JOB_INTERVAL` | no | How often the non-vote jobs run, as a Go duration (≥ 1s), e.g. `30m`. Default `1h`. |
| `VOTE_JOB_INTERVAL` | no | How often the vote jobs run (same format). Default `10m`. `chain-log` chains at most 5000 rows per run, so this also caps how many vote-log rows are chained per interval. |

All variables are validated at startup; the worker exits with every problem listed.

## Health

`GET /health` on `PORT`:

- `200 {"status":"ok"}` when every scheduled job has succeeded within twice its interval.
- `503 {"status":"stale","staleJobs":["chain-log"]}` when a job's last successful run (from `job_runs`, by any replica) is older than twice its interval, or it has never succeeded and twice its interval has passed since this process started.
- `503 {"status":"error"}` when `job_runs` cannot be read.

The worker shuts down gracefully on SIGINT/SIGTERM: in-flight runs are cancelled, recorded and their locks released.

## Running

```sh
DB_CONN=postgres://… LOG_SIGNING_KEY=… go run ./cmd/worker
```

## Verifying the public log

```sh
go run github.com/lechefran/mailban.workers/cmd/verify@latest -api https://api.example.com -key mb_…
# or
MAILBAN_API_KEY=mb_… go run ./cmd/verify -api https://api.example.com
```

It fetches `/transparency/key`, every `/transparency/checkpoints` and `/transparency/log` page, and every `/emails?reputation=all` page, then:

1. recomputes each chained row's leaf (unless the row is redacted) and chain, and checks each checkpoint's signature and that its chain matches the log at that `seq`;
2. replays the log and compares each address's positive/total votes with the API.

Exit status: `0` everything matches; `1` a leaf, chain, signature or checkpoint check failed (the log was altered); `2` only scores differ (votes cast while fetching can race the log; re-run to confirm); `3` the verification could not run (bad arguments, network or API errors). Rate-limited requests are retried after `Retry-After`.

## Deploying (Railway)

Deploy this repo as a **separate Railway service** next to the API, built from the `Dockerfile` (it builds `cmd/worker` into a distroless image). Set `DB_CONN` to the same database as the API (ideally with the role below), set `LOG_SIGNING_KEY` and give the API the matching public key, and point the service's health check at `/health`. Apply mailban2's migrations through 007 before the first deploy. Several replicas are safe (advisory locks), though one is enough.

`job_runs` gains a row per run (about 860 a day at the default intervals); the `prune-job-runs` job deletes history older than 30 days.

## Least-privilege database role

The worker needs only these privileges (the functions `sha256_hex` and `canonical_email` are executable by `PUBLIC` by default; advisory locks need no grant):

```sql
CREATE ROLE mailban_workers LOGIN PASSWORD '…';
GRANT SELECT ON schema_migrations TO mailban_workers;
GRANT SELECT, UPDATE, DELETE ON api_tokens TO mailban_workers;
GRANT SELECT, DELETE ON email_votes, optout_requests, api_usage TO mailban_workers;
GRANT SELECT, UPDATE, DELETE ON email_reputation TO mailban_workers;
GRANT INSERT ON email_reputation TO mailban_workers;               -- only with REPAIR_AGGREGATES=true
GRANT SELECT, INSERT, UPDATE (leaf, chain) ON vote_log TO mailban_workers;
GRANT SELECT, INSERT ON log_checkpoints TO mailban_workers;
GRANT SELECT, INSERT, UPDATE, DELETE ON job_runs TO mailban_workers;
GRANT SELECT, INSERT, DELETE ON blocked_domains TO mailban_workers;
```

The worker never deletes from `vote_log` and only writes its `leaf` and `chain` columns.

## Development

```sh
gofmt -l . && go vet ./... && go test ./...
```

See `CODEBASE.md` for the layout and `AGENTS.md` for contribution rules.
