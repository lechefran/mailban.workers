package db

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lechefran/mailban.workers/internal/dbtest"
	"github.com/stretchr/testify/assert"
)

func TestCheckSchema(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		rows *sqlmock.Rows
		err  error
		ok   bool
	}{
		"current":     {rows: sqlmock.NewRows([]string{"max"}).AddRow(7), ok: true},
		"newer":       {rows: sqlmock.NewRows([]string{"max"}).AddRow(9), ok: true},
		"older":       {rows: sqlmock.NewRows([]string{"max"}).AddRow(6)},
		"no rows":     {rows: sqlmock.NewRows([]string{"max"}).AddRow(nil)},
		"query fails": {err: errors.New(`relation "schema_migrations" does not exist`)},
	} {
		t.Run(name, func(t *testing.T) {
			db, mock := dbtest.NewMockDB(t)
			q := mock.ExpectQuery(dbtest.ExactSQL(SchemaVersionQuery))
			if tc.err != nil {
				q.WillReturnError(tc.err)
			} else {
				q.WillReturnRows(tc.rows)
			}
			err := CheckSchema(ctx, db)
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}
