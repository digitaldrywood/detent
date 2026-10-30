package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type progressPublicationContextKey struct{}

type progressPublicationConnector struct {
	timedConnector
	before  bool
	observe func() error
}

func (c progressPublicationConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.timedConnector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &progressPublicationConn{timedConn: conn.(*timedConn), probe: c}, nil
}

type progressPublicationConn struct {
	*timedConn
	probe progressPublicationConnector
}

func (c *progressPublicationConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	count, marked := ctx.Value(progressPublicationContextKey{}).(*int)
	if !marked || !strings.HasPrefix(query, "SELECT progress_evidence_revision FROM issues") && !strings.HasPrefix(query, "SELECT event_sequence + progress_evidence_revision FROM issues") {
		return c.timedConn.QueryContext(ctx, query, args)
	}
	*count++
	if *count != 2 {
		return c.timedConn.QueryContext(ctx, query, args)
	}
	if c.probe.before {
		if err := c.probe.observe(); err != nil {
			return nil, err
		}
		return c.timedConn.QueryContext(ctx, query, args)
	}
	rows, err := c.timedConn.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return &progressPublicationRows{Rows: rows, observe: c.probe.observe}, nil
}

type progressPublicationRows struct {
	driver.Rows
	observe func() error
}

func (r *progressPublicationRows) Close() error {
	if err := r.Rows.Close(); err != nil {
		return err
	}
	return r.observe()
}

type progressPublicationTransport func(*http.Request) (*http.Response, error)

