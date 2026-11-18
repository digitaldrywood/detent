package hubserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type conversationInterleaveRepository struct {
	tenantstore.Repository
	mu          sync.Mutex
	beforeWrite func()
}

func (r *conversationInterleaveRepository) arm(hook func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beforeWrite = hook
}

func (r *conversationInterleaveRepository) ReadTenantSnapshot(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.(tenantstore.SnapshotRepository).ReadTenantSnapshot(ctx, organization, workload, read)
}

func (r *conversationInterleaveRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	r.mu.Lock()
	hook := r.beforeWrite
	r.beforeWrite = nil
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	return r.Repository.WriteTenant(ctx, organization, workload, write)
}

func newConversationWorkerProject(t *testing.T, first *conversationWorkerFixture) *conversationWorkerFixture {
	t.Helper()
	fixture := &conversationWorkerFixture{now: first.now, chat: first.chat}
	f := newNativeFixture(t, first.service, first.project.OrganizationID, "Second example project")
	fixture.nativeFixture = f
	policy := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", policy)
	fixture.policy = policy.ID
	fixture.issue = f.create(t, "conversation-work")
	fixture.runner = conversationWorkerRunner(t, f)
	fixture.worker = fixture.runner.redemption.Credential
	token := conversationFixturePrincipal(t, f, "Second example conversation owner")
	fixture.principal = token.ID
	linked := fixture.now
	record := conversationRecord{
		ID: conversation.NewConversationID(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID,
		OwnerPrincipalID: token.ID, OwnerSubject: "second-owner@example.test", Title: "linked", Visibility: conversation.VisibilityShared, Status: conversation.StatusActive,
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

func TestConversationTurnEventsSurviveInterleavedWrites(t *testing.T) {
	t.Parallel()
	batchTypes := []string{string(conversation.EventMessageUpdated), string(conversation.EventCommandReceipt), string(conversation.EventMessageAccepted), string(conversation.EventMessageDelta)}
	post := func(t *testing.T, f *conversationWorkerFixture, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/turn-events", f.worker, body)
	}
	tests := []struct {
		name          string
		intervene     func(t *testing.T, f *conversationWorkerFixture, cursor int64, body map[string]any)
		fencingOffset tracker.FencingToken
		appended      []string
		stale         bool
	}{
		{
			name: "own control poll",
			intervene: func(t *testing.T, f *conversationWorkerFixture, cursor int64, _ map[string]any) {
				requireNativeStatus(t, f.controls(t, cursor, 0), http.StatusOK)
			},
			appended: batchTypes,
		},
		{
			name: "user command",
			intervene: func(t *testing.T, f *conversationWorkerFixture, _ int64, _ map[string]any) {
				f.queue(t, conversation.MessageText, "steer-1", "Also update the docs", nil)
			},
			appended: batchTypes,
		},
		{
			name: "duplicate delivery",
			intervene: func(t *testing.T, f *conversationWorkerFixture, _ int64, body map[string]any) {
				requireNativeStatus(t, post(t, f, body), http.StatusAccepted)
			},
		},
		{
			name: "released lease",
			intervene: func(t *testing.T, f *conversationWorkerFixture, _ int64, _ map[string]any) {
				f.release(t)
			},
			stale: true,
		},
		{
			name: "replaced owner",
			intervene: func(t *testing.T, f *conversationWorkerFixture, _ int64, _ map[string]any) {
				f.release(t)
				f.claim(t)
				requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
			},
			stale: true,
		},
		{name: "wrong fencing token", fencingOffset: 1, stale: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			first := newConversationWorkerFixture(t)
			second := newConversationWorkerProject(t, first)
			repository := &conversationInterleaveRepository{Repository: first.service.config.TenantRepository}
			projectRecordDatabases.Store(repository, conversationTestAdmin(t, first.service))
			t.Cleanup(func() { projectRecordDatabases.Delete(repository) })
			first.service.config.TenantRepository = repository
			for _, f := range []*conversationWorkerFixture{first, second} {
				requireNativeStatus(t, f.bind(t, nil), http.StatusOK)
				requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "thread-1", "turn_id": "turn-1"}), http.StatusAccepted)
				interrupt := f.queue(t, conversation.MessageInterrupt, "interrupt-1", "", nil)
				polled := f.controls(t, 0, 0)
				requireNativeStatus(t, polled, http.StatusOK)
				var page workerControlsResponse
				decodeHubResponse(t, polled, &page)
				if len(page.Controls) != 1 || page.Controls[0].Key != "interrupt-1" {
					t.Fatalf("controls = %#v, want the interrupt", page.Controls)
				}
				body := f.identity()
				body["batch_key"] = "batch-1"
				body["events"] = []map[string]any{
					{"type": "control_result", "key": "interrupt-1", "status": "delivered"},
					{"type": "delta", "provider_item_id": "item-1", "text": "partial answer"},
				}
				if test.fencingOffset != 0 {
					body["fencing_token"] = f.lease.FencingToken + test.fencingOffset
				}
				before := f.load(t)
				middle := before
				var interleaved []string
				if test.intervene != nil {
					repository.arm(func() {
						test.intervene(t, f, page.Cursor, body)
						interleaved = f.eventTypes(t, before.EventSeq)
						middle = f.load(t)
					})
				}
				response := post(t, f, body)
				repository.arm(nil)
				if test.intervene != nil && middle.EventSeq == before.EventSeq {
					t.Fatalf("the intervening write appended no event: seq=%d", middle.EventSeq)
				}
				after := f.load(t)
				answers := []conversationMessageRecord{}
				for _, message := range f.messages(t) {
					if message.ProviderItemID == "item-1" {
						answers = append(answers, message)
					}
				}
				if test.stale {
					requireConversationErrorCode(t, response, http.StatusConflict, "stale_execution")
					if after.EventSeq != middle.EventSeq || len(answers) != 0 {
						t.Fatalf("a stale batch was applied: seq %d->%d assistant messages=%d", middle.EventSeq, after.EventSeq, len(answers))
					}
					if got := f.messages(t)[interrupt.ID].Delivery; got == conversation.DeliveryDelivered {
						t.Fatal("a stale batch delivered the interrupt")
					}
					continue
				}
				requireNativeStatus(t, response, http.StatusAccepted)
				var accepted conversationTurnEventsResponse
				decodeHubResponse(t, response, &accepted)
				want := append(slices.Clone(interleaved), test.appended...)
				if got := f.eventTypes(t, before.EventSeq); !slices.Equal(got, want) {
					t.Fatalf("events after the snapshot = %v, want %v", got, want)
				}
				if accepted.EventSeq != after.EventSeq || after.EventSeq != before.EventSeq+int64(len(want)) {
					t.Fatalf("event_seq = %d, stored %d, want %d", accepted.EventSeq, after.EventSeq, before.EventSeq+int64(len(want)))
				}
				requireConversationDelivery(t, f, interrupt, "interrupt-1", conversation.DeliveryDelivered)
				if len(answers) != 1 || answers[0].Text != "partial answer" {
					t.Fatalf("assistant messages = %#v, want one carrying the delta once", answers)
				}
				replayed := post(t, f, body)
				requireNativeStatus(t, replayed, http.StatusAccepted)
				var again conversationTurnEventsResponse
				decodeHubResponse(t, replayed, &again)
				if settled := f.load(t); again.EventSeq != accepted.EventSeq || settled.EventSeq != after.EventSeq || settled.Revision != after.Revision {
					t.Fatalf("a redelivered batch changed state: event_seq %d->%d stored seq %d->%d revision %d->%d", accepted.EventSeq, again.EventSeq, after.EventSeq, settled.EventSeq, after.Revision, settled.Revision)
				}
				for _, message := range f.messages(t) {
					if message.ProviderItemID == "item-1" && (message.ID != answers[0].ID || message.Text != "partial answer") {
						t.Fatalf("a redelivered batch changed the assistant message: %#v", message)
					}
				}
			}
		})
	}
}
