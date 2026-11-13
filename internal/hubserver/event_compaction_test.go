package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestEventCompactionWorkHistory(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		archived, leased bool
		closedAgo        time.Duration
		extraActivity    int
	}{
		{name: "archived", archived: true}, {name: "open"}, {name: "archived with live lease", archived: true, leased: true},
		{name: "closed for three days", closedAgo: 73 * time.Hour}, {name: "closed recently", closedAgo: time.Hour},
		{name: "unpublished closed activity stays hot", closedAgo: 73 * time.Hour, extraActivity: 500},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			first := newTerminalAttemptFixture(t)
			for index, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
				t.Run(string(organization), func(t *testing.T) {
					a := first
					if index > 0 {
						a = newTerminalAttemptFixture(t, first)
					}
					path := strings.TrimSuffix(a.path, "/events")
					requireNativeStatus(t, performHubAPIRequest(t, a.service, http.MethodPost, path+"/comments", a.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment"}, Body: "Human decision retained"}), http.StatusOK)
					current := a.issue
					response := performHubAPIRequest(t, a.service, http.MethodGet, path, a.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &current)
					transition := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "begin"}, ExpectedRevision: current.Revision, State: "In Progress", Reason: "user_requested"}
					requireNativeStatus(t, performHubAPIRequest(t, a.service, http.MethodPost, path+"/workflow", a.token, transition), http.StatusOK)
					a.snapshots(t, 1)
					for range 21 {
						a.advance(time.Second)
						event := a.start
						event.Type = "run.observed"
						event.Data.Runtime = a.runtime("implementation")
						a.post(t, event)
					}
					for range test.extraActivity {
						event := a.start
						event.Type = "run.checkpointed"
						event.Data.Handoff = nativeTestCheckpoint()
						event.Data.Runtime = a.runtime("implementation")
						a.post(t, event)
					}
					if !test.leased {
						a.finish(t)
					}
					if test.closedAgo > 0 {
						response = performHubAPIRequest(t, a.service, http.MethodGet, path, a.token, nil)
						requireNativeStatus(t, response, http.StatusOK)
						decodeHubResponse(t, response, &current)
						requireNativeStatus(t, performHubAPIRequest(t, a.service, http.MethodPost, path+"/workflow", a.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "done"}, ExpectedRevision: current.Revision, State: "Done", Reason: "user_requested"}), http.StatusOK)
						a.advance(test.closedAgo)
					}
					if test.archived {
						response = performHubAPIRequest(t, a.service, http.MethodGet, path, a.token, nil)
						requireNativeStatus(t, response, http.StatusOK)
						decodeHubResponse(t, response, &current)
						requireNativeStatus(t, performHubAPIRequest(t, a.service, http.MethodPost, path+"/archive", a.token, archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: "archive"}, ExpectedRevision: current.Revision}), http.StatusOK)
					}
					beforeRows, beforePayloads := a.canonicalEvents(t)
					response = performHubAPIRequest(t, a.service, http.MethodGet, path+"/history?limit=1", a.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var initial tracker.Page[tracker.CollaborationEvent]
					decodeHubResponse(t, response, &initial)
					if initial.NextCursor == "" {
						t.Fatal("missing initial cursor")
					}
					admin := conversationTestAdmin(t, a.service)
					var beforeSchema int64
					if err := admin.QueryRowContext(t.Context(), `SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&beforeSchema); err != nil {
						t.Fatal(err)
					}
					for range 3 {
						if err := a.service.maintainNativeRetention(t.Context(), a.service.config.now()); err != nil {
							t.Fatal(err)
						}
						rows, payloads := a.canonicalEvents(t)
						if !reflect.DeepEqual(rows, beforeRows) || !reflect.DeepEqual(payloads, beforePayloads) {
							t.Fatal("maintenance changed immutable work history or canonical payload bytes")
						}
						events := a.history(t)
						counts := map[string]int{}
						for _, event := range events {
							counts[event.Type]++
						}
						if counts["run.checkpointed"] != 1+test.extraActivity || counts["run.observed"] != 22 {
							t.Fatalf("history lost checkpoints/observations: %v", counts)
						}
						for _, kind := range []string{"issue.created", "comment.created", "run.started", "run.checkpointed", "workflow.transitioned", "scheduler.decision"} {
							if counts[kind] == 0 {
								t.Fatalf("lost %s: %v", kind, counts)
							}
						}
						if !test.leased && counts["run.finished"] != 1 {
							t.Fatalf("history lost terminal answer: %v", counts)
						}
						a.assertSessionSpans(t)
					}
					var afterSchema int64
					if err := admin.QueryRowContext(t.Context(), `SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&afterSchema); err != nil || beforeSchema != afterSchema {
						t.Fatalf("event maintenance changed schema: %d -> %d: %v", beforeSchema, afterSchema, err)
					}
					requireNativeStatus(t, performHubAPIRequest(t, a.service, http.MethodGet, path+"/history?cursor="+url.QueryEscape(initial.NextCursor), a.token, nil), http.StatusOK)
					if test.extraActivity == 0 {
						args, _ := json.Marshal(map[string]any{"project_id": a.project.ID, "reference": a.issue.WorkItemID})
						result, err := (hostedOperatorExecutor{service: a.service}).Execute(changeOperatorContext(t, a.service, a.token, string(a.project.OrganizationID)), operatortool.Call{Name: operatortool.WorkHistory, Arguments: args})
						if err != nil || !strings.Contains(string(result.Content), "run.checkpointed") {
							t.Fatalf("MCP lost history: %s %v", result.Content, err)
						}
					}
					response = performHubAPIRequest(t, a.service, http.MethodGet, path+"/comments", a.token, nil)
					requireNativeStatus(t, response, http.StatusOK)
					if !strings.Contains(response.Body.String(), "Human decision retained") {
						t.Fatal("human comment was lost")
					}
					if _, err := admin.ExecContext(t.Context(), `DELETE FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`, a.scope.organization, a.scope.project, a.issue.WorkItemID); err == nil {
						t.Fatal("ordinary history deletion allowed")
					}
				})
			}
		})
	}
}

