package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type backupBarrierConnector struct {
	timedConnector
	onBackup func() error
}

func (c backupBarrierConnector) Connect(ctx context.Context) (driver.Conn, error) {
	connection, err := c.timedConnector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &backupBarrierConn{timedConn: connection.(*timedConn), barrier: c}, nil
}

type backupBarrierConn struct {
	*timedConn
	barrier backupBarrierConnector
}

func (c *backupBarrierConn) NewBackup(destination string) (*sqlite.Backup, error) {
	backup, err := c.timedConn.NewBackup(destination)
	if err != nil {
		return nil, err
	}
	if err := c.barrier.onBackup(); err != nil {
		return nil, errors.Join(err, backup.Finish())
	}
	return backup, nil
}

func TestAdminBackupWithConcurrentWorkItemCreates(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	before := []tracker.NativeIssue{f.create(t, "first committed item"), f.create(t, "second committed item")}
	ready, resume := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	database := f.service.database
	reader := sql.OpenDB(backupBarrierConnector{
		timedConnector: timedConnector{dsn: sqliteReaderDSN(database.path, f.service.config.BusyTimeout), driver: database.reader.Driver()},
		onBackup: func() error {
			close(ready)
			select {
			case <-resume:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	if err := database.reader.Close(); err != nil {
		t.Fatal(err)
	}
	database.reader = reader
	output := filepath.Join(t.TempDir(), "snapshot.db")
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/admin/backup", testHubAdminToken, map[string]string{"output": output})
	}()
	select {
	case <-ready:
	case response := <-responses:
		t.Fatalf("backup ended before concurrent writes: %d %s", response.Code, response.Body)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	during := []tracker.NativeIssue{f.create(t, "first concurrent item"), f.create(t, "second concurrent item")}
	write, err := database.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer write.Rollback()
	if _, err := write.ExecContext(ctx, "UPDATE projects SET name=? WHERE id=?", "in flight", f.project.ID); err != nil {
		t.Fatal(err)
	}
	close(resume)
	response := <-responses
	requireNativeStatus(t, response, http.StatusNoContent)
	if response.Body.Len() != 0 {
		t.Fatalf("backup response disclosed content: %s", response.Body)
	}
	if err := write.Rollback(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup permissions: %v, %v", info, err)
	}
	if _, err := VerifyDatabase(ctx, output); err != nil {
		t.Fatal(err)
	}
	restoredPath := filepath.Join(t.TempDir(), "restored.db")
	if _, err := RestoreDatabase(ctx, output, restoredPath, []byte(recoveryTestToken)); err != nil {
		t.Fatal(err)
	}
	restored := openTestService(t, Config{DatabasePath: restoredPath})
	for _, issue := range before {
		response := performHubAPIRequest(t, restored, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID), recoveryTestToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var got tracker.NativeIssue
		decodeHubResponse(t, response, &got)
		if got.Title != issue.Title {
			t.Fatalf("restored title = %q, want %q", got.Title, issue.Title)
		}
	}
	for _, issue := range during {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID), f.token, nil), http.StatusOK)
		requireNativeStatus(t, performHubAPIRequest(t, restored, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID), recoveryTestToken, nil), http.StatusNotFound)
	}
}

func TestAdminBackupBoundary(t *testing.T) {
	t.Parallel()
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	seedHubAPIToken(t, service, "backup-operator", "operator-backup-token", apiScopeOperator)
	seedHubAPIToken(t, service, "backup-worker", "worker-backup-token", apiScopeWorker)
	seedHubAPIToken(t, service, "backup-scoped-admin", "scoped-backup-token", apiScopeAdmin)
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET native_only=1 WHERE id=?", "backup-scoped-admin"); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(t.TempDir(), "existing.db")
	if err := os.WriteFile(existing, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeOutput, err := filepath.Rel(workingDirectory, filepath.Join(t.TempDir(), "private-snapshot.db"))
	if err != nil {
		t.Fatal(err)
	}
	for index, test := range []struct {
		name, token string
		body        any
		status      int
	}{
		{"unauthenticated", "", nil, http.StatusUnauthorized},
		{"operator", "operator-backup-token", nil, http.StatusForbidden},
		{"worker", "worker-backup-token", nil, http.StatusForbidden},
		{"scoped admin", "scoped-backup-token", nil, http.StatusForbidden},
		{"missing output", testHubAdminToken, map[string]string{}, http.StatusUnprocessableEntity},
		{"relative output", testHubAdminToken, map[string]string{"output": relativeOutput}, http.StatusUnprocessableEntity},
		{"unknown field", testHubAdminToken, map[string]string{"unknown": "private"}, http.StatusUnprocessableEntity},
		{"source", testHubAdminToken, map[string]string{"output": service.database.path}, http.StatusConflict},
		{"existing", testHubAdminToken, map[string]string{"output": existing}, http.StatusConflict},
		{"invalid path", testHubAdminToken, map[string]string{"output": filepath.Join(t.TempDir(), "private\x00path")}, http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), fmt.Sprintf("snapshot-%d.db", index))
			body := test.body
			if body == nil {
				body = map[string]string{"output": output}
			}
			response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/admin/backup", test.token, body)
			requireNativeStatus(t, response, test.status)
			if strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), existing) || strings.Contains(response.Body.String(), service.database.path) {
				t.Fatalf("backup error disclosed path: %s", response.Body)
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected backup created output: %v", err)
			}
		})
	}
	content, err := os.ReadFile(existing)
	if err != nil || string(content) != "preserve" {
		t.Fatalf("existing output changed: %q, %v", content, err)
	}
	if _, err := os.Stat(relativeOutput); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected relative backup created output: %v", err)
	}
	t.Run("hosted Cloud", func(t *testing.T) {
		f := newHostedSecurityFixture(t)
		owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
		output := filepath.Join(t.TempDir(), "snapshot.db")
		requireNativeStatus(t, f.request(t, owner, http.MethodPost, "/api/v2/admin/backup", map[string]string{"output": output}), http.StatusNotFound)
		if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("hosted backup created output: %v", err)
		}
	})
}
