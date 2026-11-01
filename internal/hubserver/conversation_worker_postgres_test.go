package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type conversationWorkerAuthorityFunc func(context.Context, tenantstore.Reader, ConversationWorkerIdentity) error

func (f conversationWorkerAuthorityFunc) VerifyConversationWorker(ctx context.Context, reader tenantstore.Reader, identity ConversationWorkerIdentity) error {
	return f(ctx, reader, identity)
}

func TestConversationPostgresWorkerHandlersUseFencedTenantState(t *testing.T) {
	f := newConversationAPIFixture(t, nil)
	var logs bytes.Buffer
	f.service.config.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	repository := projectRecordPostgres(t)
	f.worker(t, "pg-conversation-worker")
	var ownerCredential accessstore.Credential
	if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT id,name,token_hash,token_fingerprint,scope,native_only FROM api_tokens WHERE id=?`, f.ownerID).Scan(&ownerCredential.ID, &ownerCredential.Name, &ownerCredential.Hash, &ownerCredential.Fingerprint, &ownerCredential.Scope, &ownerCredential.NativeOnly); err != nil {
		t.Fatal(err)
	}
	ownerCredential.OrganizationID = f.project.OrganizationID
	ownerCredential.ProjectAccess = "all"
	ownerCredential.Kind = "personal"
	ownerCredential.CreatedAt, ownerCredential.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
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
	workerID := ""
	var workerCredential accessstore.Credential
	if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT id,name,token_hash,token_fingerprint,scope,native_only FROM api_tokens WHERE name=?`, "pg-conversation-worker").Scan(&workerCredential.ID, &workerCredential.Name, &workerCredential.Hash, &workerCredential.Fingerprint, &workerCredential.Scope, &workerCredential.NativeOnly); err != nil {
		t.Fatal(err)
	}
	workerID = workerCredential.ID
	workerCredential.OrganizationID = f.project.OrganizationID
	workerCredential.ProjectAccess = "all"
	workerCredential.Kind = "service"
	workerCredential.CreatedAt, workerCredential.UpdatedAt = time.Now().UTC(), time.Now().UTC()
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
		lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(binding.MachineID), SessionID: "session-pg-worker", LeaseID: "lease-pg-worker", TTL: time.Hour})
		if err != nil {
			return err
		}
		_, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "attempt-pg-worker", Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: "run-pg-worker", Sequence: 1, Status: "running", DispatchGeneration: 1, PayloadObjectKey: "example/payload"})
		if err != nil {
			return err
		}
		createdAt := time.Now().UTC()
		_, err = w.CreateConversation(t.Context(), conversationstore.Conversation{
			OrganizationID: f.project.OrganizationID, ID: conversation.NewConversationID(), ProjectID: f.project.ID,
			OwnerPrincipalID: ownerCredential.ID, Origin: conversationOriginWorker, Title: "Worker issue",
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
	pendingCommand := f.command(t, f.token, linked.ID, conversation.Command{Key: "pg-bind-control", Kind: conversation.CommandMessage, Text: "Example pending instruction"})
	requireNativeStatus(t, pendingCommand, http.StatusOK)
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
	f.service.config.now = func() time.Time { return currentLease.RenewedAt.Add(-time.Second) }
	futureRenewal := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		_, _, err := f.service.authorizeConversationWorkerLeasePG(t.Context(), r, scope, string(issue.NativeID), identity, "run-pg-worker", false)
		return err
	})
	f.service.config.now = originalClock
	if futureRenewal == nil {
		t.Fatal("future-dated lease renewal was accepted")
	}

	bindBody := map[string]any{"lease_id": identity.LeaseID, "fencing_token": identity.FencingToken, "attempt_id": identity.AttemptID, "run_id": "run-pg-worker", "capabilities": map[string]bool{"steer": true, "interrupt": true, "answer": true, "continue": true}}
	bound := callConversationWorkerRoute(t, router.bind, scope, http.MethodPost, "/bind", string(issue.NativeID), "", bindBody, identity)
	if bound.Code != http.StatusOK {
		t.Fatalf("bind status = %d, body = %s, log = %s", bound.Code, bound.Body.String(), logs.String())
	}
	var bindResponse conversationBindResponse
	if err := json.Unmarshal(bound.Body.Bytes(), &bindResponse); err != nil {
		t.Fatal(err)
	}
	if bindResponse.ConversationID != linked.ID || len(bindResponse.Resume.Transcript) != 1 || bindResponse.Resume.Transcript[0].Text != "Example prior response" || len(bindResponse.Pending) != 1 || bindResponse.Pending[0].Key != "pg-bind-control" || bindResponse.Pending[0].Text != "Example pending instruction" {
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

	operatorMessage := f.command(t, f.token, linked.ID, conversation.Command{Key: "pg-control", Kind: conversation.CommandMessage, Text: "Example instruction"})
	requireNativeStatus(t, operatorMessage, http.StatusOK)
	controls := callConversationWorkerRoute(t, router.controls, scope, http.MethodGet, "/controls?after=0&wait=0", "", linked.ID, nil, identity)
	if controls.Code != http.StatusOK {
		t.Fatalf("controls status = %d, body = %s", controls.Code, controls.Body.String())
	}
	var controlPage conversationControlsResponse
	if err := json.Unmarshal(controls.Body.Bytes(), &controlPage); err != nil {
		t.Fatal(err)
	}
	if len(controlPage.Controls) != 1 || controlPage.Controls[0].Key != "pg-control" {
		t.Fatalf("controls page = %+v", controlPage)
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
		acknowledged, err := router.pollControlsPostgres(t.Context(), scope, linked.ID, identity, acknowledgementCursor)
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
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "attempt-pg-worker-next", Fence: leasestore.Fence{LeaseID: nextLease.LeaseID, FencingToken: nextLease.FencingToken}, RunID: "run-pg-worker-next", Sequence: 1, Status: "running", DispatchGeneration: 2, PayloadObjectKey: "example/payload-next"})
		if err != nil {
			return fmt.Errorf("record replacement attempt: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	nextIdentity := conversationWorkerIdentity{LeaseID: tracker.LeaseID(nextLease.LeaseID), FencingToken: tracker.FencingToken(nextLease.FencingToken), AttemptID: "attempt-pg-worker-next"}
	nextBindBody := map[string]any{"lease_id": nextIdentity.LeaseID, "fencing_token": nextIdentity.FencingToken, "attempt_id": nextIdentity.AttemptID, "run_id": "run-pg-worker-next", "capabilities": map[string]bool{"steer": true, "interrupt": true, "answer": true, "continue": true}}
	takeover := callConversationWorkerRoute(t, router.bind, scope, http.MethodPost, "/bind", string(issue.NativeID), "", nextBindBody, nextIdentity)
	if takeover.Code != http.StatusOK {
		t.Fatalf("replacement bind status = %d, body = %s, log = %s", takeover.Code, takeover.Body.String(), logs.String())
	}
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
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

	reporterScope := nativeScope{
		organization: f.project.OrganizationID,
		project:      f.project.ID,
		credential: apiCredential{
			ID: ownerCredential.ID, Hash: ownerCredential.Hash, Scope: apiScopeOperator,
			HostedRole: "reporter",
		},
		sourceActor: &tracker.Actor{Kind: "human", PrincipalID: f.ownerID},
	}
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
