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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// conversationWorkerFixture drives the worker endpoints against a linked
// conversation that was seeded directly in the store: the operator API is
// built separately and the worker path must not depend on it.
type conversationWorkerFixture struct {
	nativeFixture
	mu        sync.Mutex
	now       time.Time
	chat      *conversationService
	runner    runnerFixture
	worker    string
	principal string
	issue     tracker.NativeIssue
	policy    string
	lease     tracker.NativeLease
	attempt   string
	run       string
	record    conversationRecord
}

func newConversationWorkerFixture(t *testing.T, contents ...attachment.ArchiveContentStore) *conversationWorkerFixture {
	t.Helper()
	fixture := &conversationWorkerFixture{now: time.Now().UTC().Truncate(time.Second)}
	config := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &verifiedAttemptDetails{}, Conversation: &ConversationConfig{Enabled: true, QuestionTimeout: time.Hour}, now: func() time.Time {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		return fixture.now
	}}
	var logs bytes.Buffer
	config.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	config.Conversation.ArchiveContents = &conversationContentFixture{}
	if len(contents) > 0 {
		config.Conversation.ArchiveContents = contents[0]
	}
	f := newDefaultNativeFixture(t, config)
	f.service.registerConversationRoutes(f.service.echo)
	fixture.nativeFixture = f
	fixture.chat = f.service.conversations
	if fixture.chat == nil {
		t.Fatal("conversation service must be enabled")
	}
	policy := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", policy)
	fixture.policy = policy.ID
	fixture.issue = f.create(t, "conversation-work")
	fixture.runner = conversationWorkerRunner(t, f)
	fixture.worker = fixture.runner.redemption.Credential
	token := conversationFixturePrincipal(t, f, "Example conversation owner")
	fixture.principal = token.ID
	linked := fixture.now
	record := conversationRecord{
		ID: conversation.NewConversationID(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID,
		OwnerPrincipalID: token.ID, OwnerSubject: "owner@example.test", Title: "linked", Visibility: conversation.VisibilityShared, Status: conversation.StatusActive,
		WorkItemID: string(fixture.issue.WorkItemID), LinkedAt: &linked,
		Execution: conversation.Execution{Status: conversation.ExecutionWaitingForRunner, UpdatedAt: fixture.now},
		CreatedAt: fixture.now, UpdatedAt: fixture.now,
	}
	record.Origin, record.Revision, record.Preferences = conversationOriginUser, 1, conversation.DefaultPreferences()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		stored, err := w.CreateConversation(t.Context(), conversationRecordToStore(record))
		record = conversationRecordFromStore(stored)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fixture.record = record
	fixture.claim(t)
	return fixture
}

func (f *conversationWorkerFixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *conversationWorkerFixture) tx(t *testing.T) *sql.Tx {
	t.Helper()
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// claim claims a fresh lease for the issue and starts a new attempt on it.
func (f *conversationWorkerFixture) claim(t *testing.T, start ...bool) {
	t.Helper()
	if f.lease.ID != "" && !f.lease.ExpiresAt.After(f.now) {
		if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE leases SET expires_at=$1 WHERE organization_id=$2 AND lease_id=$3 AND released_at IS NULL", f.now.Add(-time.Microsecond), f.project.OrganizationID, f.lease.ID); err != nil {
			t.Fatal(err)
		}
	}
	f.lease = nativeAttemptFixtureLease(t, f.runner, f.issue, newNativeID("session"))
	f.lease.ExpiresAt = f.now.Add(10 * time.Minute)
	if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE leases SET acquired_at=$1,renewed_at=$1,expires_at=$2 WHERE organization_id=$3 AND lease_id=$4", f.now, f.lease.ExpiresAt, f.project.OrganizationID, f.lease.ID); err != nil {
		t.Fatal(err)
	}
	f.attempt = newNativeID("attempt")
	f.run = newNativeID("run")
	if len(start) == 0 || start[0] {
		f.startAttempt(t, tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"})
	}
}

func (f *conversationWorkerFixture) startAttempt(t *testing.T, identity tracker.NativeExecutionIdentity) {
	t.Helper()
	event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("start")}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: 1, Identity: &identity, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
}

func (f *conversationWorkerFixture) release(t *testing.T) {
	t.Helper()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/release", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
}

func (f *conversationWorkerFixture) identity() map[string]any {
	return map[string]any{"lease_id": f.lease.ID, "fencing_token": f.lease.FencingToken, "attempt_id": f.attempt}
}

func (f *conversationWorkerFixture) bindBody() map[string]any {
	body := f.identity()
	body["run_id"] = f.run
	body["capabilities"] = map[string]bool{"steer": true, "interrupt": true, "answer": true, "continue": true}
	return body
}

func (f *conversationWorkerFixture) bind(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		body = f.bindBody()
	}
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/conversation/bind", f.worker, body)
}

func (f *conversationWorkerFixture) unbind(t *testing.T, outcome string) *httptest.ResponseRecorder {
	t.Helper()
	body := f.identity()
	body["outcome"] = outcome
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/conversation/unbind", f.worker, body)
}

func (f *conversationWorkerFixture) turnEvents(t *testing.T, events ...map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := f.identity()
	body["events"] = events
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/turn-events", f.worker, body)
}

func (f *conversationWorkerFixture) controls(t *testing.T, after int64, wait int) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, f.base+"/conversations/"+f.record.ID+"/controls?after="+strconv.FormatInt(after, 10)+"&wait="+strconv.Itoa(wait), nil)
	request.Header.Set("Authorization", "Bearer "+f.worker)
	request.Header.Set("X-Detent-Lease", string(f.lease.ID))
	request.Header.Set("X-Detent-Fencing-Token", strconv.FormatInt(int64(f.lease.FencingToken), 10))
	request.Header.Set("X-Detent-Attempt", f.attempt)
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

// awaitControlSubscribers waits until want broker subscriptions watch the
// conversation, so a test acts only once a long poll is actually waiting.
func (f *conversationWorkerFixture) awaitControlSubscribers(t *testing.T, want int) {
	t.Helper()
	testenv.WaitUntil(t, t.Context(), func() error {
		f.chat.broker.mu.Lock()
		got := len(f.chat.broker.subscribers[f.record.ID])
		f.chat.broker.mu.Unlock()
		if got >= want {
			return nil
		}
		return fmt.Errorf("control poll did not subscribe: subscribers=%d, want %d", got, want)
	})
}

// queue seeds an accepted user control the way the operator API would:
// reserved command key, queued message and queued receipt, then committed.
func (f *conversationWorkerFixture) queue(t *testing.T, kind conversation.MessageKind, key, text string, data map[string]any) conversationMessageRecord {
	t.Helper()
	ctx := t.Context()
	record := f.load(t)
	commandKind := map[conversation.MessageKind]conversation.CommandKind{conversation.MessageText: conversation.CommandMessage, conversation.MessageAnswer: conversation.CommandAnswer, conversation.MessageInterrupt: conversation.CommandInterrupt, conversation.MessageContinue: conversation.CommandContinue}[kind]
	now := f.service.config.now()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	message := conversationMessageRecord{Role: conversation.RoleUser, Kind: kind, Text: text, Data: encoded, Delivery: conversation.DeliveryQueued, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.principal}, CommandKey: key}
	plan := &conversationPublicationPlan{service: f.chat, record: record, original: record, now: now, coordinator: true}
	if err := plan.appendMessage(t.Context(), &message); err != nil {
		t.Fatal(err)
	}
	receiptPlan := conversation.Receipt{Key: key, Kind: commandKind, Status: conversation.DeliveryQueued, MessageID: message.ID}
	if data != nil {
		receiptPlan.QuestionID, _ = data["question_id"].(string)
	}
	if err := plan.recordReceipt(t.Context(), receiptPlan); err != nil {
		t.Fatal(err)
	}
	if err := plan.saveConversation(t.Context()); err != nil {
		t.Fatal(err)
	}
	initial, err := json.Marshal(conversation.Receipt{Key: key, Kind: commandKind, Status: conversation.DeliverySaved, UpdatedAt: now.UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.prepareValue(t.Context(), initial); err != nil {
		t.Fatal(err)
	}
	for _, body := range plan.events {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if err := plan.prepareValue(t.Context(), raw); err != nil {
			t.Fatal(err)
		}
	}
	ctx = plan.context(t.Context())
	coordinator := f.chat.coordinator.(*conversationTurnCoordinator)
	f.writeTenant(t, func(w tenantstore.Writer) error {
		if err := registerPreparedConversationObjects(ctx, w, f.project.OrganizationID, plan.prepared, now); err != nil {
			return err
		}
		if _, _, err := w.ReserveCommand(ctx, conversationstore.Command{OrganizationID: f.project.OrganizationID, ConversationID: record.ID, Key: key, Kind: commandKind, RequestHash: "hash-" + key, Receipt: conversation.Receipt{Key: key, Kind: commandKind, Status: conversation.DeliverySaved, UpdatedAt: now}, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := coordinator.appendMessage(ctx, w, &record, &message, now); err != nil {
			return err
		}
		receipt := conversation.Receipt{Key: key, Kind: commandKind, Status: conversation.DeliveryQueued, MessageID: message.ID, UpdatedAt: now}
		if data != nil {
			receipt.QuestionID, _ = data["question_id"].(string)
		}
		if err := w.UpdateCommand(ctx, conversationstore.Command{OrganizationID: f.project.OrganizationID, ConversationID: record.ID, Key: key, Kind: commandKind, RequestHash: "hash-" + key, Receipt: receipt, UpdatedAt: now}); err != nil {
			return err
		}
		if err := appendCoordinatorEvent(ctx, w, f.project.OrganizationID, record.ID, conversation.EventCommandReceipt, receipt, now); err != nil {
			return err
		}
		return coordinator.saveConversation(ctx, w, &record, now)
	})
	f.chat.committed(record)
	return message
}

func (f *conversationWorkerFixture) load(t *testing.T) conversationRecord {
	t.Helper()
	var record conversationRecord
	f.readTenant(t, func(r tenantstore.Reader) error {
		stored, err := r.Conversation(t.Context(), f.project.ID, f.record.ID)
		record = conversationRecordFromStore(stored)
		return err
	})
	return record
}

func (f *conversationWorkerFixture) messages(t *testing.T) map[string]conversationMessageRecord {
	t.Helper()
	list, err := f.chat.coordinator.(*conversationTurnCoordinator).coordinatorMessages(t.Context(), conversationstore.MessageFilter{ConversationID: f.record.ID, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]conversationMessageRecord, len(list))
	for _, message := range list {
		byID[message.ID] = message
	}
	return byID
}

func (f *conversationWorkerFixture) receipt(t *testing.T, key string) conversation.Receipt {
	t.Helper()
	var receipt conversation.Receipt
	f.readTenant(t, func(r tenantstore.Reader) error {
		command, found, err := r.Command(t.Context(), f.record.ID, key)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("missing command receipt")
		}
		receipt = command.Receipt
		return nil
	})
	return receipt
}

func (f *conversationWorkerFixture) questions(t *testing.T) []conversationQuestionRecord {
	t.Helper()
	records := []conversationQuestionRecord{}
	f.readTenant(t, func(r tenantstore.Reader) error {
		questions, err := r.Questions(t.Context(), f.record.ID, "", 200)
		for _, question := range questions {
			records = append(records, conversationQuestionRecord{Question: question.Question, RequestID: question.RequestID, ContentID: question.ContentReference})
		}
		return err
	})
	return records
}

func (f *conversationWorkerFixture) eventTypes(t *testing.T, after int64) []string {
	t.Helper()
	events, err := f.replay(t)
	if err != nil {
		t.Fatal(err)
	}
	types := []string{}
	for _, event := range events {
		if event.Seq > after {
			types = append(types, string(event.Type))
		}
	}
	return types
}

func (f *conversationWorkerFixture) readTenant(t *testing.T, fn func(tenantstore.Reader) error) {
	t.Helper()
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, fn); err != nil {
		t.Fatal(err)
	}
}

func (f *conversationWorkerFixture) writeTenant(t *testing.T, fn func(tenantstore.Writer) error) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, fn); err != nil {
		t.Fatal(err)
	}
}