func TestEventCompactionConversations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		settled   bool
		age       int
		status    conversation.ExecutionStatus
		compact   bool
		streaming bool
	}{
		{"settled past retention", true, 31, conversation.ExecutionCompleted, true, false},
		{"active", false, 31, conversation.ExecutionCompleted, false, false},
		{"recently settled", true, 29, conversation.ExecutionCompleted, false, false},
		{"retention boundary", true, 30, conversation.ExecutionCompleted, false, false},
		{"running", true, 31, conversation.ExecutionRunning, false, false},
		{"unknown execution", true, 31, conversation.ExecutionUnknown, false, false},
		{"still streaming", false, 0, conversation.ExecutionRunning, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := streamFixture{conversationAPIFixture: newConversationAPIFixture(t, nil)}
			fixture := conversationFixture{nativeFixture: f.nativeFixture, store: f.service.conversations.store, organization: f.project.OrganizationID}
			now := time.Now().UTC().Truncate(time.Microsecond)
			at := now.Add(-time.Duration(test.age) * 24 * time.Hour)
			id := f.create(t, f.token, map[string]any{"title": "preserved conversation"}).Conversation.ID
			record := conversationRecordFromStore(readStreamCompactionConversation(t, f, id))
			record.UpdatedAt = at
			record.Execution.Status = test.status
			if test.settled {
				record.Status = conversation.StatusSettled
				record.SettledAt = &at
			}
			message := conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: "Final answer retained", Delivery: conversation.DeliveryCompleted, CreatedAt: at, UpdatedAt: at, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: "test"}}
			if test.streaming {
				message.Delivery = conversation.DeliveryResponding
			}
			plan := &conversationPublicationPlan{service: f.service.conversations, record: record, now: at}
			if err := preparePGConversationMessage(t.Context(), plan, &message); err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
				if err := registerPreparedConversationObjects(t.Context(), writer, f.project.OrganizationID, plan.prepared, at); err != nil {
					return err
				}
				if _, err := writer.CreateMessage(t.Context(), conversationMessageToStore(f.project.OrganizationID, plan.messages[message.ID])); err != nil {
					return err
				}
				appendEvent := func(kind conversation.EventType, body any) error {
					raw, err := json.Marshal(body)
					if err != nil {
						return err
					}
					_, err = writer.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: f.project.OrganizationID, ConversationID: record.ID, Event: conversation.Event{Type: kind, Data: raw, CreatedAt: at}})
					return err
				}
				for range 600 {
					if err := appendEvent(conversation.EventMessageDelta, conversationDeltaBody(message.ID, 0, "stream fragment")); err != nil {
						return err
					}
				}
				for range 2 {
					for _, kind := range []conversation.EventType{conversation.EventMessageUpdated, conversation.EventExecutionUpdated} {
						if err := appendEvent(kind, map[string]string{"id": message.ID, "status": "completed"}); err != nil {
							return err
						}
					}
				}
				for _, title := range []string{"First", "First", "First", "Decision", "Decision", "Decision", "First", "First", "First"} {
					if err := appendEvent(conversation.EventConversationUpdated, map[string]string{"id": message.ID, "status": "completed", "title": title}); err != nil {
						return err
					}
				}
				for _, kind := range []conversation.EventType{conversation.EventMessageAccepted, conversation.EventMessageUpdated, conversation.EventQuestionOpened, conversation.EventQuestionUpdated, conversation.EventCommandReceipt, conversation.EventExecutionUpdated, conversation.EventConversationUpdated} {
					if err := appendEvent(kind, map[string]string{"id": message.ID, "text": "durable content", "status": "completed"}); err != nil {
						return err
					}
				}
				current, err := writer.Conversation(t.Context(), f.project.ID, record.ID)
				if err != nil {
					return err
				}
				update := conversationRecordToStore(record)
				update.EventSequence = current.EventSequence
				updated, err := writer.UpdateConversation(t.Context(), update, record.Revision)
				record = conversationRecordFromStore(updated)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for pass := range 3 {
				if _, err := f.service.conversations.compactEventsPG(t.Context(), f.project.OrganizationID, now); err != nil {
					t.Fatal(err)
				}
				events, err := streamCompactionEvents(t, f, record.ID, 0)
				if err != nil {
					t.Fatal(err)
				}
				want := 620
				if !test.streaming {
					want = 20
					if test.compact {
						want = 13
					}
					if pass == 0 {
						want = 120
					}
				}
				if len(events) != want {
					t.Fatalf("pass %d: events=%d want=%d", pass, len(events), want)
				}
				titles := []string{}
				for _, event := range events {
					if event.Type != conversation.EventConversationUpdated {
						continue
					}
					var body struct {
						Title string `json:"title"`
					}
					if err := json.Unmarshal(event.Data, &body); err != nil {
						t.Fatal(err)
					}
					if len(titles) == 0 || titles[len(titles)-1] != body.Title {
						titles = append(titles, body.Title)
					}
				}
				if strings.Join(titles, ",") != "First,Decision,First," {
					t.Fatalf("metadata history lost transitions: %v", titles)
				}
			}
			var summary tracker.EventCompaction
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+record.ID, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var snapshot conversationSnapshot
			decodeHubResponse(t, response, &snapshot)
			if snapshot.Compaction != nil {
				summary = *snapshot.Compaction
			}
			if test.compact && (summary.RemovedEvents != 607 || summary.RetainedEvents != 13) {
				t.Fatalf("summary=%+v", summary)
			}
			if !test.compact && !test.streaming && (summary.RemovedEvents != 600 || summary.RetainedEvents != 20) {
				t.Fatalf("settled delta compaction=%+v", summary)
			}
			if test.streaming && summary.RemovedEvents != 0 {
				t.Fatalf("protected conversation compacted: %+v", summary)
			}
			_, err := fixture.publishedRecordEvents(t.Context(), record, 0)
			if !test.streaming != errors.Is(err, errConversationCursorExpired) {
				t.Fatalf("stale replay: %v", err)
			}
			events, err := fixture.publishedRecordEvents(t.Context(), record, 612)
			if err != nil || len(events) != 8 {
				t.Fatalf("current replay: %v, %d events", err, len(events))
			}
			messages, err := fixture.publishedRecordMessages(t.Context(), record)
			if err != nil || len(messages) != 1 || messages[0].Text != "Final answer retained" {
				t.Fatalf("messages=%+v err=%v", messages, err)
			}
			got, err := fixture.readRecord(t.Context(), record.ProjectID, record.ID)
			if err != nil || got.EventSeq != 620 || got.Title != record.Title || got.Revision != record.Revision {
				t.Fatalf("current state changed: %+v %v", got, err)
			}
		})
	}
}

