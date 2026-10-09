package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// writeLoadSession is one claimed attempt streaming into its linked
// conversation, the shape a running agent session puts on the Hub.
type writeLoadSession struct {
	identity     map[string]any
	conversation string
	item         string
}

type writeLoadFixture struct {
	nativeFixture
	worker   string
	runners  []runnerFixture
	sessions []writeLoadSession
}

// newWriteLoadFixture builds a hosted tenant with the given history, runner
// count and streaming sessions.
func newWriteLoadFixture(t *testing.T, history, runners, sessions int) *writeLoadFixture {
	t.Helper()
	config := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Conversation: &ConversationConfig{Enabled: true, QuestionTimeout: time.Hour}}
	f := newNativeFixture(t, openTestService(t, config), "", "write-load")
	policy := hubTestPolicy()
	approveHubTestPolicy(t, f.service, f.base+"/policy", policy)
	d := f.service.database
	d.hostedOrganization = f.project.OrganizationID
	plans := capacityHostedPlans()
	plans.Base = PlanReference{ID: "scale", Version: 1}
	if err := d.configureHostedPlans(t.Context(), &HostedConfig{Plans: &plans}); err != nil {
		t.Fatal(err)
	}
	fixture := &writeLoadFixture{nativeFixture: f, worker: f.worker(t, "write-load-worker")}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", fixture.worker, map[string]any{"id": "write-load-machine", "hostname": "fixture", "display_name": "Fixture", "version": "test", "capacity": sessions}), http.StatusOK)
	seedWriteLoadHistory(t, f, history)
	for range runners {
		r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Collaborate, runnerauth.Events)
		r.enroll(t)
		fixture.runners = append(fixture.runners, r)
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v1/tokens", testHubAdminToken, map[string]any{"name": "write-load-owner", "scope": "operator"})
	requireNativeStatus(t, response, http.StatusCreated)
	var owner tokenResponse
	decodeHubResponse(t, response, &owner)
	for index := range sessions {
		issue := f.create(t, fmt.Sprintf("write-load-%d", index))
		claim := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", fixture.worker, tracker.NativeClaim{PolicyID: policy.ID, WorkItemID: issue.WorkItemID, MachineID: "write-load-machine", SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
		requireNativeStatus(t, claim, http.StatusOK)
		var lease tracker.NativeLease
		decodeHubResponse(t, claim, &lease)
		attempt, run := newNativeID("attempt"), newNativeID("run")
		event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("start")}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: lease.ID, FencingToken: lease.FencingToken, RunID: run, AttemptID: attempt, PolicyID: policy.ID}}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", fixture.worker, event), http.StatusOK)
		now := time.Now().UTC()
		record := conversationRecord{
			ID: conversation.NewConversationID(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID,
			OwnerPrincipalID: owner.ID, OwnerSubject: "owner@example.test", Title: "load", Visibility: conversation.VisibilityShared, Status: conversation.StatusActive,
			WorkItemID: string(issue.WorkItemID), LinkedAt: &now,
			Execution: conversation.Execution{Status: conversation.ExecutionWaitingForRunner, UpdatedAt: now},
			CreatedAt: now, UpdatedAt: now,
		}
		tx, err := d.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.conversations.store.createConversation(t.Context(), tx, &record); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		identity := map[string]any{"lease_id": lease.ID, "fencing_token": lease.FencingToken, "attempt_id": attempt}
		bind := map[string]any{"lease_id": lease.ID, "fencing_token": lease.FencingToken, "attempt_id": attempt, "run_id": run, "capabilities": map[string]bool{"steer": true, "interrupt": true, "answer": true, "continue": true}}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/conversation/bind", fixture.worker, bind), http.StatusOK)
		session := writeLoadSession{identity: identity, conversation: record.ID, item: string(issue.WorkItemID)}
		requireLoadStatus(t, fixture.turnEvents(t, session, map[string]any{"type": "turn_started", "thread_id": "thread-" + record.ID, "turn_id": "turn-" + record.ID}), http.StatusAccepted)
		fixture.sessions = append(fixture.sessions, session)
	}
	return fixture
}

