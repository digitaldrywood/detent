package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresConversationDispatchReachesFencedRunner(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
	service := openPostgresHubTestService(t, repository, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, Clock: clock.Now,
		Conversation: &ConversationConfig{Enabled: true, ArchiveContents: &conversationContentFixture{}}, SecretKeys: secretTestKeys(t, "1", "1"),
	})
	service.config.NativeRunEvents = tenantNativeRunEventParticipant{service: service}
	organization, err := service.createNativeOrganizationFor(t.Context(), "Example organization")
	if err != nil {
		t.Fatal(err)
	}
	seedPostgresHubAdmin(t, repository, service, organization)
	projectPath := "/api/v2/organizations/" + string(organization.ID) + "/projects"
	projectResponse := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{
		"idempotency_key": "runtime-project", "name": "Example project", "grant_access": true, "states": nativeFixtureStates(),
	})
	requireNativeStatus(t, projectResponse, http.StatusCreated)
	var project tracker.NativeProject
	decodeHubResponse(t, projectResponse, &project)
	descriptor := hubTestPolicy()
	policyRevision, err := eventstore.NewContent(service.config.RunDetails).PrepareProjectPolicy(t.Context(), organization.ID, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	policyRevision.ProjectID = project.ID
	policyRevision.ApprovedBy = "test_hub_admin"
	policyRevision.ApprovedAt = clock.Now().UTC()
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.ApproveProjectPolicy(t.Context(), policyRevision, "")
	}); err != nil {
		t.Fatal(err)
	}
	itemPath := projectPath + "/" + string(project.ID) + "/work-items"
	issueResponse := performHubAPIRequest(t, service, http.MethodPost, itemPath, testHubAdminToken, tracker.CreateIssue{
		Mutation: tracker.Mutation{IdempotencyKey: "runtime-item"}, Title: "Example issue", Body: workItemRecordBody, State: "Todo",
	})
	requireNativeStatus(t, issueResponse, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, issueResponse, &issue)
	otherResponse := performHubAPIRequest(t, service, http.MethodPost, itemPath, testHubAdminToken, tracker.CreateIssue{
		Mutation: tracker.Mutation{IdempotencyKey: "runtime-other-item"}, Title: "Other issue", Body: workItemRecordBody, State: "Todo",
	})
	requireNativeStatus(t, otherResponse, http.StatusOK)
	var other tracker.NativeIssue
	decodeHubResponse(t, otherResponse, &other)

	startedAt := clock.Now().UTC()
	const runnerID = "runner_runtime_example"
	const machineID = "machine_runtime_example"
	const credentialID = "credential_runtime_example"
	const enrollmentID = "enrollment_runtime_example"
	const token = "example_runtime_runner_token"
	tokenHash := apikey.HashToken(token)
	expires := startedAt.Add(time.Hour)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: credentialID, Name: "Example runner", Hash: tokenHash, Fingerprint: tokenFingerprint(tokenHash), Scope: "worker", NativeOnly: true, CreatedAt: startedAt, UpdatedAt: startedAt, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization.ID, TokenID: credentialID, ProjectID: project.ID}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization.ID, ID: enrollmentID, RunnerID: runnerID, MachineID: machineID, TokenHash: apikey.HashToken("example_runtime_enrollment"), Scope: "projects", ProjectIDs: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, CreatedAt: startedAt, ExpiresAt: expires, CreatedBy: "example-admin"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollmentID, runnerID, machineID, startedAt); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization.ID, ID: machineID, Hostname: "example-host", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization.ID, RunnerID: runnerID, MachineID: machineID, TokenID: credentialID, EnrollmentID: enrollmentID, Scope: "projects", State: "active", Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, DisplayName: "Example runner", CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{project.ID}, Availability: runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}}, CreatedAt: startedAt, ExpiresAt: expires, LastHeartbeatAt: startedAt}); err != nil {
			return err
		}
		return w.UpdateRunnerCapacityConfig(t.Context(), runnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: startedAt})
	}); err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: organization.ID, project: project.ID, credential: apiCredential{ID: credentialID, Hash: tokenHash, Scope: apiScopeWorker, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: runnerID, MachineID: machineID}, OrganizationID: organization.ID}}}
	now := clock.Now()
	conversationID := conversation.NewConversationID()
	if service.config.ConversationDispatch == nil {
		t.Fatal("default Hub dispatch owner is missing")
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateConversation(t.Context(), conversationstore.Conversation{
			OrganizationID: organization.ID, ProjectID: project.ID, ID: conversationID, CreationKey: "dispatch-conversation", CreationHash: "example",
			OwnerPrincipalID: "test_hub_admin", Title: "Example continuation", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive,
			WorkItemID: string(issue.WorkItemID), LinkedAt: &now, CreatedAt: now, UpdatedAt: now,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	commandPath := projectPath + "/" + string(project.ID) + "/conversations/" + conversationID + "/commands"
	command := conversation.Command{Key: "continue-dispatch", Kind: conversation.CommandContinue, Text: "Continue the example"}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, commandPath, testHubAdminToken, command), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, commandPath, testHubAdminToken, command), http.StatusOK)
	request := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: machineID, SessionID: "dispatch-session", TTLSeconds: 300, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
	lease, err := service.claimNativeIssueTenant(t.Context(), scope, request, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	publicLease := tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), WorkItemID: issue.WorkItemID, MachineID: tracker.MachineID(lease.MachineID), SessionID: lease.SessionID, FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: descriptor.ID}
	started := nativeStartedEvent(publicLease)
	eventPath := itemPath + "/" + string(issue.WorkItemID) + "/events"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, eventPath, token, started), http.StatusOK)
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, err := r.Attempt(t.Context(), started.Data.AttemptID)
		if err != nil {
			return err
		}
		if attempt.DispatchGeneration != 1 || attempt.WorkItemRevision != lease.WorkItemRevision {
			t.Fatalf("runner capture = %+v", attempt)
		}
		state, err := r.(tenantstore.DispatchReader).IssueDispatchState(t.Context(), project.ID, issue.WorkItemID)
		if err == nil && state.Generation != 1 {
			t.Fatalf("replay advanced dispatch = %+v", state)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	finished := started
	finished.IdempotencyKey = "dispatch-finished"
	finished.Type = "run.finished"
	finished.Data.Sequence = 2
	finished.Data.Outcome = "succeeded"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, eventPath, token, finished), http.StatusOK)
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "completed"); err != nil {
			return err
		}
		current, err := w.LockConversation(t.Context(), project.ID, conversationID)
		if err != nil {
			return err
		}
		current.Execution = conversation.Execution{Status: conversation.ExecutionCompleted, Settled: true, Owner: conversation.Owner{AttemptID: started.Data.AttemptID, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, UpdatedAt: now}
		_, err = w.UpdateConversation(t.Context(), current, current.Revision)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return service.config.ConversationDispatch.RequestDispatch(t.Context(), w, project.ID, string(issue.WorkItemID), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken + 1}, clock.Now())
	}); !errors.Is(err, tracker.ErrStaleFencingToken) {
		t.Fatalf("participant replaced a nonempty stale fence: %v", err)
	}
	request.SessionID = "dispatch-without-request"
	if _, err := service.claimNativeIssueTenant(t.Context(), scope, request, 5*time.Minute); !errors.Is(err, ErrNoClaimableWork) {
		t.Fatalf("consumed generation claim = %v", err)
	}
	command.Key = "continue-finished"
	command.Expected.AttemptID = started.Data.AttemptID
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, commandPath, testHubAdminToken, command), http.StatusOK)
	var retryID string
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		messages, err := w.Messages(t.Context(), conversationID, 0, 20)
		if err != nil {
			return err
		}
		message := messages.Items[0]
		retryID = message.ID
		message.Delivery = conversation.DeliveryFailed
		_, err = w.UpdateMessage(t.Context(), message, message.Revision)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	retry := conversation.Command{Key: "retry-dispatch", Kind: conversation.CommandRetry, MessageID: retryID}
	resolved := 0
	service.config.ConversationDispatch = conversationDispatchLockProbe{delegate: service.config.ConversationDispatch, probe: func() error {
		resolved++
		probe, err := admin.BeginTx(t.Context(), nil)
		if err != nil {
			return err
		}
		var id int64
		lockErr := probe.QueryRowContext(t.Context(), `SELECT id FROM issues WHERE organization_id=$1 AND native_id=$2 FOR UPDATE NOWAIT`, organization.ID, issue.WorkItemID).Scan(&id)
		rollbackErr := probe.Rollback()
		if rollbackErr != nil {
			return rollbackErr
		}
		var refused *pgconn.PgError
		if !errors.As(lockErr, &refused) || refused.Code != "55P03" {
			return fmt.Errorf("linked issue was not locked during fence resolution: %w", lockErr)
		}
		return nil
	}}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, commandPath, testHubAdminToken, retry), http.StatusOK)
	if resolved != 1 {
		t.Fatalf("current retry fence resolutions=%d want 1", resolved)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, commandPath, testHubAdminToken, retry), http.StatusOK)
	request.SessionID = "dispatch-retry-session"
	retryLease, err := service.claimNativeIssueTenant(t.Context(), scope, request, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	retryStarted := nativeStartedEvent(tracker.NativeLease{ID: tracker.LeaseID(retryLease.LeaseID), WorkItemID: issue.WorkItemID, MachineID: tracker.MachineID(retryLease.MachineID), SessionID: retryLease.SessionID, FencingToken: tracker.FencingToken(retryLease.FencingToken), PolicyID: descriptor.ID})
	retryStarted.IdempotencyKey = "retry-start"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, eventPath, token, retryStarted), http.StatusOK)
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, err := r.Attempt(t.Context(), retryStarted.Data.AttemptID)
		if err == nil && attempt.DispatchGeneration != 3 {
			t.Fatalf("retry runner generation = %d", attempt.DispatchGeneration)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessCredential(t.Context(), "test_hub_admin", clock.Now())
	}); err != nil {
		t.Fatal(err)
	}
	command.Key = "continue-revoked"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, commandPath, testHubAdminToken, command), http.StatusUnauthorized)
	if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		state, err := r.(tenantstore.DispatchReader).IssueDispatchState(t.Context(), project.ID, issue.WorkItemID)
		if err == nil && state.Generation != 3 {
			t.Fatalf("revoked command changed dispatch = %+v", state)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

type conversationDispatchLockProbe struct {
	delegate tenantstore.DispatchRequests
	probe    func() error
}

func (p conversationDispatchLockProbe) RequestDispatch(ctx context.Context, writer tenantstore.Writer, project tracker.ProjectID, item string, fence leasestore.Fence, at time.Time) error {
	return p.delegate.RequestDispatch(ctx, conversationDispatchProbeWriter{Writer: writer, DispatchWriter: writer.(tenantstore.DispatchWriter), probe: p.probe}, project, item, fence, at)
}

type conversationDispatchProbeWriter struct {
	tenantstore.Writer
	tenantstore.DispatchWriter
	probe func() error
}

func (w conversationDispatchProbeWriter) LatestAttempt(ctx context.Context, id int64) (leasestore.Attempt, error) {
	if err := w.probe(); err != nil {
		return leasestore.Attempt{}, err
	}
	return w.Writer.LatestAttempt(ctx, id)
}
