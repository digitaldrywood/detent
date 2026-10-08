package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestEventCompactionWorkHistory(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		archived, leased bool
		removed          int64
	}{
		{name: "archived", archived: true, removed: 19},
		{name: "open"},
		{name: "archived with live lease", archived: true, leased: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
			f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
			issue := f.create(t, "retain history")
			path := f.base + "/work-items/" + string(issue.WorkItemID)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment"}, Body: "Human decision retained"}), http.StatusOK)
			if test.leased {
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
				claimNativeAttempt(t, f, f.worker(t, "worker"), "machine", "session", issue.WorkItemID)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for index := range 20 {
				if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.observed", tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: "attempt_first", Sequence: int64(index + 1)}}, now); err != nil {
					t.Fatal(err)
				}
			}
			durable := []string{"run.started", "run.checkpointed", "run.finished", "workflow.transitioned", "scheduler.decision"}
			for _, kind := range durable {
				if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), kind, tracker.CollaborationData{ReasonDetail: "durable record"}, now); err != nil {
					t.Fatal(err)
				}
			}
			if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.observed", tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: "attempt_second", Finalization: &tracker.NativeFinalization{HeadSHA: strings.Repeat("a", 40)}}}, now); err != nil {
				t.Fatal(err)
			}
			if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.observed", tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: "attempt_second"}}, now); err != nil {
				t.Fatal(err)
			}
			if test.archived {
				issue.Archived = true
				if _, err := persistNativeIssue(t.Context(), tx, scope, issue, "issue.edited", tracker.CollaborationData{Operation: "archive"}, now); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?limit=1", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var before tracker.Page[tracker.CollaborationEvent]
			decodeHubResponse(t, response, &before)
			if before.NextCursor == "" {
				t.Fatal("missing initial cursor")
			}
			if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?limit=200", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var after tracker.Page[tracker.CollaborationEvent]
			decodeHubResponse(t, response, &after)
			kinds := map[string]int{}
			for _, event := range after.Items {
				kinds[event.Type]++
			}
			for _, kind := range append(durable, "issue.created", "comment.created") {
				if kinds[kind] < 1 {
					t.Fatalf("lost %s: %+v", kind, kinds)
				}
			}
			if test.removed > 0 {
				if after.Compaction == nil || after.Compaction.RemovedEvents != test.removed || after.Compaction.RemovedBytes <= 0 || kinds["run.observed"] != 3 {
					t.Fatalf("compaction=%+v kinds=%v", after.Compaction, kinds)
				}
				var raw string
				var pending bool
				if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT event_compaction_storage_before,vacuum_pending FROM attempt_diff_body_migration WHERE singleton=1`).Scan(&raw, &pending); err != nil {
					t.Fatal(err)
				}
				var storage tenantEventStorageSize
				if err := json.Unmarshal([]byte(raw), &storage); err != nil {
					t.Fatal(err)
				}
				if !pending || storage.DatabaseBytes <= 0 || storage.CollaborationRows != int64(len(after.Items))+test.removed {
					t.Fatalf("missing pre-compaction baseline: pending=%v storage=%+v", pending, storage)
				}
			} else if after.Compaction != nil || kinds["run.observed"] != 22 {
				t.Fatalf("protected stream changed: %+v", after)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?cursor="+url.QueryEscape(before.NextCursor), f.token, nil)
			want := http.StatusOK
			if test.removed > 0 {
				want = http.StatusUnprocessableEntity
			}
			requireNativeStatus(t, response, want)
			ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
			args, err := json.Marshal(map[string]any{"project_id": f.project.ID, "reference": issue.WorkItemID, "limit": 200})
			if err != nil {
				t.Fatal(err)
			}
			result, err := (hostedOperatorExecutor{service: f.service}).Execute(ctx, operatortool.Call{Name: operatortool.WorkHistory, Arguments: args})
			if err != nil {
				t.Fatal(err)
			}
			var mcp operatortool.WorkReadResult[tracker.Page[tracker.CollaborationEvent]]
			if err := json.Unmarshal(result.Content, &mcp); err != nil {
				t.Fatal(err)
			}
			if len(mcp.Data.Items) != len(after.Items) {
				t.Fatalf("MCP lost history: %s", result.Content)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, path+"/comments", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			if !strings.Contains(response.Body.String(), "Human decision retained") {
				t.Fatal("human comment was lost")
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM collaboration_events WHERE work_item_id=?", issue.WorkItemID); err == nil {
				t.Fatal("ordinary history deletion allowed")
			}
			if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			summary, err := readEventCompaction(t.Context(), f.service.database.db, scope.organization, scope.project, "work_item", string(issue.WorkItemID))
			if err != nil {
				t.Fatal(err)
			}
			if test.removed > 0 && summary.RemovedEvents != test.removed {
				t.Fatalf("repeated sweep changed summary: %+v", summary)
			}
		})
	}
}

func TestEventCompactionConversations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		settled bool
		age     int
		status  conversation.ExecutionStatus
		compact bool
	}{
		{"settled past retention", true, 31, conversation.ExecutionCompleted, true},
		{"active", false, 31, conversation.ExecutionCompleted, false},
		{"recently settled", true, 29, conversation.ExecutionCompleted, false},
		{"retention boundary", true, 30, conversation.ExecutionCompleted, false},
		{"running", true, 31, conversation.ExecutionRunning, false},
		{"unknown execution", true, 31, conversation.ExecutionUnknown, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newConversationFixture(t)
			now := time.Now().UTC()
			at := now.Add(-time.Duration(test.age) * 24 * time.Hour)
			record := f.record(f.owner, "preserved conversation", at)
			record.Execution.Status = test.status
			if test.settled {
				record.Status = conversation.StatusSettled
				record.SettledAt = &at
			}
			record = f.create(t, record)
			tx := f.tx(t)
			message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: "Final answer retained", Delivery: conversation.DeliveryCompleted, CreatedAt: at, UpdatedAt: at, Actor: conversation.Actor{Kind: conversation.ActorRunner, PrincipalID: "test"}}
			if err := f.store.appendMessage(t.Context(), tx, &message); err != nil {
				t.Fatal(err)
			}
			for range 600 {
				if _, err := f.store.appendEvent(t.Context(), tx, record.ID, conversation.EventMessageDelta, conversationDeltaBody(message.ID, 0, "stream fragment"), at); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				for _, kind := range []conversation.EventType{conversation.EventMessageUpdated, conversation.EventExecutionUpdated} {
					if _, err := f.store.appendEvent(t.Context(), tx, record.ID, kind, map[string]string{"id": message.ID, "status": "completed"}, at); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, title := range []string{"First", "First", "First", "Decision", "Decision", "Decision", "First", "First", "First"} {
				if _, err := f.store.appendEvent(t.Context(), tx, record.ID, conversation.EventConversationUpdated, map[string]string{"id": message.ID, "status": "completed", "title": title}, at); err != nil {
					t.Fatal(err)
				}
			}
			for _, kind := range []conversation.EventType{conversation.EventMessageAccepted, conversation.EventMessageUpdated, conversation.EventQuestionOpened, conversation.EventQuestionUpdated, conversation.EventCommandReceipt, conversation.EventExecutionUpdated, conversation.EventConversationUpdated} {
				if _, err := f.store.appendEvent(t.Context(), tx, record.ID, kind, map[string]string{"id": message.ID, "text": "durable content", "status": "completed"}, at); err != nil {
					t.Fatal(err)
				}
			}
			f.commit(t, tx)
			for pass := range 3 {
				if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
					t.Fatal(err)
				}
				events, err := f.store.listEvents(t.Context(), f.store.db, record.ID, 0, 1000)
				if err != nil {
					t.Fatal(err)
				}
				want := 620
				if test.compact {
					want = 13
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
			summary, err := readEventCompaction(t.Context(), f.store.db, record.OrganizationID, record.ProjectID, "conversation", record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if test.compact && (summary.RemovedEvents != 607 || summary.RetainedEvents != 13) {
				t.Fatalf("summary=%+v", summary)
			}
			if !test.compact && summary.RemovedEvents != 0 {
				t.Fatalf("protected conversation compacted: %+v", summary)
			}
			_, err = f.store.replayEvents(t.Context(), f.service.database.reader, record.ID, 0)
			if test.compact != errors.Is(err, errConversationCursorExpired) {
				t.Fatalf("stale replay: %v", err)
			}
			events, err := f.store.replayEvents(t.Context(), f.service.database.reader, record.ID, 612)
			if err != nil || len(events) != 8 {
				t.Fatalf("current replay: %v, %d events", err, len(events))
			}
			messages, err := f.store.listMessages(t.Context(), f.store.db, record.ID, 0, 50)
			if err != nil || len(messages) != 1 || messages[0].Text != "Final answer retained" {
				t.Fatalf("messages=%+v err=%v", messages, err)
			}
			got, err := f.store.readConversationByID(t.Context(), f.store.db, record.ID)
			if err != nil || got.EventSeq != 620 || got.Title != record.Title || got.Revision != record.Revision {
				t.Fatalf("current state changed: %+v %v", got, err)
			}
		})
	}
}

func TestEventCompactionRollback(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	issue := f.create(t, "rollback compaction")
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	issue.Archived = true
	if _, err := persistNativeIssue(t.Context(), tx, scope, issue, "issue.edited", tracker.CollaborationData{Operation: "archive"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.observed", tracker.CollaborationData{}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER fail_compaction BEFORE INSERT ON event_compactions BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.service.maintainNativeRetention(t.Context(), time.Now()); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("expected failure: %v", err)
	}
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=?", issue.WorkItemID).Scan(&count); err != nil || count != 4 {
		t.Fatalf("rollback events=%d err=%v", count, err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM collaboration_events WHERE work_item_id=?", issue.WorkItemID); err == nil {
		t.Fatal("rollback lost deletion protection")
	}
}