// seedWriteLoadHistory appends accumulated collaboration history to one
// issue, the history that grows with a tenant's age.
func seedWriteLoadHistory(t *testing.T, f nativeFixture, history int) {
	t.Helper()
	if history == 0 {
		return
	}
	issue := f.create(t, "write-load-history")
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var sequence int64
	if err := tx.QueryRowContext(t.Context(), "SELECT coalesce(max(sequence), 0) FROM collaboration_events WHERE work_item_id = ?", issue.WorkItemID).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	statement, err := tx.PrepareContext(t.Context(), `INSERT INTO collaboration_events (id, organization_id, project_id, work_item_id, sequence, type, schema_version, actor_json, data_json, recorded_at)
VALUES (?, ?, ?, ?, ?, 'issue.edited', 1, '{"kind":"human","principal_id":"history"}', ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = statement.Close() }()
	body := `{"revision":"1","body":"` + strings.Repeat("history ", 64) + `"}`
	recorded := formatHubTime(time.Now().UTC())
	for index := range history {
		if _, err := statement.ExecContext(t.Context(), newNativeID("evt"), f.project.OrganizationID, f.project.ID, issue.WorkItemID, sequence+int64(index)+1, body, recorded); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func (f *writeLoadFixture) heartbeat(t *testing.T, runner runnerFixture) int {
	t.Helper()
	return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential,
		map[string]any{"backend_isolation": runner.redemption.BackendIsolation, "display_name": "runner", "capacity": 8, "version": "test", "provider_reports": []any{capacityReport(time.Now().UTC())}}).Code
}

func (f *writeLoadFixture) turnEvents(t *testing.T, session writeLoadSession, events ...map[string]any) *responseRecorderCode {
	t.Helper()
	body := map[string]any{}
	for key, value := range session.identity {
		body[key] = value
	}
	body["events"] = events
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+session.conversation+"/turn-events", f.worker, body)
	return &responseRecorderCode{Code: response.Code, Body: response.Body.String()}
}

type responseRecorderCode struct {
	Code int
	Body string
}

func requireLoadStatus(t *testing.T, response *responseRecorderCode, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d: %s", response.Code, status, response.Body)
	}
}

func medianDuration(t *testing.T, values []time.Duration) time.Duration {
	t.Helper()
	if len(values) == 0 {
		t.Fatal("cannot measure median duration without samples")
		return 0
	}
	slices.Sort(values)
	return values[len(values)/2]
}

// serialWriteCost measures uncontended heartbeat and streamed-delta cost:
// the writer time one request holds, which must not grow with tenant history
// or with how long the streamed message already is.
func serialWriteCost(t *testing.T, f *writeLoadFixture, deltas int) (heartbeat, delta time.Duration) {
	t.Helper()
	heartbeats := make([]time.Duration, 0, 30)
	streamed := make([]time.Duration, 0, 30)
	for range 30 {
		started := time.Now()
		if code := f.heartbeat(t, f.runners[0]); code != http.StatusOK {
			t.Fatalf("heartbeat status = %d", code)
		}
		heartbeats = append(heartbeats, time.Since(started))
	}
	chunk := strings.Repeat("streamed agent output ", 48)
	for index := range deltas {
		started := time.Now()
		requireLoadStatus(t, f.turnEvents(t, f.sessions[0], map[string]any{"type": "delta", "provider_item_id": "item-long", "text": chunk}), http.StatusAccepted)
		if index >= deltas-30 {
			streamed = append(streamed, time.Since(started))
		}
	}
	return medianDuration(t, heartbeats), medianDuration(t, streamed)
}

// TestRunnerWritesStayBoundedAsTenantsGrow reproduces the 2026-10-08
// production write latency: heartbeats and streamed turn events whose cost
// grew with accumulated history and message length while they held the
// tenant's single writer.
func TestRunnerWritesStayBoundedAsTenantsGrow(t *testing.T) {
	if testing.Short() {
		t.Skip("runner write load reproduction")
	}
	t.Run("serial cost does not grow with history or message length", func(t *testing.T) {
		small := newWriteLoadFixture(t, 5000, 1, 1)
		smallHeartbeat, shortDelta := serialWriteCost(t, small, 30)
		large := newWriteLoadFixture(t, 50000, 1, 1)
		largeHeartbeat, longDelta := serialWriteCost(t, large, 300)
		t.Logf("history=5000 heartbeat_p50=%s delta_p50(message≈30KB)=%s", smallHeartbeat, shortDelta)
		t.Logf("history=50000 heartbeat_p50=%s delta_p50(message≈300KB)=%s", largeHeartbeat, longDelta)
		if largeHeartbeat > 2*smallHeartbeat+5*time.Millisecond {
			t.Errorf("heartbeat cost grew with history: %s at 5000 events, %s at 50000", smallHeartbeat, largeHeartbeat)
		}
		if longDelta > 2*shortDelta+5*time.Millisecond {
			t.Errorf("delta cost grew with message length: %s short, %s long", shortDelta, longDelta)
		}
	})
	for _, test := range []struct {
		name     string
		sessions int
		runners  int
	}{
		{name: "twelve sessions on two runners", sessions: 12, runners: 2},
		{name: "fifty sessions on eight runners", sessions: 50, runners: 8},
		{name: "one hundred sessions on sixteen runners", sessions: 100, runners: 16},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newWriteLoadFixture(t, 50000, test.runners, test.sessions)
			ctx, stop := context.WithTimeout(t.Context(), 8*time.Second)
			defer stop()
			var mu sync.Mutex
			latencies := map[string][]time.Duration{}
			var failures []string
			record := func(route string, elapsed time.Duration, failure string) {
				mu.Lock()
				defer mu.Unlock()
				latencies[route] = append(latencies[route], elapsed)
				if failure != "" {
					failures = append(failures, failure)
				}
			}
			var workers sync.WaitGroup
			for _, runner := range f.runners {
				workers.Go(func() {
					for ctx.Err() == nil {
						started := time.Now()
						code := f.heartbeat(t, runner)
						failure := ""
						if code != http.StatusOK {
							failure = fmt.Sprintf("heartbeat %d", code)
						}
						record("heartbeat", time.Since(started), failure)
						time.Sleep(500 * time.Millisecond)
					}
				})
			}
			for index, session := range f.sessions {
				workers.Go(func() {
					chunk := strings.Repeat("output ", 64)
					for ctx.Err() == nil {
						started := time.Now()
						response := f.turnEvents(t, session,
							map[string]any{"type": "delta", "provider_item_id": fmt.Sprintf("item-%d", index), "text": chunk},
							map[string]any{"type": "delta", "provider_item_id": fmt.Sprintf("item-%d", index), "text": chunk})
						failure := ""
						if response.Code != http.StatusAccepted {
							failure = fmt.Sprintf("turn-events %d %s", response.Code, response.Body)
						}
						record("turn-events", time.Since(started), failure)
						time.Sleep(200 * time.Millisecond)
					}
				})
			}
			workers.Wait()
			for _, route := range []string{"heartbeat", "turn-events"} {
				values := latencies[route]
				if len(values) == 0 {
					t.Fatalf("no %s requests completed", route)
				}
				slices.Sort(values)
				t.Logf("sessions=%d runners=%d route=%s requests=%d p50=%s p95=%s max=%s", test.sessions, test.runners, route, len(values), values[len(values)/2], values[len(values)*95/100], values[len(values)-1])
			}
			if len(failures) != 0 {
				t.Fatalf("requests failed: %v", failures[:min(len(failures), 5)])
			}
		})
	}
}
