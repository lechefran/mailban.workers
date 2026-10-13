package jobs

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/lechefran/mailban.workers/internal/logchain"
)

// The checkpoint statements; tests match them exactly.
const (
	checkpointHeadSQL = `SELECT v.seq, v.chain, (SELECT COALESCE(MAX(seq), 0) FROM log_checkpoints)
FROM vote_log v
WHERE v.chain IS NOT NULL
ORDER BY v.seq DESC
LIMIT 1`
	insertCheckpointSQL = `INSERT INTO log_checkpoints (seq, chain, signed_at, signature) VALUES ($1, $2, $3, $4)`
)

// ParseSigningKey decodes LOG_SIGNING_KEY: a standard base64 32-byte Ed25519 seed.
func ParseSigningKey(s string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("LOG_SIGNING_KEY is not standard base64: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("LOG_SIGNING_KEY must decode to %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// CheckpointLog returns the checkpoint job: when the highest chained row is
// newer than the latest checkpoint, it signs
// "mailban-checkpoint-v1:<seq>:<chain>" with key and stores the checkpoint.
// It returns 1 when a checkpoint was written, else 0.
func CheckpointLog(key ed25519.PrivateKey) func(context.Context, *sql.DB, time.Time) (int64, error) {
	return func(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
		var seq, latest int64
		var chain string
		err := db.QueryRowContext(ctx, checkpointHeadSQL).Scan(&seq, &chain, &latest)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil // nothing chained yet
		}
		if err != nil {
			return 0, fmt.Errorf("read chain head: %w", err)
		}
		if seq <= latest {
			return 0, nil
		}
		sig := base64.StdEncoding.EncodeToString(ed25519.Sign(key, logchain.CheckpointMessage(seq, chain)))
		if _, err := db.ExecContext(ctx, insertCheckpointSQL, seq, chain, now, sig); err != nil {
			return 0, fmt.Errorf("store checkpoint: %w", err)
		}
		return 1, nil
	}
}
