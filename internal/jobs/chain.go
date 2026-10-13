package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lechefran/mailban.workers/internal/logchain"
)

const (
	// ChainBatch is the most rows one ChainLog run chains.
	ChainBatch = 5000
	// ChainDelay is how old a row must be before it is chained, so
	// transactions that started earlier have time to commit their rows.
	ChainDelay = time.Minute
)

// The chain statements; tests match them exactly.
const (
	lastChainedSQL = `SELECT seq, chain FROM vote_log WHERE chain IS NOT NULL ORDER BY seq DESC LIMIT 1`
	lateRowsSQL    = `SELECT COUNT(*) FROM vote_log WHERE chain IS NULL AND seq < $1`
	unchainedSQL   = `SELECT seq, at, action, address_id, mailbox_id, voter_id, reputable
FROM vote_log
WHERE chain IS NULL AND seq > $1
ORDER BY seq
LIMIT $2`
	setChainSQL = `UPDATE vote_log AS v SET leaf = u.leaf, chain = u.chain
FROM unnest($1::bigint[], $2::text[], $3::text[]) AS u(seq, leaf, chain)
WHERE v.seq = u.seq`
)

// ChainLog fills leaf and chain for the next unchained vote_log rows in seq
// order, per the spec's "Leaf and chain" section, in one transaction. It
// chains at most ChainBatch rows above the last chained seq and stops at the
// first row not older than ChainDelay, so a younger row is never skipped by
// an older one with a higher seq. Unchained rows below the last chained seq
// (late commits) are left unchained and reported as an error after the
// eligible rows have been chained. It returns the number of rows chained.
func ChainLog(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	lastSeq, prev := int64(0), logchain.Genesis
	err = tx.QueryRowContext(ctx, lastChainedSQL).Scan(&lastSeq, &prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read last chained row: %w", err)
	}

	var late int64
	if lastSeq > 0 {
		if err := tx.QueryRowContext(ctx, lateRowsSQL, lastSeq).Scan(&late); err != nil {
			return 0, fmt.Errorf("count late rows: %w", err)
		}
	}

	rows, err := readUnchained(ctx, tx, lastSeq, now.Add(-ChainDelay))
	if err != nil {
		return 0, err
	}

	if len(rows) > 0 {
		seqs := make([]int64, len(rows))
		leaves := make([]string, len(rows))
		chains := make([]string, len(rows))
		for i, r := range rows {
			seqs[i] = r.Seq
			leaves[i] = logchain.Leaf(r)
			chains[i] = logchain.Chain(prev, leaves[i])
			prev = chains[i]
		}
		if _, err := tx.ExecContext(ctx, setChainSQL, seqs, leaves, chains); err != nil {
			return 0, fmt.Errorf("store chain: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
	}

	n := int64(len(rows))
	if late > 0 {
		return n, fmt.Errorf("%d late-committed vote_log rows below seq %d are left unchained", late, lastSeq)
	}
	return n, nil
}

// readUnchained returns the unchained rows after lastSeq in seq order, up to
// ChainBatch, stopping before the first row whose at is not before cutoff.
func readUnchained(ctx context.Context, tx *sql.Tx, lastSeq int64, cutoff time.Time) ([]logchain.Row, error) {
	rs, err := tx.QueryContext(ctx, unchainedSQL, lastSeq, ChainBatch)
	if err != nil {
		return nil, fmt.Errorf("read unchained rows: %w", err)
	}
	defer rs.Close()

	var out []logchain.Row
	for rs.Next() {
		var (
			r                       logchain.Row
			at                      time.Time
			address, mailbox, voter sql.NullString
			reputable               sql.NullBool
		)
		if err := rs.Scan(&r.Seq, &at, &r.Action, &address, &mailbox, &voter, &reputable); err != nil {
			return nil, err
		}
		if !at.Before(cutoff) {
			break
		}
		r.At = at.UnixMicro()
		r.AddressID = nullString(address)
		r.MailboxID = nullString(mailbox)
		r.VoterID = nullString(voter)
		if reputable.Valid {
			r.Reputable = &reputable.Bool
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

func nullString(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}
