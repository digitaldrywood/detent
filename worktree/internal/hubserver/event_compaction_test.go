package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/gate"
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
		removed          int64
		extraActivity    int
	}{
		{name: "archived", archived: true, removed: 0},
		{name: "open"},
		{name: "archived with live lease", archived: true, leased: true},
		{name: "closed for three days", closedAgo: 73 * time.Hour, removed: 0},
		{name: "closed recently", closedAgo: time.Hour},
		{name: "unpublished closed activity stays hot", closedAgo: 73 * time.Hour, removed: 0, extraActivity: 500},
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
				data := tracker.CollaborationData{ReasonDetail: "durable record"}
				copies := 1
				if kind == "run.checkpointed" {
					copies += test.extraActivity
					data.Run = &tracker.NativeRunData{AttemptID: "attempt_first", Runtime: &tracker.NativeRuntimeObservation{Activity: &workflowmetrics.ActivityProfile{Schema: 1, Spans: []workflowmetrics.ActivitySpan{{ID: "span", Kind: "tool", Evidence: strings.Repeat("x", 4096)}}}}}
				}
				for duplicate := range copies {
					if duplicate > 0 {
						run := *data.Run
						run.AttemptID = fmt.Sprintf("attempt_checkpoint_%d", duplicate)
						data.Run = &run
					}
					if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), kind, data, now); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.closedAgo > 0 {
				if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "Done"}, now.Add(-test.closedAgo)); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=issues.project_id AND detent_state='Done') WHERE native_id=?`, issue.WorkItemID); err != nil {
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
			if test.extraActivity > 0 {
				for _, want := range []struct{ compacted, remaining int64 }{{0, 501}} {
					summary, err := readEventCompaction(t.Context(), f.service.database.db, scope.organization, scope.project, "work_item", string(issue.WorkItemID))
					if err != nil {
						t.Fatal(err)
					}
					var remaining, checkpoints int64
					if err := f.service.database.db.QueryRowContext(t.Context(), `SELECT count(*),coalesce(sum(json_type(data_json,'$.run.runtime.activity.spans') IS NOT NULL),0) FROM collaboration_events WHERE work_item_id=? AND type='run.checkpointed'`, issue.WorkItemID).Scan(&checkpoints, &remaining); err != nil {
						t.Fatal(err)
					}
					if summary.RemovedEvents != 0 || summary.CompactedActivityEvents != want.compacted || remaining != want.remaining || checkpoints != 501 {
						t.Fatalf("compaction=%+v remaining=%d checkpoints=%d want=%+v", summary, remaining, checkpoints, want)
					}
					if err := f.service.maintainNativeRetention(t.Context(), now); err != nil {
						t.Fatal(err)
					}
				}
				return
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
				if after.Compaction == nil || after.Compaction.RemovedEvents != test.removed || after.Compaction.RemovedBytes <= 0 || kinds["run.observed"] != 3 || after.Compaction.CompactedActivityEvents != 1 {
					t.Fatalf("compaction=%+v kinds=%v", after.Compaction, kinds)
				}
				for _, event := range after.Items {
					if event.Type == "run.checkpointed" && (event.Data.Run == nil || event.Data.Run.Runtime.Activity.Spans != nil || !slices.Contains(event.Data.Run.Runtime.Activity.CoverageNotes, "activity_spans_compacted")) {
						t.Fatalf("checkpoint kept activity spans: %+v", event.Data.Run)
					}
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
			if test.streaming {
				message.Delivery = conversation.DeliveryResponding
			}
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
			summary, err := readEventCompaction(t.Context(), f.store.db, record.OrganizationID, record.ProjectID, "conversation", record.ID)
			if err != nil {
				t.Fatal(err)
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
			_, err = f.store.replayEvents(t.Context(), f.service.database.reader, record.ID, 0)
			if !test.streaming != errors.Is(err, errConversationCursorExpired) {
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
	a := newTerminalAttemptFixture(t)
	a.snapshots(t, 2)
	a.finish(t)
	a.advance(2 * time.Hour)
	if _, err := a.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER fail_compaction BEFORE INSERT ON event_compactions BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := a.service.maintainNativeRetention(t.Context(), a.service.config.now()); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("expected failure: %v", err)
	}
	counts := a.runEvents(t)
	if counts["run.started"] != 1 || counts["run.checkpointed"] != 2 || counts["run.observed"] != 2 || counts["run.finished"] != 1 {
		t.Fatalf("rollback lost run events: %v", counts)
	}
	var spans int
	if err := a.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND json_type(data_json,'$.run.runtime.activity.spans')='array'`, a.issue.WorkItemID).Scan(&spans); err != nil {
		t.Fatal(err)
	}
	if spans != 6 {
		t.Fatalf("rollback lost activity payloads: %d", spans)
	}
	if _, err := a.service.database.db.ExecContext(t.Context(), "DELETE FROM collaboration_events WHERE work_item_id=?", a.issue.WorkItemID); err == nil {
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

func newTerminalAttemptFixture(t *testing.T) *terminalAttemptFixture {
	t.Helper()
	clock := &atomic.Int64{}
	clock.Store(time.Now().UTC().Truncate(time.Second).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	f := newDefaultNativeFixture(t, Config{now: now})
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
	rows, err := a.service.database.db.QueryContext(t.Context(), `SELECT type,count(*) FROM collaboration_events WHERE work_item_id=? AND type LIKE 'run.%' GROUP BY type`, a.issue.WorkItemID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			t.Fatal(err)
		}
		counts[kind] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestTerminalAttemptCompactionPreservesReads(t *testing.T) {
	t.Parallel()
	a := newTerminalAttemptFixture(t)
	a.snapshots(t, 12)
	if err := a.service.maintainNativeRetention(t.Context(), a.service.config.now()); err != nil {
		t.Fatal(err)
	}
	if counts := a.runEvents(t); counts["run.checkpointed"] != 12 || counts["run.observed"] != 12 {
		t.Fatalf("running attempt was compacted: %v", counts)
	}
	a.finish(t)
	a.advance(2 * time.Hour)
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
	counts := a.runEvents(t)
	if counts["run.started"] != 1 || counts["run.checkpointed"] != 12 || counts["run.observed"] != 12 || counts["run.finished"] != 1 {
		t.Fatalf("terminal attempt history=%v", counts)
	}
	summary, err := readEventCompaction(t.Context(), a.service.database.db, a.scope.organization, a.scope.project, "work_item", string(a.issue.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	if summary.RemovedEvents != 0 || summary.CompactedActivityEvents != 26 {
		t.Fatalf("compaction receipt=%+v", summary)
	}
	after := reads()
	for tool, answer := range before {
		if after[tool] != answer {
			t.Fatalf("%s changed after compaction:\nbefore=%s\nafter=%s", tool, answer, after[tool])
		}
	}
	response := performHubAPIRequest(t, a.service, http.MethodGet, a.base+"/work-items/"+string(a.issue.WorkItemID)+"/history?limit=200", a.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var history tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, response, &history)
	if history.Compaction != nil {
		t.Fatalf("history does not disclose compaction: %+v", history.Compaction)
	}
}

func TestEventCompactionPassIsBoundedAndYieldsToWrites(t *testing.T) {
	t.Parallel()
	a := newTerminalAttemptFixture(t)
	a.snapshots(t, 1)
	tx, err := a.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	backlog := 2*eventCompactionBatch + 37
	for range backlog {
		if err := appendNativeHistory(t.Context(), tx, a.scope, string(a.issue.WorkItemID), "run.observed", tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: a.start.Data.AttemptID, Runtime: a.runtime("implementation")}}, a.service.config.now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	a.finish(t)
	a.advance(2 * time.Hour)
	writes := make(chan error, 1)
	stop := make(chan struct{})
	var written atomic.Int64
	go func() {
		for index := 0; ; index++ {
			select {
			case <-stop:
				writes <- nil
				return
			default:
			}
			body := tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("comment-%d", index)}, Body: "written during retention"}
			response := performHubAPIRequest(t, a.service, http.MethodPost, a.base+"/work-items/"+string(a.issue.WorkItemID)+"/comments", a.token, body)
			if response.Code != http.StatusOK {
				writes <- fmt.Errorf("comment write status %d: %s", response.Code, response.Body.String())
				return
			}
			written.Add(1)
		}
	}()
	var removed []int64
	previous := int64(0)
	for range 4 {
		if err := a.service.maintainNativeRetention(t.Context(), a.service.config.now()); err != nil {
			t.Fatal(err)
		}
		summary, err := readEventCompaction(t.Context(), a.service.database.db, a.scope.organization, a.scope.project, "work_item", string(a.issue.WorkItemID))
		if err != nil {
			t.Fatal(err)
		}
		removed = append(removed, summary.CompactedActivityEvents-previous)
		previous = summary.CompactedActivityEvents
	}
	close(stop)
	if err := <-writes; err != nil {
		t.Fatal(err)
	}
	if written.Load() == 0 {
		t.Fatal("no request write completed while retention passes ran")
	}
	if want := []int64{eventCompactionBatch, eventCompactionBatch, int64(backlog) + 4 - 2*eventCompactionBatch, 0}; !slices.Equal(removed, want) {
		t.Fatalf("removed per pass=%v want %v", removed, want)
	}
	if counts := a.runEvents(t); counts["run.observed"] != backlog+1 || counts["run.checkpointed"] != 1 {
		t.Fatalf("history after backfill=%v", counts)
	}
}
