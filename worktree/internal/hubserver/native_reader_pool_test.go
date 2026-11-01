package hubserver

import (
	"database/sql"
	"database/sql/driver"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestNativeReadsDoNotWaitForTheWriter(t *testing.T) {
	f := newNativeFixture(t, nil, "", "reader-pool")
	issue := f.create(t, "reader-pool")
	tests := []struct {
		name string
		path string
	}{
		{name: "authenticated comment page", path: f.base + "/work-items/" + string(issue.WorkItemID) + "/comments"},
		{name: "authenticated runtime evidence", path: f.base + "/work-items/" + string(issue.WorkItemID) + "/runtime"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writer, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Rollback() }()
			done := make(chan int, 1)
			go func() {
				done <- performHubAPIRequest(t, f.service, http.MethodGet, test.path, f.token, nil).Code
			}()
			select {
			case code := <-done:
				if code != http.StatusOK {
					t.Fatalf("status = %d while the writer is held", code)
				}
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("read waited for the held writer transaction")
			}
		})
	}
}

func TestAPITokenUseIsRecordedOncePerInterval(t *testing.T) {
	f := newNativeFixture(t, nil, "", "token-use")
	hash := apikey.HashToken(f.token)
	var writer *writerTiming
	f.service.echo.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			writer = writerTimingFrom(c.Request().Context())
			return err
		}
	})
	lastUsed := func() sql.NullString {
		t.Helper()
		f.service.tokenUseWork.Wait()
		var value sql.NullString
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT last_used_at FROM api_tokens WHERE token_hash = ?", hash).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	read := func() {
		t.Helper()
		var backgroundStatements atomic.Int64
		timing := &requestTiming{trace: func(query string, _ []driver.NamedValue) {
			if strings.HasPrefix(query, "UPDATE api_tokens SET last_used_at") {
				backgroundStatements.Add(1)
			}
		}}
		request := newHubAPIRequest(t, http.MethodGet, f.base+"/work-items?limit=1", f.token, nil)
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(request.Context(), timing)))
		requireNativeStatus(t, response, http.StatusOK)
		f.service.tokenUseWork.Wait()
		if writer == nil || writer.txs.Load() != 0 || backgroundStatements.Load() != 0 {
			t.Fatalf("token use inherited request timing: writer=%+v, statements=%d", writer, backgroundStatements.Load())
		}
	}
	stale := formatHubTime(time.Now().Add(-2 * tokenUseInterval))
	tests := []struct {
		name    string
		before  *string
		changed bool
	}{
		{name: "recent use is not rewritten", changed: false},
		{name: "stale use is recorded", before: &stale, changed: true},
	}
	read()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.before != nil {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET last_used_at = ? WHERE token_hash = ?", *test.before, hash); err != nil {
					t.Fatal(err)
				}
			}
			before := lastUsed()
			if !before.Valid {
				t.Fatal("token use was never recorded")
			}
			read()
			if after := lastUsed(); (after != before) != test.changed {
				t.Fatalf("last_used_at %q -> %q, changed want %t", before.String, after.String, test.changed)
			}
		})
	}
}
