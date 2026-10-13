// Package dbtest holds the go-sqlmock helpers shared by this module's tests.
package dbtest

import (
	"database/sql"
	"database/sql/driver"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// passthroughConverter lets slice arguments through, as the pgx driver does for ANY($1).
type passthroughConverter struct{}

func (passthroughConverter) ConvertValue(v any) (driver.Value, error) { return v, nil }

// NewMockDB returns a sqlmock database whose expectations are checked when the test ends.
func NewMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.ValueConverterOption(passthroughConverter{}))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, mock.ExpectationsWereMet())
		db.Close()
	})
	return db, mock
}

// ExactSQL matches a query's full text, so tests pin the fixed statements.
func ExactSQL(q string) string { return "^" + regexp.QuoteMeta(q) + "$" }