func (f progressPublicationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type progressPublicationAgent struct {
	runner.AgentBackend
	attempt string
}

func (progressPublicationAgent) SupportsReadOnlyInvestigation() bool { return true }

func (a progressPublicationAgent) RunTurn(_ context.Context, request runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	if !strings.Contains(request.Prompt, "TestExample: expected repaired value, got old value") || !request.ReadOnly || request.TextOnly || !request.IsolatedTools {
		return runner.AgentTurnResult{}, errors.New("progress turn did not receive retained failure output and read-only investigation access")
	}
	if err := update(runner.AgentUpdate{Type: runner.AgentUpdateTokenUsage, Tokens: runner.AgentTokenUsage{InputTokens: 10, OutputTokens: 1}}); err != nil {
		return runner.AgentTurnResult{}, err
	}
	data, err := json.Marshal(progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "implement", Diagnosis: a.attempt + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Repair the failed source", Evidence: []string{a.attempt}})
	if err != nil {
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, ItemID: "decision", Delta: string(data)})
}

type progressPublicationWorkspace struct {
	workspace.Backend
	path string
}

func (w progressPublicationWorkspace) Head(_ context.Context, _ workspace.Info, issue workspace.Issue) (string, error) {
	return issue.PullRequestHeadSHA, nil
}
func (w progressPublicationWorkspace) VerifyReviewTree(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}
func (w progressPublicationWorkspace) Existing(workspace.Issue) (workspace.Info, error) {
	return workspace.Info{Path: w.path}, nil
}

func (w progressPublicationWorkspace) Create(context.Context, workspace.Issue) (workspace.Info, error) {
	return workspace.Info{Path: w.path}, nil
}

func TestNativeProgressExecutionPublication(t *testing.T) {
	for _, test := range []struct {
		name         string
		attempts     int
		staleRunning int
	}{
		{"sequential", 1, 0},
		{"sequential at attempt bound", progresscheck.MaxAttempts + 1, 0},
		{"stale running attempt inside history bound", progresscheck.MaxAttempts + 1, 1},
		{"multiple stale running attempts", progresscheck.MaxAttempts + 3, 3},
		{"construction guard", 1, 0},
		{"transaction guard", 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			boundary := test.name
			overlap := strings.HasSuffix(boundary, "guard")
			f := newDispatchGuardFixture(t)
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/changes", testHubAdminToken, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "progress-source"}, Title: "Retained progress source"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			input := changeTestInput()
			bundle := changeTestSource(t, &input)
			response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/changes/"+change.ID+"/versions", testHubAdminToken, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "progress-version"}, ChangeVersionInput: input, SourceBundle: bundle})
			requireNativeStatus(t, response, http.StatusOK)
			for i := range test.attempts {
				if i > 0 {
					f.claim(t)
				}
				event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("finish")}, Type: "run.finished", SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: 2, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, Outcome: "failed"}}
				event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: f.service.config.now(), Validation: &gate.CommandResult{Command: "go test ./example", ExitCode: 2, Stage: gate.StageSourceFinalization, HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), DurationNS: 1, Output: "TestExample: expected repaired value, got old value"}}
				if i == 0 || i > test.staleRunning {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, event), http.StatusOK)
				}
				f.release(t)
			}
			if test.staleRunning > 0 {
				plan := explainQueryPlan(t, f.service.database.reader, nativeProgressAttemptsSQL, []any{f.project.OrganizationID, f.project.ID, f.issue.WorkItemID, progresscheck.MaxAttempts})
				if joined := strings.Join(plan, "\n"); !strings.Contains(joined, "USING INDEX native_attempts_progress_terminal") || strings.Contains(joined, "TEMP B-TREE") {
					t.Fatalf("terminal history lookup is not indexed and bounded: %v", plan)
				}
			}
			var deferred *http.Request
			var usage []tracker.NativeUsage
			var publicationStatuses []int
			transport := progressPublicationTransport(func(r *http.Request) (*http.Response, error) {
				if overlap && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events") {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					var event tracker.NativeRunEvent
					if err := json.Unmarshal(body, &event); err != nil {
						return nil, err
					}
					if event.Type == "run.observed" && len(event.Data.Usage) > 0 {
						deferred, usage = r.Clone(t.Context()), event.Data.Usage
						deferred.Body = io.NopCloser(bytes.NewReader(body))
						return nil, context.DeadlineExceeded
					}
				}
				publication := r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/progress")
				if publication {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					var input progresscheck.Publication
					if err := json.Unmarshal(body, &input); err != nil {
						return nil, err
					}
					current := f.reload(t)
					if input.ExpectedRevision != current.Revision {
						t.Fatalf("client published stale item revision: got %d want %d", input.ExpectedRevision, current.Revision)
					}
					count := 0
					r = r.WithContext(context.WithValue(r.Context(), progressPublicationContextKey{}, &count))
				}
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, r)
				if publication {
					publicationStatuses = append(publicationStatuses, response.Code)
				}
				return response.Result(), nil
			})
			client, err := hubclient.New(hubclient.Config{URL: "http://hub.example.test", TokenSource: func() string { return f.worker }, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{OrganizationID: f.project.OrganizationID, NativeProjects: map[string]tracker.ProjectID{"local": f.project.ID}, Machine: hubclient.Machine{ID: "conversation-machine", Hostname: "fixture", Capacity: 1, Version: "test"}, LeaseTTL: 90 * time.Second, HeartbeatInterval: time.Second, LeaseHold: func(context.Context) (func(), error) { return func() {}, nil }})
			if err != nil {
				t.Fatal(err)
			}
			candidates, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: hubTestPolicy(), ProjectID: "local", WorkflowStates: []string{"Todo", "In Progress"}})
			if err != nil || len(candidates) != 1 {
				t.Fatalf("claim candidates=%v err=%v", candidates, err)
			}
			claim, err := scheduler.AdoptClaim(t.Context(), candidates[0], time.Now())
			if err != nil {
				t.Fatal(err)
			}
			execution := scheduler.RunExecution(claim.Issue.ID)
			beforeTransition, _, err := f.service.nativeProgressRequest(t.Context(), f.scope(), string(f.issue.WorkItemID), f.policy, "conversation-machine")
			if err != nil {
				t.Fatal(err)
			}
			current := f.reload(t)
			native, err := client.Native(f.project.OrganizationID, f.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			transitioned, err := native.Transition(t.Context(), current.WorkItemID, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("transition")}, ExpectedRevision: current.Revision, State: "In Progress", Reason: "worker_progress", ReasonDetail: "dispatch start"})
			if err != nil || transitioned.Revision != current.Revision+1 {
				t.Fatalf("dispatch transition: issue=%+v err=%v", transitioned, err)
			}
			request, owner, err := scheduler.RetryProgress(t.Context(), claim.Issue)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeTransition, request) {
				t.Fatalf("workflow transition changed progress evidence: before=%+v after=%+v", beforeTransition, request)
			}
			var attempts int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempts WHERE work_item_id=?", f.issue.WorkItemID).Scan(&attempts); err != nil || attempts != test.attempts {
				t.Fatalf("evidence fetch started the check execution: attempts=%d err=%v", attempts, err)
			}
			if len(request.Attempts) != min(test.attempts-test.staleRunning, progresscheck.MaxAttempts) {
				t.Fatalf("evidence omitted terminal attempts: got %d", len(request.Attempts))
			}
			worker, err := runner.NewRunner(runner.Dependencies{Workflow: config.Workflow{Config: config.Config{Agents: config.Agents{Routes: []config.AgentRoute{{Name: "implementation", Backend: "codex", Model: "code-model", Default: true}}}}}, Workspace: progressPublicationWorkspace{path: t.TempDir()}, AgentBackend: progressPublicationAgent{attempt: f.attempt}})
			if err != nil {
				t.Fatal(err)
			}
			decision, err := worker.CheckProgress(t.Context(), runner.RunRequest{Issue: claim.Issue, Mode: runner.RunModeImplement, Execution: execution}, request)
			if err != nil {
				t.Fatal(err)
			}
			if overlap && (deferred == nil || len(usage) != 1 || usage[0].Input != 10 || usage[0].Output != 1) {
				t.Fatalf("real check did not defer its usage: request=%v usage=%+v", deferred, usage)
			}
			fresh, _, err := scheduler.RetryProgress(t.Context(), claim.Issue)
			if err != nil {
				t.Fatal(err)
			}
			observed := 0
			if overlap {
				original := f.service.database.reader
				f.service.database.reader = sql.OpenDB(progressPublicationConnector{timedConnector: timedConnector{dsn: sqliteReaderDSN(f.service.database.path, f.service.config.BusyTimeout), driver: original.Driver()}, before: boundary == "construction guard", observe: func() error {
					observed++
					if observed == 1 {
						response := httptest.NewRecorder()
						f.service.Handler().ServeHTTP(response, deferred)
						if response.Code != http.StatusOK {
							return fmt.Errorf("deliver deferred usage: status %d", response.Code)
						}
					}
					return nil
				}})
				t.Cleanup(func() { _ = original.Close() })
			}
			id, err := request.ID()
			if err != nil {
				t.Fatal(err)
			}
			record, err := owner.Publish(t.Context(), progresscheck.Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: decision})
			if err != nil || record.CommentID == "" {
				freshID, idErr := fresh.ID()
				t.Fatalf("own execution invalidated publication: statuses=%v attempts=%d->%d evidence_revision=%s->%s request_id=%s->%s id_error=%v observations=%d err=%v", publicationStatuses, len(request.Attempts), len(fresh.Attempts), request.EvidenceRevision, fresh.EvidenceRevision, id, freshID, idErr, observed, err)
			}
			if !reflect.DeepEqual(request, fresh) {
				t.Fatalf("start or usage changed request: before=%+v after=%+v", request, fresh)
			}
			if len(publicationStatuses) != 1 || publicationStatuses[0] != http.StatusOK {
				t.Fatalf("own usage conflicted before publication retry: statuses=%v", publicationStatuses)
			}
			if overlap && observed != 1 {
				t.Fatalf("publication observed %d boundary events", observed)
			}
			var attemptID string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT id FROM native_attempts WHERE work_item_id=? AND status='running' ORDER BY fencing_token DESC LIMIT 1", f.issue.WorkItemID).Scan(&attemptID); err != nil {
				t.Fatal(err)
			}
			attempt, err := readNativeAttemptRecord(f.service.runDetailContext(t.Context()), f.service.database.reader, f.scope(), string(f.issue.WorkItemID), attemptID, f.now)
			if err != nil || len(attempt.Usage) != 1 || attempt.Usage[0].Input != 10 || attempt.Usage[0].Output != 1 {
				t.Fatalf("real check did not publish its usage: attempt=%+v err=%v", attempt, err)
			}
		})
	}
}

