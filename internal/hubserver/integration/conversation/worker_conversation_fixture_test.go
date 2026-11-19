package conversation_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type workerConversationProject struct {
	project      hubfixture.Native
	runner       hubfixture.Runner
	issue        tracker.NativeIssue
	lease        tracker.NativeLease
	attempt, run string
	worker, id   string
}

func newWorkerConversationProject(t *testing.T, h *hubfixture.Hub, name string) workerConversationProject {
	t.Helper()
	p := hubfixture.NewNative(t, h, h.Primary, name)
	hubfixture.ApprovePolicy(t, h, p.Base+"/policy", hubfixture.Policy())
	issue := p.Create(t, name+" issue")
	runner := hubfixture.PrepareRunner(t, p, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
	runner.Enroll(t)
	capacity := hubfixture.CapacityReport(h.Now())
	capacity.SharedAccountAlias = string(runner.Binding.RunnerID)
	hubfixture.PublishCapacity(t, p, runner, capacity)
	worker := runner.Redemption.Credential
	claimed := h.Request(t, http.MethodPost, p.Base+"/claims", worker, hubfixture.ProviderClaim(runner, issue, hubapi.NewID("session")))
	hubfixture.RequireStatus(t, claimed, http.StatusOK)
	var lease tracker.NativeLease
	hubfixture.Decode(t, claimed, &lease)
	owner, err := p.Credential(t, p.Token)
	if err != nil {
		t.Fatal(err)
	}
	now := h.Now().UTC().Truncate(time.Microsecond)
	id := conversation.NewConversationID()
	if err := h.Repository.WriteTenant(t.Context(), p.Organization(), corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateConversation(t.Context(), conversationstore.Conversation{
			OrganizationID: p.Organization(), ProjectID: p.Project.ID, ID: id, OwnerPrincipalID: owner.ID,
			Origin: conversations.ConversationOriginUser, Title: "Linked example", Visibility: conversation.VisibilityShared,
			Status: conversation.StatusActive, Revision: 1, Preferences: conversation.DefaultPreferences(),
			WorkItemID: string(issue.WorkItemID), LinkedAt: &now, Execution: conversation.Execution{Status: conversation.ExecutionWaitingForRunner, UpdatedAt: now},
			CreatedAt: now, UpdatedAt: now,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	start := hubfixture.StartedEvent(lease)
	itemPath := p.Base + "/work-items/" + string(issue.WorkItemID)
	hubfixture.RequireStatus(t, h.Request(t, http.MethodPost, itemPath+"/events", worker, start), http.StatusOK)
	f := workerConversationProject{project: p, runner: runner, issue: issue, lease: lease, run: start.Data.RunID, attempt: start.Data.AttemptID, worker: worker, id: id}
	bindWorkerConversation(t, f)
	return f
}

func bindWorkerConversation(t *testing.T, f workerConversationProject) {
	t.Helper()
	bind := map[string]any{
		"lease_id": f.lease.ID, "fencing_token": f.lease.FencingToken, "attempt_id": f.attempt, "run_id": f.run,
		"capabilities": map[string]bool{"steer": true, "interrupt": true, "answer": true, "continue": true},
	}
	itemPath := f.project.Base + "/work-items/" + string(f.issue.WorkItemID)
	response := f.project.Hub.Request(t, http.MethodPost, itemPath+"/conversation/bind", f.worker, bind)
	hubfixture.RequireStatus(t, response, http.StatusOK)
}

func (f workerConversationProject) path() string {
	return f.project.Base + "/conversations/" + f.id
}

func (f workerConversationProject) controlsRequest(ctx context.Context, after int64, wait int) *http.Request {
	path := f.path() + "/controls?after=" + strconv.FormatInt(after, 10) + "&wait=" + strconv.Itoa(wait)
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer "+f.worker)
	request.Header.Set("X-Detent-Lease", string(f.lease.ID))
	request.Header.Set("X-Detent-Fencing-Token", strconv.FormatInt(int64(f.lease.FencingToken), 10))
	request.Header.Set("X-Detent-Attempt", f.attempt)
	return request
}

func (f workerConversationProject) controls(t *testing.T, after int64, wait int) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	f.project.Hub.Handler().ServeHTTP(response, f.controlsRequest(t.Context(), after, wait))
	return response
}