type workerControlEnvelope struct {
	Cursor      int64                     `json:"cursor"`
	Key         string                    `json:"key"`
	Kind        string                    `json:"kind"`
	MessageID   string                    `json:"message_id"`
	Text        string                    `json:"text"`
	QuestionID  string                    `json:"question_id"`
	RequestID   string                    `json:"request_id"`
	Answers     map[string][]string       `json:"answers"`
	Attachments []conversation.Attachment `json:"attachments"`
	Expected    conversation.Expected     `json:"expected"`
}

type workerBindResponse struct {
	ConversationID string `json:"conversation_id"`
	Continuation   bool   `json:"continuation"`
	Resume         struct {
		ThreadID   string            `json:"thread_id"`
		Transcript []json.RawMessage `json:"transcript"`
	} `json:"resume"`
	Pending []workerControlEnvelope `json:"pending"`
	Cursor  int64                   `json:"cursor"`
}

type workerControlsResponse struct {
	Controls []workerControlEnvelope `json:"controls"`
	Cursor   int64                   `json:"cursor"`
}

type conversationStartupInterleaveRepository struct {
	tenantstore.Repository
	afterMissing func()
	latest       *leasestore.Attempt
}

func (r conversationStartupInterleaveRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(conversationStartupInterleaveReader{Reader: reader, afterMissing: r.afterMissing, latest: r.latest})
	})
}

type conversationStartupInterleaveReader struct {
	tenantstore.Reader
	afterMissing func()
	latest       *leasestore.Attempt
}

func (w conversationStartupInterleaveReader) Attempt(ctx context.Context, id string) (leasestore.Attempt, error) {
	attempt, err := w.Reader.Attempt(ctx, id)
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		w.afterMissing()
	}
	return attempt, err
}

func (w conversationStartupInterleaveReader) LatestAttempt(ctx context.Context, id int64) (leasestore.Attempt, error) {
	attempt, err := w.Reader.LatestAttempt(ctx, id)
	*w.latest = attempt
	return attempt, err
}

func requireConversationErrorCode(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	requireNativeStatus(t, response, status)
	var failure struct {
		Code string `json:"code"`
	}
	decodeHubResponse(t, response, &failure)
	if failure.Code != code {
		t.Fatalf("code = %q, want %q: %s", failure.Code, code, response.Body.String())
	}
}

func TestConversationWorkerBind(t *testing.T) {
	t.Parallel()
	t.Run("startup commits between reconciliation reads", func(t *testing.T) {
		for _, test := range []struct {
			name       string
			different  string
			terminal   string
			wantActive bool
		}{
			{name: "exact running owner", wantActive: true},
			{name: "different attempt", different: "attempt_id"},
			{name: "different run", different: "run_id"},
			{name: "succeeded owner", terminal: "succeeded"},
			{name: "failed owner", terminal: "failed"},
			{name: "cancelled owner", terminal: "cancelled"},
			{name: "interrupted owner", terminal: "interrupted"},
		} {
			t.Run(test.name, func(t *testing.T) {
				f := newConversationWorkerFixture(t)
				f.release(t)
				f.claim(t, false)
				body := f.bindBody()
				if test.different != "" {
					body[test.different] = newNativeID(strings.TrimSuffix(test.different, "_id"))
				}
				requireNativeStatus(t, f.bind(t, body), http.StatusOK)
				before := f.load(t)
				started := 0
				var latest leasestore.Attempt
				repository := conversationStartupInterleaveRepository{
					Repository: f.service.config.TenantRepository,
					afterMissing: func() {
						started++
						f.startAttempt(t, tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "example-model"})
						if test.terminal != "" {
							if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE native_attempts SET status=$1 WHERE organization_id=$2 AND id=$3", test.terminal, f.project.OrganizationID, f.attempt); err != nil {
								t.Fatal(err)
							}
						}
					},
					latest: &latest,
				}
				if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Background, func(reader tenantstore.Reader) error {
					current, err := conversationLeaseCurrentPG(t.Context(), reader, before, f.now)
					if err == nil && current != test.wantActive {
						t.Fatalf("interleaved startup ownership=%t, want %t", current, test.wantActive)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				reconciler := &conversationService{server: &Service{config: f.service.config}}
				changed, err := reconciler.reconcileExecutionPG(t.Context(), f.project.OrganizationID, conversationRecordToStore(before))
				if err != nil {
					t.Fatal(err)
				}
				if started != 1 || latest.ID != f.attempt || latest.LeaseID != string(f.lease.ID) || latest.RunID != f.run {
					t.Fatalf("startup interleave: starts=%d latest=%+v", started, latest)
				}
				after := f.load(t)
				if test.wantActive {
					if changed || after.Execution != before.Execution || after.Revision != before.Revision || after.EventSeq != before.EventSeq {
						t.Fatalf("live startup reconciled: changed=%t execution=%+v revision=%d events=%d", changed, after.Execution, after.Revision, after.EventSeq)
					}
				} else if !changed || after.Execution.Status != conversation.ExecutionInterrupted || !after.Execution.Settled {
					t.Fatalf("stale startup remained live: changed=%t execution=%+v", changed, after.Execution)
				}
			})
		}
	})
	t.Run("failed setup unbinds before provider start", func(t *testing.T) {
		f := newConversationWorkerFixture(t)
		f.release(t)
		f.claim(t, false)
		requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
		requireConversationErrorCode(t, f.controls(t, 0, 0), http.StatusConflict, "stale_execution")
		body := f.identity()
		body["batch_key"] = "setup-status"
		body["events"] = []map[string]any{{"type": "execution_status", "status": conversation.ExecutionStarting}}
		publish := func() *httptest.ResponseRecorder {
			return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/turn-events", f.worker, body)
		}
		first := publish()
		requireNativeStatus(t, first, http.StatusAccepted)
		var receipt conversationTurnEventsResponse
		decodeHubResponse(t, first, &receipt)
		replayed := publish()
		requireNativeStatus(t, replayed, http.StatusAccepted)
		var replay conversationTurnEventsResponse
		decodeHubResponse(t, replayed, &replay)
		if replay.EventSeq != receipt.EventSeq {
			t.Fatalf("pre-start replay sequence = %d, want %d", replay.EventSeq, receipt.EventSeq)
		}
		requireNativeStatus(t, f.unbind(t, "failed"), http.StatusOK)
		requireNativeStatus(t, f.unbind(t, "failed"), http.StatusOK)
		requireConversationErrorCode(t, f.controls(t, 0, 0), http.StatusConflict, "stale_execution")
		if record := f.load(t); record.Execution.Status != conversation.ExecutionFailed || !record.Execution.Settled {
			t.Fatalf("pre-start binding did not settle: %+v", record.Execution)
		}
	})
	t.Run("bind before provider identity is persisted", func(t *testing.T) {
		f := newConversationWorkerFixture(t)
		f.release(t)
		f.claim(t, false)
		body := f.bindBody()
		body["fencing_token"] = f.lease.FencingToken + 1
		requireConversationErrorCode(t, f.bind(t, body), http.StatusConflict, "stale_execution")
		requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
		if err := f.chat.reconcileExecutions(t.Context()); err != nil {
			t.Fatal(err)
		}
		if record := f.load(t); record.Execution.Status != conversation.ExecutionStarting {
			t.Fatalf("pre-start binding lost its current lease: %+v", record.Execution)
		}
		body = f.bindBody()
		body["attempt_id"] = newNativeID("attempt")
		requireConversationErrorCode(t, f.bind(t, body), http.StatusConflict, "stale_execution")
		identity := tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "example-model", ReasoningEffort: "high"}
		f.startAttempt(t, identity)
		data, err := f.service.readNativeAttemptTenant(t.Context(), nativeAttemptListingScope(t, f.nativeFixture), string(f.issue.WorkItemID), f.attempt)
		if err != nil || data.Identity == nil || *data.Identity != identity {
			t.Fatalf("started identity=%+v, error=%v", data.Identity, err)
		}
		requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
		body = f.bindBody()
		body["attempt_id"] = newNativeID("attempt")
		requireConversationErrorCode(t, f.bind(t, body), http.StatusConflict, "stale_execution")
	})
	for _, test := range []struct{ confirmed, legacy bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		confirmed := test.confirmed
		t.Run("ordinary Continue lease loss confirmed="+strconv.FormatBool(confirmed)+" legacy="+strconv.FormatBool(test.legacy), func(t *testing.T) {
			f := newConversationWorkerFixture(t)
			bind := func() *httptest.ResponseRecorder {
				if test.legacy {
					if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), `UPDATE conversation_events SET query_metadata_version=0,message_role=NULL,message_kind=NULL,attempt_id=NULL,execution_status=NULL WHERE conversation_id=$1`, f.record.ID); err != nil {
						t.Fatal(err)
					}
				}
				return f.bind(t, nil)
			}
			if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "DELETE FROM conversations WHERE id = $1", f.record.ID); err != nil {
				t.Fatal(err)
			}
			var bound workerBindResponse
			response := bind()
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &bound)
			f.record.ID = bound.ConversationID
			if bound.Continuation {
				t.Fatal("ordinary observation took completion ownership")
			}
			originalAttempt := f.attempt
			operator, _ := conversationOperatorToken(t, f.nativeFixture, "continue-operator")
			for _, status := range []conversation.ExecutionStatus{conversation.ExecutionStarting, conversation.ExecutionRunning} {
				if status == conversation.ExecutionRunning {
					requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "turn_id": "ordinary-turn"}), http.StatusAccepted)
				}
				activeContinue := conversation.Command{Key: "continue-active-" + string(status), Kind: conversation.CommandContinue, Expected: conversation.Expected{AttemptID: originalAttempt}}
				failure := requireNativeError(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/commands", operator, activeContinue), http.StatusConflict, "stale_execution")
				if failure.Message != "An attempt is still running" {
					t.Fatalf("active Continue refusal = %q", failure.Message)
				}
				if got := f.load(t).Execution; got.Status != status || got.Owner.AttemptID != originalAttempt || len(f.messages(t)) != 0 {
					t.Fatalf("active Continue changed execution or queued a control: %+v", got)
				}
			}
			requireNativeStatus(t, f.unbind(t, "succeeded"), http.StatusOK)
			f.release(t)
			command := conversation.Command{Key: "continue-lease-lost", Kind: conversation.CommandContinue, Text: "Continue implementation", Expected: conversation.Expected{AttemptID: originalAttempt}}
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/commands", operator, command)
			requireNativeStatus(t, response, http.StatusOK)
			var receipt conversation.Receipt
			decodeHubResponse(t, response, &receipt)
			message := f.messages(t)[receipt.MessageID]
			requireConversationDelivery(t, f, message, command.Key, conversation.DeliverySaved)
			f.claim(t)
			response = bind()
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &bound)
			if !bound.Continuation || len(bound.Pending) != 1 || bound.Pending[0].MessageID != message.ID {
				t.Fatal("saved Continue did not retain its execution owner")
			}
			interruptedAttempt := f.attempt
			if confirmed {
				requireNativeStatus(t, f.controls(t, bound.Cursor, 0), http.StatusOK)
				requireConversationDelivery(t, f, message, command.Key, conversation.DeliverySent)
			}
			f.release(t)
			record := f.load(t)
			if record.Execution.Status != conversation.ExecutionInterrupted || record.Execution.Error != conversationLeaseLostError || record.Execution.Owner.AttemptID != interruptedAttempt {
				t.Fatalf("lease loss settlement = %+v", record.Execution)
			}
			wantDelivery := conversation.DeliveryUnknown
			if confirmed {
				wantDelivery = conversation.DeliveryInterrupted
			}
			requireConversationDelivery(t, f, message, command.Key, wantDelivery)
			if got := f.messages(t)[message.ID].AttemptID; got != interruptedAttempt {
				t.Fatalf("Continue intent lost prior attempt: %s", got)
			}
			f.claim(t)
			response = bind()
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &bound)
			if !bound.Continuation || len(bound.Pending) != 0 {
				t.Fatalf("replacement lost continuation ownership or replayed control: %+v", bound)
			}
			requireConversationDelivery(t, f, message, command.Key, wantDelivery)
			replacementAttempt := f.attempt
			f.release(t)
			record = f.load(t)
			if record.Execution.Status != conversation.ExecutionInterrupted || record.Execution.Error != conversationLeaseLostError || record.Execution.Owner.AttemptID != replacementAttempt {
				t.Fatalf("second lease loss settlement = %+v", record.Execution)
			}
			f.claim(t)
			response = bind()
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &bound)
			if !bound.Continuation || len(bound.Pending) != 0 {
				t.Fatalf("second replacement lost continuation ownership or replayed control: %+v", bound)
			}
			requireConversationDelivery(t, f, message, command.Key, wantDelivery)
			if got := f.messages(t)[message.ID].AttemptID; got != interruptedAttempt {
				t.Fatalf("replacement rewrote Continue's original attempt: %s", got)
			}
			requireNativeStatus(t, f.unbind(t, "succeeded"), http.StatusOK)
			f.release(t)
			for range 2 {
				f.claim(t)
				response = bind()
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &bound)
				if bound.Continuation || len(bound.Pending) != 0 {
					t.Fatalf("completed Continue reclaimed ordinary completion: %+v", bound)
				}
				f.release(t)
			}
		})
	}
	t.Run("ordinary issue", func(t *testing.T) {
		f := newConversationWorkerFixture(t)
		if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "DELETE FROM conversations WHERE id = $1", f.record.ID); err != nil {
			t.Fatal(err)
		}
		body := f.bindBody()
		body["fencing_token"] = f.lease.FencingToken + 1
		requireConversationErrorCode(t, f.bind(t, body), http.StatusConflict, "stale_execution")
		var count int
		if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM conversations").Scan(&count); err != nil || count != 0 {
			t.Fatalf("stale owner created conversation: %d, %v", count, err)
		}
		response := f.bind(t, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var bound workerBindResponse
		decodeHubResponse(t, response, &bound)
		f.record.ID = bound.ConversationID
		record := f.load(t)
		if record.Origin != conversationOriginWorker {
			t.Fatalf("ordinary binding origin = %q, want worker", record.Origin)
		}
		if bound.Continuation || record.WorkItemID != string(f.issue.WorkItemID) || record.Visibility != conversation.VisibilityShared || record.Execution.Owner.AttemptID != f.attempt || record.Execution.Owner.RunID != f.run || len(bound.Pending) != 0 || len(f.messages(t)) != 0 {
			t.Fatalf("ordinary binding = %#v, pending = %#v", record, bound.Pending)
		}
		requireConversationErrorCode(t, f.bind(t, nil), http.StatusConflict, "stale_execution")
		if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM conversations").Scan(&count); err != nil || count != 1 {
			t.Fatalf("duplicate conversations: %d, %v", count, err)
		}
		before := f.load(t)
		if before.Execution.Owner != record.Execution.Owner || before.EventSeq != record.EventSeq {
			t.Fatal("repeated binding changed the selected execution")
		}
		if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE conversations SET visibility = 'private' WHERE id = $1", f.record.ID); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, f.bind(t, nil), http.StatusNotFound)
		if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM conversations").Scan(&count); err != nil || count != 1 {
			t.Fatalf("private history replaced: %d, %v", count, err)
		}
	})

	t.Run("imported issue with integration actor", func(t *testing.T) {
		f := newConversationWorkerFixture(t)
		if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "DELETE FROM conversations WHERE id = $1", f.record.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE issues SET actor_kind='integration',actor_principal_id='github' WHERE organization_id=$1 AND project_id=$2 AND native_id=$3", f.project.OrganizationID, f.project.ID, f.issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
		response := f.bind(t, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var bound workerBindResponse
		decodeHubResponse(t, response, &bound)
		f.record.ID = bound.ConversationID
		var tokens int
		if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM api_tokens WHERE id = $1", f.load(t).OwnerPrincipalID).Scan(&tokens); err != nil || tokens != 1 {
			t.Fatalf("integration-authored binding owner is not a token: %d, %v", tokens, err)
		}
	})

	f := newConversationWorkerFixture(t)
	first := f.queue(t, conversation.MessageText, "key-1", "First question", nil)
	second := f.queue(t, conversation.MessageContinue, "key-2", "Keep going", nil)

	response := f.bind(t, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var bound workerBindResponse
	decodeHubResponse(t, response, &bound)
	if bound.ConversationID != f.record.ID {
		t.Fatalf("conversation_id = %q, want %q", bound.ConversationID, f.record.ID)
	}
	if len(bound.Pending) != 2 || bound.Pending[0].MessageID != first.ID || bound.Pending[1].MessageID != second.ID {
		t.Fatalf("pending = %#v", bound.Pending)
	}
	if bound.Pending[0].Kind != "message" || bound.Pending[1].Kind != "message" || bound.Pending[0].Key != "key-1" || bound.Pending[1].Text != "Keep going" {
		t.Fatalf("pending envelopes = %#v", bound.Pending)
	}
	if bound.Pending[0].Expected.AttemptID != f.attempt || bound.Cursor != second.Seq {
		t.Fatalf("expected/cursor = %#v/%d", bound.Pending[0].Expected, bound.Cursor)
	}
	record := f.load(t)
	if record.Origin != conversationOriginUser {
		t.Fatalf("linked user conversation origin = %q, want user", record.Origin)
	}
	if record.Execution.Status != conversation.ExecutionStarting || record.Execution.Owner.AttemptID != f.attempt || record.Execution.Owner.LeaseID != string(f.lease.ID) || record.Execution.Owner.FencingToken != int64(f.lease.FencingToken) || record.Execution.Owner.RunID != f.run || record.Execution.Owner.MachineID != string(f.runner.binding.MachineID) {
		t.Fatalf("execution = %#v", record.Execution)
	}
	if !record.Execution.Capabilities.Steer || !record.Execution.Capabilities.Answer {
		t.Fatalf("capabilities = %#v", record.Execution.Capabilities)
	}
	messages := f.messages(t)
	for _, message := range []conversationMessageRecord{first, second} {
		if got := messages[message.ID]; got.Delivery != conversation.DeliverySending || got.AttemptID != f.attempt {
			t.Fatalf("message %s = %#v", message.ID, got)
		}
	}
	if receipt := f.receipt(t, "key-1"); receipt.Status != conversation.DeliverySending {
		t.Fatalf("receipt = %#v", receipt)
	}

	t.Run("same attempt binds once", func(t *testing.T) {
		requireConversationErrorCode(t, f.bind(t, nil), http.StatusConflict, "stale_execution")
	})
	t.Run("stale fencing token", func(t *testing.T) {
		body := f.bindBody()
		body["fencing_token"] = f.lease.FencingToken + 1
		body["attempt_id"] = newNativeID("attempt")
		requireConversationErrorCode(t, f.bind(t, body), http.StatusConflict, "stale_execution")
	})
	t.Run("unknown attempt", func(t *testing.T) {
		body := f.bindBody()
		body["attempt_id"] = newNativeID("attempt")
		requireConversationErrorCode(t, f.bind(t, body), http.StatusConflict, "stale_execution")
	})
	t.Run("missing identity is invalid", func(t *testing.T) {
		body := f.bindBody()
		delete(body, "lease_id")
		requireNativeStatus(t, f.bind(t, body), http.StatusUnprocessableEntity)
	})
	t.Run("issue without conversation", func(t *testing.T) {
		other := f.create(t, "no-conversation")
		body := f.bindBody()
		requireConversationErrorCode(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(other.WorkItemID)+"/conversation/bind", f.worker, body), http.StatusNotFound, "not_found")
	})
	t.Run("foreign project worker", func(t *testing.T) {
		foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign-project")
		token := conversationWorkerRunner(t, foreign).redemption.Credential
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/conversation/bind", token, f.bindBody()), http.StatusForbidden)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, foreign.base+"/work-items/"+string(f.issue.WorkItemID)+"/conversation/bind", token, f.bindBody()), http.StatusNotFound)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, foreign.base+"/conversations/"+f.record.ID+"/turn-events", token, map[string]any{"lease_id": f.lease.ID, "fencing_token": f.lease.FencingToken, "attempt_id": f.attempt, "events": []map[string]any{}}), http.StatusNotFound)
	})
}