func TestEventCompactionRollback(t *testing.T) {
	t.Parallel()
	a := newTerminalAttemptFixture(t)
	a.snapshots(t, 2)
	a.finish(t)
	a.advance(2 * time.Hour)
	beforeRows, beforePayloads := a.canonicalEvents(t)
	scope := nativeAttemptListingScope(t, a.nativeFixture)
	value := tracker.CollaborationEvent{ID: newNativeID("event"), OrganizationID: a.scope.organization, ProjectID: a.scope.project, AggregateType: "issue", AggregateID: a.issue.WorkItemID, Type: "run.observed", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: a.service.config.now(), Data: tracker.CollaborationData{Run: &a.start.Data}}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	row, err := eventstore.NewContent(a.service.runDetailStore()).PrepareEvent(t.Context(), a.scope.organization, raw)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected rollback")
	err = a.service.config.TenantRepository.WriteTenant(t.Context(), a.scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: a.scope.organization, ID: "archive_" + row.PayloadSHA256, Reference: eventstore.Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes), CreatedAt: a.service.config.now()}); err != nil {
			return err
		}
		if _, err := w.AppendEvent(t.Context(), row); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("expected failure: %v", err)
	}
	counts := a.runEvents(t)
	if counts["run.started"] != 1 || counts["run.checkpointed"] != 2 || counts["run.observed"] != 2 || counts["run.finished"] != 1 {
		t.Fatalf("rollback lost run events: %v", counts)
	}
	rows, payloads := a.canonicalEvents(t)
	if !reflect.DeepEqual(rows, beforeRows) || !reflect.DeepEqual(payloads, beforePayloads) {
		t.Fatal("rollback changed immutable event references or payload bytes")
	}
	var stored []eventstore.Event
	if err := a.service.config.TenantRepository.ReadTenant(t.Context(), a.scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		stored, err = r.Events(t.Context(), a.scope.project, a.issue.WorkItemID, 0, corestore.MaxPageSize)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	spans := 0
	for _, row := range stored {
		event, err := eventstore.NewContent(a.service.runDetailStore()).Event(t.Context(), row)
		if err != nil {
			t.Fatal(err)
		}
		if event.Data.Run != nil && event.Data.Run.Runtime != nil && event.Data.Run.Runtime.Activity != nil && len(event.Data.Run.Runtime.Activity.Spans) > 0 {
			spans++
		}
	}
	if spans != 6 {
		t.Fatalf("rollback lost activity payloads: %d", spans)
	}
	if err := a.service.config.TenantRepository.ReadTenant(t.Context(), a.scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.(attemptArchiveReader).ArchiveObjectByKey(t.Context(), row.PayloadObjectKey)
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("rollback retained archive ownership: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := conversationTestAdmin(t, a.service).ExecContext(t.Context(), `DELETE FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`, a.scope.organization, a.scope.project, a.issue.WorkItemID); err == nil {
		t.Fatal("rollback lost deletion protection")
	}
}

type terminalAttemptFixture struct {
	nativeFixture
	clock   *atomic.Int64
	issue   tracker.NativeIssue
	worker  string
	lease   tracker.NativeLease
	start   tracker.NativeRunEvent
	path    string
	scope   nativeScope
	posted  int64
	runtime func(string) *tracker.NativeRuntimeObservation
}

func newTerminalAttemptFixture(t *testing.T, shared ...*terminalAttemptFixture) *terminalAttemptFixture {
	t.Helper()
	clock := &atomic.Int64{}
	clock.Store(time.Now().UTC().Truncate(time.Second).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	var f nativeFixture
	if len(shared) == 0 {
		f = newDefaultNativeFixture(t, Config{now: now})
	} else {
		clock = shared[0].clock
		f = newNativeFixture(t, shared[0].service, "org_first_second", "terminal history second organization")
	}

	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	a := &terminalAttemptFixture{nativeFixture: f, clock: clock, issue: f.create(t, "terminal history"), worker: f.worker(t, "worker")}
	a.scope = nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	a.path = f.base + "/work-items/" + string(a.issue.WorkItemID) + "/events"
	a.lease = claimNativeAttempt(t, f, a.worker, "machine", "session", a.issue.WorkItemID)
	a.runtime = func(phase string) *tracker.NativeRuntimeObservation {
		at := now()
		activity := &workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 7, Generation: 1, SessionID: 1, Stage: "implementation", Status: "running", Coverage: "partial", StartedAt: at, AsOf: at,
			Spans: []workflowmetrics.ActivitySpan{{ID: "edit", Kind: "implementation", Evidence: "edit_tool", Outcome: "running", StartedAt: at}}}
		return &tracker.NativeRuntimeObservation{LocalAttemptID: 7, Generation: 1, Phase: phase, HeartbeatAt: at, Phases: []tracker.NativePhase{{Name: phase, StartedAt: at}}, Activity: activity}
	}
	a.start = nativeStartedEvent(a.lease)
	a.start.Data.Runtime = a.runtime("implementation")
	a.post(t, a.start)
	return a
}

func (a *terminalAttemptFixture) advance(d time.Duration) {
	a.clock.Add(int64(d))
}

func (a *terminalAttemptFixture) post(t *testing.T, event tracker.NativeRunEvent) {
	t.Helper()
	a.posted++
	event.Data.Sequence = a.posted
	event.IdempotencyKey = fmt.Sprintf("%s-%d", event.Type, a.posted)
	requireNativeStatus(t, performHubAPIRequest(t, a.service, http.MethodPost, a.path, a.worker, event), http.StatusOK)
}

func (a *terminalAttemptFixture) snapshots(t *testing.T, count int) {
	t.Helper()
	for index := range count {
		a.advance(time.Second)
		checkpoint := a.start
		checkpoint.Type = "run.checkpointed"
		checkpoint.Data.Handoff = nativeTestCheckpoint()
		checkpoint.Data.Runtime = a.runtime("implementation")
		checkpoint.Data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "test-model", Input: int64(10 * (index + 1)), Output: int64(index + 1), Currency: "USD"}}
		a.post(t, checkpoint)
		observed := a.start
		observed.Type = "run.observed"
		observed.Data.Runtime = a.runtime([]string{"validation", "implementation"}[index%2])
		a.post(t, observed)
	}
}

