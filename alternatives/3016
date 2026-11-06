package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReleaseRefreshLeavesLandingPostgres(t *testing.T) {
	f := newReleaseTenantFixture(t)
	seedReleaseTenantAudit(t, f)
	path := f.f.base + "/work-items/" + string(f.delivery.WorkItemID)
	response := f.f.request(t, f.owner, http.MethodPost, path+"/release-pick", f.request)
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, f.f.request(t, f.owner, http.MethodGet, path, nil), &issue)
	priorRevision := issue.Revision
	issue.State = "Merging"
	issue.Revision++
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(f.f.service.runDetailStore())
	version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: "org_security", ProjectID: f.f.project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	search, err := searchstore.NewContent(f.f.service.runDetailStore()).Prepare(t.Context(), searchstore.Document{OrganizationID: "org_security", ProjectID: f.f.project, WorkItemID: issue.WorkItemID, ID: "issue:" + string(issue.WorkItemID), SourceID: string(issue.WorkItemID), Kind: "issue", Scope: "issue", Revision: int64(issue.Revision), UpdatedAt: issue.UpdatedAt}, searchstore.Text{Title: issue.Title, Body: issue.Body, Labels: issue.Labels})
	if err != nil {
		t.Fatal(err)
	}
	err = f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.f.project)
		if err != nil {
			return err
		}
		project.States = append(project.States, tracker.NativeState{Name: "Merging", Transitions: []string{"In Progress"}})
		if _, err = w.UpdateProjectRecord(t.Context(), project); err != nil {
			return err
		}
		row, err := w.WorkItem(t.Context(), f.f.project, issue.WorkItemID)
		if err != nil {
			return err
		}
		row.WorkflowStateName = &issue.State
		if _, err = w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		if _, err = w.CreateCollaborationVersion(t.Context(), version); err != nil {
			return err
		}
		return w.PutSearch(t.Context(), search, int64(priorRevision))
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatched := false
	f.f.service.config.ConversationDispatch = conversationDispatchRequestFunc(func(context.Context, tenantstore.Writer, tracker.ProjectID, string, leasestore.Fence, time.Time) error {
		dispatched = true
		return nil
	})
	refresh := f.request
	refresh.IdempotencyKey = "refresh-from-merging"
	refresh.ExpectedRevision = issue.Revision
	refresh.HeadSHA, refresh.BaseSHA = strings.Repeat("1", 40), strings.Repeat("2", 40)
	f.verifier.input = refresh.ReleasePickInput
	requireNativeStatus(t, f.f.request(t, f.owner, http.MethodPost, path+"/release-pick", refresh), http.StatusOK)
	var after tracker.NativeIssue
	decodeHubResponse(t, f.f.request(t, f.owner, http.MethodGet, path, nil), &after)
	if after.State != "In Progress" || after.Revision != issue.Revision+2 || !dispatched {
		t.Fatalf("state=%s revision=%d dispatched=%v", after.State, after.Revision, dispatched)
	}
	var events []eventstore.Event
	if err := f.repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		events, err = r.Events(t.Context(), f.f.project, f.delivery.WorkItemID, 0, 50)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range events {
		event, err := content.Event(t.Context(), row)
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == "workflow.transitioned" && event.Data.ToState == "In Progress" {
			found = true
			if event.Data.Revision != after.Revision {
				t.Fatalf("event revision=%d, issue=%d", event.Data.Revision, after.Revision)
			}
		}
	}
	if !found {
		t.Fatalf("missing refresh workflow event: %+v", events)
	}
}
