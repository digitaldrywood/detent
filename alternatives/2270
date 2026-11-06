package hubserver

import (
	"net/http"
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