func (a *terminalAttemptFixture) finish(t *testing.T) {
	t.Helper()
	a.advance(time.Second)
	finish := a.start
	finish.Type, finish.Data.Outcome = "run.finished", "succeeded"
	finish.Data.CompletionBody = "Completed attempt details"
	finish.Data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "test-model", Input: 500, Output: 50, Currency: "USD"}}
	finish.Data.Disposition = &tracker.NativeDisposition{Status: "complete"}
	finish.Data.Runtime = a.runtime("completed")
	finish.Data.Runtime.Validation = &gate.CommandResult{Command: "focused test", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), DurationNS: 1, Output: "ok"}
	finish.Data.Runtime.Landing = &tracker.NativeLandingReceipt{Waiting: true, HeadSHA: strings.Repeat("a", 40)}
	a.post(t, finish)
	requireNativeStatus(t, performHubAPIRequest(t, a.service, http.MethodPost, a.base+"/leases/"+string(a.lease.ID)+"/release", a.worker, tracker.NativeLeaseMutation{FencingToken: a.lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
}

func (a *terminalAttemptFixture) runEvents(t *testing.T) map[string]int {
	t.Helper()
	counts := map[string]int{}
	after := int64(0)
	for {
		var page []eventstore.Event
		if err := a.service.config.TenantRepository.ReadTenant(t.Context(), a.scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			page, err = r.Events(t.Context(), a.scope.project, a.issue.WorkItemID, after, corestore.MaxPageSize)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, row := range page {
			if strings.HasPrefix(row.Type, "run.") {
				counts[row.Type]++
			}
		}
		if len(page) < corestore.MaxPageSize {
			return counts
		}
		after = page[len(page)-1].Sequence
	}
}

func (a *terminalAttemptFixture) canonicalEvents(t *testing.T) ([]eventstore.Event, map[string][]byte) {
	t.Helper()
	var rows []eventstore.Event
	after := int64(0)
	for {
		var page []eventstore.Event
		if err := a.service.config.TenantRepository.ReadTenant(t.Context(), a.scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			page, err = r.HistoryEvents(t.Context(), a.scope.project, a.issue.WorkItemID, after, corestore.MaxPageSize)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, page...)
		if len(page) < corestore.MaxPageSize {
			break
		}
		after = page[len(page)-1].Sequence
	}
	payloads := map[string][]byte{}
	for _, row := range rows {
		ref := eventstore.Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes)
		raw, err := a.service.runDetailStore().ReadDetail(t.Context(), string(a.scope.organization), ref)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := eventstore.NewContent(a.service.runDetailStore()).Event(t.Context(), row); err != nil {
			t.Fatal(err)
		}
		payloads[row.ID] = bytes.Clone(raw)
	}
	return rows, payloads
}

func (a *terminalAttemptFixture) history(t *testing.T) []tracker.CollaborationEvent {
	t.Helper()
	path := strings.TrimSuffix(a.path, "/events") + "/history?limit=200"
	var events []tracker.CollaborationEvent
	cursor := ""
	for {
		target := path
		if cursor != "" {
			target += "&cursor=" + url.QueryEscape(cursor)
		}
		response := performHubAPIRequest(t, a.service, http.MethodGet, target, a.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page tracker.Page[tracker.CollaborationEvent]
		decodeHubResponse(t, response, &page)
		if page.Compaction != nil {
			t.Fatal("PostgreSQL work history fabricated a compaction summary")
		}
		for _, event := range page.Items {
			if event.Data.Run != nil && event.Data.Run.Runtime != nil && event.Data.Run.Runtime.Activity != nil {
				profile := event.Data.Run.Runtime.Activity
				if len(profile.Spans) != 0 || !slices.Contains(profile.CoverageNotes, "activity_spans_omitted_read_with_board_session_history") {
					t.Fatalf("history did not honestly omit activity spans: %+v", profile)
				}
			}
		}
		events = append(events, page.Items...)
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			t.Fatal("work history cursor did not progress")
		}
		cursor = page.NextCursor
	}
	return events
}

func (a *terminalAttemptFixture) assertSessionSpans(t *testing.T) {
	t.Helper()
	args, err := json.Marshal(map[string]any{"project_id": a.project.ID, "reference": a.issue.WorkItemID, "native_attempt_id": a.start.Data.AttemptID})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (hostedOperatorExecutor{service: a.service}).Execute(changeOperatorContext(t, a.service, a.token, string(a.project.OrganizationID)), operatortool.Call{Name: operatortool.BoardSessionHistory, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var history struct {
		Data struct {
			Page struct {
				Items []workflowmetrics.ActivitySpan `json:"items"`
			} `json:"page"`
		} `json:"data"`
	}
	if err := json.Unmarshal(result.Content, &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Data.Page.Items) != 1 || history.Data.Page.Items[0].Evidence != "edit_tool" {
		t.Fatalf("board session lost canonical activity spans: %s", result.Content)
	}
}

func TestTerminalAttemptCompactionPreservesReads(t *testing.T) {
	t.Parallel()
	a := newTerminalAttemptFixture(t)
	runtime := a.runtime
	a.runtime = func(phase string) *tracker.NativeRuntimeObservation {
		r := runtime(phase)
		r.Validation = &gate.CommandResult{Command: "focused test", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), DurationNS: 1, Output: strings.Repeat("historical output\n", 2048)}
		return r
	}
	a.snapshots(t, 12)
	if err := a.service.maintainNativeRetention(t.Context(), a.service.config.now()); err != nil {
		t.Fatal(err)
	}
	if counts := a.runEvents(t); counts["run.checkpointed"] != 12 || counts["run.observed"] != 12 {
		t.Fatalf("running attempt was compacted: %v", counts)
	}
	a.finish(t)
	beforeRows, beforePayloads := a.canonicalEvents(t)
	a.advance(2 * time.Hour)
	if err := a.service.maintainNativeRetention(t.Context(), a.service.config.now()); err != nil {
		t.Fatal(err)
	}
	ctx := changeOperatorContext(t, a.service, a.token, string(a.project.OrganizationID))
	executor := hostedOperatorExecutor{service: a.service}
	reads := func() map[string]string {
		t.Helper()
		answers := map[string]string{}
		for _, tool := range []string{operatortool.ExplainItem, operatortool.WorkRuns, operatortool.WorkAttemptReceipt, operatortool.BoardSessionHistory, operatortool.BoardSession, operatortool.WorkItem} {
			args, err := json.Marshal(map[string]any{"project_id": a.project.ID, "reference": a.issue.WorkItemID, "native_attempt_id": a.start.Data.AttemptID})
			if tool == operatortool.ExplainItem || tool == operatortool.WorkRuns || tool == operatortool.WorkItem || tool == operatortool.BoardSession {
				args, err = json.Marshal(map[string]any{"project_id": a.project.ID, "reference": a.issue.WorkItemID})
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(ctx, operatortool.Call{Name: tool, Arguments: args})
			if err != nil {
				t.Fatalf("%s: %v", tool, err)
			}
			answers[tool] = string(result.Content)
		}
		return answers
	}
	before := reads()
	for _, tool := range []string{operatortool.ExplainItem, operatortool.WorkRuns, operatortool.WorkAttemptReceipt, operatortool.BoardSessionHistory} {
		if !strings.Contains(before[tool], a.start.Data.AttemptID) && !strings.Contains(before[tool], `"edit"`) {
			t.Fatalf("%s lacks the terminal attempt: %s", tool, before[tool])
		}
	}
	if !strings.Contains(before[operatortool.WorkAttemptReceipt], `"status":"succeeded"`) || !strings.Contains(before[operatortool.WorkAttemptReceipt], `"cost_estimate"`) && !strings.Contains(before[operatortool.WorkAttemptReceipt], `"usage"`) {
		t.Fatalf("receipt lacks the terminal answer: %s", before[operatortool.WorkAttemptReceipt])
	}
	for range 2 {
		if err := a.service.maintainNativeRetention(t.Context(), a.service.config.now()); err != nil {
			t.Fatal(err)
		}
	}
	rows, payloads := a.canonicalEvents(t)
	if !reflect.DeepEqual(rows, beforeRows) || !reflect.DeepEqual(payloads, beforePayloads) {
		t.Fatal("maintenance changed terminal attempt history or canonical payloads")
	}
	a.history(t)
	a.assertSessionSpans(t)
	counts := a.runEvents(t)
	if counts["run.started"] != 1 || counts["run.checkpointed"] != 12 || counts["run.observed"] != 12 || counts["run.finished"] != 1 {
		t.Fatalf("terminal attempt history=%v", counts)
	}
	after := reads()
	for tool, answer := range before {
		var previous, current map[string]any
		if err := json.Unmarshal([]byte(answer), &previous); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(after[tool]), &current); err != nil {
			t.Fatal(err)
		}
		normalizeTestDashboardWatermark(t, previous, current)
		field := "data"
		if tool == operatortool.ExplainItem {
			field = "native_runtime"
		}
		previousData, previousOK := previous[field].(map[string]any)
		currentData, currentOK := current[field].(map[string]any)
		if !previousOK || !currentOK {
			t.Fatalf("%s lacks response data", tool)
		}
		normalizeTestDashboardWatermark(t, previousData, currentData)
		if !reflect.DeepEqual(previous, current) {
			t.Fatalf("%s changed after compaction:\nbefore=%s\nafter=%s", tool, answer, after[tool])
		}
	}
	if _, err := a.service.readHistory(t.Context(), a.scope, string(a.issue.WorkItemID), url.Values{"limit": {"200"}}); err != nil {
		t.Fatal(err)
	}
	response := performHubAPIRequest(t, a.service, http.MethodGet, a.base+"/work-items/"+string(a.issue.WorkItemID)+"/history?limit=200", a.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var history tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, response, &history)
	if history.Compaction != nil {
		t.Fatalf("history does not disclose compaction: %+v", history.Compaction)
	}
}
