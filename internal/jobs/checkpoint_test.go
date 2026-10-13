package jobs

import (
	"context"
	"crypto/ed25519"
	"database/sql/driver"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lechefran/mailban.workers/internal/dbtest"
	"github.com/lechefran/mailban.workers/internal/logchain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testSeed = strings.Repeat("k", ed25519.SeedSize)

func TestParseSigningKey(t *testing.T) {
	key, err := ParseSigningKey(base64.StdEncoding.EncodeToString([]byte(testSeed)))
	require.NoError(t, err)
	assert.Equal(t, ed25519.NewKeyFromSeed([]byte(testSeed)), key)

	_, err = ParseSigningKey("not base64!")
	assert.Error(t, err)
	_, err = ParseSigningKey(base64.StdEncoding.EncodeToString([]byte("short")))
	assert.Error(t, err)
}

// signatureArg captures the signature and checks it verifies with the public key.
type signatureArg struct {
	pub   ed25519.PublicKey
	msg   []byte
	valid bool
}

func (s *signatureArg) Match(v driver.Value) bool {
	str, ok := v.(string)
	if !ok {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(str)
	s.valid = err == nil && ed25519.Verify(s.pub, s.msg, sig)
	return s.valid
}

func TestCheckpointLogSignsNewHead(t *testing.T) {
	key := ed25519.NewKeyFromSeed([]byte(testSeed))
	chain := logchain.Chain(logchain.Genesis, "leaf")
	db, mock := dbtest.NewMockDB(t)
	mock.ExpectQuery(dbtest.ExactSQL(checkpointHeadSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"seq", "chain", "latest"}).AddRow(42, chain, 30))
	sig := &signatureArg{pub: key.Public().(ed25519.PublicKey), msg: []byte("mailban-checkpoint-v1:42:" + chain)}
	mock.ExpectExec(dbtest.ExactSQL(insertCheckpointSQL)).
		WithArgs(int64(42), chain, now, sig).
		WillReturnResult(sqlmock.NewResult(0, 1))

	n, err := CheckpointLog(key)(context.Background(), db, now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.True(t, sig.valid, "signature verifies with the public key")
}

func TestCheckpointLogSkipsWhenUpToDate(t *testing.T) {
	key := ed25519.NewKeyFromSeed([]byte(testSeed))
	ctx := context.Background()

	db, mock := dbtest.NewMockDB(t)
	mock.ExpectQuery(dbtest.ExactSQL(checkpointHeadSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"seq", "chain", "latest"}).AddRow(42, "c", 42))
	n, err := CheckpointLog(key)(ctx, db, now)
	require.NoError(t, err)
	assert.Zero(t, n)

	db, mock = dbtest.NewMockDB(t)
	mock.ExpectQuery(dbtest.ExactSQL(checkpointHeadSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"seq", "chain", "latest"}))
	n, err = CheckpointLog(key)(ctx, db, now)
	require.NoError(t, err, "an unchained log is not an error")
	assert.Zero(t, n)
}
