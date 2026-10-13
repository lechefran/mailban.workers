package jobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lechefran/mailban.workers/internal/dbtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDomains(t *testing.T) {
	body := "# disposable domains\n\nMailinator.com\r\n  temp-mail.org \nmailinator.com\nnot a domain\nlocalhost\n-bad.com\nbad-.com\na..b\nsub.example.co.uk\n#comment.com\n"
	domains, invalid := ParseDomains([]byte(body))
	assert.Equal(t, []string{"mailinator.com", "temp-mail.org", "sub.example.co.uk"}, domains)
	assert.Equal(t, 5, invalid)

	assert.False(t, validDomain(strings.Repeat("a", 64)+".com"))
	assert.True(t, validDomain(strings.Repeat("a", 63)+".com"))
}

func serveList(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRefreshBlockedDomains(t *testing.T) {
	srv := serveList(t, http.StatusOK, "# list\nA.com\nb.org\n")
	url := srv.URL + "/list.txt"
	domains := []string{"a.com", "b.org"}

	db, mock := dbtest.NewMockDB(t)
	mock.ExpectBegin()
	mock.ExpectExec(dbtest.ExactSQL(upsertDomainsSQL)).WithArgs(domains, url).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(dbtest.ExactSQL(deleteDroppedDomainsSQL)).WithArgs(url, domains).WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()

	n, err := RefreshBlockedDomains(srv.Client(), url)(context.Background(), db, now)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
}

func TestRefreshBlockedDomainsRollsBackOnError(t *testing.T) {
	srv := serveList(t, http.StatusOK, "a.com\n")
	db, mock := dbtest.NewMockDB(t)
	mock.ExpectBegin()
	mock.ExpectExec(dbtest.ExactSQL(upsertDomainsSQL)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(dbtest.ExactSQL(deleteDroppedDomainsSQL)).WillReturnError(assert.AnError)
	mock.ExpectRollback()

	_, err := RefreshBlockedDomains(srv.Client(), srv.URL)(context.Background(), db, now)
	assert.Error(t, err)
}

func TestRefreshBlockedDomainsRejectsBadDownloads(t *testing.T) {
	for name, srv := range map[string]*httptest.Server{
		"bad status": serveList(t, http.StatusNotFound, "a.com\n"),
		"empty list": serveList(t, http.StatusOK, "# nothing\n\n"),
		"too large":  serveList(t, http.StatusOK, strings.Repeat("a.com\n", BlocklistMaxBytes/6+1)),
	} {
		t.Run(name, func(t *testing.T) {
			db, _ := dbtest.NewMockDB(t) // no statements expected
			_, err := RefreshBlockedDomains(srv.Client(), srv.URL)(context.Background(), db, now)
			assert.Error(t, err)
		})
	}
}
