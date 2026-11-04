package hubserver

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
	"strings"
)

func humanWorkflowFixtureStates() []tracker.NativeState {
	states := nativeFixtureStates()
	states[2].Transitions = append(states[2].Transitions, "Rework", "Cancelled", "Blocked")
	states[0].Transitions = append(states[0].Transitions, "Rework", "Cancelled", "Blocked", "Todo")
	return append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo", "Cancelled"}}, tracker.NativeState{Name: "Rework", Dispatchable: true, Transitions: []string{"Todo", "Cancelled"}}, tracker.NativeState{Name: "Cancelled", Terminal: true, Transitions: []string{"Todo"}})
}

func verifyPostgresHumanWorkflow(t *testing.T, service *Service, admin *sql.DB, project tracker.NativeProject, claim tracker.NativeClaim, owner, reporter, worker string, clock *atomic.Int64) {
	t.Helper()
	repository := service.config.TenantRepository
	base := "/api/v2/organizations/" + string(project.OrganizationID) + "/projects/" + string(project.ID)
	configureHumanWorkflowProject(t, service, project.OrganizationID, project.ID)
	if err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.ProjectPolicy(t.Context(), project.ID)
		claim.PolicyID = current.PolicyID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	create := func(token, key, state string) tracker.NativeIssue {
		t.Helper()
		return seedConfiguredHumanWorkflowIssue(t, service, project.OrganizationID, project.ID, token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, Title: "Example human workflow", Type: "bug", Body: workItemRecordBody, State: state})
	}
	transition := func(issue tracker.NativeIssue, key, state string) tracker.Transition {
		return tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: key}, ExpectedRevision: issue.Revision, State: state, Reason: "user_requested"}
	}
	workflow := func(issue tracker.NativeIssue) string {
		return base + "/work-items/" + string(issue.WorkItemID) + "/workflow"
	}
	t.Run("human Done and reopen", func(t *testing.T) {
		item := create(owner, "human-journey-create", "Todo")
		response := performHubAPIRequest(t, service, http.MethodPost, workflow(item), owner, transition(item, "human-journey-done", "Done"))
		requireNativeStatus(t, response, http.StatusOK)
		var done tracker.NativeIssue
		decodeHubResponse(t, response, &done)
		if done.State != "Done" || done.Verification == nil || done.Verification.State != tracker.VerificationPending {
			t.Fatal("human Done must retain filer verification")
		}
		response = performHubAPIRequest(t, service, http.MethodPost, workflow(done), owner, transition(done, "human-journey-reopen", "Todo"))
		requireNativeStatus(t, response, http.StatusOK)
		var reopened tracker.NativeIssue
		decodeHubResponse(t, response, &reopened)
		if reopened.State != "Todo" || reopened.Revision != done.Revision+1 || reopened.Verification != nil {
			t.Fatal("reopen must advance intent and clear verification")
		}
		response = performHubAPIRequest(t, service, http.MethodPost, workflow(reopened), owner, transition(reopened, "human-journey-same", "Todo"))
		requireNativeStatus(t, response, http.StatusOK)
		previousRevision := reopened.Revision
		decodeHubResponse(t, response, &reopened)
		if reopened.Revision != previousRevision {
			t.Fatal("same-state command must preserve the current issue revision")
		}
		response = performHubAPIRequest(t, service, http.MethodPost, workflow(reopened), owner, transition(reopened, "human-journey-blocked", "Blocked"))
		requireNativeStatus(t, response, http.StatusOK)
	})
	t.Run("workflow source revision", func(t *testing.T) {
		item := create(owner, "human-source-revision", "Todo")
		changeRevision := func(revision string) {
			t.Helper()
			if err := repository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				current, err := w.ProjectRecord(t.Context(), project.ID)
				if err != nil {
					return err
				}
				current.Core.WorkflowSourceRevision = revision
				_, err = w.UpdateProjectRecord(t.Context(), current)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		changeRevision(strings.Repeat("b", 40))
		defer changeRevision(strings.Repeat("a", 40))
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(item), owner, transition(item, "human-wrong-source", "Done")), http.StatusConflict)
	})
	t.Run("linked source workflow", func(t *testing.T) {
		item := create(owner, "human-linked-workflow", "Todo")
		if err := repository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateLinkedSource(t.Context(), eventstore.LinkedSource{OrganizationID: project.OrganizationID, ProjectID: project.ID, WorkItemID: item.WorkItemID, URL: "https://github.com/example-org/example-project/issues/1", TitleSupplied: true, BodySupplied: true})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		response := performHubAPIRequest(t, service, http.MethodPost, workflow(item), owner, transition(item, "human-linked-block", "Blocked"))
		requireNativeStatus(t, response, http.StatusOK)
		var blocked tracker.NativeIssue
		decodeHubResponse(t, response, &blocked)
		if blocked.LinkedSource == nil || blocked.LinkedSource.Status != "pending" {
			t.Fatal("human workflow must retain current linked-source authority")
		}
		response = performHubAPIRequest(t, service, http.MethodGet, base+"/work-items/"+string(item.WorkItemID), owner, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var read tracker.NativeIssue
		decodeHubResponse(t, response, &read)
		count := 0
		for _, reference := range read.ExternalReferences {
			if reference.Provider == "github" && reference.Kind == "issue" && reference.URL == blocked.LinkedSource.URL {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("linked-source reference count=%d, want one", count)
		}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(blocked), owner, transition(blocked, "human-linked-premature", "Todo")), http.StatusUnprocessableEntity)
	})
	t.Run("human contract hold", func(t *testing.T) {
		item := seedConfiguredHumanWorkflowIssue(t, service, project.OrganizationID, project.ID, owner, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "human-contract-draft"}, Title: "Example contract draft", Type: "bug", Body: "## Acceptance criteria\nIncomplete draft", State: "Backlog"})
		hold := transition(item, "human-contract-hold", "Blocked")
		hold.ReasonDetail = "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Complete the acceptance criteria\nfields:\n  issue_contract_return_state: Todo\n```"
		response := performHubAPIRequest(t, service, http.MethodPost, workflow(item), owner, hold)
		requireNativeStatus(t, response, http.StatusOK)
		var blocked tracker.NativeIssue
		decodeHubResponse(t, response, &blocked)
		if blocked.IssueContract == nil || blocked.IssueContract.ReturnState != "Todo" || blocked.IssueContract.HumanAction != "Complete the acceptance criteria" {
			t.Fatal("contract hold must retain its typed human action and return lane")
		}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(blocked), owner, transition(blocked, "human-contract-premature", "Todo")), http.StatusUnprocessableEntity)
		response = performHubAPIRequest(t, service, http.MethodPost, workflow(blocked), owner, transition(blocked, "human-contract-cancel", "Cancelled"))
		requireNativeStatus(t, response, http.StatusOK)
		var cancelled tracker.NativeIssue
		decodeHubResponse(t, response, &cancelled)
		if cancelled.IssueContract.HumanAction != "" || cancelled.IssueContract.ReturnState != "Todo" {
			t.Fatal("explicit human cancellation must clear attention without erasing the return intent")
		}
	})
	t.Run("configured human attention lane", func(t *testing.T) {
		var previous policystore.Revision
		if err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			previous, err = r.ProjectPolicy(t.Context(), project.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		descriptor, err := eventstore.NewContent(service.runDetailStore()).ProjectPolicy(t.Context(), previous)
		if err != nil {
			t.Fatal(err)
		}
		descriptor.Configuration = &policy.Configuration{Behavior: json.RawMessage(`{"BacklogAdmission":{"Sources":{"States":["Backlog"]},"TargetState":"Todo"},"Agent":{"StopRun":{"TargetState":"Needs input"}}}`), Prompt: "Follow the approved workflow"}
		descriptor = descriptor.WithID()
		approved, err := eventstore.NewContent(service.runDetailStore()).PrepareProjectPolicy(t.Context(), project.OrganizationID, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		approved.ProjectID, approved.ApprovedBy, approved.ApprovedAt = project.ID, "credential_owner", service.config.now()
		if err := repository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			return w.ApproveProjectPolicy(t.Context(), approved, previous.PolicyID)
		}); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := repository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ApproveProjectPolicy(t.Context(), previous, approved.PolicyID)
			}); err != nil {
				t.Error(err)
			}
		}()
		item := seedConfiguredHumanWorkflowIssue(t, service, project.OrganizationID, project.ID, owner, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "human-custom-draft"}, Title: "Example custom attention", Type: "bug", Body: "## Acceptance criteria\nIncomplete draft", State: "Backlog"})
		hold := transition(item, "human-custom-hold", "Needs input")
		hold.ReasonDetail = "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Complete the acceptance criteria\nfields:\n  issue_contract_return_state: Todo\n```"
		response := performHubAPIRequest(t, service, http.MethodPost, workflow(item), owner, hold)
		requireNativeStatus(t, response, http.StatusOK)
		var attention tracker.NativeIssue
		decodeHubResponse(t, response, &attention)
		if attention.State != "Needs input" || attention.IssueContract.HumanAction == "" {
			t.Fatal("configured attention lane lost its typed action")
		}
		response = performHubAPIRequest(t, service, http.MethodPost, workflow(attention), owner, transition(attention, "human-custom-cancel", "Cancelled"))
		requireNativeStatus(t, response, http.StatusOK)
		var cancelled tracker.NativeIssue
		decodeHubResponse(t, response, &cancelled)
		if cancelled.IssueContract.HumanAction != "" {
			t.Fatal("authorized human exit from configured attention must clear its action")
		}
	})
	t.Run("completion cancellation lock order", func(t *testing.T) {
		verifyHumanWorkflowCompletionRace(t, service, admin, project, claim, owner, worker, "completion")
	})
	t.Run("worker release cancellation lock order", func(t *testing.T) {
		verifyHumanWorkflowCompletionRace(t, service, admin, project, claim, owner, worker, "release")
	})
	issue := create(owner, "human-completed", "Done")
	var oldLease leasestore.Lease
	var oldAttempt leasestore.Attempt
	if err := repository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), project.ID, issue.WorkItemID)
		if err != nil {
			return err
		}
		oldLease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: string(claim.MachineID), SessionID: "human-completed-session", LeaseID: "human-completed-lease", TTL: time.Minute})
		if err != nil {
			return err
		}
		fence := leasestore.Fence{LeaseID: oldLease.LeaseID, FencingToken: oldLease.FencingToken}
		oldAttempt, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "human-completed-attempt", RunID: "human-completed-run", Fence: fence, Sequence: 1, Status: "running", StartedAt: service.config.now(), PayloadObjectKey: "example/attempt"})
		if err != nil {
			return err
		}
		oldAttempt, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: oldAttempt.ID, RunID: oldAttempt.RunID, Fence: fence, Sequence: 2, Status: "succeeded", PayloadObjectKey: "example/completed"})
		if err != nil {
			return err
		}
		_, err = w.Release(t.Context(), fence, "completed")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if oldAttempt.WorkItemRevision != int64(issue.Revision) {
		t.Fatal("succeeded attempt must cover the pre-command current revision")
	}
	dispatchCursor := service.notifications.dispatchCursor(dispatchNotificationKey(project.OrganizationID))
	request := transition(issue, "human-rework", "Rework")
	response := performHubAPIRequest(t, service, http.MethodPost, workflow(issue), owner, request)
	requireNativeStatus(t, response, http.StatusOK)
	if service.notifications.dispatchCursor(dispatchNotificationKey(project.OrganizationID)) == dispatchCursor {
		t.Fatal("human Rework did not wake dispatch waiters")
	}
	var changed tracker.NativeIssue
	decodeHubResponse(t, response, &changed)
	if changed.State != "Rework" || changed.Revision != issue.Revision+1 {
		t.Fatalf("human Rework state=%s revision=%d", changed.State, changed.Revision)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(issue), owner, request), http.StatusOK)
	stale := transition(issue, "human-stale", "Rework")
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(issue), owner, stale), http.StatusConflict)
	if err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItem(t.Context(), project.ID, issue.WorkItemID)
		if err != nil {
			return err
		}
		if row.Revision != int64(changed.Revision) || stringValue(row.WorkflowStateName) != "Rework" {
			return fmt.Errorf("current human intent was not stored")
		}
		version, err := r.CollaborationVersion(t.Context(), string(issue.WorkItemID), row.Revision)
		if err != nil {
			return err
		}
		raw, err := eventstore.NewContent(service.config.RunDetails).CollaborationVersion(t.Context(), version)
		if err != nil {
			return err
		}
		var stored tracker.NativeIssue
		if err := json.Unmarshal(raw, &stored); err != nil {
			return err
		}
		if stored.State != "Rework" || stored.Revision != changed.Revision {
			return fmt.Errorf("current collaboration version differs")
		}
		events, err := r.Events(t.Context(), project.ID, issue.WorkItemID, 0, 100)
		if err != nil {
			return err
		}
		found := false
		for _, event := range events {
			if event.Type == "workflow.transitioned" && event.EventRevision == row.Revision && event.FromState == "Done" && event.ToState == "Rework" {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("current human collaboration event missing")
		}
		attempt, err := r.LatestAttempt(t.Context(), row.ID)
		if err != nil {
			return err
		}
		if attempt.ID != oldAttempt.ID || attempt.Status != "succeeded" || attempt.Sequence != oldAttempt.Sequence {
			return fmt.Errorf("historical succeeded attempt was changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	verifyPostgresHumanTenantIsolation(t, service, project, issue, owner, clock)
	for _, mutation := range []struct {
		name  string
		query string
	}{
		{"revoked target token", "UPDATE api_tokens SET revoked_at=$3 WHERE organization_id=$1 AND id=$2"},
		{"expired target token", "UPDATE api_tokens SET expires_at=$3 WHERE organization_id=$1 AND id=$2"},
		{"removed target operation", "UPDATE runner_identities SET operations=ARRAY['heartbeat'] WHERE organization_id=$1 AND token_id=$2"},
		{"expired target identity", "UPDATE runner_identities SET expires_at=$3 WHERE organization_id=$1 AND token_id=$2"},
		{"removed target identity", "UPDATE runner_identities SET removed_at=$3 WHERE organization_id=$1 AND token_id=$2"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			item := create(owner, newNativeID("human-target"), "Todo")
			request := claim
			request.ProviderCandidates = append([]tracker.NativeCapacityCandidate(nil), claim.ProviderCandidates...)
			request.WorkItemID, request.SessionID = item.WorkItemID, newNativeID("session")
			request.ProviderCandidates[0].WorkItemID, request.ProviderCandidates[0].Revision = item.WorkItemID, item.Revision
			response := performHubAPIRequest(t, service, http.MethodPost, base+"/claims", worker, request)
			requireNativeStatus(t, response, http.StatusOK)
			var held tracker.NativeLease
			decodeHubResponse(t, response, &held)
			start := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("start")}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{AttemptID: newNativeID("attempt"), RunID: newNativeID("run"), LeaseID: held.ID, FencingToken: held.FencingToken, PolicyID: claim.PolicyID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "example-backend", Model: "example-model"}}}
			events := base + "/work-items/" + string(item.WorkItemID) + "/events"
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, events, worker, start), http.StatusOK)
			credential, _, err := service.authenticateAccessHash(t.Context(), apikey.HashToken(worker), "", "")
			if err != nil {
				t.Fatal(err)
			}
			var operations string
			var expiry time.Time
			var tokenExpiry, tokenRevocation *time.Time
			if err := admin.QueryRowContext(t.Context(), "SELECT expires_at, revoked_at FROM api_tokens WHERE organization_id=$1 AND id=$2", project.OrganizationID, credential.ID).Scan(&tokenExpiry, &tokenRevocation); err != nil {
				t.Fatal(err)
			}
			if err := admin.QueryRowContext(t.Context(), "SELECT operations::text, expires_at FROM runner_identities WHERE organization_id=$1 AND token_id=$2", project.OrganizationID, credential.ID).Scan(&operations, &expiry); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := admin.ExecContext(t.Context(), "UPDATE runner_identities SET operations=$3::text[], expires_at=$4, removed_at=NULL WHERE organization_id=$1 AND token_id=$2", project.OrganizationID, credential.ID, operations, expiry); err != nil {
					t.Error(err)
				}
				if _, err := admin.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at=$3, expires_at=$4 WHERE organization_id=$1 AND id=$2", project.OrganizationID, credential.ID, tokenRevocation, tokenExpiry); err != nil {
					t.Error(err)
				}
			}()
			args := []any{project.OrganizationID, credential.ID}
			if mutation.name != "removed target operation" {
				args = append(args, service.config.now().Add(-time.Second))
			}
			if _, err := admin.ExecContext(t.Context(), mutation.query, args...); err != nil {
				t.Fatal(err)
			}
			finish := start
			finish.IdempotencyKey, finish.Type, finish.Data.Sequence, finish.Data.Outcome = newNativeID("finish"), "run.finished", 2, "succeeded"
			denied := performHubAPIRequest(t, service, http.MethodPost, events, worker, finish)
			if denied.Code < 400 || denied.Code >= 500 {
				t.Fatalf("invalid target worker event accepted: %d", denied.Code)
			}
			denied = performHubAPIRequest(t, service, http.MethodPost, base+"/leases/"+string(held.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: held.FencingToken, Reason: "cancelled", CancelAttempt: true})
			if denied.Code < 400 || denied.Code >= 500 {
				t.Fatalf("invalid target worker release accepted: %d", denied.Code)
			}
			cancel := transition(item, newNativeID("cancel"), "Cancelled")
			cancel.LeaseID, cancel.FencingToken = held.ID, held.FencingToken
			response = performHubAPIRequest(t, service, http.MethodPost, workflow(item), owner, cancel)
			requireNativeStatus(t, response, http.StatusOK)
			var cancelled tracker.NativeIssue
			decodeHubResponse(t, response, &cancelled)
			if cancelled.Revision != item.Revision+1 || cancelled.State != "Cancelled" {
				t.Fatal("human cancellation did not advance intent")
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(item), owner, cancel), http.StatusOK)
			if err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				lease, err := r.Lease(t.Context(), string(held.ID))
				if err != nil {
					return err
				}
				attempt, err := r.Attempt(t.Context(), start.Data.AttemptID)
				if err != nil {
					return err
				}
				if lease.ReleasedAt == nil || lease.FencingToken != int64(held.FencingToken) || attempt.Status != "cancelled" || attempt.Sequence != 2 {
					return fmt.Errorf("human cancellation lost fenced terminal attempt")
				}
				event, err := r.LatestRunEvent(t.Context(), project.ID, attempt.ID)
				if err != nil {
					return err
				}
				stored, err := eventstore.NewContent(service.runDetailStore()).Event(t.Context(), event)
				if err != nil {
					return err
				}
				if stored.Actor.Kind != "integration" || stored.Actor.PrincipalID != "lease_release" || stored.Data.Run == nil || stored.Data.Run.Outcome != "cancelled" || stored.Data.Run.Sequence != 2 {
					return fmt.Errorf("human cancellation lost truthful canonical event")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	next := claim
	next.ProviderCandidates = append([]tracker.NativeCapacityCandidate(nil), claim.ProviderCandidates...)
	next.WorkItemID, next.SessionID = issue.WorkItemID, "human-rework-session"
	next.ProviderCandidates[0].WorkItemID, next.ProviderCandidates[0].Revision = changed.WorkItemID, changed.Revision
	claimed := performHubAPIRequest(t, service, http.MethodPost, base+"/claims", worker, next)
	requireNativeStatus(t, claimed, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, claimed, &lease)
	if lease.WorkItemID != issue.WorkItemID || lease.ID == tracker.LeaseID(oldLease.LeaseID) || lease.FencingToken <= tracker.FencingToken(oldLease.FencingToken) {
		t.Fatal("Rework did not claim a new fenced execution")
	}
	currentAttemptID := newNativeID("attempt")
	started := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "human-current-start"}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{AttemptID: currentAttemptID, RunID: newNativeID("run"), LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: claim.PolicyID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "example-backend", Model: "example-model"}}}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, started), http.StatusOK)
	for _, state := range []string{"Todo"} {
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(changed), owner, transition(changed, "human-active-"+state, state)), http.StatusConflict)
	}
	if err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.Lease(t.Context(), string(lease.ID))
		if err != nil {
			return err
		}
		attempt, err := r.Attempt(t.Context(), currentAttemptID)
		if err != nil {
			return err
		}
		if current.ReleasedAt != nil || attempt.Status != "running" || attempt.Sequence != 1 {
			return fmt.Errorf("human refusal changed current execution")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cancel := transition(changed, "human-live-cancel", "Cancelled")
	cancel.LeaseID, cancel.FencingToken = lease.ID, lease.FencingToken
	staleCancel := cancel
	staleCancel.IdempotencyKey = "human-live-stale-fence"
	staleCancel.FencingToken++
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(changed), owner, staleCancel), http.StatusConflict)
	cancelResponse := performHubAPIRequest(t, service, http.MethodPost, workflow(changed), owner, cancel)
	requireNativeStatus(t, cancelResponse, http.StatusOK)
	var cancelled tracker.NativeIssue
	decodeHubResponse(t, cancelResponse, &cancelled)
	if cancelled.State != "Cancelled" || cancelled.Revision != changed.Revision+1 {
		t.Fatal("live cancellation must advance the current human intent")
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(changed), owner, cancel), http.StatusOK)
	if err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.Lease(t.Context(), string(lease.ID))
		if err != nil {
			return err
		}
		attempt, err := r.Attempt(t.Context(), currentAttemptID)
		if err != nil {
			return err
		}
		if current.ReleasedAt == nil || current.ReleaseReason != "cancelled" || attempt.Status != "cancelled" {
			return fmt.Errorf("live cancellation did not settle the current fenced attempt")
		}
		event, err := r.LatestRunEvent(t.Context(), project.ID, currentAttemptID)
		if err != nil {
			return err
		}
		stored, err := eventstore.NewContent(service.runDetailStore()).Event(t.Context(), event)
		if err != nil {
			return err
		}
		if stored.Type != "run.finished" || stored.Data.Run == nil || stored.Data.Run.Outcome != "cancelled" || stored.Data.Run.Sequence != attempt.Sequence {
			return fmt.Errorf("cancellation omitted the canonical terminal run event")
		}
		old, err := r.Attempt(t.Context(), oldAttempt.ID)
		if err != nil {
			return err
		}
		if old.Status != "succeeded" || old.Sequence != oldAttempt.Sequence {
			return fmt.Errorf("cancellation changed immutable success")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	staleCompletion := started
	staleCompletion.IdempotencyKey, staleCompletion.Type, staleCompletion.Data.Sequence, staleCompletion.Data.Outcome = "human-cancel-stale-completion", "run.finished", 2, "succeeded"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, staleCompletion), http.StatusConflict)
	staleWorker := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "human-cancel-stale-worker", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ExpectedRevision: cancelled.Revision, State: "Todo", Reason: "worker_progress"}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(cancelled), worker, staleWorker), http.StatusConflict)
	reopenedResponse := performHubAPIRequest(t, service, http.MethodPost, workflow(cancelled), owner, transition(cancelled, "human-live-reopen", "Todo"))
	requireNativeStatus(t, reopenedResponse, http.StatusOK)
	var reopened tracker.NativeIssue
	decodeHubResponse(t, reopenedResponse, &reopened)
	if reopened.State != "Todo" || reopened.Revision != cancelled.Revision+1 {
		t.Fatal("reopen must preserve newer human intent")
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(changed), owner, cancel), http.StatusOK)
	if err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItem(t.Context(), project.ID, reopened.WorkItemID)
		if err != nil {
			return err
		}
		if row.Revision != int64(reopened.Revision) || stringValue(row.WorkflowStateName) != "Todo" {
			return fmt.Errorf("cancel replay overwrote newer human intent")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		lease tracker.LeaseID
		fence tracker.FencingToken
	}{
		{"released", tracker.LeaseID(oldLease.LeaseID), tracker.FencingToken(oldLease.FencingToken)},
		{"partial", "", tracker.FencingToken(oldLease.FencingToken)},
	} {
		supplied := transition(changed, "human-stale-fence-"+test.name, "Todo")
		supplied.LeaseID, supplied.FencingToken = test.lease, test.fence
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(changed), owner, supplied), http.StatusConflict)
	}
	const revokedToken = "example_revoked_human_token"
	now := service.config.now()
	if err := repository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: project.OrganizationID, ID: "credential_human_revoked", Name: "Example human", Hash: apikey.HashToken(revokedToken), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: "user_owner", MembershipID: "membership_user_owner", KeyScope: "admin", ProjectAccess: "all", Kind: "personal"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, status, err := service.authenticateAccessHash(t.Context(), apikey.HashToken(revokedToken), "", ""); err != nil || status != http.StatusOK {
		t.Fatalf("authenticate before revocation: %d %v", status, err)
	}
	if err := repository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessCredential(t.Context(), "credential_human_revoked", service.config.now())
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(changed), revokedToken, transition(changed, "human-revoked", "Todo")), http.StatusUnauthorized)
	expired := create(owner, "human-expired", "Done")
	var expiredLease leasestore.Lease
	if err := repository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), project.ID, expired.WorkItemID)
		if err != nil {
			return err
		}
		expiredLease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: string(claim.MachineID), SessionID: "human-expired-session", LeaseID: "human-expired-lease", TTL: time.Minute})
		if err != nil {
			return err
		}
		_, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "human-expired-attempt", RunID: "human-expired-run", Fence: leasestore.Fence{LeaseID: expiredLease.LeaseID, FencingToken: expiredLease.FencingToken}, Sequence: 1, Status: "running", PayloadObjectKey: "example/expired"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=acquired_at-interval '3 minutes',renewed_at=renewed_at-interval '2 minutes',expires_at=expires_at-interval '2 minutes' WHERE organization_id=$1 AND lease_id=$2`, project.OrganizationID, expiredLease.LeaseID); err != nil {
		t.Fatal(err)
	}
	expiredRequest := transition(expired, "human-expired-cancel", "Cancelled")
	expiredRequest.LeaseID, expiredRequest.FencingToken = tracker.LeaseID(expiredLease.LeaseID), tracker.FencingToken(expiredLease.FencingToken)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(expired), owner, expiredRequest), http.StatusOK)
	if err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		lease, err := r.Lease(t.Context(), expiredLease.LeaseID)
		if err != nil {
			return err
		}
		attempt, err := r.Attempt(t.Context(), "human-expired-attempt")
		if err != nil {
			return err
		}
		if lease.ReleasedAt == nil || lease.ReleaseReason != "expired" || attempt.Status != "cancelled" {
			return fmt.Errorf("expired cancellation did not use fenced lease owner")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	verifyPostgresHumanReceiptExpiry(t, service, project.OrganizationID, base, owner, clock)
	own := create(reporter, "human-reporter-own", "Triage")
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, workflow(own), reporter, transition(own, "human-own-cancel", "Cancelled")), http.StatusOK)
	denied := performHubAPIRequest(t, service, http.MethodPost, workflow(changed), reporter, transition(changed, "human-foreign-cancel", "Cancelled"))
	if denied.Code != http.StatusNotFound && denied.Code != http.StatusForbidden {
		t.Fatalf("foreign reporter cancel=%d %s", denied.Code, denied.Body.String())
	}
}

func verifyPostgresHumanTenantIsolation(t *testing.T, first *Service, project tracker.NativeProject, original tracker.NativeIssue, firstToken string, clock *atomic.Int64) {
	t.Helper()
	const organization tracker.OrganizationID = "org_human_other"
	const token = "example_other_human_token"
	repository := first.config.TenantRepository
	seedStartupOrganization(t, repository, organization)
	cfg := first.config
	hosted := *cfg.Hosted
	hosted.OrganizationID, hosted.WorkOSOrganizationID = string(organization), "provider_org_human_other"
	cfg.Hosted = &hosted
	if _, err := cfg.Hosted.Provider.CreateMembership(t.Context(), "user_human_other", "provider_org_human_other", "owner"); err != nil {
		t.Fatal(err)
	}
	now := cfg.now()
	descriptor := configuredHumanWorkflowPolicy(hubTestPolicy(), humanWorkflowFixtureStates())
	approved, err := eventstore.NewContent(first.runDetailStore()).PrepareProjectPolicy(t.Context(), organization, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	approved.ProjectID, approved.ApprovedBy, approved.ApprovedAt = project.ID, "credential_human_other", now
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project.ID, Name: "Other example project", Slug: "other-example", Profile: "native", WorkflowSource: "detent.yaml", WorkflowSourceRevision: strings.Repeat("a", 40)}, States: humanWorkflowFixtureStates()}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "credential_human_other", Name: "Other owner", Hash: apikey.HashToken(token), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: "user_human_other", MembershipID: "membership_user_human_other", KeyScope: "admin", ProjectAccess: "all", Kind: "personal"}); err != nil {
			return err
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: "user_human_other", Email: "other@example.test", MembershipID: "membership_user_human_other", Role: "owner", Active: true, PrincipalID: "credential_human_other", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: "user_human_other", ProjectID: project.ID, CanWrite: true}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "credential_human_other", ProjectID: project.ID}); err != nil {
			return err
		}
		return w.ApproveProjectPolicy(t.Context(), approved, "")
	}); err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	}()
	base := "/api/v2/organizations/" + string(organization) + "/projects/" + string(project.ID) + "/work-items"
	issue := seedConfiguredHumanWorkflowIssue(t, second, organization, project.ID, token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "other-human-create"}, Title: "Other human issue", Type: "bug", Body: workItemRecordBody, State: "Done"})
	request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "other-human-rework"}, ExpectedRevision: issue.Revision, State: "Rework", Reason: "user_requested"}
	requireNativeStatus(t, performHubAPIRequest(t, second, http.MethodPost, base+"/"+string(issue.WorkItemID)+"/workflow", token, request), http.StatusOK)
	verifyPostgresHumanReceiptExpiry(t, second, organization, "/api/v2/organizations/"+string(organization)+"/projects/"+string(project.ID), token, clock)
	for _, test := range []struct {
		service     *Service
		path, token string
	}{
		{first, base + "/" + string(original.WorkItemID) + "/workflow", firstToken},
		{second, "/api/v2/organizations/" + string(project.OrganizationID) + "/projects/" + string(project.ID) + "/work-items/" + string(original.WorkItemID) + "/workflow", token},
	} {
		response := performHubAPIRequest(t, test.service, http.MethodPost, test.path, test.token, request)
		if response.Code != http.StatusForbidden && response.Code != http.StatusNotFound && response.Code != http.StatusUnauthorized {
			t.Fatalf("wrong-tenant transition=%d %s", response.Code, response.Body.String())
		}
	}
}

func verifyPostgresHumanReceiptExpiry(t *testing.T, service *Service, organization tracker.OrganizationID, base, token string, clock *atomic.Int64) {
	t.Helper()
	start := service.config.now().UTC().Truncate(time.Microsecond)
	clock.Store(start.UnixNano())
	defer clock.Store(0)
	cfg := service.config
	issue := seedConfiguredHumanWorkflowIssue(t, service, organization, "project_example", token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "human-expiry-create"}, Title: "Example receipt expiry", Type: "bug", Body: workItemRecordBody, State: "Done"})
	path := base + "/work-items/" + string(issue.WorkItemID) + "/workflow"
	request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "human-expiry-rework"}, ExpectedRevision: issue.Revision, State: "Rework", Reason: "user_requested"}
	first := performHubAPIRequest(t, service, http.MethodPost, path, token, request)
	requireNativeStatus(t, first, http.StatusOK)
	var changed tracker.NativeIssue
	decodeHubResponse(t, first, &changed)
	clock.Store(start.Add(receiptstore.ReplayRetention - time.Nanosecond).UnixNano())
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, token, request), http.StatusOK)
	newerRequest := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "human-expiry-newer"}, ExpectedRevision: changed.Revision, State: "Todo", Reason: "user_requested"}
	newerResponse := performHubAPIRequest(t, service, http.MethodPost, path, token, newerRequest)
	requireNativeStatus(t, newerResponse, http.StatusOK)
	var newer tracker.NativeIssue
	decodeHubResponse(t, newerResponse, &newer)
	var receipt eventstore.Receipt
	var before int64
	if err := cfg.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		page, err := r.(receiptstore.RetentionReader).ReceiptExpiryPage(t.Context(), start.Add(receiptstore.ReplayRetention+time.Nanosecond), receiptstore.MaxRetentionBatch)
		if err != nil {
			return err
		}
		for _, row := range page {
			if row.CommandKey == request.IdempotencyKey {
				receipt = row
			}
		}
		before, err = r.ArchiveBytes(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	clock.Store(start.Add(receiptstore.ReplayRetention).UnixNano())
	expired := performHubAPIRequest(t, service, http.MethodPost, path, token, request)
	requireNativeStatus(t, expired, http.StatusGone)
	var after int64
	if receipt.CommandKey == "" || !receipt.CreatedAt.Equal(start) || receipt.ResponseReplayUntilNs != start.Add(receiptstore.ReplayRetention).UnixNano() {
		t.Fatalf("receipt lifecycle metadata = %+v", receipt)
	}
	if err := cfg.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.Receipt(t.Context(), receipt.ActorID, receipt.Operation, receipt.CommandKey)
		if err != nil {
			return err
		}
		if !stored.ResponseExpired || stored.RequestHash != receipt.RequestHash || stored.PayloadSHA256 != receipt.PayloadSHA256 {
			return fmt.Errorf("expiry lost tombstone identity")
		}
		row, err := r.WorkItem(t.Context(), newer.ProjectID, newer.WorkItemID)
		if err != nil {
			return err
		}
		if row.Revision != int64(newer.Revision) || stringValue(row.WorkflowStateName) != "Todo" {
			return fmt.Errorf("expiry overwrote newer workflow intent")
		}
		after, err = r.ArchiveBytes(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after > before {
		t.Fatalf("expiry increased archive charge: %d -> %d", before, after)
	}
	if err := cfg.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		changed, err := w.(receiptstore.RetentionWriter).ExpireReceipt(t.Context(), receipt, start.Add(receiptstore.ReplayRetention+time.Nanosecond))
		if changed {
			return fmt.Errorf("owner sweep expired tombstone twice")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, token, request), http.StatusGone)
	if err := cfg.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		charged, err := r.ArchiveBytes(t.Context())
		if err == nil && charged != after {
			return fmt.Errorf("replay or sweep changed archive charge: %d -> %d", after, charged)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func configuredHumanWorkflowPolicy(descriptor policy.Descriptor, states []tracker.NativeState) policy.Descriptor {
	descriptor.Workflow = &policy.Workflow{Source: "detent.yaml", Revision: descriptor.SourceRevision, States: states}
	return descriptor.WithID()
}
