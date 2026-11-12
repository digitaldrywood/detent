package hubserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
			writer, err := hostedPostgresAdmin(t, f.service).BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Rollback() }()
			var lockedID int64
			if err := writer.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3 FOR UPDATE", f.project.OrganizationID, f.project.ID, issue.WorkItemID).Scan(&lockedID); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
			request := newHubAPIRequest(t, http.MethodGet, test.path, f.token, nil).WithContext(ctx)
			done := make(chan int, 1)
			finished := make(chan struct{})
			defer func() {
				cancel()
				_ = writer.Rollback()
				<-finished
			}()
			go func() {
				defer close(finished)
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, request)
				done <- response.Code
			}()
			select {
			case code := <-done:
				if code != http.StatusOK {
					t.Fatalf("status = %d while the writer is held", code)
				}
			case <-ctx.Done():
				t.Fatal("read waited for the held writer transaction")
			}
		})
	}
}
