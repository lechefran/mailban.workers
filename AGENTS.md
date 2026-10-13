# Agents

1. Read `CODEBASE.md` before planning or changing anything, and update it after every change.
2. The vote log, leaf/chain/checkpoint byte formats and verification rules are defined by `docs/reputation-spec.md` in mailban2. Follow it exactly; a format change must land in the spec, mailban2 and this repo together.
3. Every code change needs tests (go-sqlmock for SQL, pinning statements with `dbtest.ExactSQL`; httptest for HTTP). Run `gofmt -l .`, `go vet ./...` and `go test ./...` until clean.
4. Match mailban2 style: stdlib first (`database/sql` with pgx, `log`, `net/http`), small focused files, godoc comments, minimal dependencies, no frameworks.
5. Jobs share the API's database: keep every statement bounded, transactional where it touches more than one table, and limited to the tables listed in the README's database role section.
