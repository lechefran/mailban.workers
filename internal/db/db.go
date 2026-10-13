// Package db opens the shared PostgreSQL database and refuses to run against
// a schema older than the one the jobs are written for.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
)

// MinSchemaVersion is the lowest schema_migrations version the jobs support
// (007_reputation.sql in mailban2).
const MinSchemaVersion = 7

// SchemaVersionQuery reads the applied schema version.
const SchemaVersionQuery = `SELECT MAX(version) FROM schema_migrations`

// Open connects to dsn with the pgx driver and pings it. Every running job
// holds one connection for its advisory lock and uses others for its work,
// so the pool is sized well above twice the number of jobs.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("could not successfully ping database: %w", err)
	}
	return db, nil
}

// CheckSchema returns an error unless schema_migrations reports at least
// MinSchemaVersion. A missing table or no rows also fails, since both mean
// migration 007 has not been applied.
func CheckSchema(ctx context.Context, db *sql.DB) error {
	var version sql.NullInt64
	if err := db.QueryRowContext(ctx, SchemaVersionQuery).Scan(&version); err != nil {
		return fmt.Errorf("read schema version (is migration 007 applied?): %w", err)
	}
	if !version.Valid || version.Int64 < MinSchemaVersion {
		return fmt.Errorf("schema version %d is older than required %d; apply mailban2 migrations first", version.Int64, MinSchemaVersion)
	}
	return nil
}
