package hubserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestCoordinatorActionQueryMetadata(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "action metadata")
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"typed", "legacy", "backfilled"} {
		for index := range 2 {
			record := f.seed(t, fmt.Sprintf("%s action %d", stage, index), nil)
			agent := fmt.Sprintf("Example Agent %d", index)
			for _, proposal := range []struct{ id, agent string }{{"action", "Earlier Agent"}, {"action", agent}, {"other-action", "Other Agent"}} {
				data, err := json.Marshal(map[string]any{"operator_action": map[string]any{"action": map[string]string{"id": proposal.id, "agent": proposal.agent}}})
				if err != nil {
					t.Fatal(err)
				}
				message := conversationMessageRecord{Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: "Example proposal", Data: data, Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}}
				f.transact(t, func(tx *sql.Tx) error {
					return f.conversations.appendMessage(t.Context(), tx, &record, &message, f.service.config.now())
				})
			}
			prepare := func() {
				t.Helper()
				if stage == "typed" {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversation_messages SET data_json='{}' WHERE conversation_id=?", record.ID); err != nil {
						t.Fatal(err)
					}
					return
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE conversation_messages SET query_metadata_version=0,operator_action_id=NULL,operator_action_agent=NULL,operator_action_result=NULL WHERE conversation_id=?`, record.ID); err != nil {
					t.Fatal(err)
				}
				if stage == "backfilled" {
					if err := f.service.convertConversationQueryMetadata(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
			}
			prepare()
			action := chat.Action{ID: "action", ConversationID: record.ID, OrganizationID: string(f.organization), ProjectID: string(f.project.ID), Mutation: mutation.Metadata{Source: "chat"}, Title: "Example action", Status: chat.ActionSucceeded, Result: "Applied"}
			got, err := f.service.coordinatorProposalAgent(t.Context(), action)
			if err != nil || got != agent {
				t.Fatalf("%s conversation %d agent=%q want %q: %v", stage, index, got, agent, err)
			}
			missing := action
			missing.ID = "missing-action"
			got, err = f.service.coordinatorProposalAgent(t.Context(), missing)
			if err != nil || got != coordinatorAgentName {
				t.Fatalf("%s missing proposal agent=%q: %v", stage, got, err)
			}
			if err := f.service.publishCoordinatorDecision(t.Context(), action); err != nil {
				t.Fatal(err)
			}
			messages := f.messages(t, record.ID)
			if len(messages) != 4 || messages[3].Text != "Example action: succeeded. Applied" {
				t.Fatalf("%s conversation %d lost its result: %+v", stage, index, messages)
			}
			prepare()
			if err := f.service.publishCoordinatorDecision(t.Context(), action); err != nil {
				t.Fatal(err)
			}
			if got := f.messages(t, record.ID); len(got) != len(messages) || got[3].ID != messages[3].ID {
				t.Fatalf("%s replay duplicated its result: %+v", stage, got)
			}
			if err := f.service.publishCoordinatorDecision(t.Context(), missing); err != nil {
				t.Fatal(err)
			}
			if got := f.messages(t, record.ID); len(got) != 5 {
				t.Fatalf("%s another action result was hidden: %+v", stage, got)
			}
		}
	}
}

func TestCollaborationActivityQueryMetadata(t *testing.T) {
	t.Parallel()
	a := newTerminalAttemptFixture(t)
	if err := a.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	other := a.create(t, "other activity history")
	var started int64
	if err := a.service.database.reader.QueryRowContext(t.Context(), "SELECT sequence FROM collaboration_events WHERE work_item_id=? AND type='run.started'", a.issue.WorkItemID).Scan(&started); err != nil {
		t.Fatal(err)
	}
	wantSettled := []int64{started}
	for _, item := range []tracker.NativeWorkItemID{a.issue.WorkItemID, other.WorkItemID} {
		for _, test := range []struct {
			kind, attempt string
			activity      *workflowmetrics.ActivityProfile
			eligible      bool
		}{
			{"run.observed", a.start.Data.AttemptID, &workflowmetrics.ActivityProfile{Spans: []workflowmetrics.ActivitySpan{{ID: "example-span", Kind: "tool"}}}, true},
			{"run.checkpointed", a.start.Data.AttemptID, &workflowmetrics.ActivityProfile{Spans: []workflowmetrics.ActivitySpan{}}, true},
			{"run.observed", a.start.Data.AttemptID, &workflowmetrics.ActivityProfile{}, false},
			{"run.observed", a.start.Data.AttemptID, nil, false},
			{"run.checkpointed", "missing-attempt", &workflowmetrics.ActivityProfile{Spans: []workflowmetrics.ActivitySpan{{ID: "foreign-span", Kind: "tool"}}}, false},
		} {
			tx, err := a.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tx.Rollback() })
			data := tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: test.attempt, Runtime: &tracker.NativeRuntimeObservation{Activity: test.activity}}}
			if err := appendNativeHistory(t.Context(), tx, a.scope, string(item), test.kind, data, a.service.config.now()); err != nil {
				t.Fatal(err)
			}
			var sequence int64
			if err := tx.QueryRowContext(t.Context(), "SELECT event_sequence FROM issues WHERE native_id=?", item).Scan(&sequence); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if item == a.issue.WorkItemID && test.eligible {
				wantSettled = append(wantSettled, sequence)
			}
		}
	}
	for _, phase := range []string{"running", "recently finished", "settled"} {
		if phase == "recently finished" {
			a.finish(t)
			var sequence int64
			if err := a.service.database.reader.QueryRowContext(t.Context(), "SELECT sequence FROM collaboration_events WHERE work_item_id=? AND type='run.finished'", a.issue.WorkItemID).Scan(&sequence); err != nil {
				t.Fatal(err)
			}
			wantSettled = append(wantSettled, sequence)
		}
		if phase == "settled" {
			a.advance(2 * time.Hour)
		}
		for _, stage := range []string{"typed", "legacy", "backfilled"} {
			if stage == "legacy" {
				if _, err := a.service.database.db.ExecContext(t.Context(), "UPDATE collaboration_events SET query_metadata_version=0,run_attempt_id=NULL,activity_spans_present=NULL"); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "backfilled" {
				if err := a.service.convertConversationQueryMetadata(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			for _, item := range []tracker.NativeWorkItemID{a.issue.WorkItemID, other.WorkItemID} {
				candidate := eventCompactionCandidate{organization: a.scope.organization, project: a.scope.project, kind: "work_item", id: string(item)}
				events, spans, complete, err := obsoleteWorkItemEvents(t.Context(), a.service.database.reader, candidate, a.service.config.now())
				if err != nil || !complete || len(events) != 0 {
					t.Fatalf("%s %s plan: events=%v complete=%t: %v", phase, stage, events, complete, err)
				}
				got, want := []int64{}, []int64{}
				for _, span := range spans {
					got = append(got, span.sequence)
				}
				if phase == "settled" && item == a.issue.WorkItemID {
					want = wantSettled
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s %s item %s spans=%v want %v", phase, stage, item, got, want)
				}
			}
		}
	}
	for _, field := range []struct {
		column string
		value  any
	}{{"activity_spans_present", false}, {"run_attempt_id", "missing-attempt"}} {
		if _, err := a.service.database.db.ExecContext(t.Context(), "UPDATE collaboration_events SET "+field.column+"=? WHERE work_item_id=?", field.value, a.issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
		candidate := eventCompactionCandidate{organization: a.scope.organization, project: a.scope.project, kind: "work_item", id: string(a.issue.WorkItemID)}
		_, spans, _, err := obsoleteWorkItemEvents(t.Context(), a.service.database.reader, candidate, a.service.config.now())
		if err != nil || len(spans) != 0 {
			t.Fatalf("typed %s did not filter spans: %v %v", field.column, spans, err)
		}
		if _, err := a.service.database.db.ExecContext(t.Context(), "UPDATE collaboration_events SET query_metadata_version=0 WHERE work_item_id=?", a.issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
		if err := a.service.convertConversationQueryMetadata(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConversationEventQueryMetadata(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, field := range []string{"same", "title", "visibility", "status", "preferences", "subject_work_item_id", "work_item_id", "linked_at"} {
		t.Run(field, func(t *testing.T) {
			record := f.create(t, f.record(f.owner, field, at))
			base := map[string]any{"title": "Example", "visibility": "private", "status": "settled", "preferences": map[string]string{"model": "auto", "reasoning_effort": "auto", "access": "auto"}, "subject_work_item_id": nil, "work_item_id": nil, "linked_at": nil}
			changed := map[string]any{}
			for key, value := range base {
				changed[key] = value
			}
			switch field {
			case "same":
			case "preferences":
				changed[field] = map[string]string{"model": "example-model", "reasoning_effort": "high", "access": "restricted"}
			default:
				changed[field] = "changed"
			}
			tx := f.tx(t)
			for _, body := range []any{base, changed, base} {
				if _, err := f.insertLegacyEvent(t, t.Context(), tx, record.ID, conversation.EventConversationUpdated, body, at); err != nil {
					t.Fatal(err)
				}
			}
			f.commit(t, tx)
			want := []int64{}
			if field == "same" {
				want = []int64{2}
			}
			for _, stage := range []string{"typed", "legacy", "backfilled"} {
				if stage == "legacy" {
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE conversation_events SET query_metadata_version=0,snapshot_title=NULL,snapshot_visibility=NULL,snapshot_status=NULL,snapshot_preferences_key=NULL,snapshot_subject_work_item_id=NULL,snapshot_work_item_id=NULL,snapshot_linked_at=NULL WHERE conversation_id=?`, record.ID); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "backfilled" {
					if err := f.service.convertConversationQueryMetadata(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				events, err := obsoleteConversationEvents(t.Context(), f.service.database.reader, record.ID, 500, true)
				if err != nil {
					t.Fatal(err)
				}
				got := []int64{}
				for _, event := range events {
					got = append(got, event.sequence)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: obsolete=%v want %v", stage, got, want)
				}
			}
		})
	}
	record := f.create(t, f.record(f.owner, "event identity", at))
	tx := f.tx(t)
	delivered := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: "answer", Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}, CreatedAt: at}
	if err := f.insertLegacyMessage(t, t.Context(), tx, &delivered); err != nil {
		t.Fatal(err)
	}
	for _, body := range []map[string]string{{"message_id": delivered.ID}, {"message_id": "missing"}} {
		if _, err := f.insertLegacyEvent(t, t.Context(), tx, record.ID, conversation.EventMessageDelta, body, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []map[string]string{{"id": "one"}, {"id": "two"}, {"id": "one"}} {
		if _, err := f.insertLegacyEvent(t, t.Context(), tx, record.ID, conversation.EventMessageUpdated, body, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []map[string]string{
		{"attempt_id": "attempt", "status": "completed", "thread_id": "thread", "turn_id": "turn"},
		{"attempt_id": "other", "status": "completed", "thread_id": "thread", "turn_id": "turn"},
		{"attempt_id": "attempt", "status": "failed", "thread_id": "thread", "turn_id": "turn"},
		{"attempt_id": "attempt", "status": "completed", "thread_id": "other", "turn_id": "turn"},
		{"attempt_id": "attempt", "status": "completed", "thread_id": "thread", "turn_id": "other"},
		{"attempt_id": "attempt", "status": "completed", "thread_id": "thread", "turn_id": "turn"},
		{}, {},
	} {
		if _, err := f.insertLegacyEvent(t, t.Context(), tx, record.ID, conversation.EventExecutionUpdated, body, at); err != nil {
			t.Fatal(err)
		}
	}
	f.commit(t, tx)
	for _, stage := range []string{"typed", "legacy", "mixed", "backfilled"} {
		if stage == "legacy" {
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE conversation_events SET query_metadata_version=0,message_id=NULL,object_id=NULL,attempt_id=NULL,execution_status=NULL,thread_id=NULL,turn_id=NULL WHERE conversation_id=?`, record.ID); err != nil {
				t.Fatal(err)
			}
		}
		if stage == "mixed" {
			var row int64
			var raw string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT rowid,body_json FROM conversation_events WHERE conversation_id=? AND seq=11", record.ID).Scan(&row, &raw); err != nil {
				t.Fatal(err)
			}
			m, err := eventQueryMetadata([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.writeConversationQueryMetadata(t.Context(), conversationMetadataFamily{"conversation_events", "body_json"}, row, raw, "", m); err != nil {
				t.Fatal(err)
			}
		}
		if stage == "backfilled" {
			if err := f.service.convertConversationQueryMetadata(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		for _, full := range []bool{false, true} {
			events, err := obsoleteConversationEvents(t.Context(), f.service.database.reader, record.ID, 500, full)
			if err != nil {
				t.Fatal(err)
			}
			got := []int64{}
			for _, event := range events {
				got = append(got, event.sequence)
			}
			want := []int64{1}
			if full {
				want = []int64{1, 2, 3, 6, 12}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s full=%v obsolete=%v want %v", stage, full, got, want)
			}
		}
	}
}

func TestConversationQueryMetadataBackfill(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	now := f.service.config.now().UTC()
	first := f.create(t, f.record(f.owner, "first", now))
	second := f.create(t, f.record(f.owner, "second", now))
	tx := f.tx(t)
	for i := range storageConversionRows + 1 {
		id := first.ID
		if i%2 == 1 {
			id = second.ID
		}
		message := conversationMessageRecord{ConversationID: id, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: "proposal", Data: json.RawMessage(`{"operator_action":{"action":{"id":"action","agent":"Example Agent"}},"operator_action_result":"action-result","tool":"search","item_id":"item","status":"completed"}`), Delivery: conversation.DeliveryCompleted, Actor: conversation.Actor{Kind: conversation.ActorCoordinator}, CreatedAt: now}
		if err := f.insertLegacyMessage(t, t.Context(), tx, &message); err != nil {
			t.Fatal(err)
		}
	}
	for _, record := range []conversationRecord{first, second} {
		execution, err := json.Marshal(record.Execution)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `UPDATE conversations SET content_attachment_id='',execution_json=? WHERE id=?`, string(execution), record.ID); err != nil {
			t.Fatal(err)
		}
		receipt, err := json.Marshal(conversation.Receipt{Key: "key", Kind: conversation.CommandMessage, Status: conversation.DeliverySaved, UpdatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO conversation_commands(conversation_id,key,kind,request_hash,receipt_json,created_at,updated_at) VALUES(?,'key',?,'hash',?,?,?)`, record.ID, conversation.CommandMessage, string(receipt), conversationTime(now), conversationTime(now)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.insertLegacyEvent(t, t.Context(), tx, record.ID, conversation.EventMessageUpdated, map[string]string{"role": "user", "kind": "continue", "attempt_id": "attempt"}, now); err != nil {
			t.Fatal(err)
		}
	}
	f.commit(t, tx)
	for _, table := range []string{"conversations", "conversation_messages", "conversation_commands", "conversation_events"} {
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE "+table+" SET query_metadata_version=0"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE conversations SET execution_status=NULL,execution_lease_id=NULL,execution_settled=NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE conversation_messages SET actor_kind=NULL,tool_name=NULL,tool_item_id=NULL,tool_status=NULL,operator_action_id=NULL,operator_action_agent=NULL,operator_action_result=NULL`); err != nil {
		t.Fatal(err)
	}
	for _, record := range []conversationRecord{first, second} {
		agent, err := f.service.coordinatorProposalAgent(t.Context(), chat.Action{ConversationID: record.ID, ID: "action"})
		if err != nil || agent != "Example Agent" {
			t.Fatalf("legacy agent=%q err=%v", agent, err)
		}
		results, err := f.service.legacyCoordinatorResults(t.Context(), record.ID, "action-result")
		if err != nil || len(results) == 0 {
			t.Fatalf("legacy results=%v err=%v", results, err)
		}
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER reject_second_metadata BEFORE UPDATE OF query_metadata_version ON conversations WHEN OLD.title='second' BEGIN SELECT RAISE(ABORT,'injected second-row failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.convertConversationQueryMetadata(t.Context()); err == nil || !strings.Contains(err.Error(), "injected second-row failure") {
		t.Fatalf("missing injection: %v", err)
	}
	var firstVersion, secondVersion int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT query_metadata_version FROM conversations WHERE id=?", first.ID).Scan(&firstVersion); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT query_metadata_version FROM conversations WHERE id=?", second.ID).Scan(&secondVersion); err != nil {
		t.Fatal(err)
	}
	if firstVersion != 1 || secondVersion != 0 {
		t.Fatalf("rows did not commit separately: %d %d", firstVersion, secondVersion)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER reject_second_metadata"); err != nil {
		t.Fatal(err)
	}
	pending := func() int {
		t.Helper()
		var count int
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM conversations WHERE query_metadata_version=0)+(SELECT count(*) FROM conversation_messages WHERE query_metadata_version=0)+(SELECT count(*) FROM conversation_commands WHERE query_metadata_version=0)+(SELECT count(*) FROM conversation_events WHERE query_metadata_version=0)`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := pending()
	for tick := range 2 {
		if err := f.service.convertConversationQueryMetadata(t.Context()); err != nil {
			t.Fatal(err)
		}
		after := pending()
		if before-after != min(before, storageConversionRows) {
			t.Fatalf("tick %d converted %d of %d", tick, before-after, before)
		}
		before = after
	}
	if before != 0 {
		t.Fatalf("backfill did not finish: %d", before)
	}
	for _, record := range []conversationRecord{first, second} {
		var actor, tool, item, status, action, agent, result string
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT actor_kind,tool_name,tool_item_id,tool_status,operator_action_id,operator_action_agent,operator_action_result FROM conversation_messages WHERE conversation_id=? ORDER BY seq LIMIT 1`, record.ID).Scan(&actor, &tool, &item, &status, &action, &agent, &result); err != nil {
			t.Fatal(err)
		}
		if got := []string{actor, tool, item, status, action, agent, result}; !reflect.DeepEqual(got, []string{"coordinator", "search", "item", "completed", "action", "Example Agent", "action-result"}) {
			t.Fatalf("message metadata=%v", got)
		}
	}
	stale := executionQueryMetadata(conversation.Execution{Status: conversation.ExecutionRunning})
	var row int64
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT rowid FROM conversations WHERE id=?", first.ID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if err := f.service.writeConversationQueryMetadata(t.Context(), conversationMetadataFamily{"conversations", "execution_json"}, row, `{"status":"running"}`, "", stale); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT execution_status FROM conversations WHERE id=?", first.ID).Scan(&status); err != nil || status != "idle" {
		t.Fatalf("stale projection overwrote current source: %s %v", status, err)
	}
}

func TestConversationCompactionCursorUnknownMetadata(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	now := f.service.config.now().UTC()
	for index := range 2 {
		record := f.create(t, f.record(f.owner, fmt.Sprintf("cursor %d", index), now))
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO event_compactions(organization_id,project_id,aggregate_kind,aggregate_id,summary_json) VALUES(?,?,'conversation',?,?)`, record.OrganizationID, record.ProjectID, record.ID, `{"through_sequence":20}`); err != nil {
			t.Fatal(err)
		}
		for _, stage := range []string{"legacy", "typed"} {
			if stage == "typed" {
				if err := f.service.convertConversationQueryMetadata(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			_, err := f.store.replayEvents(t.Context(), f.service.database.reader, record.ID, 19)
			if !errors.Is(err, errConversationCursorExpired) {
				t.Fatalf("%s unknown cursor authorized replay: %v", stage, err)
			}
			if _, err := f.store.replayEvents(t.Context(), f.service.database.reader, record.ID, 20); err != nil {
				t.Fatal(err)
			}
		}
	}
	var sqlText sql.NullString
	if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT group_concat(sql) FROM sqlite_master WHERE name IN ('conversation_events_message_update','conversation_events_execution_update','conversation_events_delta_message','collaboration_events_observation_attempt') AND lower(sql) LIKE '%json_%'`).Scan(&sqlText); err != nil || sqlText.Valid {
		t.Fatalf("JSON expression indexes remain: %v %v", sqlText, err)
	}
}

func TestConversationExecutionQueryMetadata(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	service := &conversationService{server: f.service, store: f.store}
	now := f.service.config.now().UTC()
	at := now.Add(-48 * time.Hour)
	type executionCase struct {
		status       conversation.ExecutionStatus
		settled      bool
		lease        string
		live, settle bool
	}
	cases := []executionCase{
		{conversation.ExecutionIdle, false, "", false, true},
		{conversation.ExecutionStarting, false, "lease-first", true, false},
		{conversation.ExecutionRunning, false, "lease-second", true, false},
		{conversation.ExecutionWaitingInput, false, "lease-first", true, false},
		{conversation.ExecutionInterrupting, false, "lease-second", true, false},
		{conversation.ExecutionCompleted, false, "lease-first", true, true},
		{conversation.ExecutionInterrupted, true, "lease-second", false, true},
		{conversation.ExecutionFailed, false, "", false, true},
		{conversation.ExecutionUnknown, false, "lease-first", true, true},
	}
	legacyExecutions := map[string]string{}
	wantLive, wantSettle, wantLease := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, c := range cases {
		record := f.record(f.owner, fmt.Sprintf("execution %d", i), at)
		record.Execution = conversation.Execution{Status: c.status, Settled: c.settled, Owner: conversation.Owner{LeaseID: c.lease}, UpdatedAt: at}
		record = f.create(t, record)
		legacyExecutions[record.ID] = mustMarshalConversationExecution(t, f.store, record.ID, nil)
		if c.live {
			wantLive[record.ID] = true
		}
		if c.settle {
			wantSettle[record.ID] = true
		}
		if c.live && c.lease == "lease-first" {
			wantLease[record.ID] = true
		}
	}
	for _, stage := range []string{"typed", "legacy", "backfilled"} {
		if stage == "legacy" {
			for id, raw := range legacyExecutions {
				if _, err := f.store.db.ExecContext(t.Context(), "UPDATE conversations SET content_attachment_id='',execution_json=? WHERE id=?", raw, id); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE conversations SET query_metadata_version=0,execution_status=NULL,execution_lease_id=NULL,execution_settled=NULL`); err != nil {
				t.Fatal(err)
			}
		}
		if stage == "backfilled" {
			if err := f.service.convertConversationQueryMetadata(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		refs, err := service.inFlightExecutions(t.Context(), conversationInFlightQuery)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, ref := range refs {
			got[ref.id] = true
		}
		if !reflect.DeepEqual(got, wantLive) {
			t.Fatalf("%s in-flight=%v want %v", stage, got, wantLive)
		}
		refs, err = service.inFlightExecutions(t.Context(), conversationInFlightQuery+"AND (query_metadata_version=0 OR execution_lease_id=?)", "lease-first")
		if err != nil {
			t.Fatal(err)
		}
		got = map[string]bool{}
		for _, ref := range refs {
			got[ref.id] = true
		}
		if !reflect.DeepEqual(got, wantLease) {
			t.Fatalf("%s lease=%v want %v", stage, got, wantLease)
		}
		records, err := f.store.listSettleCandidates(t.Context(), f.service.database.reader, now.Add(-time.Hour), 100)
		if err != nil {
			t.Fatal(err)
		}
		got = map[string]bool{}
		for _, record := range records {
			got[record.ID] = true
		}
		if !reflect.DeepEqual(got, wantSettle) {
			t.Fatalf("%s settle=%v want %v", stage, got, wantSettle)
		}
	}
}

func TestConversationLegacyToolRevision(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	now := f.service.config.now().UTC()
	for _, stage := range []string{"typed", "legacy", "backfilled"} {
		for _, actor := range []conversation.ActorKind{conversation.ActorCoordinator, conversation.ActorHuman} {
			for index := range 2 {
				record := f.create(t, f.record(f.owner, fmt.Sprintf("%s %s tool %d", stage, actor, index), now))
				message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageTool, Text: "original", Data: json.RawMessage(`{"tool":"search","item_id":"item","status":"running"}`), Delivery: conversation.DeliveryResponding, Actor: conversation.Actor{Kind: actor, PrincipalID: f.owner}, CreatedAt: now}
				tx := f.tx(t)
				if err := f.insertLegacyMessage(t, t.Context(), tx, &message); err != nil {
					t.Fatal(err)
				}
				f.commit(t, tx)
				if stage == "typed" {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversation_messages SET data_json='{}',actor_json='{}' WHERE id=?", message.ID); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE conversation_messages SET query_metadata_version=0,actor_kind=NULL,tool_name=NULL,tool_item_id=NULL,tool_status=NULL WHERE id=?`, message.ID); err != nil {
						t.Fatal(err)
					}
					if stage == "backfilled" {
						if err := f.service.convertConversationQueryMetadata(t.Context()); err != nil {
							t.Fatal(err)
						}
					}
				}
				prepared, err := f.store.prepareLegacyToolRevisions(t.Context(), record.ID)
				if err != nil {
					t.Fatal(err)
				}
				message.Text = "finished"
				message.Data = json.RawMessage(`{"tool":"search","item_id":"item","status":"completed"}`)
				message.Actor = conversation.Actor{Kind: conversation.ActorCoordinator}
				tx = f.tx(t)
				if err := f.updateMessage(t, prepared, tx, message); err != nil {
					t.Fatal(err)
				}
				f.commit(t, tx)
				var text, tool, item, status, contentID string
				err = f.service.database.reader.QueryRowContext(t.Context(), `SELECT text,tool,item_id,status,content_attachment_id FROM conversation_tool_revisions WHERE conversation_id=? AND message_id=? AND revision=1`, record.ID, message.ID).Scan(&text, &tool, &item, &status, &contentID)
				if actor != conversation.ActorCoordinator {
					if !errors.Is(err, sql.ErrNoRows) {
						t.Fatalf("%s preserved a non-coordinator revision: %v", stage, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if text != "" || contentID == "" {
					t.Fatalf("legacy tool remained inline: text=%q content=%q", text, contentID)
				}
				raw, readErr := f.store.readConversationArchive(t.Context(), f.store.reader, string(record.OrganizationID), contentID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				var content conversationContent
				if err := json.Unmarshal(raw, &content); err != nil {
					t.Fatal(err)
				}
				text = content.Text
				if !reflect.DeepEqual([]string{text, tool, item, status}, []string{"original", "search", "item", "running"}) {
					t.Fatalf("%s lost revision: %q %q %q %q", stage, text, tool, item, status)
				}
			}
		}
	}
}

func TestConversationUnknownExecutionNeverCompactsLiveLease(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	now := f.service.config.now().UTC()
	at := now.Add(-31 * 24 * time.Hour)
	record := f.record
	record.ID = conversation.NewConversationID()
	record.WorkItemID = ""
	record.SubjectWorkItemID = ""
	record.LinkedAt = nil
	record.Status = conversation.StatusSettled
	record.SettledAt = &at
	record.CreatedAt, record.UpdatedAt = at, at
	record.Execution = conversation.Execution{Status: conversation.ExecutionCompleted, Owner: conversation.Owner{LeaseID: string(f.lease.ID)}, UpdatedAt: at}
	ctx := prepareTestConversationRecord(t, f.chat.store, record)
	tx := f.tx(t)
	if err := f.chat.store.createConversation(ctx, tx, &record); err != nil {
		t.Fatal(err)
	}
	if _, err := (conversationFixture{store: f.chat.store}).appendEvent(t, t.Context(), tx, record.ID, conversation.EventHeartbeat, map[string]string{}, at); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	executionRaw, err := json.Marshal(record.Execution)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"typed", "legacy", "backfilled"} {
		if stage == "legacy" {
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE conversations SET content_attachment_id='',execution_json=?,query_metadata_version=0,execution_status=NULL,execution_lease_id=NULL,execution_settled=NULL WHERE id=?`, string(executionRaw), record.ID); err != nil {
				t.Fatal(err)
			}
		}
		if stage == "backfilled" {
			if err := f.service.convertConversationQueryMetadata(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		full, err := conversationIsCompactable(t.Context(), f.service.database.reader, record.ID, now)
		if err != nil || full {
			t.Fatalf("%s compacted a live lease: %t %v", stage, full, err)
		}
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE leases SET released_at=? WHERE lease_id=?`, formatHubTime(now), f.lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE conversations SET query_metadata_version=0,execution_status=NULL,execution_lease_id=NULL,execution_settled=NULL WHERE id=?`, record.ID); err != nil {
		t.Fatal(err)
	}
	full, err := conversationIsCompactable(t.Context(), f.service.database.reader, record.ID, now)
	if err != nil || !full {
		t.Fatalf("legacy settled conversation did not become compactable: %t %v", full, err)
	}
}
