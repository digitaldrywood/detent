package hubserver

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func threadBase(f nativeFixture) string {
	return "/api/v2/organizations/" + string(f.project.OrganizationID) + "/threads"
}

func createTestThread(t *testing.T, f nativeFixture, scope string, issue tracker.NativeWorkItemID) teamThread {
	t.Helper()
	in := teamThreadInput{Scope: scope, Title: "Shared research", Key: hubapi.NewID("intent")}
	if scope == "project" {
		in.ProjectID = string(f.project.ID)
	}
	if scope == "issue" {
		in.IssueID = string(issue)
		in.ProjectID = "prj_ignored"
	}
	r := performHubAPIRequest(t, f.service, http.MethodPost, threadBase(f), f.token, in)
	requireNativeStatus(t, r, http.StatusCreated)
	var out teamThread
	decodeHubResponse(t, r, &out)
	return out
}