func TestConversationWorkerControlsLongPoll(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)

	first := f.queue(t, conversation.MessageText, "steer-1", "Also do this", nil)
	response := f.controls(t, 0, 5)
	requireNativeStatus(t, response, http.StatusOK)
	var page workerControlsResponse
	decodeHubResponse(t, response, &page)
	if len(page.Controls) != 1 || page.Controls[0].MessageID != first.ID || page.Controls[0].Cursor != first.Seq || page.Cursor != first.Seq {
		t.Fatalf("controls = %#v", page)
	}
	if f.messages(t)[first.ID].Delivery != conversation.DeliverySending {
		t.Fatal("handed-off control must be sending")
	}

	t.Run("acknowledge marks sent once", func(t *testing.T) {
		before := f.load(t).EventSeq
		response := f.controls(t, first.Seq, 0)
		requireNativeStatus(t, response, http.StatusOK)
		var page workerControlsResponse
		decodeHubResponse(t, response, &page)
		if len(page.Controls) != 0 || page.Cursor != first.Seq {
			t.Fatalf("controls after ack = %#v", page)
		}
		if got := f.messages(t)[first.ID].Delivery; got != conversation.DeliverySent {
			t.Fatalf("delivery = %s, want sent", got)
		}
		if receipt := f.receipt(t, "steer-1"); receipt.Status != conversation.DeliverySent {
			t.Fatalf("receipt = %#v", receipt)
		}
		types := f.eventTypes(t, before)
		if len(types) != 2 || types[0] != "message.updated" || types[1] != "command.receipt" {
			t.Fatalf("ack events = %v", types)
		}
		after := f.load(t).EventSeq
		requireNativeStatus(t, f.controls(t, first.Seq, 0), http.StatusOK)
		if f.load(t).EventSeq != after {
			t.Fatal("a repeated acknowledgement must not emit events")
		}
	})

	t.Run("waits for a queued control", func(t *testing.T) {
		admin := conversationTestAdmin(t, f.service)
		admin.SetMaxOpenConns(2)
		writer, err := admin.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var revision int64
		if err := writer.QueryRowContext(t.Context(), "SELECT revision FROM conversations WHERE organization_id=$1 AND id=$2 FOR UPDATE", f.project.OrganizationID, f.record.ID).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		done := make(chan *httptest.ResponseRecorder, 1)
		exited := make(chan struct{})
		t.Cleanup(func() {
			_ = writer.Rollback()
			<-exited
		})
		go func() {
			defer close(exited)
			done <- f.controls(t, first.Seq, 10)
		}()
		f.awaitControlSubscribers(t, 1)
		testenv.WaitUntil(t, t.Context(), func() error {
			var waiting bool
			if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND pid<>pg_backend_pid())`).Scan(&waiting); err != nil {
				return err
			}
			if waiting {
				return nil
			}
			return errors.New("control preparation has not reached the writer")
		})
		if _, err := writer.ExecContext(t.Context(), "UPDATE conversations SET revision=revision+1 WHERE organization_id=$1 AND id=$2", f.project.OrganizationID, f.record.ID); err != nil {
			t.Fatal(err)
		}
		if err := writer.Commit(); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		second := f.queue(t, conversation.MessageInterrupt, "stop-1", "", nil)
		select {
		case response := <-done:
			requireNativeStatus(t, response, http.StatusOK)
			var page workerControlsResponse
			decodeHubResponse(t, response, &page)
			if len(page.Controls) != 1 || page.Controls[0].MessageID != second.ID || page.Controls[0].Kind != "interrupt" {
				t.Fatalf("controls = %#v", page)
			}
			if time.Since(started) > 5*time.Second {
				t.Fatal("poll did not return promptly after the control committed")
			}
		case <-testenv.WaitContext(t, t.Context()).Done():
			t.Fatal("long poll did not return after a control was queued")
		}
	})

	t.Run("returns empty after the wait", func(t *testing.T) {
		messages := f.messages(t)
		var cursor int64
		for _, message := range messages {
			cursor = max(cursor, message.Seq)
		}
		started := time.Now()
		response := f.controls(t, cursor, 1)
		requireNativeStatus(t, response, http.StatusOK)
		var page workerControlsResponse
		decodeHubResponse(t, response, &page)
		if len(page.Controls) != 0 || page.Cursor != cursor {
			t.Fatalf("controls = %#v", page)
		}
		if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
			t.Fatalf("poll returned after %s without waiting", elapsed)
		}
	})

	t.Run("wrong attempt is stale", func(t *testing.T) {
		attempt := f.attempt
		f.attempt = newNativeID("attempt")
		defer func() { f.attempt = attempt }()
		requireConversationErrorCode(t, f.controls(t, 0, 0), http.StatusConflict, "stale_execution")
	})

	t.Run("shutdown drains a waiting poll", func(t *testing.T) {
		var cursor int64
		for _, message := range f.messages(t) {
			cursor = max(cursor, message.Seq)
		}
		pollResult := make(chan *httptest.ResponseRecorder, 1)
		go func() { pollResult <- f.controls(t, cursor, 30) }()

		waitContext := testenv.WaitContext(t, t.Context())
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			f.chat.broker.mu.Lock()
			waiting := len(f.chat.broker.subscribers[f.record.ID]) > 0
			f.chat.broker.mu.Unlock()
			if waiting {
				break
			}
			select {
			case <-ticker.C:
			case response := <-pollResult:
				t.Fatalf("poll returned before shutdown: %s", response.Body.String())
			case <-waitContext.Done():
				t.Fatal("poll did not subscribe")
			}
		}
		shutdownContext := testenv.WaitContext(t, context.Background())
		if err := f.service.Shutdown(shutdownContext); err != nil {
			t.Fatalf("Shutdown() with waiting poll: %v", err)
		}
		select {
		case response := <-pollResult:
			requireNativeStatus(t, response, http.StatusOK)
			var result workerControlsResponse
			decodeHubResponse(t, response, &result)
			if len(result.Controls) != 0 || result.Cursor != cursor {
				t.Fatalf("shutdown poll = %+v", result)
			}
		case <-shutdownContext.Done():
			t.Fatal("Shutdown() left the control poll waiting")
		}
	})
}

func TestConversationWorkerTurnEvents(t *testing.T) {
	t.Parallel()
	for _, metadataVersion := range []int{0, 1} {
		t.Run("active legacy tool history metadata="+strconv.Itoa(metadataVersion), func(t *testing.T) {
			f := newConversationWorkerFixture(t)
			requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
			requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-legacy", "turn_id": "turn-legacy"}), http.StatusAccepted)
			f.writeTenant(t, func(w tenantstore.Writer) error {
				for range 101 {
					_, err := w.CreateMessage(t.Context(), conversationstore.Message{OrganizationID: f.project.OrganizationID, ConversationID: f.record.ID, ID: conversation.NewMessageID(), Role: conversation.RoleSystem, Kind: conversation.MessageTool, Text: "historical tool output", Data: json.RawMessage(`{"tool":"example","item_id":"item","status":"completed"}`), Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}, Revision: 1, CreatedAt: f.now, UpdatedAt: f.now})
					if err != nil {
						return err
					}
				}
				return nil
			})
			if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE conversation_messages SET query_metadata_version=$1 WHERE organization_id=$2 AND conversation_id=$3", metadataVersion, f.project.OrganizationID, f.record.ID); err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, f.turnEvents(t,
				map[string]any{"type": "delta", "provider_item_id": "assistant-legacy", "text": "finished"},
				map[string]any{"type": "turn_completed", "turn_id": "turn-legacy", "status": "completed"},
			), http.StatusAccepted)
			if record := f.load(t); record.Execution.Owner.TurnID != "" {
				t.Fatalf("completed turn remained active: %s", record.Execution.Owner.TurnID)
			}
			requireNativeStatus(t, f.unbind(t, "succeeded"), http.StatusOK)
			if record := f.load(t); record.Execution.Status != conversation.ExecutionCompleted {
				t.Fatalf("execution did not complete: %s", record.Execution.Status)
			}
			var untouched int
			if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND kind='tool' AND content_attachment_id='' AND revision=1", f.record.ID).Scan(&untouched); err != nil || untouched != 101 {
				t.Fatalf("unrelated legacy messages changed: untouched=%d err=%v", untouched, err)
			}
			var inline int
			if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND role='assistant' AND (content_attachment_id='' OR text<>'' OR data_json<>'{}' OR actor_json<>'{}')", f.record.ID).Scan(&inline); err != nil || inline != 0 {
				t.Fatalf("new assistant payload stayed inline: count=%d err=%v", inline, err)
			}
		})
	}
	t.Run("immutable content and compact replay", func(t *testing.T) {
		objects := &conversationContentFixture{}
		f := newConversationWorkerFixture(t, objects)
		requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
		f.advance(123 * time.Nanosecond)
		var logs bytes.Buffer
		f.service.config.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		user := f.queue(t, conversation.MessageText, "inline-user", "user text", nil)
		first := strings.Repeat("streamed text ", 300)
		second := "final suffix"
		events := []map[string]any{{"type": "turn_started", "thread_id": "thread-content", "turn_id": "turn-content"}, {"type": "delta", "provider_item_id": "assistant-content", "text": first}, {"type": "delta", "provider_item_id": "assistant-content", "text": second}}
		toolText := strings.Repeat("tool result ", 250)
		for i := range 50 {
			events = append(events, map[string]any{"type": "item", "kind": "tool", "summary": toolText, "provider_item_id": fmt.Sprintf("tool-%d", i), "data": map[string]string{"output": toolText}})
		}
		events = append(events, map[string]any{"type": "turn_completed", "turn_id": "turn-content", "status": "completed"})
		objects.setFail(true)
		before := f.load(t)
		failed := f.turnEvents(t, events...)
		if failed.Code < 500 {
			t.Fatalf("publication failure accepted: %d", failed.Code)
		}
		after := f.load(t)
		if after.EventSeq != before.EventSeq || len(f.messages(t)) != 1 {
			t.Fatal("failed upload committed conversation content")
		}
		objects.setFail(false)
		response := f.turnEvents(t, events...)
		if response.Code != http.StatusAccepted {
			t.Fatalf("turn response=%d logs=%s", response.Code, logs.String())
		}
		messages, err := f.chat.coordinator.(*conversationTurnCoordinator).coordinatorMessages(t.Context(), conversationstore.MessageFilter{ConversationID: f.record.ID, Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 52 || messages[0].ID != user.ID || messages[0].Text != "user text" || messages[1].Text != first+second {
			t.Fatalf("message content/order changed: %d messages, %v", len(messages), err)
		}
		for _, message := range messages[1:] {
			if message.ContentID == "" {
				t.Fatalf("message %s kept its content inline", message.ID)
			}
		}
		var largeEvents, bodyCopies, inlineTools int
		if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_events WHERE conversation_id=$1 AND octet_length(body_json::text)+octet_length(conversation_id)+octet_length(type)+octet_length(created_at::text)+8>2048`, f.record.ID).Scan(&largeEvents); err != nil {
			t.Fatal(err)
		}
		rows, err := conversationTestAdmin(t, f.service).QueryContext(t.Context(), "SELECT body_json FROM conversation_events WHERE organization_id=$1 AND conversation_id=$2", f.project.OrganizationID, f.record.ID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(raw, &body); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if body["text"] != nil || body["data"] != nil {
				bodyCopies++
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}

		if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND role='system' AND (length(text)>256 OR data_json<>'{}' OR content_attachment_id='')`, f.record.ID).Scan(&inlineTools); err != nil {
			t.Fatal(err)
		}
		if largeEvents != 0 || bodyCopies != 0 || inlineTools != 0 {
			t.Fatalf("large events=%d body copies=%d inline tools=%d", largeEvents, bodyCopies, inlineTools)
		}
		replay, err := f.replay(t)
		if err != nil {
			t.Fatal(err)
		}
		var deltas string
		var accepted []string
		for _, event := range replay {
			switch event.Type {
			case conversation.EventMessageAccepted:
				var message conversationMessageResource
				if err := json.Unmarshal(event.Data, &message); err != nil {
					t.Fatal(err)
				}
				accepted = append(accepted, message.ID)
				if message.Role == conversation.RoleAssistant && message.Text != "" {
					t.Fatal("accepted frame included future delta text")
				}
				if message.Role == conversation.RoleSystem && message.Text != toolText {
					t.Fatal("tool replay content changed")
				}
			case conversation.EventMessageDelta:
				var delta struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(event.Data, &delta); err != nil {
					t.Fatal(err)
				}
				deltas += delta.Text
			}
		}
		if deltas != first+second || len(accepted) != len(messages) {
			t.Fatalf("replay changed: %d accepted, %d delta bytes", len(accepted), len(deltas))
		}
		for index, id := range accepted {
			if id != messages[index].ID {
				t.Fatal("replay reordered messages")
			}
		}
		snapshot := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+f.record.ID+"/messages?limit=500", f.token, nil)
		requireNativeStatus(t, snapshot, http.StatusOK)
		var page conversationMessagesPage
		decodeHubResponse(t, snapshot, &page)
		if len(page.Messages) != len(messages) || page.Messages[1].Text != first+second {
			t.Fatal("API did not hydrate external content")
		}
		operatorCtx := workspaceOperatorContext(t, f.service, f.token, string(f.project.OrganizationID), "content-read")
		var operatorMessages []conversationMessageResource
		var beforeCursor int64
		for {
			args := map[string]any{"project_id": f.project.ID, "conversation_id": f.record.ID, "limit": 5, "before": beforeCursor}
			result, err := workspaceOperatorCall(t, f.service, operatorCtx, "list_conversation_messages", args)
			if err != nil {
				t.Fatal(err)
			}
			var operatorPage conversationMessagesPage
			if err := json.Unmarshal(workspaceOperatorData(t, result), &operatorPage); err != nil {
				t.Fatal(err)
			}
			operatorMessages = append(operatorPage.Messages, operatorMessages...)
			if operatorPage.NextCursor == nil {
				break
			}
			next, err := strconv.ParseInt(*operatorPage.NextCursor, 10, 64)
			if err != nil || next <= 0 || (beforeCursor > 0 && next >= beforeCursor) {
				t.Fatalf("MCP cursor failed to advance: %q", *operatorPage.NextCursor)
			}
			beforeCursor = next
		}
		if len(operatorMessages) != len(messages) {
			t.Fatalf("MCP messages=%d want=%d", len(operatorMessages), len(messages))
		}
		for index, message := range operatorMessages {
			if message.ID != messages[index].ID || message.Text != messages[index].Text {
				t.Fatal("MCP message content or order changed")
			}
		}

		output := httptest.NewRecorder()
		stream := &conversationStream{response: echo.NewResponse(output, echo.New())}
		head := f.load(t).EventSeq
		cursor, err := f.service.replayPGConversationEvents(t.Context(), nativeAttemptListingScope(t, f.nativeFixture), stream, f.record.ID, 0, head)
		if err != nil || cursor != head || !strings.Contains(output.Body.String(), toolText) || !strings.Contains(output.Body.String(), second) {
			t.Fatalf("SSE replay lost content: cursor=%d head=%d error=%v", cursor, head, err)
		}

	})

	t.Run("message page spans typed reader batches", checkPGWorkerMessagePage)
	t.Run("completion ignores unrelated assistant history", checkPGWorkerCompletionHistory)

	t.Run("registered runner middleware and fencing", func(t *testing.T) {
		f := newConversationWorkerFixture(t)
		f.release(t)
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
			if err := f.service.config.ConversationDispatch.RequestDispatch(t.Context(), writer, f.project.ID, string(f.issue.WorkItemID), leasestore.Fence{}, f.now); err != nil {
				return err
			}
			item, err := writer.WorkItem(t.Context(), f.project.ID, f.issue.WorkItemID)
			f.issue.Revision = tracker.Revision(item.Revision)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Collaborate, runnerauth.Events)
		r.enroll(t)
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
			return writer.UpdateRunnerCapacityConfig(t.Context(), r.binding.RunnerID, runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: f.now})
		}); err != nil {
			t.Fatal(err)
		}
		publishCapacity(t, f.nativeFixture, r, capacityReport(f.now))
		f.worker = r.redemption.Credential
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, providerClaim(r, f.issue, "conversation-registered"))
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &f.lease)
		f.attempt, f.run = newNativeID("attempt"), newNativeID("run")
		event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("start")}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test-model"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy}}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
		requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
		foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign-registered")
		foreignRunner := prepareRunner(t, foreign, runnerauth.Read, runnerauth.Collaborate, runnerauth.Events)
		foreignRunner.enroll(t)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, foreign.base+"/conversations/"+f.record.ID+"/turn-events", f.worker, map[string]any{"lease_id": f.lease.ID, "fencing_token": f.lease.FencingToken, "attempt_id": f.attempt, "events": []map[string]any{}}), http.StatusForbidden)
		other := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Collaborate, runnerauth.Events)
		other.enroll(t)
		unpermitted := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Collaborate)
		unpermitted.enroll(t)
		for _, test := range []struct {
			name, base, token string
			change            func(map[string]any)
			status            int
		}{
			{name: "wrong project", base: foreign.base, token: foreignRunner.redemption.Credential, status: http.StatusNotFound},
			{name: "wrong runner", token: other.redemption.Credential, status: http.StatusNotFound},
			{name: "missing events operation", token: unpermitted.redemption.Credential, status: http.StatusForbidden},
			{name: "wrong lease", change: func(body map[string]any) { body["lease_id"] = newNativeID("lease") }, status: http.StatusConflict},
			{name: "wrong fence", change: func(body map[string]any) { body["fencing_token"] = f.lease.FencingToken + 1 }, status: http.StatusConflict},
			{name: "wrong attempt", change: func(body map[string]any) { body["attempt_id"] = newNativeID("attempt") }, status: http.StatusConflict},
		} {
			t.Run(test.name, func(t *testing.T) {
				body := f.identity()
				body["events"] = []map[string]any{{"type": "turn_started", "thread_id": "thread-registered", "turn_id": "turn-registered"}}
				if test.change != nil {
					test.change(body)
				}
				base, token := test.base, test.token
				if base == "" {
					base = f.base
				}
				if token == "" {
					token = f.worker
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/conversations/"+f.record.ID+"/turn-events", token, body), test.status)
				if f.load(t).Execution.Status != conversation.ExecutionStarting {
					t.Fatal("refused event changed execution")
				}
			})
		}
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-registered", "turn_id": "turn-registered"}, map[string]any{"type": "delta", "provider_item_id": "item-registered", "text": "Authentic runner transcript"}), http.StatusAccepted)
		owner := f.load(t).Execution.Owner
		if owner.RunnerID != r.binding.RunnerID || owner.AttemptID != f.attempt || owner.LeaseID != string(f.lease.ID) || owner.FencingToken != int64(f.lease.FencingToken) {
			t.Fatalf("event lost registered owner: %+v", owner)
		}
		found := false
		for _, message := range f.messages(t) {
			if message.Text == "Authentic runner transcript" {
				found = message.Actor.Kind == conversation.ActorRunner && message.AttemptID == f.attempt && message.TurnID == "turn-registered"
			}
		}
		if !found {
			t.Fatal("registered event did not persist authentic transcript")
		}
		oldExpiry := f.lease.ExpiresAt
		renewConversationFixtureLease(t, f, 1800)
		f.advance(11 * time.Minute)
		if !f.now.After(oldExpiry) {
			t.Fatal("fixture did not cross the original registered lease expiry")
		}
		publishCapacity(t, f.nativeFixture, r, capacityReport(f.now))
		before := f.load(t)
		if err := f.chat.reconcileExecutions(t.Context()); err != nil {
			t.Fatal(err)
		}
		if after := f.load(t); after.Execution != before.Execution || after.EventSeq != before.EventSeq {
			t.Fatalf("expiry lost the registered worker: %+v", after.Execution)
		}
		requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-registered", "text": " during expiry"}), http.StatusAccepted)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/renew", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, TTLSeconds: 90}), http.StatusOK)
		f.restart(t)
		if execution := f.load(t).Execution; execution.Owner != owner || execution.Status != conversation.ExecutionRunning {
			t.Fatalf("restart lost the registered worker: %+v", execution)
		}
		requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-registered", "text": " after restart"}), http.StatusAccepted)
		if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at=$1 WHERE organization_id=$2 AND id=(SELECT token_id FROM runner_identities WHERE organization_id=$2 AND runner_id=$3)", f.now, f.project.OrganizationID, r.binding.RunnerID); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, f.controls(t, 0, 0), http.StatusUnauthorized)
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-registered", "text": " revoked"}), http.StatusUnauthorized)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/renew", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, TTLSeconds: 90}), http.StatusUnauthorized)
		if err := f.chat.reconcileExecutions(t.Context()); err != nil {
			t.Fatal(err)
		}
		if execution := f.load(t).Execution; execution.Status != conversation.ExecutionInterrupted || execution.Owner != owner {
			t.Fatalf("revoked runner retained conversation authority: %+v", execution)
		}

	})
	f := newConversationWorkerFixture(t)
	prompt := f.queue(t, conversation.MessageText, "prompt-1", "Fix the bug", nil)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)

	t.Run("turn started delivers the prompt and persists the thread", func(t *testing.T) {
		response := f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"})
		requireNativeStatus(t, response, http.StatusAccepted)
		var accepted struct {
			EventSeq int64 `json:"event_seq"`
		}
		decodeHubResponse(t, response, &accepted)
		record := f.load(t)
		if accepted.EventSeq != record.EventSeq || accepted.EventSeq == 0 {
			t.Fatalf("event_seq = %d, record %d", accepted.EventSeq, record.EventSeq)
		}
		if record.Execution.Status != conversation.ExecutionRunning || record.Execution.Owner.ThreadID != "thread-1" || record.Execution.Owner.TurnID != "turn-1" || record.ProviderThreadID != "thread-1" {
			t.Fatalf("execution = %#v thread %q", record.Execution, record.ProviderThreadID)
		}
		if got := f.messages(t)[prompt.ID]; got.Delivery != conversation.DeliveryDelivered || got.TurnID != "turn-1" {
			t.Fatalf("prompt = %#v", got)
		}
		if receipt := f.receipt(t, "prompt-1"); receipt.Status != conversation.DeliveryDelivered {
			t.Fatalf("receipt = %#v", receipt)
		}
	})

	t.Run("deltas assemble one assistant message per item", func(t *testing.T) {
		requireNativeStatus(t, f.turnEvents(t,
			map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "Hello"},
			map[string]any{"type": "delta", "provider_item_id": "item-1", "text": ", world"},
			map[string]any{"type": "item", "provider_item_id": "tool-1", "kind": "tool", "summary": "ran go test"},
			map[string]any{"type": "delta", "provider_item_id": "item-2", "text": "Second"},
		), http.StatusAccepted)
		var assistant, tool, second int
		for _, message := range f.messages(t) {
			switch {
			case message.Role == conversation.RoleAssistant && message.ProviderItemID == "item-1":
				assistant++
				if message.Text != "Hello, world" || message.Delivery != conversation.DeliveryResponding || message.AttemptID != f.attempt || message.TurnID != "turn-1" || message.Actor.Kind != conversation.ActorRunner {
					t.Fatalf("assistant = %#v", message)
				}
			case message.Role == conversation.RoleSystem && message.Kind == conversation.MessageTool:
				tool++
				if message.Text != "ran go test" || message.Delivery != conversation.DeliveryCompleted {
					t.Fatalf("tool = %#v", message)
				}
			case message.ProviderItemID == "item-2":
				second++
			}
		}
		if assistant != 1 || tool != 1 || second != 1 {
			t.Fatalf("assistant %d tool %d second %d", assistant, tool, second)
		}
	})

	var questionID string
	t.Run("question opened waits for input", func(t *testing.T) {
		before := f.load(t).EventSeq
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "choice", "header": "Pick", "question": "Which?", "options": []map[string]string{{"label": "Blue"}}, "free_text": false}}}), http.StatusAccepted)
		questions := f.questions(t)
		if len(questions) != 1 {
			t.Fatalf("questions = %#v", questions)
		}
		question := questions[0]
		questionID = question.ID
		expected := f.service.config.now().Add(time.Hour)
		if question.Status != conversation.QuestionPending || question.RequestID != "req-1" || question.Owner.AttemptID != f.attempt || question.Owner.TurnID != "turn-1" || question.ExpiresAt == nil || !question.ExpiresAt.Equal(expected) || len(question.Prompts) != 1 {
			t.Fatalf("question = %#v", question)
		}
		if status := f.messages(t)[question.MessageID]; status.Kind != conversation.MessageStatus || status.Role != conversation.RoleAssistant {
			t.Fatalf("question message = %#v", status)
		}
		if record := f.load(t); record.Execution.Status != conversation.ExecutionWaitingInput {
			t.Fatalf("execution = %#v", record.Execution)
		}
		var opened bool
		for _, kind := range f.eventTypes(t, before) {
			opened = opened || kind == "question.opened"
		}
		if !opened {
			t.Fatal("question.opened event missing")
		}
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "choice", "question": "Which?"}}}), http.StatusAccepted)
		if len(f.questions(t)) != 1 {
			t.Fatal("a repeated question_opened must not duplicate the question")
		}
	})

	t.Run("control results update receipts", func(t *testing.T) {
		steer := f.queue(t, conversation.MessageText, "steer-ok", "More", nil)
		rejected := f.queue(t, conversation.MessageText, "steer-bad", "Bad", nil)
		lost := f.queue(t, conversation.MessageInterrupt, "stop-lost", "", nil)
		answer := f.queue(t, conversation.MessageAnswer, "answer-1", "", map[string]any{"question_id": questionID, "answers": map[string][]string{"choice": {"Blue"}}})
		response := f.controls(t, 0, 0)
		requireNativeStatus(t, response, http.StatusOK)
		var page workerControlsResponse
		decodeHubResponse(t, response, &page)
		if len(page.Controls) != 4 || page.Controls[3].Kind != "answer" || page.Controls[3].QuestionID != questionID || page.Controls[3].RequestID != "req-1" || page.Controls[3].Answers["choice"][0] != "Blue" || page.Controls[3].Expected.TurnID != "turn-1" {
			t.Fatalf("controls = %#v", page.Controls)
		}
		requireNativeStatus(t, f.controls(t, page.Cursor, 0), http.StatusOK)
		requireNativeStatus(t, f.turnEvents(t,
			map[string]any{"type": "control_result", "key": "steer-ok", "status": "delivered"},
			map[string]any{"type": "control_result", "key": "steer-bad", "status": "rejected", "error": "turn ended"},
			map[string]any{"type": "control_result", "key": "stop-lost", "status": "unknown", "error": "connection lost"},
			map[string]any{"type": "control_result", "key": "answer-1", "status": "delivered"},
		), http.StatusAccepted)
		messages := f.messages(t)
		for _, test := range []struct {
			message conversationMessageRecord
			key     string
			want    conversation.Delivery
			code    string
		}{
			{steer, "steer-ok", conversation.DeliveryDelivered, ""},
			{rejected, "steer-bad", conversation.DeliveryRejected, "rejected"},
			{lost, "stop-lost", conversation.DeliveryUnknown, "unknown"},
			{answer, "answer-1", conversation.DeliverySent, ""},
		} {
			if got := messages[test.message.ID].Delivery; got != test.want {
				t.Fatalf("%s delivery = %s, want %s", test.key, got, test.want)
			}
			receipt := f.receipt(t, test.key)
			if receipt.Status != test.want || (test.code == "" && receipt.Error != nil) || (test.code != "" && (receipt.Error == nil || receipt.Error.Code != test.code)) {
				t.Fatalf("%s receipt = %#v", test.key, receipt)
			}
		}
		if question := f.questions(t)[0]; question.Status != conversation.QuestionAnswered {
			t.Fatalf("question = %#v", question)
		}
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "control_result", "key": "missing", "status": "delivered"}), http.StatusUnprocessableEntity)
	})

	t.Run("turn completed settles messages and questions", func(t *testing.T) {
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "question_opened", "request_id": "req-2", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "again", "question": "Again?"}}}), http.StatusAccepted)
		requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "turn_completed", "turn_id": "turn-other", "status": "completed"}), http.StatusConflict, "stale_execution")
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_completed", "turn_id": "turn-1", "status": "completed"}), http.StatusAccepted)
		for _, message := range f.messages(t) {
			if message.Role == conversation.RoleAssistant && message.Kind == conversation.MessageText && message.Delivery != conversation.DeliveryCompleted {
				t.Fatalf("assistant message not completed: %#v", message)
			}
		}
		for _, question := range f.questions(t) {
			if question.RequestID == "req-2" && question.Status != conversation.QuestionExpired {
				t.Fatalf("question = %#v", question)
			}
			if question.RequestID == "req-1" && question.Status != conversation.QuestionAnswered {
				t.Fatalf("answered question changed: %#v", question)
			}
		}
		record := f.load(t)
		if record.Execution.Status != conversation.ExecutionRunning || record.Execution.Owner.TurnID != "" {
			t.Fatalf("execution = %#v", record.Execution)
		}
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "execution_status", "status": "bogus"}), http.StatusUnprocessableEntity)
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "unknown_event"}), http.StatusUnprocessableEntity)
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "execution_status", "status": "failed", "error": "provider crashed"}), http.StatusAccepted)
		if record := f.load(t); record.Execution.Status != conversation.ExecutionFailed || record.Execution.Error != "provider crashed" {
			t.Fatalf("execution = %#v", record.Execution)
		}
		requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-3", "text": "late"}), http.StatusConflict, "stale_execution")
	})
}

func TestConversationWorkerUnbind(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		outcome string
		want    conversation.ExecutionStatus
	}{
		{"succeeded", conversation.ExecutionCompleted},
		{"failed", conversation.ExecutionFailed},
		{"cancelled", conversation.ExecutionInterrupted},
		{"interrupted", conversation.ExecutionInterrupted},
	} {
		t.Run(test.outcome, func(t *testing.T) {
			t.Parallel()
			f := newConversationWorkerFixture(t)
			requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
			requireNativeStatus(t, f.turnEvents(t,
				map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
				map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "partial"},
				map[string]any{"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "q", "question": "?"}}},
			), http.StatusAccepted)
			text := f.queue(t, conversation.MessageText, "text-1", "Later", nil)
			stop := f.queue(t, conversation.MessageInterrupt, "stop-1", "", nil)
			requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
			// Accepted after the hand-off, so it was never given to the worker.
			waiting := f.queue(t, conversation.MessageText, "text-2", "And this", nil)
			requireNativeStatus(t, f.unbind(t, test.outcome), http.StatusOK)
			record := f.load(t)
			if record.Execution.Status != test.want || record.Execution.Owner.AttemptID != f.attempt {
				t.Fatalf("execution = %#v", record.Execution)
			}
			messages := f.messages(t)
			// A control the worker was handed is never replayed, whatever
			// its kind; one that was never handed out stays queued for the
			// next attempt.
			if messages[text.ID].Delivery != conversation.DeliveryUnknown || messages[stop.ID].Delivery != conversation.DeliveryUnknown {
				t.Fatalf("text = %s stop = %s", messages[text.ID].Delivery, messages[stop.ID].Delivery)
			}
			if messages[waiting.ID].Delivery != conversation.DeliveryQueued {
				t.Fatalf("waiting = %s, want queued", messages[waiting.ID].Delivery)
			}
			if f.receipt(t, "stop-1").Status != conversation.DeliveryUnknown || f.receipt(t, "text-1").Status != conversation.DeliveryUnknown || f.receipt(t, "text-2").Status != conversation.DeliveryQueued {
				t.Fatal("receipts must follow the control deliveries")
			}
			for _, message := range messages {
				if message.ProviderItemID == "item-1" && message.Delivery != conversation.Delivery(test.want) {
					t.Fatalf("assistant = %#v", message)
				}
			}
			if question := f.questions(t)[0]; question.Status != conversation.QuestionExpired {
				t.Fatalf("question = %#v", question)
			}
			requireNativeStatus(t, f.unbind(t, test.outcome), http.StatusOK)
			requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "late"}), http.StatusConflict, "stale_execution")
			if test.outcome == "succeeded" {
				g := newConversationWorkerFixture(t)
				requireNativeStatus(t, g.bind(t, nil), http.StatusOK)
				requireNativeStatus(t, g.turnEvents(t,
					map[string]any{"type": "turn_started", "thread_id": "thread-finished", "turn_id": "turn-finished"},
					map[string]any{"type": "turn_completed", "turn_id": "turn-finished", "status": "completed"},
				), http.StatusAccepted)
				requireNativeStatus(t, g.unbind(t, "succeeded"), http.StatusOK)
				g.release(t)
				requireNativeStatus(t, g.unbind(t, "succeeded"), http.StatusOK)
				if record := g.load(t); record.Execution.Status != conversation.ExecutionCompleted || record.Execution.Error != "" {
					t.Fatalf("execution after late unbind = %#v", record.Execution)
				}
				requireNativeStatus(t, g.unbind(t, "failed"), http.StatusOK)
				if record := g.load(t); record.Execution.Status != conversation.ExecutionCompleted {
					t.Fatalf("a settled execution must not change again: %#v", record.Execution)
				}
			}
		})
	}
}

func TestConversationReconcileLeaseLoss(t *testing.T) {
	t.Run("expired and released owners cannot execute", func(t *testing.T) {
		for _, released := range []bool{false, true} {
			t.Run(strconv.FormatBool(released), func(t *testing.T) {
				f := newConversationWorkerFixture(t)
				requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
				if released {
					f.release(t)
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
						attempt, err := reader.Attempt(t.Context(), f.attempt)
						if err == nil && (attempt.Status != "cancelled" || attempt.LeaseID != string(f.lease.ID) || attempt.FencingToken != int64(f.lease.FencingToken)) {
							t.Fatalf("release did not durably cancel the exact attempt: %+v", attempt)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if record := f.load(t); record.Execution.Status != conversation.ExecutionInterrupted || record.Execution.Error != "runner lease lost" {
						t.Fatalf("execution after release = %#v", record.Execution)
					}
				} else {
					f.advance(11 * time.Minute)
				}
				before := f.load(t)
				requireConversationErrorCode(t, f.controls(t, 0, 0), http.StatusConflict, "stale_execution")
				requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "expired-item", "text": "refused"}), http.StatusConflict, "stale_execution")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/renew", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, TTLSeconds: 90}), http.StatusConflict)
				if after := f.load(t); after.Execution != before.Execution || after.EventSeq != before.EventSeq || after.Revision != before.Revision {
					t.Fatalf("refused owner changed execution: %+v", after.Execution)
				}
				if released {
					requireNativeStatus(t, f.unbind(t, "succeeded"), http.StatusOK)
					requireNativeStatus(t, f.unbind(t, "failed"), http.StatusOK)
					if after := f.load(t); after.Execution != before.Execution || after.EventSeq != before.EventSeq || after.Revision != before.Revision {
						t.Fatalf("cancelled owner cleanup changed terminal result: %+v", after.Execution)
					}
				}
			})
		}
	})
	t.Parallel()
	for _, test := range []struct {
		name         string
		organization bool
		expired      bool
		mode         budget.ExhaustionMode
	}{
		{name: "expired organization hard stop", organization: true, expired: true, mode: budget.HardStop},
		{name: "expired project hard stop", expired: true, mode: budget.HardStop},
		{name: "current project hard stop", mode: budget.HardStop},
		{name: "expired project drain", expired: true, mode: budget.DrainIssue},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newConversationWorkerFixture(t)
			requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
			requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-budget", "turn_id": "turn-budget"}), http.StatusAccepted)
			before := f.load(t)
			f.monthlyBudget(t, test.organization, budget.MonthlyPolicy{Enabled: true, Mode: test.mode, RunnerMicros: new(int64)})
			if test.expired {
				oldExpiry := f.lease.ExpiresAt
				if test.mode == budget.DrainIssue {
					renewConversationFixtureLease(t, f, 1800)
				}
				f.advance(11 * time.Minute)
				if !f.now.After(oldExpiry) {
					t.Fatal("fixture did not cross the original lease expiry")
				}
			}
			renewalStatus, controlStatus, eventStatus := http.StatusOK, http.StatusOK, http.StatusAccepted
			if test.mode == budget.HardStop {
				renewalStatus, controlStatus, eventStatus = http.StatusConflict, http.StatusConflict, http.StatusConflict
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/renew", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, TTLSeconds: 90}), renewalStatus)
			requireNativeStatus(t, f.controls(t, 0, 0), controlStatus)
			requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-budget", "text": "continued"}), eventStatus)
			if test.mode == budget.HardStop {
				if after := f.load(t); after.Execution != before.Execution || after.EventSeq != before.EventSeq {
					t.Fatalf("refused requests changed the execution: %+v", after.Execution)
				}
			}
			if err := f.chat.reconcileExecutions(t.Context()); err != nil {
				t.Fatal(err)
			}
			after := f.load(t)
			if test.mode == budget.HardStop {
				if after.Execution.Status != conversation.ExecutionInterrupted || after.Execution.Error != conversationLeaseLostError {
					t.Fatalf("hard stop retained execution: %+v", after.Execution)
				}
			} else if after.Execution != before.Execution {
				t.Fatalf("drain interrupted admitted execution: %+v", after.Execution)
			}
		})
	}
	t.Run("same authority renews after reconciliation", func(t *testing.T) {
		f := newConversationWorkerFixture(t)
		requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
		requireNativeStatus(t, f.turnEvents(t,
			map[string]any{"type": "turn_started", "thread_id": "thread-renew", "turn_id": "turn-renew"},
			map[string]any{"type": "question_opened", "request_id": "req-renew", "thread_id": "thread-renew", "turn_id": "turn-renew", "prompts": []map[string]any{{"id": "q", "question": "Continue?"}}},
		), http.StatusAccepted)
		handed := f.queue(t, conversation.MessageText, "renew-control", "Keep working", nil)
		requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
		before := f.load(t)
		observation, cancel := context.WithCancel(t.Context())
		cancel()
		if err := f.chat.reconcileExecutions(observation); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled observation = %v", err)
		}
		if after := f.load(t); after.Execution != before.Execution || after.EventSeq != before.EventSeq {
			t.Fatalf("failed observation changed the live execution: %+v", after.Execution)
		}
		oldExpiry := f.lease.ExpiresAt
		renewConversationFixtureLease(t, f, 1800)
		f.advance(11 * time.Minute)
		if !f.now.After(oldExpiry) {
			t.Fatal("fixture did not cross the original lease expiry")
		}
		if err := f.chat.reconcileExecutions(t.Context()); err != nil {
			t.Fatal(err)
		}
		if after := f.load(t); after.Execution != before.Execution || after.EventSeq != before.EventSeq {
			t.Fatalf("expiry reconciliation changed the live execution: %+v", after.Execution)
		}
		if question := f.questions(t)[0]; question.Status != conversation.QuestionPending {
			t.Fatalf("expiry reconciliation settled the question: %+v", question)
		}
		requireConversationDelivery(t, f, handed, "renew-control", conversation.DeliverySending)
		requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-renew", "text": "during expiry"}), http.StatusAccepted)
		var databaseNow time.Time
		if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT clock_timestamp()").Scan(&databaseNow); err != nil {
			t.Fatal(err)
		}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/renew", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, TTLSeconds: 90})
		requireNativeStatus(t, response, http.StatusOK)
		var renewed tracker.NativeLease
		decodeHubResponse(t, response, &renewed)
		if renewed.ID != f.lease.ID || renewed.FencingToken != f.lease.FencingToken || !renewed.ExpiresAt.After(databaseNow) {
			t.Fatalf("renewed authority = %+v", renewed)
		}
		requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-renew", "text": "continued"}), http.StatusAccepted)
		if after := f.load(t); after.Execution.Owner != before.Execution.Owner || after.Execution.Status != before.Execution.Status {
			t.Fatalf("renewal changed the live execution: %+v", after.Execution)
		}
		var attempts int
		if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE i.organization_id=$1 AND i.native_id=$2", f.project.OrganizationID, f.issue.WorkItemID).Scan(&attempts); err != nil || attempts != 1 {
			t.Fatalf("renewal created another attempt: count=%d err=%v", attempts, err)
		}
		oldLease, oldAttempt, oldRun := f.lease, f.attempt, f.run
		f.advance(91 * time.Second)
		f.claim(t)
		requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-new", "turn_id": "turn-new"}), http.StatusAccepted)
		replacement := f.load(t)
		f.lease, f.attempt, f.run = oldLease, oldAttempt, oldRun
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/renew", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, TTLSeconds: 90}), http.StatusConflict)
		requireConversationErrorCode(t, f.controls(t, 0, 0), http.StatusConflict, "stale_execution")
		requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-old", "text": "late"}), http.StatusConflict, "stale_execution")
		if after := f.load(t); after.Execution != replacement.Execution || after.EventSeq != replacement.EventSeq {
			t.Fatalf("superseded owner changed the replacement: %+v", after.Execution)
		}
	})
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t,
		map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
		map[string]any{"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "q", "question": "?"}}},
	), http.StatusAccepted)
	text := f.queue(t, conversation.MessageText, "text-1", "Still waiting", nil)
	stop := f.queue(t, conversation.MessageInterrupt, "stop-1", "", nil)
	requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
	// Accepted after the hand-off, so it was never given to the worker.
	waiting := f.queue(t, conversation.MessageText, "text-2", "And this one", nil)

	t.Run("released lease is reconciled", func(t *testing.T) {
		if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE leases SET released_at = $1 WHERE lease_id = $2", formatHubTime(f.now), f.lease.ID); err != nil {
			t.Fatal(err)
		}
		f.advance(11 * time.Minute)
		requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "late"}), http.StatusConflict, "stale_execution")
		requireConversationErrorCode(t, f.controls(t, 0, 0), http.StatusConflict, "stale_execution")
		if err := f.chat.reconcileExecutions(t.Context()); err != nil {
			t.Fatalf("reconcileExecutions() error = %v", err)
		}
		record := f.load(t)
		if record.Execution.Status != conversation.ExecutionInterrupted || record.Execution.Error != "runner lease lost" {
			t.Fatalf("execution = %#v", record.Execution)
		}
		messages := f.messages(t)
		// Everything the worker was handed becomes unknown; only what was
		// never handed out stays queued.
		if messages[text.ID].Delivery != conversation.DeliveryUnknown || messages[stop.ID].Delivery != conversation.DeliveryUnknown {
			t.Fatalf("text = %s stop = %s", messages[text.ID].Delivery, messages[stop.ID].Delivery)
		}
		if messages[waiting.ID].Delivery != conversation.DeliveryQueued {
			t.Fatalf("waiting = %s, want queued", messages[waiting.ID].Delivery)
		}
		if question := f.questions(t)[0]; question.Status != conversation.QuestionExpired {
			t.Fatalf("question = %#v", question)
		}
		if err := f.chat.reconcileExecutions(t.Context()); err != nil {
			t.Fatalf("second reconcileExecutions() error = %v", err)
		}
	})

	t.Run("release hook interrupts and a new attempt rebinds", func(t *testing.T) {
		f.claim(t)
		body := f.bindBody()
		body["thread_id"] = "thread-1"
		response := f.bind(t, body)
		requireNativeStatus(t, response, http.StatusOK)
		var bound workerBindResponse
		decodeHubResponse(t, response, &bound)
		if bound.Resume.ThreadID != "thread-1" || len(bound.Pending) != 1 || bound.Pending[0].MessageID != waiting.ID {
			t.Fatalf("bound = %#v", bound)
		}
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-2"}), http.StatusAccepted)
		f.release(t)
		record := f.load(t)
		if record.Execution.Status != conversation.ExecutionInterrupted {
			t.Fatalf("execution after release = %#v", record.Execution)
		}
		requireConversationErrorCode(t, f.controls(t, 0, 0), http.StatusConflict, "stale_execution")
		requireConversationErrorCode(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-released", "text": "late"}), http.StatusConflict, "stale_execution")
	})

	t.Run("restart preserves a current owner then fresh bind", func(t *testing.T) {
		f.claim(t)
		requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-3"}), http.StatusAccepted)
		if err := f.chat.start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if record := f.load(t); record.Execution.Status != conversation.ExecutionRunning {
			t.Fatalf("execution after restart = %#v", record.Execution)
		}
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "item-9", "text": "resumed"}), http.StatusAccepted)
		f.release(t)
		f.claim(t)
		requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
		if record := f.load(t); record.Execution.Status != conversation.ExecutionStarting || record.Execution.Owner.AttemptID != f.attempt {
			t.Fatalf("execution after rebind = %#v", record.Execution)
		}
	})
}

func TestConversationControlRouterWakeAndStop(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	subscription, cancel := f.chat.broker.subscribe(f.record.ID)
	defer cancel()
	f.chat.controls.Wake(f.record.ID)
	select {
	case <-subscription.wake:
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("Wake must notify broker subscribers")
	}
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	done := make(chan int, 1)
	go func() { done <- f.controls(t, 0, 10).Code }()
	f.awaitControlSubscribers(t, 2)
	f.chat.controls.Stop()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("poll after stop = %d", code)
		}
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("Stop must end in-flight polls")
	}
}

// TestConversationWorkerQuestionSnapshot covers what a reloading tab must
// see: a prompt that allows more than one selection keeps that flag through
// storage, and a question the current attempt already settled stays in the
// snapshot so its card renders locked instead of disappearing.
func TestConversationWorkerQuestionSnapshot(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t, &conversationContentFixture{})
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"}), http.StatusAccepted)
	requireNativeStatus(t, f.turnEvents(t, map[string]any{
		"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1",
		"prompts": []map[string]any{
			{"id": "surfaces", "header": "Surfaces", "question": "Which surfaces?", "options": []map[string]string{{"label": "Checkout lock"}, {"label": "Orphan lease"}}, "multiple": true},
			{"id": "notes", "header": "Notes", "question": strings.Repeat("Details? ", 400), "free_text": true},
		},
	}), http.StatusAccepted)

	questions := f.questions(t)
	if len(questions) != 1 || len(questions[0].Prompts) != 2 {
		t.Fatalf("questions = %#v", questions)
	}
	if questions[0].Prompts[1].Question != strings.Repeat("Details? ", 400) {
		t.Fatal("pending prompt was truncated")
	}
	replay, err := f.replay(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range replay {
		if event.Type == conversation.EventQuestionOpened {
			var question struct {
				Prompts []conversation.Prompt `json:"questions"`
			}
			if err := json.Unmarshal(event.Data, &question); err != nil {
				t.Fatal(err)
			}
			if len(question.Prompts) != 2 || question.Prompts[1].Question != questions[0].Prompts[1].Question {
				t.Fatal("pending question replay lost content")
			}
		}
	}
	t.Run("multiple round trips through storage", func(t *testing.T) {
		if !questions[0].Prompts[0].Multiple || questions[0].Prompts[1].Multiple {
			t.Fatalf("prompts = %#v", questions[0].Prompts)
		}
		encoded, err := json.Marshal(questions[0].Question)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Questions []map[string]any `json:"questions"`
		}
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatal(err)
		}
		if wire.Questions[0]["multiple"] != true {
			t.Errorf("a multiple-choice prompt must report multiple: %#v", wire.Questions[0])
		}
		if _, present := wire.Questions[1]["multiple"]; present {
			t.Errorf("a single-choice prompt must omit multiple: %#v", wire.Questions[1])
		}
	})

	t.Run("a settled question stays in the snapshot", func(t *testing.T) {
		question := questions[0]
		answer := strings.Repeat("Detailed answer ", 200)
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/commands", f.token, conversation.Command{Key: "answer-1", Kind: conversation.CommandAnswer, QuestionID: question.ID, Answers: map[string][]string{"surfaces": {"Checkout lock"}, "notes": {answer}}, Expected: conversation.Expected{AttemptID: f.attempt}})
		requireNativeStatus(t, response, http.StatusOK)
		if err := f.service.expireNativeRecords(t.Context(), f.now); err != nil {
			t.Fatal(err)
		}
		controls := f.controls(t, 0, 0)
		requireNativeStatus(t, controls, http.StatusOK)
		var page conversationControlsResponse
		decodeHubResponse(t, controls, &page)
		found := false
		for _, control := range page.Controls {
			if control.Key == "answer-1" {
				found = true
				if control.Answers["notes"][0] != answer {
					t.Fatal("external answer was not delivered in full")
				}
			}
		}
		if !found {
			t.Fatal("undelivered answer disappeared")
		}

		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "control_result", "key": "answer-1", "status": "delivered"}), http.StatusAccepted)
		f.readTenant(t, func(r tenantstore.Reader) error {
			pending, err := r.Questions(t.Context(), f.record.ID, "", conversationSnapshotQuestions)
			if err != nil {
				return err
			}
			for _, q := range pending {
				if q.Status == conversation.QuestionPending || q.Status == conversation.QuestionSending || q.Status == conversation.QuestionSent {
					t.Fatalf("pending question=%#v", q)
				}
			}
			snapshot, err := r.Questions(t.Context(), f.record.ID, f.attempt, conversationSnapshotQuestions)
			if err != nil {
				return err
			}
			if len(snapshot) != 1 || snapshot[0].ID != question.ID || snapshot[0].Status != conversation.QuestionAnswered {
				t.Fatalf("snapshot questions=%#v", snapshot)
			}
			other, err := r.Questions(t.Context(), f.record.ID, "att_other", conversationSnapshotQuestions)
			if err != nil {
				return err
			}
			if len(other) != 0 {
				t.Fatalf("settled questions of another attempt=%#v, want none", other)
			}
			return nil
		})
	})
}

// TestConversationBindReportsResume covers what a bind hands the runner: a
// runner that names the thread resumes it and the execution says so, while a
// runner that cannot reach the thread is handed a transcript, the execution
// says transcript, and the history records the hand-off with the number of
// messages the transcript carried.
func TestConversationBindReportsResume(t *testing.T) {
	t.Parallel()
	for _, protected := range []bool{false, true} {
		t.Run(fmt.Sprintf("protected=%t", protected), func(t *testing.T) {
			f := newConversationWorkerFixture(t, &conversationContentFixture{})
			body := f.bindBody()
			body["thread_id"] = "thread-1"
			requireNativeStatus(t, f.bind(t, body), http.StatusOK)
			if record := f.load(t); record.Execution.Resume != conversation.ResumeThread {
				t.Fatalf("resume = %q, want thread", record.Execution.Resume)
			}
			for _, message := range f.messages(t) {
				if message.Kind == conversation.MessageStatus {
					t.Fatalf("a resumable thread must not append a transcript notice: %#v", message)
				}
			}
			requireNativeStatus(t, f.turnEvents(t,
				map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
				map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "the renewal returns early"},
				map[string]any{"type": "turn_completed", "turn_id": "turn-1", "status": "completed"},
			), http.StatusAccepted)
			requireNativeStatus(t, f.unbind(t, "succeeded"), http.StatusOK)
			history := len(f.messages(t))

			// The hub forgets which runner produced the thread, which is what a
			// hub-side coordinator turn looks like to the next runner.
			if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE conversations SET provider_thread_runner_id = 'rnr_elsewhere' WHERE id = $1", f.record.ID); err != nil {
				t.Fatal(err)
			}
			if protected {
				if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE conversations SET read_audience='protected-screen-audience' WHERE id=$1", f.record.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.release(t)
			f.claim(t)
			response := f.bind(t, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var bound workerBindResponse
			decodeHubResponse(t, response, &bound)
			if bound.Resume.ThreadID != "" {
				t.Fatalf("resume.thread_id = %q, want empty for a runner that cannot reach the thread", bound.Resume.ThreadID)
			}
			record := f.load(t)
			if protected {
				if record.Execution.Resume != "" || len(bound.Resume.Transcript) != 0 {
					t.Fatalf("worker received protected history: %+v", bound.Resume)
				}
				requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+record.ID+"/messages", f.worker, nil)
				requireNativeStatus(t, response, http.StatusForbidden)
				response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+record.ID+"/messages", f.token, nil)
				requireNativeStatus(t, response, http.StatusNotFound)
				return
			}
			if record.Execution.Resume != conversation.ResumeTranscript {
				t.Fatalf("resume = %q, want transcript", record.Execution.Resume)
			}
			requireConversationTranscriptNotice(t, f)
			var notice string
			for _, message := range f.messages(t) {
				if message.Kind == conversation.MessageStatus && message.Role == conversation.RoleSystem {
					notice = message.Text
				}
			}
			if want := conversationTranscriptNotice(history); notice != want {
				t.Fatalf("notice = %q, want %q", notice, want)
			}
		})
	}
}

// TestConversationInterruptedTurnStaysInterrupted covers a turn that ended
// without a provider error: the execution records interrupted or failed
// rather than reporting running, the turn is cleared, and the unbind outcome
// still wins afterwards.
func TestConversationInterruptedTurnStaysInterrupted(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		status string
		want   conversation.ExecutionStatus
	}{
		{"interrupted", conversation.ExecutionInterrupted},
		{"failed", conversation.ExecutionFailed},
		{"completed", conversation.ExecutionRunning},
	} {
		t.Run(test.status, func(t *testing.T) {
			t.Parallel()
			f := newConversationWorkerFixture(t)
			requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
			requireNativeStatus(t, f.turnEvents(t,
				map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
				map[string]any{"type": "delta", "provider_item_id": "item-1", "text": "partial"},
			), http.StatusAccepted)
			requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_completed", "turn_id": "turn-1", "status": test.status}), http.StatusAccepted)
			record := f.load(t)
			if record.Execution.Status != test.want {
				t.Fatalf("execution = %q, want %q", record.Execution.Status, test.want)
			}
			if record.Execution.Owner.TurnID != "" {
				t.Fatalf("turn = %q, want cleared", record.Execution.Owner.TurnID)
			}
			if record.Execution.Owner.AttemptID != f.attempt {
				t.Fatalf("attempt = %q, want the bound %q", record.Execution.Owner.AttemptID, f.attempt)
			}
			if record.Execution.Settled {
				t.Fatal("a completed turn is not an unbind; the worker's outcome must still win")
			}

			// The unbind outcome wins over whatever the turn reported.
			requireNativeStatus(t, f.unbind(t, "succeeded"), http.StatusOK)
			after := f.load(t)
			if after.Execution.Status != conversation.ExecutionCompleted || !after.Execution.Settled {
				t.Fatalf("execution after unbind = %#v, want completed and settled", after.Execution)
			}
			// The turn's own output keeps the turn's verdict; the unbind
			// outcome is about the execution, not about settled messages.
			for _, message := range f.messages(t) {
				if message.ProviderItemID == "item-1" && string(message.Delivery) != test.status {
					t.Fatalf("assistant message = %q, want %q", message.Delivery, test.status)
				}
			}

			// A repeated unbind is idempotent.
			before := f.load(t)
			requireNativeStatus(t, f.unbind(t, "failed"), http.StatusOK)
			if again := f.load(t); again.EventSeq != before.EventSeq || again.Execution.Status != before.Execution.Status {
				t.Fatalf("a repeated unbind changed state: %#v -> %#v", before.Execution, again.Execution)
			}
		})
	}
}

// TestConversationTurnEventBatchIsIdempotent covers a worker that retries a
// batch after a timeout: the repeat applies nothing, answers with the event
// sequence the first attempt produced, and never appends the same delta or
// item twice.
func TestConversationTurnEventBatchIsIdempotent(t *testing.T) {
	t.Parallel()
	objects := &conversationContentFixture{}
	f := newConversationWorkerFixture(t, objects)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	batch := func(key string, events ...map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		body := f.identity()
		body["events"] = events
		body["batch_key"] = key
		return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/turn-events", f.worker, body)
	}
	events := []map[string]any{
		{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
		{"type": "delta", "provider_item_id": "item-1", "text": "the renewal returns early"},
	}

	first := batch("batch-1", events...)
	requireNativeStatus(t, first, http.StatusAccepted)
	var applied struct {
		EventSeq int64 `json:"event_seq"`
	}
	decodeHubResponse(t, first, &applied)
	before := f.load(t)
	messages := f.messages(t)

	replay := batch("batch-1", events...)
	requireNativeStatus(t, replay, http.StatusAccepted)
	var again struct {
		EventSeq int64 `json:"event_seq"`
	}
	decodeHubResponse(t, replay, &again)
	if again.EventSeq != applied.EventSeq {
		t.Fatalf("replayed event_seq = %d, want %d", again.EventSeq, applied.EventSeq)
	}
	after := f.load(t)
	if after.EventSeq != before.EventSeq || after.Revision != before.Revision {
		t.Fatalf("a replayed batch changed state: seq %d->%d revision %d->%d", before.EventSeq, after.EventSeq, before.Revision, after.Revision)
	}
	repeated := f.messages(t)
	if len(repeated) != len(messages) {
		t.Fatalf("messages = %d, want %d (a replayed batch must not duplicate history)", len(repeated), len(messages))
	}
	for id, message := range repeated {
		if message.Text != messages[id].Text {
			t.Fatalf("message %s = %q, want %q (a replayed delta must not be appended twice)", id, message.Text, messages[id].Text)
		}
	}

	t.Run("a different key still applies", func(t *testing.T) {
		requireNativeStatus(t, batch("batch-2", map[string]any{"type": "delta", "provider_item_id": "item-1", "text": " under load"}), http.StatusAccepted)
		for _, message := range f.messages(t) {
			if message.ProviderItemID == "item-1" && message.Text != "the renewal returns early under load" {
				t.Fatalf("text = %q, want both deltas applied once", message.Text)
			}
		}
	})

	t.Run("a replay survives the next turn", func(t *testing.T) {
		requireNativeStatus(t, f.turnEvents(t,
			map[string]any{"type": "turn_completed", "turn_id": "turn-1", "status": "completed"},
			map[string]any{"type": "turn_started", "turn_id": "turn-2"},
		), http.StatusAccepted)
		before := f.load(t)
		objects.mu.Lock()
		publications := objects.publications
		objects.mu.Unlock()
		replay := batch("batch-1", events...)
		requireNativeStatus(t, replay, http.StatusAccepted)
		var again conversationTurnEventsResponse
		decodeHubResponse(t, replay, &again)
		after := f.load(t)
		if again.EventSeq != applied.EventSeq || after.EventSeq != before.EventSeq || after.Execution.Owner.TurnID != "turn-2" {
			t.Fatalf("replay after next turn changed state: response=%+v before=%+v after=%+v", again, before.Execution, after.Execution)
		}
		objects.mu.Lock()
		defer objects.mu.Unlock()
		if objects.publications != publications {
			t.Fatalf("replayed batch published %d objects; want 0", objects.publications-publications)
		}
	})

	t.Run("retired receipts reject late batches", func(t *testing.T) {
		requireNativeStatus(t, f.unbind(t, "succeeded"), http.StatusOK)
		f.release(t)
		if err := f.service.expireNativeRecords(t.Context(), f.now); err != nil {
			t.Fatal(err)
		}
		var remaining int
		if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_turn_batches WHERE attempt_id=$1", f.attempt).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 0 {
			t.Fatalf("terminal attempt retained %d receipts", remaining)
		}
		before := f.load(t)
		response := batch("batch-1", events...)
		if response.Code < 400 || response.Code >= 500 {
			t.Fatalf("late batch response=%d %s", response.Code, response.Body.String())
		}
		after := f.load(t)
		if after.EventSeq != before.EventSeq || after.Revision != before.Revision {
			t.Fatal("retired batch executed again")
		}
	})
	t.Run("a batch key that is too long is refused", func(t *testing.T) {
		requireConversationErrorCode(t, batch(strings.Repeat("k", 129), events...), http.StatusUnprocessableEntity, "invalid_request")
	})
}

// A turn that reported how it ended but whose worker then lost the lease
// without unbinding still settles: the controls it was handed resolve and the
// execution keeps the status the turn reported.
func TestConversationReconcileSettlesTerminalTurnAfterLeaseLoss(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"}), http.StatusAccepted)
	handed := f.queue(t, conversation.MessageText, "text-1", "Handed over", nil)
	requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "execution_status", "status": "failed", "error": "provider crashed"}), http.StatusAccepted)
	if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE leases SET released_at = $1 WHERE lease_id = $2", formatHubTime(f.now), f.lease.ID); err != nil {
		t.Fatal(err)
	}
	if record := f.load(t); record.Execution.Status != conversation.ExecutionFailed || record.Execution.Settled {
		t.Fatalf("execution before reconciliation = %#v", record.Execution)
	}

	if err := f.chat.reconcileExecutions(t.Context()); err != nil {
		t.Fatalf("reconcileExecutions() error = %v", err)
	}
	record := f.load(t)
	if record.Execution.Status != conversation.ExecutionFailed || record.Execution.Error != "provider crashed" || !record.Execution.Settled {
		t.Fatalf("execution after lease loss = %#v", record.Execution)
	}
	if delivery := f.messages(t)[handed.ID].Delivery; delivery != conversation.DeliveryUnknown {
		t.Fatalf("handed control = %s, want unknown", delivery)
	}
	seq := record.EventSeq
	if err := f.chat.reconcileExecutions(t.Context()); err != nil {
		t.Fatalf("second reconcileExecutions() error = %v", err)
	}
	if again := f.load(t); again.EventSeq != seq {
		t.Fatalf("a settled execution was reconciled again: event_seq %d -> %d", seq, again.EventSeq)
	}
}

// An answer to a question past its expires_at is refused even while the turn
// that asked it is still live.
func TestConversationAnswerRefusesExpiredQuestion(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
	requireNativeStatus(t, f.turnEvents(t,
		map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"},
		map[string]any{"type": "question_opened", "request_id": "req-1", "thread_id": "thread-1", "turn_id": "turn-1", "prompts": []map[string]any{{"id": "q", "question": "?", "free_text": true}}},
	), http.StatusAccepted)
	question := f.questions(t)[0]
	answer := func(key string) *httptest.ResponseRecorder {
		return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/commands", f.token, conversation.Command{
			Key: key, Kind: conversation.CommandAnswer, QuestionID: question.ID,
			Answers: map[string][]string{"q": {"yes"}}, Expected: conversation.Expected{AttemptID: f.attempt},
		})
	}
	setExpiry := func(at time.Time) {
		if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE conversation_questions SET expires_at = $1 WHERE id = $2", conversationTime(at), question.ID); err != nil {
			t.Fatal(err)
		}
	}
	setExpiry(f.service.config.now().Add(-time.Second))
	requireConversationErrorCode(t, answer("late"), http.StatusConflict, "stale_execution")
	setExpiry(f.service.config.now().Add(time.Hour))
	requireNativeStatus(t, answer("in-time"), http.StatusOK)
}

func TestConversationControlRejectionDoesNotPublish(t *testing.T) {
	for _, test := range []struct {
		name       string
		invalidate func(*testing.T, *conversationWorkerFixture)
	}{
		{name: "unknown lease", invalidate: func(t *testing.T, f *conversationWorkerFixture) { f.lease.ID = "lease_unknown" }},
		{name: "stale fencing token", invalidate: func(t *testing.T, f *conversationWorkerFixture) { f.lease.FencingToken++ }},
		{name: "wrong runner", invalidate: func(t *testing.T, f *conversationWorkerFixture) {
			f.worker = conversationWorkerRunner(t, f.nativeFixture).redemption.Credential
		}},
	} {
		for _, acknowledge := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/acknowledge=%t", test.name, acknowledge), func(t *testing.T) {
				objects := &conversationContentFixture{}
				f := newConversationWorkerFixture(t, objects)
				requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/commands", f.token,
					conversation.Command{Key: "queued-message", Kind: conversation.CommandMessage, Text: "pending control"})
				requireNativeStatus(t, response, http.StatusOK)
				var receipt conversation.Receipt
				decodeHubResponse(t, response, &receipt)
				var after int64
				if acknowledge {
					requireNativeStatus(t, f.controls(t, 0, 0), http.StatusOK)
					if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT seq FROM conversation_messages WHERE id=$1", receipt.MessageID).Scan(&after); err != nil {
						t.Fatal(err)
					}
				}
				test.invalidate(t, f)
				objects.mu.Lock()
				before := objects.publications
				objects.mu.Unlock()
				response = f.controls(t, after, 0)
				if response.Code < 400 || response.Code >= 500 {
					t.Fatalf("invalid worker status=%d; want rejection: %s", response.Code, response.Body)
				}
				objects.mu.Lock()
				afterPublications := objects.publications
				objects.mu.Unlock()
				if afterPublications != before {
					t.Fatalf("rejected control poll published %d objects; want 0", afterPublications-before)
				}
			})
		}
	}
}
