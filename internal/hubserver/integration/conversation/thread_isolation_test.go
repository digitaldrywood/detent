package conversation_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTeamThreadWorkerReadIsolation(t *testing.T) {
	t.Parallel()
	hub := hubfixture.Open(t, hubfixture.Config{})
	project := hubfixture.NewNative(t, hub, "", "Thread isolation")
	issue := project.Create(t, "Thread subject")
	base := hubfixture.OrganizationBase(project.Organization()) + "/threads"
	issuePath := project.Base + "/work-items/" + string(issue.WorkItemID)
	readIssue := func() tracker.NativeIssue {
		t.Helper()
		response := project.Request(t, http.MethodGet, issuePath, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var current tracker.NativeIssue
		hubfixture.Decode(t, response, &current)
		return current
	}
	created := project.Request(t, http.MethodPost, base, operatortool.ThreadArguments{Scope: "issue", IssueID: string(issue.WorkItemID), ProjectID: "prj_ignored", Title: "Shared research"})
	hubfixture.RequireStatus(t, created, http.StatusCreated)
	var thread threadstore.Thread
	hubfixture.Decode(t, created, &thread)

	before := readIssue()
	author := hubtest.Credential(t, hub.Repository, project.Organization(), hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.Project.ID}, Now: hub.Now()})
	const threadBody = "thread-only-sentinel"
	for _, kind := range []string{"note", "finding", "question", "decision"} {
		response := hub.Request(t, http.MethodPost, base+"/"+thread.ID+"/posts", author.Token, operatortool.ThreadArguments{Kind: kind, Body: threadBody})
		hubfixture.RequireStatus(t, response, http.StatusCreated)
	}
	after := readIssue()
	if after.Revision != before.Revision || after.State != before.State || !after.LastActivityAt.Equal(before.LastActivityAt) {
		t.Fatalf("thread changed issue revision, lane or last activity: before=%+v after=%+v", before, after)
	}
	for _, path := range []string{"/comments", "/history", "/attempts"} {
		response := project.Request(t, http.MethodGet, issuePath+path, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		if strings.Contains(response.Body.String(), threadBody) {
			t.Fatalf("thread leaked into %s: %s", path, response.Body)
		}
	}
	mcp := hubfixture.MCP(t, hub.Handler(), hubfixture.MCPPath(project.Organization()), project.Token)
	args := operatortool.WorkReadRequest{ProjectID: string(project.Project.ID), Reference: string(issue.WorkItemID)}
	for _, tool := range []string{operatortool.WorkItem, operatortool.WorkComments, operatortool.WorkHistory, operatortool.WorkRuns, operatortool.WorkRelationships, operatortool.WorkReferences} {
		data := hubfixture.MCPData(t, mcp("tools/call", tool, args), false)
		if strings.Contains(string(data), threadBody) {
			t.Fatalf("thread leaked into %s: %s", tool, data)
		}
	}

	if err := hub.Repository.ReadTenant(t.Context(), project.Organization(), corestore.Serving, func(r tenantstore.Reader) error {
		item, err := r.WorkItem(t.Context(), project.Project.ID, issue.WorkItemID)
		if err != nil {
			return err
		}
		attempts, err := r.AttemptPage(t.Context(), item.ID, leasestore.AttemptCursor{}, 1)
		if err != nil {
			return err
		}
		comments, err := r.Comments(t.Context(), project.Project.ID, issue.WorkItemID, 0, 1)
		if err != nil {
			return err
		}
		if len(attempts.Items) != 0 || len(comments) != 0 {
			return fmt.Errorf("posting a thread created issue attempts or comments")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
