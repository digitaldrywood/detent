package runnerruntime_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresRuntimeReportsMissingAttempt(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	owner := f.User(t, "owner", "owner", "owner@example.test", "", "")
	{
		const key = "first"
		response := f.Request(t, owner, http.MethodPost, f.Base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "missing-attempt-" + key}, Title: "Example " + key, Body: hubfixture.IssueBody(), State: "Todo"})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		hubfixture.Decode(t, response, &issue)
		response = f.Request(t, owner, http.MethodGet, f.Base+"/work-items/"+string(issue.WorkItemID)+"/runtime", nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var evidence tracker.NativeRuntimeEvidence
		hubfixture.Decode(t, response, &evidence)
		if evidence.Attempt != nil || !slices.Contains(evidence.Unavailable, "native_attempt") {
			t.Fatalf("missing attempt availability = %+v", evidence)
		}
	}
}

func TestNativeRuntimeHidesUnreadableProjectDependencies(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	foreign := hubfixture.NewNative(t, f.Hub, f.Project.OrganizationID, "runtime-foreign")
	{
		const key = "first"
		item := f.Create(t, "visible "+key)
		hidden := foreign.Create(t, "hidden "+key)
		itemPath := f.Base + "/work-items/" + string(item.WorkItemID)
		response := f.Hub.Request(t, http.MethodPost, itemPath+"/dependencies", hubfixture.AdminToken, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "runtime-hidden-" + key}, ExpectedRevision: item.Revision, RelatedWorkItemID: hidden.WorkItemID, Operation: "add"})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		response = f.Hub.Request(t, http.MethodGet, itemPath+"/runtime", f.Token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		if body := response.Body.String(); strings.Contains(body, string(hidden.WorkItemID)) || strings.Contains(body, hidden.Title) {
			t.Fatalf("runtime evidence leaked dependency from unreadable project: %s", body)
		}
	}
}
