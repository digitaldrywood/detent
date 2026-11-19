package runnerruntime_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
	item := f.Create(t, "Visible runtime item")
	hidden := foreign.Create(t, "Unreadable runtime dependency")
	ctx := t.Context()
	edge := corestore.Relationship{
		OrganizationID:    f.Project.OrganizationID,
		ProjectID:         f.Project.ID,
		WorkItemID:        item.WorkItemID,
		RelatedProjectID:  foreign.Project.ID,
		RelatedWorkItemID: hidden.WorkItemID,
		Type:              "dependency",
		Provenance:        "native",
	}
	if err := f.Hub.Repository.WriteTenant(ctx, f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetRelationship(ctx, edge, "add", f.Hub.Now())
	}); err != nil {
		t.Fatal(err)
	}
	itemPath := f.Base + "/work-items/" + string(item.WorkItemID)
	response := f.Request(t, http.MethodGet, itemPath+"/runtime", nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	if body := response.Body.String(); strings.Contains(body, string(hidden.WorkItemID)) || strings.Contains(body, hidden.Title) {
		t.Fatalf("runtime evidence leaked dependency from unreadable project: %s", body)
	}
}
