package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"net/http"
	"testing"
)

func readWorkItem(t *testing.T, f nativeFixture, id tracker.NativeWorkItemID, query string) tracker.NativeIssue {
	t.Helper()
	if err := f.service.refreshBoardRecord(t.Context(), f.project.OrganizationID, viewstore.BoardCandidate{ProjectID: f.project.ID, WorkItemID: id}, f.service.config.Clock()); err != nil {
		t.Fatal(err)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(id)+query, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	return issue
}
