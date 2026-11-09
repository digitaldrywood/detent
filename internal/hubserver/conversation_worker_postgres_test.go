package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type conversationWorkerAuthorityFunc func(context.Context, tenantstore.Reader, ConversationWorkerIdentity) error

func (f conversationWorkerAuthorityFunc) VerifyConversationWorker(ctx context.Context, reader tenantstore.Reader, identity ConversationWorkerIdentity) error {
	return f(ctx, reader, identity)
}

func TestConversationPostgresWorkerHandlersUseFencedTenantState(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent_event=%t", concurrent), func(t *testing.T) {
			for _, origin := range []string{conversationOriginWorker, conversationOriginUser} {
				t.Run("origin="+origin, func(t *testing.T) { testConversationPostgresWorkerHandlers(t, concurrent, origin) })
			}
		})
	}
}

func testConversationPostgresWorkerHandlers(t *testing.T, concurrent bool, origin string) {
	repository, admin := projectRecordPostgresWithAdmin(t, 4)
	f := newConversationTenantFixture(t, repository, time.Now().UTC())
	var logs bytes.Buffer
	f.service.config.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	ownerCredential := conversationLifecyclePostgresOwner(t, f, time.Now().UTC())
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: f.project.OrganizationID, Name: "Example organization", CreatedAt: time.Now().UTC()}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), ownerCredential); err != nil {
			return err
		}
		_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{
			Core:     corestore.Project{OrganizationID: f.project.OrganizationID, ID: f.project.ID, Name: f.project.Name, Slug: f.project.Slug, Profile: f.project.Profile, RequireDependencies: f.project.RequireDependencies, CreatedAt: time.Now().UTC()},
			Settings: projectstore.Settings{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID},
			States:   f.project.States,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.service.config.TenantRepository = repository
	workerID := "worker_conversation"
	workerCredential := accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: workerID, Name: "Example worker", Hash: "example-worker-hash", Scope: "worker", NativeOnly: true, ProjectAccess: "all", Kind: "service", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	binding := runnerauth.NewBinding()
	workerIdentity := runnerauth.Identity{Binding: binding, Scope: "projects", OrganizationID: f.project.OrganizationID, ProjectIDs: []tracker.ProjectID{f.project.ID}, Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim}, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: workerID, Hash: workerCredential.Hash, Scope: apiScopeWorker, NativeOnly: true, Runner: workerIdentity}}
	authorityCalls := 0
	deniedAuthorityCall := 0
	f.service.config.ConversationWorkerAuthority = conversationWorkerAuthorityFunc(func(_ context.Context, _ tenantstore.Reader, identity ConversationWorkerIdentity) error {
		if identity.Credential.ID != workerID || identity.Runner.Binding != binding || identity.Organization != f.project.OrganizationID || identity.Project != f.project.ID {
			t.Fatalf("worker authority identity = %+v, want the seeded binding and tenant", identity)
		}
		authorityCalls++
		if deniedAuthorityCall != 0 && authorityCalls == deniedAuthorityCall {
			return accessstore.ErrDenied
		}
		return nil
	})
	var issue corestore.WorkItem
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), workerCredential); err != nil {
			return err
		}
		createdIssue, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, NativeID: "wi_pg_worker", Number: 1, Title: "Example worker item", ActorKind: new("human"), ActorPrincipalID: new(f.ownerID), WorkflowStateName: new("Todo"), CreatedAt: time.Now().UTC()})
		if err != nil {
			return err
		}
		issue = createdIssue
		item, err := w.WorkItem(t.Context(), f.project.ID, issue.NativeID)
		if err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: f.project.OrganizationID, ID: string(binding.MachineID), Hostname: "example-worker", DisplayName: "Example worker", Capacity: 1}); err != nil {
			return err
		}
		now := time.Now().UTC()
		enrollment := runnerstore.Enrollment{OrganizationID: f.project.OrganizationID, ID: "enrollment_worker", RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenHash: apikey.HashToken("example-enrollment"), Scope: "projects", ProjectIDs: workerIdentity.ProjectIDs, Operations: workerIdentity.Operations, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: ownerCredential.ID}
		if err := w.CreateRunnerEnrollment(t.Context(), enrollment); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: f.project.OrganizationID, RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenID: workerID, EnrollmentID: enrollment.ID, Scope: "projects", State: "active", Operations: workerIdentity.Operations, ProjectIDs: workerIdentity.ProjectIDs, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now}); err != nil {
			return err
		}
		lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(binding.MachineID), SessionID: "session-pg-worker", LeaseID: "lease-pg-worker", TTL: time.Hour})
		if err != nil {
			return err
		}
		f.service.config.now = func() time.Time { return lease.RenewedAt }
		_, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "attempt-pg-worker", Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: "run-pg-worker", Sequence: 1, Status: "running", DispatchGeneration: 1, PayloadObjectKey: "example/payload"})
		if err != nil {
			return err
		}
		createdAt := time.Now().UTC()
		_, err = w.CreateConversation(t.Context(), conversationstore.Conversation{
			OrganizationID: f.project.OrganizationID, ID: conversation.NewConversationID(), ProjectID: f.project.ID,
			OwnerPrincipalID: ownerCredential.ID, Origin: origin, Title: "Worker issue",
			Visibility: conversation.VisibilityShared, Status: conversation.StatusActive, WorkItemID: string(issue.NativeID),
			LinkedAt: &createdAt, Revision: 1, Execution: conversation.Execution{Status: conversation.ExecutionWaitingForRunner, UpdatedAt: createdAt},
			Preferences: conversation.DefaultPreferences(), CreatedAt: createdAt, UpdatedAt: createdAt,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var linked conversationstore.Conversation
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		linked, err = r.ConversationByWorkItem(t.Context(), f.project.ID, string(issue.NativeID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		message, err := w.CreateMessage(t.Context(), conversationMessageToStore(f.project.OrganizationID, conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: linked.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: "Example prior response", Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: workerID}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}))
		if err != nil {
			return err
		}
		return appendPGConversationEvent(t.Context(), w, f.project.OrganizationID, linked.ID, conversation.EventMessageAccepted, projectMessage(conversationMessageFromStore(message)), time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.commandPostConversation(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: ownerCredential.ID, Hash: ownerCredential.Hash, Scope: apiScopeOperator}}, linked.ID, conversation.Command{Key: "pg-bind-control", Kind: conversation.CommandMessage, Text: "Example pending instruction"}); err != nil {
		t.Fatal(err)
	}
	identity := conversationWorkerIdentity{LeaseID: "lease-pg-worker", FencingToken: 1, AttemptID: "attempt-pg-worker"}
	router := f.service.conversations.controls.(*conversationWorkerRouter)
	var currentLease leasestore.Lease
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		currentLease, err = r.Lease(t.Context(), string(identity.LeaseID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	originalClock := f.service.config.now
	initialAuthority := f.service.config.ConversationWorkerAuthority
	t.Cleanup(func() {
		f.service.config.now = originalClock
		f.service.config.ConversationWorkerAuthority = initialAuthority
		deadline, ok := t.Deadline()
		if !ok {
			t.Error("worker fixture cleanup requires a test deadline")
			return
		}
		cleanupCtx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if _, err := admin.ExecContext(cleanupCtx, `UPDATE leases SET renewed_at=$3,expires_at=$4 WHERE organization_id=$1 AND lease_id=$2`, scope.organization, identity.LeaseID, currentLease.RenewedAt, currentLease.ExpiresAt); err != nil {
			t.Error(err)
		}
	})
	futureClock := time.Now().UTC()
	f.service.config.now = func() time.Time { return futureClock }
	if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET renewed_at=$3,expires_at=$4 WHERE organization_id=$1 AND lease_id=$2`, scope.organization, identity.LeaseID, futureClock.Add(time.Second), futureClock.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	futureRenewal := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		_, _, err := f.service.authorizeConversationWorkerLeasePG(t.Context(), r, scope, string(issue.NativeID), identity, "run-pg-worker", false, false)
		return err
	})
	if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET renewed_at=$3,expires_at=$4 WHERE organization_id=$1 AND lease_id=$2`, scope.organization, identity.LeaseID, currentLease.RenewedAt, currentLease.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	f.service.config.now = originalClock
	if futureRenewal == nil {
		t.Fatal("future-dated lease renewal was accepted")
	}
	var futureRefusal *nativeError
	if !errors.As(futureRenewal, &futureRefusal) || futureRefusal.Code != "stale_execution" {
		t.Fatalf("future renewal did not reach lease validation: %v", futureRenewal)
	}

	originalAuthority := f.service.config.ConversationWorkerAuthority
	var beforeVerification time.Time
	var verificationRenewal leasestore.Lease
	f.service.config.now = func() time.Time {
		now := time.Now().UTC()
		if beforeVerification.IsZero() {
			beforeVerification = now
		}
		return now
	}
	f.service.config.ConversationWorkerAuthority = conversationWorkerAuthorityFunc(func(ctx context.Context, reader tenantstore.Reader, worker ConversationWorkerIdentity) error {
		if verificationRenewal.LeaseID == "" {
			if err := repository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				var err error
				verificationRenewal, err = w.Renew(ctx, leasestore.Fence{LeaseID: string(identity.LeaseID), FencingToken: int64(identity.FencingToken)}, time.Hour)
				return err
			}); err != nil {
				return err
			}
			if !verificationRenewal.RenewedAt.After(beforeVerification) {
				return errors.New("worker renewal did not follow the authority clock sample")
			}
		}
		return originalAuthority.VerifyConversationWorker(ctx, reader, worker)
	})
	renewalAuthority := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		_, _, err := f.service.authorizeConversationWorkerLeasePG(t.Context(), reader, scope, string(issue.NativeID), identity, "run-pg-worker", false, false)
		return err
	})
	f.service.config.ConversationWorkerAuthority = originalAuthority
	f.service.config.now = func() time.Time { return verificationRenewal.RenewedAt }
	if renewalAuthority != nil || verificationRenewal.LeaseID == "" {
		t.Fatalf("valid renewal during verification lost worker authority: renewed=%t error=%v", verificationRenewal.LeaseID != "", renewalAuthority)
	}

	bindBody := map[string]any{"lease_id": identity.LeaseID, "fencing_token": identity.FencingToken, "attempt_id": identity.AttemptID, "run_id": "run-pg-worker", "capabilities": map[string]bool{"steer": true, "interrupt": true, "answer": true, "continue": true}}
	bind := func() *httptest.ResponseRecorder {
		return callConversationWorkerRoute(t, router.bind, scope, http.MethodPost, "/bind", string(issue.NativeID), "", bindBody, identity)
	}
	var bound *httptest.ResponseRecorder
	if concurrent {
		order := &conversationBindOrderRepository{Repository: repository, first: make(chan string, 1), projectRelease: make(chan struct{})}
		f.service.config.TenantRepository = order
		bound = bindWithConcurrentRunnerEvent(t, admin, order, f.project.OrganizationID, f.project.ID, binding.RunnerID, bind)
		f.service.config.TenantRepository = repository
	} else {
		bound = bind()
	}
	if bound.Code != http.StatusOK {
		t.Fatalf("bind status = %d, body = %s, log = %s", bound.Code, bound.Body.String(), logs.String())
	}
	var bindResponse conversationBindResponse
	if err := json.Unmarshal(bound.Body.Bytes(), &bindResponse); err != nil {
		t.Fatal(err)
	}
	if bindResponse.Continuation != (origin == conversationOriginUser) || bindResponse.ConversationID != linked.ID || len(bindResponse.Resume.Transcript) != 1 || bindResponse.Resume.Transcript[0].Text != "Example prior response" || len(bindResponse.Pending) != 1 || bindResponse.Pending[0].Key != "pg-bind-control" || bindResponse.Pending[0].Text != "Example pending instruction" {
		t.Fatalf("bind response = %+v", bindResponse)
	}
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		messages, err := r.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: linked.ID, Role: conversation.RoleSystem, Limit: 10})
		if err != nil {
			return err
		}
		for _, message := range messages.Items {
			if message.Kind == conversation.MessageStatus && message.Text == conversationTranscriptNotice(1) {
				return nil
			}
		}
		return errors.New("transcript resume notice was not recorded")
	}); err != nil {
		t.Fatal(err)
	}

	var reconcileCandidate conversationstore.Conversation
	if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		reconcileCandidate, err = r.Conversation(t.Context(), scope.project, linked.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var sampled, renewedAt time.Time
	f.service.config.now = func() time.Time {
		sampled = time.Now().UTC()
		return sampled
	}
	f.service.config.TenantRepository = &conversationControlSnapshotRepository{Repository: repository, beforeWrite: func() error {
		return repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			renewed, err := w.Renew(t.Context(), leasestore.Fence{LeaseID: string(identity.LeaseID), FencingToken: int64(identity.FencingToken)}, time.Hour)
			if err != nil {
				return err
			}
			renewedAt = renewed.RenewedAt
			if !sampled.IsZero() && !renewedAt.After(sampled) {
				return errors.New("renewal did not follow the earlier reconciliation clock sample")
			}
			return nil
		})
	}}
	changed, reconcileErr := f.service.conversations.reconcileExecutionPG(t.Context(), scope.organization, reconcileCandidate)
	f.service.config.TenantRepository = repository
	f.service.config.now = func() time.Time { return renewedAt }
	if reconcileErr != nil || changed || renewedAt.IsZero() {
		t.Fatalf("reconciliation interrupted a worker renewed during write admission: changed=%t renewed=%t error=%v", changed, !renewedAt.IsZero(), reconcileErr)
	}
	for _, held := range []string{"work_item", "lease"} {
		t.Run("reconciliation_waits_before_conversation/"+held, func(t *testing.T) {
			reconcileWithWorkerLock(t, admin, f.service, repository, scope, reconcileCandidate, held)
		})
	}
	turnBody := map[string]any{"lease_id": identity.LeaseID, "fencing_token": identity.FencingToken, "attempt_id": identity.AttemptID, "batch_key": "batch-pg-worker", "events": []map[string]any{{"type": "turn_started", "thread_id": "thread-pg-worker", "turn_id": "turn-pg-worker"}, {"type": "delta", "provider_item_id": "item-pg-worker", "text": "example response"}}}
	authorityCalls, deniedAuthorityCall = 0, 2
	rejectedTurn := callConversationWorkerRoute(t, router.turnEvents, scope, http.MethodPost, "/turn-events", "", linked.ID, turnBody, identity)
	if rejectedTurn.Code != http.StatusForbidden {
		t.Fatalf("rejected worker identity status = %d, body = %s", rejectedTurn.Code, rejectedTurn.Body.String())
	}
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		if _, found, err := r.TurnBatch(t.Context(), identity.AttemptID, "batch-pg-worker"); err != nil || found {
			return errors.Join(err, errors.New("rejected worker identity retained a turn batch"))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	authorityCalls, deniedAuthorityCall = 0, 0
	turn := callConversationWorkerRoute(t, router.turnEvents, scope, http.MethodPost, "/turn-events", "", linked.ID, turnBody, identity)
	if turn.Code != http.StatusAccepted {
		t.Fatalf("turn events status = %d, body = %s, log = %s", turn.Code, turn.Body.String(), logs.String())
	}
	var turnResponse conversationTurnEventsResponse
	if err := json.Unmarshal(turn.Body.Bytes(), &turnResponse); err != nil {
		t.Fatal(err)
	}
	if turnResponse.EventSeq == 0 {
		t.Fatalf("turn event sequence = %d", turnResponse.EventSeq)
	}
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		messages, err := r.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: linked.ID, AttemptID: identity.AttemptID, Role: conversation.RoleAssistant, Limit: 10})
		if err != nil {
			return err
		}
		if len(messages.Items) != 1 || messages.Items[0].Text != "example response" {
			return fmt.Errorf("stored assistant delta messages = %+v", messages.Items)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stale := identity
	stale.FencingToken++
	staleTurnBody := map[string]any{"lease_id": stale.LeaseID, "fencing_token": stale.FencingToken, "attempt_id": stale.AttemptID, "batch_key": "stale-batch-pg-worker", "events": turnBody["events"]}
	staleTurn := callConversationWorkerRoute(t, router.turnEvents, scope, http.MethodPost, "/turn-events", "", linked.ID, staleTurnBody, stale)
	if staleTurn.Code != http.StatusConflict {
		t.Fatalf("stale fencing token status = %d, body = %s", staleTurn.Code, staleTurn.Body.String())
	}
	replayed := callConversationWorkerRoute(t, router.turnEvents, scope, http.MethodPost, "/turn-events", "", linked.ID, turnBody, identity)
	if replayed.Code != http.StatusAccepted {
		t.Fatalf("turn replay status = %d, body = %s", replayed.Code, replayed.Body.String())
	}
	var replayResponse conversationTurnEventsResponse
	if err := json.Unmarshal(replayed.Body.Bytes(), &replayResponse); err != nil {
		t.Fatal(err)
	}
	if replayResponse.EventSeq != turnResponse.EventSeq {
		t.Fatalf("turn replay sequence = %d, want %d", replayResponse.EventSeq, turnResponse.EventSeq)
	}
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		page, err := r.ConversationEvents(t.Context(), linked.ID, 0, 100)
		if err != nil {
			return err
		}
		if len(page.Items) != int(turnResponse.EventSeq) {
			return fmt.Errorf("event rows = %d, want %d after replay and stale-fence refusal", len(page.Items), turnResponse.EventSeq)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.service.commandPostConversation(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: ownerCredential.ID, Hash: ownerCredential.Hash, Scope: apiScopeOperator}}, linked.ID, conversation.Command{Key: "pg-control", Kind: conversation.CommandMessage, Text: "Example instruction"}); err != nil {
		t.Fatal(err)
	}
	controls := callConversationWorkerRoute(t, router.controls, scope, http.MethodGet, "/controls?after=0&wait=0", "", linked.ID, nil, identity)
	if controls.Code != http.StatusOK {
		t.Fatalf("controls status = %d, body = %s", controls.Code, controls.Body.String())
	}
	var controlPage conversationControlsResponse
	if err := json.Unmarshal(controls.Body.Bytes(), &controlPage); err != nil {
		t.Fatal(err)
	}
	if len(controlPage.Controls) != 1 || controlPage.Controls[0].Key != "pg-control" || controlPage.Controls[0].Text != "Example instruction" {
		t.Fatalf("controls page = %+v", controlPage)
	}
	t.Run("concurrent revision retries with the same worker", func(t *testing.T) {
		f.service.config.TenantRepository = &conversationControlSnapshotRepository{Repository: repository, beforeWrite: func() error {
			return repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				stored, err := w.Conversation(t.Context(), scope.project, linked.ID)
				if err != nil {
					return err
				}
				stored.Title = "Example updated conversation"
				_, err = w.UpdateConversation(t.Context(), stored, stored.Revision)
				return err
			})
		}}
		t.Cleanup(func() { f.service.config.TenantRepository = repository })
		response := callConversationWorkerRoute(t, router.controls, scope, http.MethodGet, fmt.Sprintf("/controls?after=%d&wait=0", controlPage.Cursor), "", linked.ID, nil, identity)
		f.service.config.TenantRepository = repository
		requireConversationErrorCode(t, response, http.StatusConflict, "revision_conflict")
		if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			message, err := r.Message(t.Context(), linked.ID, controlPage.Controls[0].MessageID)
			if err != nil {
				return err
			}
			if message.Delivery != conversation.DeliverySending {
				return fmt.Errorf("revision conflict acknowledged control delivery: %s", message.Delivery)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		page, err := router.pollControlsPostgres(t.Context(), scope, linked.ID, identity, 0)
		if err != nil || len(page.Controls) != 1 || page.Controls[0].Key != "pg-control" || page.Controls[0].Expected.AttemptID != identity.AttemptID {
			t.Fatalf("same worker could not resume its control poll: %+v, %v", page, err)
		}
	})
	controlCursor := controlPage.Cursor
	for _, test := range []struct {
		name    string
		late    bool
		changed bool
	}{
		{name: "preloaded_inline_control"},
		{name: "control_arrives_after_preload", late: true},
		{name: "control_changes_after_preload", changed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var message conversationstore.Message
			seed := func() error {
				return repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					var err error
					message, err = w.CreateMessage(t.Context(), conversationstore.Message{
						OrganizationID: f.project.OrganizationID, ConversationID: linked.ID, ID: conversation.NewMessageID(),
						Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "Example inline control", Delivery: conversation.DeliveryQueued,
						Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: ownerCredential.ID}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
					})
					return err
				})
			}
			wantText := "Example inline control"
			if test.late {
				f.service.config.TenantRepository = &conversationControlSnapshotRepository{Repository: repository, beforeWrite: seed}
			} else if err := seed(); err != nil {
				t.Fatal(err)
			}
			if test.changed {
				wantText = "Example updated inline control"
				f.service.config.TenantRepository = &conversationControlSnapshotRepository{Repository: repository, beforeWrite: func() error {
					return repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
						current, err := w.Message(t.Context(), linked.ID, message.ID)
						if err != nil {
							return err
						}
						previous := current.Revision
						current.Revision++
						current.Text, current.UpdatedAt = wantText, time.Now().UTC()
						message, err = w.UpdateMessage(t.Context(), current, previous)
						return err
					})
				}}
			}
			t.Cleanup(func() { f.service.config.TenantRepository = repository })
			page, err := router.pollControlsPostgres(t.Context(), scope, linked.ID, identity, controlCursor)
			f.service.config.TenantRepository = repository
			if test.late || test.changed {
				if err != nil || len(page.Controls) != 0 || page.Cursor != controlCursor {
					t.Fatalf("unloaded control advanced delivery: %+v, %v", page, err)
				}
				page, err = router.pollControlsPostgres(t.Context(), scope, linked.ID, identity, controlCursor)
			}
			if err != nil || len(page.Controls) != 1 || page.Controls[0].MessageID != message.ID || page.Controls[0].Text != wantText || page.Controls[0].Expected.AttemptID != identity.AttemptID {
				t.Fatalf("control lost its loaded body or owner: %+v, %v", page, err)
			}
			controlCursor = page.Cursor
		})
	}
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.Conversation(t.Context(), f.project.ID, linked.ID)
		if err != nil {
			return err
		}
		events, err := r.ConversationEvents(t.Context(), linked.ID, 0, 100)
		if err != nil {
			return err
		}
		if stored.EventSequence != int64(len(events.Items)) {
			return fmt.Errorf("event head %d differs from %d rows before unbind", stored.EventSequence, len(events.Items))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var acknowledgementCursor int64
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		for i := range 128 {
			message, err := w.CreateMessage(t.Context(), conversationstore.Message{
				OrganizationID: f.project.OrganizationID, ConversationID: linked.ID, ID: conversation.NewMessageID(),
				Role: conversation.RoleUser, Kind: conversation.MessageText, Text: fmt.Sprintf("Example recovered control %d", i),
				Delivery: conversation.DeliverySending, AttemptID: identity.AttemptID, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: ownerCredential.ID},
				CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
			})
			if err != nil {
				return err
			}
			acknowledgementCursor = message.Sequence
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, remaining := range []int{1, 0} {
		var acknowledged conversationControlsResponse
		var err error
		if remaining == 1 {
			acknowledged = pollControlsAgainstRunnerEvent(t, admin, f.service, router, scope, linked.ID, identity, acknowledgementCursor)
		} else {
			acknowledged, err = router.pollControlsPostgres(t.Context(), scope, linked.ID, identity, acknowledgementCursor)
		}
		if err != nil || len(acknowledged.Controls) != 0 {
			t.Fatalf("bounded acknowledgement poll controls=%d error=%v", len(acknowledged.Controls), err)
		}
		if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			messages, err := r.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: linked.ID, AttemptID: identity.AttemptID, Role: conversation.RoleUser, Deliveries: []conversation.Delivery{conversation.DeliverySending}, Limit: 200})
			if err != nil {
				return err
			}
			if len(messages.Items) != remaining {
				return fmt.Errorf("sending acknowledgements after bounded poll = %d, want %d", len(messages.Items), remaining)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var outstandingQuestionID string
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		createdAt := time.Now().UTC().Add(-time.Hour)
		for range conversationPGWorkerPageSize + 1 {
			question := conversationstore.Question{OrganizationID: f.project.OrganizationID, ConversationID: linked.ID, Question: conversation.Question{ID: conversation.NewQuestionID(), Status: conversation.QuestionAnswered, Owner: conversation.Owner{AttemptID: identity.AttemptID}, CreatedAt: createdAt, UpdatedAt: createdAt}}
			if err := w.UpsertQuestion(t.Context(), question); err != nil {
				return err
			}
		}
		outstandingQuestionID = conversation.NewQuestionID()
		return w.UpsertQuestion(t.Context(), conversationstore.Question{OrganizationID: f.project.OrganizationID, ConversationID: linked.ID, Question: conversation.Question{ID: outstandingQuestionID, Status: conversation.QuestionPending, Owner: conversation.Owner{AttemptID: identity.AttemptID}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}})
	}); err != nil {
		t.Fatal(err)
	}

	var nextLease leasestore.Lease
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		released, err := w.Release(t.Context(), leasestore.Fence{LeaseID: string(identity.LeaseID), FencingToken: int64(identity.FencingToken)}, "released")
		if err != nil {
			return fmt.Errorf("release previous lease: %w", err)
		}
		return f.service.conversations.leaseReleasedTenant(t.Context(), w, released.LeaseID, released.FencingToken, "released")
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		question, err := r.Question(t.Context(), linked.ID, outstandingQuestionID)
		if err != nil {
			return err
		}
		if question.Status != conversation.QuestionExpired {
			return fmt.Errorf("pending question behind completed history status = %q, want expired", question.Status)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	staleRebind := callConversationWorkerRoute(t, router.bind, scope, http.MethodPost, "/bind", string(issue.NativeID), "", bindBody, identity)
	if staleRebind.Code != http.StatusConflict {
		t.Fatalf("released lease bind status = %d, body = %s", staleRebind.Code, staleRebind.Body.String())
	}
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		currentItem, err := w.WorkItem(t.Context(), f.project.ID, issue.NativeID)
		if err != nil {
			return fmt.Errorf("read current work item: %w", err)
		}
		nextLease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: currentItem.ID, ExpectedRevision: currentItem.Revision, MachineID: string(binding.MachineID), SessionID: "session-pg-worker-next", LeaseID: "lease-pg-worker-next", TTL: time.Hour})
		if err != nil {
			return fmt.Errorf("claim replacement lease: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.service.config.now = func() time.Time { return nextLease.RenewedAt }
	nextIdentity := conversationWorkerIdentity{LeaseID: tracker.LeaseID(nextLease.LeaseID), FencingToken: tracker.FencingToken(nextLease.FencingToken), AttemptID: newNativeID("attempt")}
	nextRunID := newNativeID("run")
	nextBindBody := map[string]any{"lease_id": nextIdentity.LeaseID, "fencing_token": nextIdentity.FencingToken, "attempt_id": nextIdentity.AttemptID, "run_id": nextRunID, "capabilities": map[string]bool{"steer": true, "interrupt": true, "answer": true, "continue": true}}
	takeover := callConversationWorkerRoute(t, router.bind, scope, http.MethodPost, "/bind", string(issue.NativeID), "", nextBindBody, nextIdentity)
	if takeover.Code != http.StatusOK {
		t.Fatalf("replacement bind status = %d, body = %s, log = %s", takeover.Code, takeover.Body.String(), logs.String())
	}
	var unstarted conversationstore.Conversation
	if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		unstarted, err = r.Conversation(t.Context(), scope.project, linked.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		now    time.Time
		status conversation.ExecutionStatus
		fence  int64
		want   bool
	}{
		{name: "current unstarted", now: nextLease.RenewedAt, status: conversation.ExecutionStarting, fence: nextLease.FencingToken, want: true},
		{name: "future renewal", now: nextLease.RenewedAt.Add(-time.Second), status: conversation.ExecutionStarting, fence: nextLease.FencingToken},
		{name: "expired unstarted", now: nextLease.ExpiresAt, status: conversation.ExecutionStarting, fence: nextLease.FencingToken},
		{name: "stale fence", now: nextLease.RenewedAt, status: conversation.ExecutionStarting, fence: nextLease.FencingToken + 1},
		{name: "missing running attempt", now: nextLease.RenewedAt, status: conversation.ExecutionRunning, fence: nextLease.FencingToken},
	} {
		if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			record := conversationRecordFromStore(unstarted)
			record.Execution.Status, record.Execution.Owner.FencingToken = test.status, test.fence
			current, err := conversationLeaseCurrentPG(t.Context(), r, record, test.now)
			if err == nil && current != test.want {
				t.Errorf("%s current=%t want %t", test.name, current, test.want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := f.service.conversations.reconcileExecutionPG(t.Context(), scope.organization, unstarted); err != nil || changed {
		t.Fatalf("current bound execution was settled before its first attempt event: changed=%t error=%v", changed, err)
	}
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: nextIdentity.AttemptID, Fence: leasestore.Fence{LeaseID: nextLease.LeaseID, FencingToken: nextLease.FencingToken}, RunID: nextRunID, Sequence: 1, Status: "running", DispatchGeneration: 2, PayloadObjectKey: "example/payload-next"})
		if err != nil {
			return fmt.Errorf("record replacement attempt: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		for _, test := range []struct {
			attemptID string
			want      bool
		}{
			{attemptID: nextIdentity.AttemptID, want: true},
			{attemptID: newNativeID("attempt")},
		} {
			record := conversationRecordFromStore(unstarted)
			record.Execution.Owner.AttemptID = test.attemptID
			current, err := conversationLeaseCurrentPG(t.Context(), r, record, nextLease.RenewedAt)
			if err != nil {
				return err
			}
			if current != test.want {
				return fmt.Errorf("same-lease persisted attempt ownership current=%t want %t", current, test.want)
			}
		}
		messages, err := r.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: linked.ID, AttemptID: identity.AttemptID, Role: conversation.RoleAssistant, Limit: 10})
		if err != nil {
			return err
		}
		if len(messages.Items) != 1 || messages.Items[0].Delivery != conversation.DeliveryInterrupted {
			return fmt.Errorf("previous attempt messages after takeover = %+v", messages.Items)
		}
		stored, err := r.Conversation(t.Context(), f.project.ID, linked.ID)
		if err != nil {
			return err
		}
		if stored.Execution.Owner.AttemptID != nextIdentity.AttemptID {
			return fmt.Errorf("replacement execution owner = %+v", stored.Execution.Owner)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		for i := range corestore.MaxPageSize + 1 {
			now := time.Now().UTC()
			message := conversationstore.Message{OrganizationID: f.project.OrganizationID, ConversationID: linked.ID, ID: conversation.NewMessageID(), Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: fmt.Sprintf("Example response %d", i), Delivery: conversation.DeliveryResponding, AttemptID: nextIdentity.AttemptID, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: workerID}, CreatedAt: now, UpdatedAt: now}
			if _, err := w.CreateMessage(t.Context(), message); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	unbound := callConversationWorkerRoute(t, router.unbind, scope, http.MethodPost, "/unbind", string(issue.NativeID), "", map[string]any{"lease_id": nextIdentity.LeaseID, "fencing_token": nextIdentity.FencingToken, "attempt_id": nextIdentity.AttemptID, "outcome": "succeeded"}, nextIdentity)
	if unbound.Code != http.StatusOK {
		t.Fatalf("unbind status = %d, body = %s, log = %s", unbound.Code, unbound.Body.String(), logs.String())
	}
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.Conversation(t.Context(), f.project.ID, linked.ID)
		if err != nil {
			return err
		}
		if !stored.Execution.Settled || stored.Execution.Status != conversation.ExecutionCompleted {
			return fmt.Errorf("unbound execution = %+v", stored.Execution)
		}
		messages, err := r.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: linked.ID, AttemptID: nextIdentity.AttemptID, Role: conversation.RoleAssistant, Deliveries: []conversation.Delivery{conversation.DeliveryCompleted}, Limit: corestore.MaxPageSize})
		if err != nil {
			return err
		}
		if messages.Next == 0 || len(messages.Items) != corestore.MaxPageSize {
			return fmt.Errorf("settled message page = %d items, next %d", len(messages.Items), messages.Next)
		}
		remainder, err := r.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: linked.ID, AttemptID: nextIdentity.AttemptID, Role: conversation.RoleAssistant, Deliveries: []conversation.Delivery{conversation.DeliveryCompleted}, BeforeSequence: messages.Items[0].Sequence, Limit: corestore.MaxPageSize})
		if err != nil {
			return err
		}
		if len(remainder.Items) != 1 || remainder.Items[0].Delivery != conversation.DeliveryCompleted {
			return fmt.Errorf("settled message remainder = %+v", remainder.Items)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if origin == conversationOriginWorker {
		ownerScope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: ownerCredential.ID, Hash: ownerCredential.Hash, Scope: apiScopeOperator}}
		testPGConversationContinuation(t, f.service, repository, scope, ownerScope, issue, linked.ID, nextIdentity)
	}

	reporterScope := nativeScope{
		organization: f.project.OrganizationID,
		project:      f.project.ID,
		credential: apiCredential{
			ID: ownerCredential.ID, Hash: ownerCredential.Hash, Scope: apiScopeOperator,
			HostedRole: "reporter",
		},
		sourceActor: &tracker.Actor{Kind: "human", PrincipalID: f.ownerID},
	}
	referenceAt := time.Now().UTC()
	referenceTargetID := tracker.NativeWorkItemID("wi_reference_target")
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, NativeID: referenceTargetID, Number: 2, Title: "Example reference target", ActorKind: new("human"), ActorPrincipalID: new(f.ownerID), WorkflowStateName: new("Todo"), CreatedAt: referenceAt}); err != nil {
			return err
		}
		message, err := w.CreateMessage(t.Context(), conversationstore.Message{OrganizationID: f.project.OrganizationID, ConversationID: linked.ID, ID: conversation.NewMessageID(), Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "Example reference excerpt", Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.ownerID}, CreatedAt: referenceAt, UpdatedAt: referenceAt})
		if err != nil {
			return err
		}
		return w.ReplaceMessageReferences(t.Context(), linked.ID, message.ID, []conversationstore.MessageReference{{OrganizationID: f.project.OrganizationID, ConversationID: linked.ID, MessageID: message.ID, Kind: conversation.ReferenceIssue, TargetID: string(referenceTargetID), Label: "Example"}}, referenceAt)
	}); err != nil {
		t.Fatal(err)
	}
	assertReferenceCount := func(want int) {
		t.Helper()
		raw, err := f.service.readWorkItemReferences(t.Context(), reporterScope, string(referenceTargetID))
		if err != nil {
			t.Fatal(err)
		}
		var page conversationWorkItemReferencePage
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.References) != want {
			t.Fatalf("visible conversation references = %d, want %d", len(page.References), want)
		}
	}
	assertReferenceCount(1)
	if _, err := f.service.readPGConversation(t.Context(), reporterScope, linked.ID); err != nil {
		t.Fatalf("reporter could not read their linked conversation: %v", err)
	}
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		current, err := w.WorkItem(t.Context(), f.project.ID, issue.NativeID)
		if err != nil {
			return err
		}
		current.ActorPrincipalID = new("other-reporter")
		_, err = w.UpdateWorkItem(t.Context(), current)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertReferenceCount(0)
	if _, err := f.service.readWorkItemReferences(t.Context(), reporterScope, string(issue.NativeID)); err == nil {
		t.Fatal("reporter read references to another reporter's item")
	}
	if _, err := f.service.readPGConversation(t.Context(), reporterScope, linked.ID); err == nil {
		t.Fatal("reporter read a conversation after its linked item was reassigned")
	}
	if _, err := f.service.readPGConversationPage(t.Context(), reporterScope, linked.ID, 0, 20); err == nil {
		t.Fatal("reporter read messages after the linked item was reassigned")
	}
	list, err := f.service.readConversationsPage(t.Context(), reporterScope, conversationListQuery{Project: f.project.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var listed conversationListPage
	if err := json.Unmarshal(list, &listed); err != nil {
		t.Fatal(err)
	}
	for _, record := range listed.Conversations {
		if record.ID == linked.ID {
			t.Fatal("reporter list included a conversation after its linked item was reassigned")
		}
	}
	if _, err := f.service.commandPatchConversationPG(t.Context(), reporterScope, linked.ID, conversationPatchRequest{}, "Example changed title"); err == nil {
		t.Fatal("reporter changed a conversation after its linked item was reassigned")
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/events?after=0", nil)
	response := httptest.NewRecorder()
	echoContext := echo.New().NewContext(request, response)
	echoContext.Set("native_scope", reporterScope)
	echoContext.SetParamNames("conversation")
	echoContext.SetParamValues(linked.ID)
	if err := f.service.streamConversationEvents(echoContext); err != nil || response.Code != http.StatusNotFound {
		t.Fatalf("reporter stream after linked item reassignment = status %d, error %v", response.Code, err)
	}
	for _, replaced := range []bool{true, false} {
		t.Run(fmt.Sprintf("missing_lease/replaced=%t", replaced), func(t *testing.T) {
			var selected conversationstore.Conversation
			if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				stored, err := w.Conversation(t.Context(), scope.project, linked.ID)
				if err != nil {
					return err
				}
				stored.Execution.Status, stored.Execution.Settled = conversation.ExecutionRunning, false
				stored.Execution.Owner = conversation.Owner{LeaseID: "lease_missing", FencingToken: int64(identity.FencingToken), AttemptID: "attempt_missing", RunID: "run_missing"}
				selected, err = w.UpdateConversation(t.Context(), stored, stored.Revision)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			wantOwner := selected.Execution.Owner
			if replaced {
				wantOwner.RunID = "replacement_run"
				f.service.config.TenantRepository = &conversationControlSnapshotRepository{Repository: repository, beforeWrite: func() error {
					return repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
						stored, err := w.Conversation(t.Context(), scope.project, linked.ID)
						if err != nil {
							return err
						}
						stored.Execution.Owner.RunID = wantOwner.RunID
						_, err = w.UpdateConversation(t.Context(), stored, stored.Revision)
						return err
					})
				}}
			}
			t.Cleanup(func() { f.service.config.TenantRepository = repository })
			changed, err := f.service.conversations.reconcileExecutionPG(t.Context(), scope.organization, selected)
			f.service.config.TenantRepository = repository
			if err != nil || changed == replaced {
				t.Fatalf("missing lease reconciliation: replaced=%t changed=%t error=%v", replaced, changed, err)
			}
			if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				stored, err := r.Conversation(t.Context(), scope.project, linked.ID)
				if err != nil {
					return err
				}
				wantStatus := conversation.ExecutionInterrupted
				if replaced {
					wantStatus = conversation.ExecutionRunning
				}
				if stored.Execution.Owner != wantOwner || stored.Execution.Status != wantStatus || stored.Execution.Settled == replaced {
					return fmt.Errorf("reconciliation rewrote the selected owner generation: %+v", stored.Execution)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}

}

func testPGConversationContinuation(t *testing.T, service *Service, repository tenantstore.Repository, scope, ownerScope nativeScope, item corestore.WorkItem, conversationID string, identity conversationWorkerIdentity) {
	t.Helper()
	originalClock, originalDispatch := service.config.now, service.config.ConversationDispatch
	defer func() { service.config.now, service.config.ConversationDispatch = originalClock, originalDispatch }()
	service.config.ConversationDispatch = tenantNativeRunEventParticipant{service: service}
	router := service.conversations.controls.(*conversationWorkerRouter)
	claimNumber := 0
	release := func() {
		t.Helper()
		if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			released, err := w.Release(t.Context(), leasestore.Fence{LeaseID: string(identity.LeaseID), FencingToken: int64(identity.FencingToken)}, "released")
			if err != nil {
				return err
			}
			return service.conversations.leaseReleasedTenant(t.Context(), w, released.LeaseID, released.FencingToken, "released")
		}); err != nil {
			t.Fatal(err)
		}
	}
	bind := func() conversationBindResponse {
		t.Helper()
		claimNumber++
		suffix := fmt.Sprint(claimNumber)
		var lease leasestore.Lease
		if err := repository.WriteTenant(t.Context(), scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			current, err := w.WorkItem(t.Context(), scope.project, item.NativeID)
			if err != nil {
				return err
			}
			lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: current.ID, ExpectedRevision: current.Revision, MachineID: string(scope.credential.Runner.MachineID), SessionID: "session-pg-continuation-" + suffix, LeaseID: "lease-pg-continuation-" + suffix, TTL: time.Hour})
			if err != nil {
				return err
			}
			_, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "attempt-pg-continuation-" + suffix, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: "run-pg-continuation-" + suffix, Sequence: 1, Status: "running", DispatchGeneration: int64(claimNumber + 2), PayloadObjectKey: "example/continuation-" + suffix})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		service.config.now = func() time.Time { return lease.RenewedAt }
		identity = conversationWorkerIdentity{LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), AttemptID: "attempt-pg-continuation-" + suffix}
		body := map[string]any{"lease_id": identity.LeaseID, "fencing_token": identity.FencingToken, "attempt_id": identity.AttemptID, "run_id": "run-pg-continuation-" + suffix, "capabilities": map[string]bool{"steer": true, "interrupt": true, "answer": true, "continue": true}}
		response := callConversationWorkerRoute(t, router.bind, scope, http.MethodPost, "/bind", string(item.NativeID), "", body, identity)
		if response.Code != http.StatusOK {
			t.Fatalf("continuation bind = %d: %s", response.Code, response.Body.String())
		}
		var bound conversationBindResponse
		if err := json.Unmarshal(response.Body.Bytes(), &bound); err != nil {
			t.Fatal(err)
		}
		return bound
	}
	for _, acknowledged := range []bool{false, true} {
		release()
		key := fmt.Sprintf("pg-continue-ack-%t", acknowledged)
		raw, err := service.commandPostConversation(t.Context(), ownerScope, conversationID, conversation.Command{Key: key, Kind: conversation.CommandContinue, Text: "Continue the example work", Expected: conversation.Expected{AttemptID: identity.AttemptID}})
		if err != nil {
			t.Fatal(err)
		}
		var receipt conversation.Receipt
		if err := json.Unmarshal(raw, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.Status != conversation.DeliverySaved {
			t.Fatalf("Continue receipt = %+v", receipt)
		}
		bound := bind()
		if !bound.Continuation || len(bound.Pending) != 1 || bound.Pending[0].MessageID != receipt.MessageID {
			t.Fatalf("saved Continue lost completion ownership: %+v", bound)
		}
		originalAttempt := identity.AttemptID
		_, err = service.commandPostConversation(t.Context(), ownerScope, conversationID, conversation.Command{Key: key + "-active", Kind: conversation.CommandContinue, Expected: conversation.Expected{AttemptID: identity.AttemptID}})
		var refusal *nativeError
		if !errors.As(err, &refusal) || refusal.status != http.StatusConflict || refusal.Code != "stale_execution" {
			t.Fatalf("active Continue = %v", err)
		}
		if acknowledged {
			if _, err := router.pollControlsPostgres(t.Context(), scope, conversationID, identity, bound.Cursor); err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			release()
			bound = bind()
			if !bound.Continuation || len(bound.Pending) != 0 {
				t.Fatalf("replacement lost ownership or replayed Continue: %+v", bound)
			}
			if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				message, err := r.Message(t.Context(), conversationID, receipt.MessageID)
				if err != nil {
					return err
				}
				want := conversation.DeliveryUnknown
				if acknowledged {
					want = conversation.DeliveryInterrupted
				}
				if message.AttemptID != originalAttempt || message.Delivery != want {
					t.Fatalf("replacement rewrote Continue: %+v", message)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if acknowledged {
			for _, status := range []string{"completed", "interrupted"} {
				response := callConversationWorkerRoute(t, router.turnEvents, scope, http.MethodPost, "/turn-events", "", conversationID, map[string]any{"lease_id": identity.LeaseID, "fencing_token": identity.FencingToken, "attempt_id": identity.AttemptID, "batch_key": key + "-" + status, "events": []map[string]any{{"type": "execution_status", "status": status}}}, identity)
				wantStatus := http.StatusAccepted
				if status == "interrupted" {
					wantStatus = http.StatusConflict
				}
				if response.Code != wantStatus {
					t.Fatalf("execution-level %s = %d: %s", status, response.Code, response.Body.String())
				}
				if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
					stored, err := r.Conversation(t.Context(), scope.project, conversationID)
					if err != nil {
						return err
					}
					if stored.ExecutionContinuation {
						return fmt.Errorf("execution-level %s retained or restored completed continuation", status)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		}

		response := callConversationWorkerRoute(t, router.unbind, scope, http.MethodPost, "/unbind", string(item.NativeID), "", map[string]any{"lease_id": identity.LeaseID, "fencing_token": identity.FencingToken, "attempt_id": identity.AttemptID, "outcome": "succeeded"}, identity)
		if response.Code != http.StatusOK {
			t.Fatalf("continuation unbind = %d: %s", response.Code, response.Body.String())
		}
		for range 2 {
			release()
			bound = bind()
			if bound.Continuation || len(bound.Pending) != 0 {
				t.Fatalf("completed Continue reclaimed ordinary ownership: %+v", bound)
			}
		}
		response = callConversationWorkerRoute(t, router.unbind, scope, http.MethodPost, "/unbind", string(item.NativeID), "", map[string]any{"lease_id": identity.LeaseID, "fencing_token": identity.FencingToken, "attempt_id": identity.AttemptID, "outcome": "succeeded"}, identity)
		if response.Code != http.StatusOK {
			t.Fatalf("ordinary unbind = %d: %s", response.Code, response.Body.String())
		}
	}
	release()
}

func callConversationWorkerRoute(t *testing.T, handler echo.HandlerFunc, scope nativeScope, method, path, item, conversationID string, body any, identity conversationWorkerIdentity) *httptest.ResponseRecorder {
	t.Helper()
	var encoded bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequestWithContext(t.Context(), method, path, &encoded)
	request.Header.Set("Content-Type", "application/json")
	if method == http.MethodGet {
		request.Header.Set(conversationHeaderLease, string(identity.LeaseID))
		request.Header.Set(conversationHeaderFencingToken, fmt.Sprint(identity.FencingToken))
		request.Header.Set(conversationHeaderAttempt, identity.AttemptID)
	}
	response := httptest.NewRecorder()
	c := echo.New().NewContext(request, response)
	c.Set("native_scope", scope)
	c.SetParamNames("item", "conversation")
	c.SetParamValues(item, conversationID)
	if err := handler(c); err != nil {
		echo.New().HTTPErrorHandler(err, c)
	}
	return response
}

type conversationControlSnapshotRepository struct {
	tenantstore.Repository
	beforeWrite func() error
	once        sync.Once
}

func (r *conversationControlSnapshotRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	var err error
	r.once.Do(func() { err = r.beforeWrite() })
	if err != nil {
		return err
	}
	return r.Repository.WriteTenant(ctx, org, workload, write)
}

type conversationBindOrderRepository struct {
	tenantstore.Repository
	first          chan string
	projectRelease chan struct{}
	once           sync.Once
}

type conversationBindOrderWriter struct {
	tenantstore.Writer
	order *conversationBindOrderRepository
}

func (r *conversationBindOrderRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, org, workload, func(w tenantstore.Writer) error { return write(conversationBindOrderWriter{Writer: w, order: r}) })
}

func (w conversationBindOrderWriter) RunnerIdentityByCredential(ctx context.Context, id string) (runnerstore.Identity, error) {
	w.order.once.Do(func() { w.order.first <- "runner" })
	return w.Writer.RunnerIdentityByCredential(ctx, id)
}

func (w conversationBindOrderWriter) LockWorkItemProject(ctx context.Context, project tracker.ProjectID) error {
	if err := w.Writer.LockWorkItemProject(ctx, project); err != nil {
		return err
	}
	w.order.once.Do(func() { w.order.first <- "project" })
	select {
	case <-w.order.projectRelease:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w conversationBindOrderWriter) LockLease(ctx context.Context, id string) (leasestore.Lease, error) {
	return w.Writer.(leasestore.FencedLeaseLocker).LockLease(ctx, id)
}

func bindWithConcurrentRunnerEvent(t *testing.T, admin *sql.DB, order *conversationBindOrderRepository, org tracker.OrganizationID, project tracker.ProjectID, runner string, bind func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	event, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer event.Rollback()
	var id string
	if err := event.QueryRowContext(ctx, `SELECT runner_id FROM runner_identities WHERE organization_id=$1 AND runner_id=$2 FOR UPDATE`, org, runner).Scan(&id); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	var release sync.Once
	joined := false
	go func() { done <- bind() }()
	defer func() {
		release.Do(func() { close(order.projectRelease) })
		event.Rollback()
		if !joined {
			<-done
		}
	}()
	select {
	case <-order.first:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	projectErr := event.QueryRowContext(ctx, `SELECT id FROM projects WHERE organization_id=$1 AND id=$2 FOR UPDATE NOWAIT`, org, project).Scan(&id)
	if err := event.Rollback(); err != nil {
		t.Error(err)
	}
	release.Do(func() { close(order.projectRelease) })
	if projectErr != nil {
		t.Errorf("runner event could not acquire project while bind waited for runner: %v", projectErr)
	}
	select {
	case response := <-done:
		joined = true
		return response
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return nil
}

type conversationControlOrderRepository struct {
	tenantstore.Repository
	attempted chan struct{}
	once      sync.Once
}

type conversationControlOrderWriter struct {
	tenantstore.Writer
	order *conversationControlOrderRepository
}

func (r *conversationControlOrderRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, org, workload, func(w tenantstore.Writer) error {
		return write(conversationControlOrderWriter{Writer: w, order: r})
	})
}

func (w conversationControlOrderWriter) RunnerIdentityByCredential(ctx context.Context, id string) (runnerstore.Identity, error) {
	w.order.once.Do(func() { close(w.order.attempted) })
	return w.Writer.RunnerIdentityByCredential(ctx, id)
}

func pollControlsAgainstRunnerEvent(t *testing.T, admin *sql.DB, service *Service, router *conversationWorkerRouter, scope nativeScope, id string, identity conversationWorkerIdentity, after int64) conversationControlsResponse {
	t.Helper()
	blocker, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var runner string
	if err := blocker.QueryRowContext(t.Context(), `SELECT runner_id FROM runner_identities WHERE organization_id=$1 AND token_id=$2 FOR UPDATE`, scope.organization, scope.credential.ID).Scan(&runner); err != nil {
		blocker.Rollback()
		t.Fatal(err)
	}
	finished := make(chan struct{})
	previousRepository, previousAuthority := service.config.TenantRepository, service.config.ConversationWorkerAuthority
	order := &conversationControlOrderRepository{Repository: previousRepository, attempted: make(chan struct{})}
	service.config.TenantRepository = order
	service.config.ConversationWorkerAuthority = conversationWorkerAuthorityFunc(func(ctx context.Context, reader tenantstore.Reader, worker ConversationWorkerIdentity) error {
		if _, err := reader.RunnerIdentityByCredential(ctx, worker.Credential.ID); err != nil {
			return err
		}
		return previousAuthority.VerifyConversationWorker(ctx, reader, worker)
	})
	defer func() {
		blocker.Rollback()
		<-finished
		service.config.TenantRepository, service.config.ConversationWorkerAuthority = previousRepository, previousAuthority
	}()
	var page conversationControlsResponse
	var pollErr error
	go func() {
		defer close(finished)
		page, pollErr = router.pollControlsPostgres(t.Context(), scope, id, identity, after)
	}()
	select {
	case <-order.attempted:
	case <-finished:
		t.Fatalf("control poll finished before runner authority: %v", pollErr)
	}
	var credentialID string
	if err := blocker.QueryRowContext(t.Context(), `SELECT id FROM api_tokens WHERE organization_id=$1 AND id=$2 FOR UPDATE NOWAIT`, scope.organization, scope.credential.ID).Scan(&credentialID); err != nil {
		t.Fatalf("control poll locked credential while waiting for runner identity: %v", err)
	}
	var conversationID string
	if err := blocker.QueryRowContext(t.Context(), `SELECT id FROM conversations WHERE organization_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE NOWAIT`, scope.organization, scope.project, id).Scan(&conversationID); err != nil {
		t.Fatalf("control poll locked conversation while waiting for runner identity: %v", err)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	<-finished
	if pollErr != nil {
		t.Fatal(pollErr)
	}
	return page
}

type conversationReconcileOrderRepository struct {
	tenantstore.Repository
	workItemAttempted chan struct{}
	leaseAttempted    chan struct{}
	workItemOnce      sync.Once
	leaseOnce         sync.Once
}

type conversationReconcileOrderWriter struct {
	tenantstore.Writer
	order *conversationReconcileOrderRepository
}

func (r *conversationReconcileOrderRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, org, workload, func(w tenantstore.Writer) error {
		return write(conversationReconcileOrderWriter{Writer: w, order: r})
	})
}

func (w conversationReconcileOrderWriter) LockWorkItem(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (corestore.WorkItem, error) {
	w.order.workItemOnce.Do(func() { close(w.order.workItemAttempted) })
	return w.Writer.LockWorkItem(ctx, project, item)
}

func (w conversationReconcileOrderWriter) LockLease(ctx context.Context, id string) (leasestore.Lease, error) {
	w.order.leaseOnce.Do(func() { close(w.order.leaseAttempted) })
	return w.Writer.(leasestore.FencedLeaseLocker).LockLease(ctx, id)
}

func reconcileWithWorkerLock(t *testing.T, admin *sql.DB, service *Service, repository tenantstore.Repository, scope nativeScope, candidate conversationstore.Conversation, held string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	blocker, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	var itemID int64
	var leaseID string
	if held == "work_item" {
		err = blocker.QueryRowContext(ctx, `SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3 FOR UPDATE`, scope.organization, scope.project, candidate.WorkItemID).Scan(&itemID)
	} else {
		err = blocker.QueryRowContext(ctx, `SELECT lease_id FROM leases WHERE organization_id=$1 AND lease_id=$2 FOR UPDATE`, scope.organization, candidate.Execution.Owner.LeaseID).Scan(&leaseID)
	}
	if err != nil {
		t.Fatal(err)
	}
	order := &conversationReconcileOrderRepository{Repository: repository, workItemAttempted: make(chan struct{}), leaseAttempted: make(chan struct{})}
	service.config.TenantRepository = order
	finished := make(chan struct{})
	joined := false
	var changed bool
	var reconcileErr error
	defer func() {
		blocker.Rollback()
		cancel()
		if !joined {
			<-finished
		}
		service.config.TenantRepository = repository
	}()
	go func() {
		defer close(finished)
		changed, reconcileErr = service.conversations.reconcileExecutionPG(ctx, scope.organization, candidate)
	}()
	attempted := order.workItemAttempted
	if held == "lease" {
		attempted = order.leaseAttempted
	}
	select {
	case <-attempted:
	case <-finished:
		joined = true
		t.Fatalf("reconciliation did not wait on worker %s authority: changed=%t error=%v", held, changed, reconcileErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if held == "work_item" {
		if err := blocker.QueryRowContext(ctx, `SELECT lease_id FROM leases WHERE organization_id=$1 AND lease_id=$2 FOR UPDATE NOWAIT`, scope.organization, candidate.Execution.Owner.LeaseID).Scan(&leaseID); err != nil {
			t.Fatalf("reconciliation locked lease while waiting for work item: %v", err)
		}
	}
	var conversationID string
	if err := blocker.QueryRowContext(ctx, `SELECT id FROM conversations WHERE organization_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE NOWAIT`, scope.organization, scope.project, candidate.ID).Scan(&conversationID); err != nil {
		t.Fatalf("reconciliation locked conversation while waiting for %s: %v", held, err)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
		joined = true
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if reconcileErr != nil || changed {
		t.Fatalf("worker authority changed while locks were held: changed=%t error=%v", changed, reconcileErr)
	}
}