func TestNativeProgressRequestReplayAndFencing(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name           string
		changed        bool
		released       bool
		editedOlder    bool
		fullContext    bool
		legacy         bool
		active         bool
		emptyLanding   bool
		success        bool
		noProduct      bool
		blocked        bool
		staleRevision  bool
		evidenceChange string
	}{
		{name: "decision comment does not create new evidence"},
		{name: "successful empty landing checks and replays decision", emptyLanding: true},
		{name: "decision without diagnosis is refused on publication", legacy: true},
		{name: "succeeded without change is checked", success: true},
		{name: "succeeded without work product is checked", success: true, noProduct: true},
		{name: "succeeded with source recovery blocker is checked", success: true, blocked: true},
		{name: "stale item revision rejects publication", staleRevision: true},
		{name: "full context remains stable after publication", fullContext: true},
		{name: "admitted active worker still reads prior failures", active: true},
		{name: "new human input rejects stale decision", changed: true},
		{name: "edited older human input rejects stale decision", changed: true, editedOlder: true},
		{name: "released lease cannot publish decision", released: true},
		{name: "edit beyond truncated context rejects stale decision", changed: true, evidenceChange: "issue.edited"},
		{name: "checkpoint rejects stale decision", changed: true, active: true, evidenceChange: "run.checkpointed"},
		{name: "terminal attempt rejects stale decision", changed: true, active: true, evidenceChange: "run.finished"},
		{name: "source change rejects stale decision", changed: true, evidenceChange: "change.created"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDispatchGuardFixture(t)
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("finish")}, Type: "run.finished", SchemaVersion: 1, Data: tracker.NativeRunData{
				Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "failed",
				TerminalFailure: &tracker.NativeTerminalFailure{Error: "422: landing requires security audit evidence for its exact base and head", ErrorClass: "publication", Stage: "publication", ObservedAt: f.now},
			}}
			if test.emptyLanding {
				event.Data.Outcome = "succeeded"
				event.Data.TerminalFailure = nil
				event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: f.now, Landing: &tracker.NativeLandingReceipt{RefusalKind: "nothing_to_land", ObservedAt: f.now}}
			}
			if test.success {
				event.Data.Outcome = "succeeded"
				event.Data.TerminalFailure = nil
				if !test.noProduct {
					event.Data.Finalization = &tracker.NativeFinalization{Changed: false, Settled: true, ObservedAt: f.now}
				}
				if test.blocked {
					event.Data.Finalization.Changed = true
					event.Data.Disposition = &tracker.NativeDisposition{Status: "blocked", ReasonCode: "source_recovery_required"}
				}
			}
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			if test.success {
				checkpoint := event
				checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Outcome = "run.checkpointed", newNativeID("checkpoint"), ""
				checkpoint.Data.Finalization, checkpoint.Data.Disposition = nil, nil
				checkpoint.Data.Handoff = nativeTestCheckpoint()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, checkpoint), http.StatusOK)
				event.Data.Sequence++
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, event), http.StatusOK)
			f.release(t)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, tracker.NativeClaim{PolicyID: f.policy, WorkItemID: f.issue.WorkItemID, MachineID: "conversation-machine", SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &f.lease)
			params := url.Values{"policy": {f.policy}, "instance": {string(f.lease.MachineID)}}
			read := func() nativeProgressView {
				response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/progress?"+params.Encode(), f.worker, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var view nativeProgressView
				decodeHubResponse(t, response, &view)
				return view
			}
			started := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("start")}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: newNativeID("run"), AttemptID: newNativeID("attempt"), PolicyID: f.policy}}
			if test.active {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, started), http.StatusOK)
			}
			longBody := strings.Repeat("Acceptance context. ", 200) + strings.Replace(issueContractTestSections, "Complete the requested work.", "Required result: original", 1)
			if test.evidenceChange == "issue.edited" {
				response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.worker, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var current tracker.NativeIssue
				decodeHubResponse(t, response, &current)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path, testHubAdminToken, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("long-body")}, ExpectedRevision: current.Revision, Body: &longBody}), http.StatusOK)
			}
			if test.fullContext {
				for i := range progresscheck.MaxComments {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", testHubAdminToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("full-%d", i)}, Body: "Recorded evidence"}), http.StatusOK)
				}
			}
			var older tracker.NativeComment
			if test.editedOlder {
				response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", testHubAdminToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "older-input"}, Body: "Original human input"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &older)
				for i := range progresscheck.MaxComments {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", testHubAdminToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("context-%d", i)}, Body: "Later context"}), http.StatusOK)
				}
			}
			view := read()
			if len(view.Request.Attempts) != 1 || view.Request.Attempts[0].ID != f.attempt || (!test.success && !test.emptyLanding && view.Request.Attempts[0].FailureClass != "publication") {
				t.Fatalf("request attempts=%+v", view.Request.Attempts)
			}
			if test.emptyLanding && (view.Request.Attempts[0].Outcome != "success" || view.Request.Attempts[0].LandingRefusal != "nothing_to_land") {
				t.Fatalf("successful landing lost refusal: %+v", view.Request.Attempts[0])
			}
			id, err := view.Request.ID()
			if err != nil {
				t.Fatal(err)
			}
			input := progresscheck.Publication{Mutation: tracker.Mutation{LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, RequestID: id, PolicyID: f.policy, Instance: string(f.lease.MachineID), Decision: progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "implement", Diagnosis: f.attempt + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Produce audit evidence on the current source", Evidence: []string{f.attempt}}}
			if test.legacy {
				input.Decision.Diagnosis, input.Decision.Instruction, input.Decision.Convergence = "", "", ""
			}
			currentResponse := performHubAPIRequest(t, f.service, http.MethodGet, path, f.worker, nil)
			requireNativeStatus(t, currentResponse, http.StatusOK)
			var current tracker.NativeIssue
			decodeHubResponse(t, currentResponse, &current)
			input.ExpectedRevision = current.Revision
			if test.staleRevision {
				input.ExpectedRevision++
			}
			if test.changed {
				if test.evidenceChange == "issue.edited" {
					longBody = strings.Replace(longBody, "Required result: original", "Required result: changed", 1)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path, testHubAdminToken, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("edited-body")}, ExpectedRevision: current.Revision, Body: &longBody}), http.StatusOK)
					fresh := read()
					freshID, err := fresh.Request.ID()
					if err != nil || fresh.Request.Context != view.Request.Context || freshID == id {
						t.Fatalf("truncated edit lost invalidation: same context=%v same ID=%v err=%v", fresh.Request.Context == view.Request.Context, freshID == id, err)
					}
				} else if test.evidenceChange == "change.created" {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/changes", testHubAdminToken, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("source")}, Title: "Updated source"}), http.StatusOK)
				} else if strings.HasPrefix(test.evidenceChange, "run.") {
					started.Type, started.IdempotencyKey, started.Data.Sequence = test.evidenceChange, newNativeID("evidence"), 2
					if test.evidenceChange == "run.finished" {
						started.Data.Outcome = "failed"
					} else {
						started.Data.Handoff = nativeTestCheckpoint()
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, started), http.StatusOK)
				} else if test.editedOlder {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path+"/comments/"+older.ID, testHubAdminToken, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edited-input"}, ExpectedRevision: older.Revision, Body: "The audit protocol changed"}), http.StatusOK)
				} else {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", testHubAdminToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "human-input"}, Body: "The audit protocol changed"}), http.StatusOK)
				}
			}
			if test.released {
				f.release(t)
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/progress", f.worker, input)
			if test.changed || test.released || test.staleRevision || test.legacy {
				if response.Code == http.StatusOK {
					t.Fatal("stale evidence or released authority published decision")
				}
				var count int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_progress_decisions WHERE work_item_id=?", f.issue.WorkItemID).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatal("rejected decision persisted")
				}
				return
			}
			requireNativeStatus(t, response, http.StatusOK)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/progress", f.worker, input), http.StatusOK)
			replay := read()
			replayID, err := replay.Request.ID()
			if err != nil || replayID != id || replay.Record == nil {
				t.Fatalf("decision self-invalidated: id=%s want=%s record=%+v err=%v", replayID, id, replay.Record, err)
			}
			var comments int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_comments WHERE work_item_id=?", f.issue.WorkItemID).Scan(&comments); err != nil {
				t.Fatal(err)
			}
			wantComments := 1
			if test.fullContext {
				wantComments += progresscheck.MaxComments
			}
			if comments != wantComments {
				t.Fatalf("replay created %d comments", comments)
			}
		})
	}
}
