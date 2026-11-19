package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type progressPublicationContextKey struct{}

type progressPublicationRepository struct {
	*postgres.Repository
	skipWrites int
	before     bool
	fired      bool
	observe    func() error
}

func (r *progressPublicationRepository) ReadTenantSnapshot(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, use func(tenantstore.Reader) error) error {
	if err := r.Repository.ReadTenantSnapshot(ctx, organization, workload, use); err != nil {
		return err
	}
	reads, marked := ctx.Value(progressPublicationContextKey{}).(*int)
	if !marked {
		return nil
	}
	(*reads)++
	if !r.before || r.fired {
		return nil
	}
	r.fired = true
	return r.observe()
}

func (r *progressPublicationRepository) beforeWrite(ctx context.Context) error {
	reads, marked := ctx.Value(progressPublicationContextKey{}).(*int)
	if !marked || r.before || r.fired || *reads < 1 {
		return nil
	}
	if r.skipWrites > 0 {
		r.skipWrites--
		return nil
	}
	r.fired = true
	return r.observe()
}
func (r *progressPublicationRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, use func(tenantstore.Writer) error) error {
	if err := r.beforeWrite(ctx); err != nil {
		return err
	}
	return r.Repository.WriteTenant(ctx, organization, workload, use)
}

type progressPublicationTransport func(*http.Request) (*http.Response, error)

func (f progressPublicationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type progressPublicationAgent struct {
	runner.AgentBackend
	attempt  string
	recordID string
	retrieve bool
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

func (a progressPublicationAgent) RunTurnWithTools(ctx context.Context, request runner.AgentTurnRequest, tools []runner.AgentTool, handle runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	if a.retrieve {
		bounded := request.ToolBudget.Handler(tools, handle)
		cursor := ""
		found := false
		var evidence progresscheck.Request
		input := strings.SplitN(request.Prompt, "Evidence:\n", 2)
		if len(input) != 2 {
			return runner.AgentTurnResult{}, errors.New("missing request evidence")
		}
		if err := json.NewDecoder(strings.NewReader(input[1])).Decode(&evidence); err != nil {
			return runner.AgentTurnResult{}, err
		}
		for range progresscheck.MaxToolCalls - 3 {
			args, err := json.Marshal(operatortool.WorkReadRequest{ProjectID: evidence.ProjectID, Reference: evidence.IssueID, View: "recovery", Cursor: cursor})
			if err != nil {
				return runner.AgentTurnResult{}, err
			}
			result, err := bounded(ctx, runner.AgentToolCall{Name: operatortool.WorkItem, Arguments: args})
			if err != nil {
				return runner.AgentTurnResult{}, err
			}
			var page operatortool.WorkReadResult[tracker.NativeRecoveryPage]
			if err := json.Unmarshal([]byte(result.Content), &page); err != nil {
				return runner.AgentTurnResult{}, err
			}
			found = page.Data.RecordID == a.recordID
			cursor = page.Data.NextCursor
			if found || cursor == "" {
				break
			}
		}
		if !found {
			return runner.AgentTurnResult{}, errors.New("citation was not returned by recovery reads")
		}
	}
	return a.RunTurn(ctx, request, update)
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
	t.Parallel()
	for _, test := range []struct {
		name         string
		attempts     int
		staleRunning int
	}{
		{"sequential", 1, 0},
		{"retrieved comment", 1, 0},
		{"retrieved older attempt", progresscheck.MaxAttempts + 1, 0},
		{"retrieved command receipt", 1, 0},
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
			admin := hostedPostgresAdmin(t, f.service)
			scope := analyticsTestScope(t, f.nativeFixture)
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/changes", testHubAdminToken, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "progress-source"}, Title: "Retained progress source"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			input := changeTestInput()
			bundle := changeTestSource(t, &input)
			response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/changes/"+change.ID+"/versions", testHubAdminToken, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "progress-version"}, ChangeVersionInput: input, SourceBundle: bundle})
			requireNativeStatus(t, response, http.StatusOK)
			commandReceipt := hubapi.NewID("receipt")
			var oldestAttempt string
			for i := range test.attempts {
				if i > 0 {
					f.claim(t)
				}
				if i == 0 {
					oldestAttempt = f.attempt
				}
				event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("finish")}, Type: "run.finished", SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: 2, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, Outcome: "failed"}}
				event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: f.service.config.Clock(), Validation: &gate.CommandResult{ReceiptID: commandReceipt, Command: "go test ./example", ExitCode: 2, Stage: gate.StageSourceFinalization, HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), DurationNS: 1, Output: "TestExample: expected repaired value, got old value"}}
				if i == 0 || i > test.staleRunning {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, event), http.StatusOK)
				}
				f.release(t)
			}

			citation := f.attempt
			retrieve := strings.HasPrefix(test.name, "retrieved")
			if test.name == "retrieved older attempt" {
				citation = oldestAttempt
			}
			if test.name == "retrieved comment" {
				for i := range progresscheck.MaxComments + 2 {
					response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", testHubAdminToken, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("citation-comment-%d", i)}, Body: "Retained diagnostic evidence"})
					requireNativeStatus(t, response, http.StatusOK)
					if i == 0 {
						var comment tracker.NativeComment
						decodeHubResponse(t, response, &comment)
						citation = comment.ID
					}
				}
			}
			recordID := citation
			if test.name == "retrieved command receipt" {
				citation = commandReceipt
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
			scheduler, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{Now: f.service.config.Clock, OrganizationID: f.project.OrganizationID, NativeProjects: map[string]tracker.ProjectID{"local": f.project.ID}, Machine: hubclient.Machine{ID: f.runner.binding.MachineID, Hostname: "fixture", Capacity: 1, Version: "test", BackendIsolation: f.runner.redemption.BackendIsolation}, LeaseTTL: 90 * time.Second, HeartbeatInterval: time.Second, LeaseHold: func(context.Context) (func(), error) { return func() {}, nil }})
			if err != nil {
				t.Fatal(err)
			}
			candidates, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: hubTestPolicy(), ProjectID: "local", WorkflowStates: []string{"Todo", "In Progress"}})
			if err != nil || len(candidates) != 1 {
				t.Fatalf("claim candidates=%v err=%v", candidates, err)
			}
			claim, err := scheduler.AdoptClaim(t.Context(), candidates[0], f.service.config.Clock())
			if err != nil {
				t.Fatal(err)
			}
			execution := scheduler.RunExecution(claim.Issue.ID)
			beforeTransition, _, err := f.service.nativeProgressRequest(t.Context(), scope, string(f.issue.WorkItemID), f.policy, string(f.runner.binding.MachineID))
			if err != nil {
				t.Fatal(err)
			}
			current := f.reload(t)
			native, err := client.Native(f.project.OrganizationID, f.project.ID)
			if err != nil {
				t.Fatal(err)
			}
			transitioned, err := native.Transition(t.Context(), current.WorkItemID, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("transition")}, ExpectedRevision: current.Revision, State: "In Progress", Reason: "worker_progress", ReasonDetail: "dispatch start"})
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
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3", scope.Organization, scope.Project, f.issue.WorkItemID).Scan(&attempts); err != nil || attempts != test.attempts {
				t.Fatalf("evidence fetch started the check execution: attempts=%d err=%v", attempts, err)
			}
			if len(request.Attempts) != min(test.attempts-test.staleRunning, progresscheck.MaxAttempts) {
				t.Fatalf("evidence omitted terminal attempts: got %d", len(request.Attempts))
			}
			worker, err := runner.NewRunner(runner.Dependencies{Workflow: config.Workflow{Config: config.Config{Agents: config.Agents{Routes: []config.AgentRoute{{Name: "implementation", Backend: "codex", Model: "code-model", Default: true}}}}}, Workspace: progressPublicationWorkspace{path: t.TempDir()}, AgentBackend: progressPublicationAgent{attempt: citation, recordID: recordID, retrieve: retrieve}})
			if err != nil {
				t.Fatal(err)
			}
			decision, err := worker.CheckProgress(t.Context(), runner.RunRequest{Issue: claim.Issue, Mode: runner.RunModeImplement, Execution: execution}, request)
			if err != nil {
				t.Fatal(err)
			}
			flusher := execution.(interface{ FlushRuntime(context.Context) error })
			flushErr := flusher.FlushRuntime(t.Context())
			if overlap {
				if !errors.Is(flushErr, context.DeadlineExceeded) {
					t.Fatalf("deferred usage delivery error=%v", flushErr)
				}
			} else if flushErr != nil {
				t.Fatal(flushErr)
			}
			if retrieve {
				for _, attempt := range request.Attempts {
					if attempt.ID == citation {
						t.Fatal("retrieved attempt was already in request")
					}
				}
				for _, comment := range request.Comments {
					if comment.ID == citation {
						t.Fatal("retrieved comment was already in request")
					}
				}
				expected := progresscheck.EvidenceReference{ID: citation}
				if test.name == "retrieved command receipt" {
					expected.AttemptID = recordID
				}
				if !reflect.DeepEqual(decision.RetrievedEvidence, []progresscheck.EvidenceReference{expected}) {
					t.Fatalf("retrieved citation missing: %+v", decision)
				}
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
				original, ok := f.service.config.TenantRepository.(*postgres.Repository)
				if !ok {
					t.Fatal("progress publication requires the PostgreSQL owner")
				}
				f.service.config.TenantRepository = &progressPublicationRepository{Repository: original, before: boundary == "construction guard", observe: func() error {
					observed++
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, deferred)
					if response.Code != http.StatusOK {
						return fmt.Errorf("deliver deferred usage: status %d", response.Code)
					}
					return nil
				}}
				t.Cleanup(func() { f.service.config.TenantRepository = original })
			}
			id, err := request.ID()
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "retrieved command receipt" {
				other := (nativeFixture{service: f.service, project: f.project, base: f.base, token: testHubAdminToken}).createIndependent(t, "Example receipt scope")
				for _, invalid := range []struct{ name, citation, item string }{
					{"nonexistent receipt", "receipt-missing", request.IssueID},
					{"cross item receipt", citation, string(other.WorkItemID)},
				} {
					candidateRequest := request
					candidateRequest.IssueID = invalid.item
					candidateID, err := candidateRequest.ID()
					if err != nil {
						t.Fatal(err)
					}
					candidate := decision
					candidate.Evidence = []string{invalid.citation}
					candidate.Diagnosis = invalid.citation + ": assertion still returns the old value"
					candidate.RetrievedEvidence = []progresscheck.EvidenceReference{{ID: invalid.citation, AttemptID: recordID}}
					direct := nativeProgressOwner{service: f.service, scope: scope, request: candidateRequest}
					if _, err := direct.Publish(t.Context(), progresscheck.Record{RequestID: candidateID, ProjectID: candidateRequest.ProjectID, IssueID: candidateRequest.IssueID, EvidenceRevision: candidateRequest.EvidenceRevision, Decision: candidate}); err == nil {
						t.Fatalf("%s was published", invalid.name)
					}
				}
			}
			record, err := owner.Publish(t.Context(), progresscheck.Record{RequestID: id, ProjectID: request.ProjectID, IssueID: request.IssueID, EvidenceRevision: request.EvidenceRevision, Decision: decision})
			if err != nil || record.CommentID == "" {
				freshID, idErr := fresh.ID()
				t.Fatalf("own execution invalidated publication: statuses=%v attempts=%d->%d evidence_revision=%s->%s request_id=%s->%s id_error=%v observations=%d err=%v", publicationStatuses, len(request.Attempts), len(fresh.Attempts), request.EvidenceRevision, fresh.EvidenceRevision, id, freshID, idErr, observed, err)
			}
			if err := record.Validate(request); err != nil {
				t.Fatalf("published decision failed validation: %v", err)
			}
			_, replayOwner, err := scheduler.RetryProgress(t.Context(), claim.Issue)
			if err != nil {
				t.Fatal(err)
			}
			retained, found, err := replayOwner.Load(t.Context(), request.ProjectID, request.IssueID, id)
			if err != nil || !found || !reflect.DeepEqual(record.Decision, retained.Decision) {
				t.Fatalf("publication replay lost citation: record=%+v found=%v error=%v", retained, found, err)
			}
			if err := retained.Validate(request); err != nil {
				t.Fatalf("retained decision failed validation: %v", err)
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
			if err := admin.QueryRowContext(t.Context(), "SELECT a.id FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3 AND a.status='running' ORDER BY a.fencing_token DESC LIMIT 1", scope.Organization, scope.Project, f.issue.WorkItemID).Scan(&attemptID); err != nil {
				t.Fatal(err)
			}
			snapshot, err := f.service.readNativeAttemptSnapshotTenant(t.Context(), scope, string(f.issue.WorkItemID), attemptID)
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := f.service.readNativeAttemptRunTenant(t.Context(), snapshot)
			if err != nil || len(attempt.Usage) != 1 || attempt.Usage[0].Input != 10 || attempt.Usage[0].Output != 1 {
				t.Fatalf("real check did not publish its usage: attempt=%+v err=%v", attempt, err)
			}
		})
	}
}

func TestNativeProgressRequestReplayAndFencing(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name            string
		changed         bool
		released        bool
		editedOlder     bool
		fullContext     bool
		legacy          bool
		active          bool
		emptyLanding    bool
		success         bool
		noProduct       bool
		blocked         bool
		staleRevision   bool
		cleanupReplay   string
		edges           int
		open, olderOpen bool
		evidenceChange  string
	}{
		{name: "decision comment does not create new evidence"},
		{name: "absent open", cleanupReplay: "matrix", open: true},
		{name: "ordinary replay during usage", cleanupReplay: "usage", open: true, active: true},
		{name: "cleanup replay during usage", cleanupReplay: "usage", edges: 1, active: true},
		{name: "absent done", cleanupReplay: "matrix"},
		{name: "one open", cleanupReplay: "matrix", edges: 1, open: true},
		{name: "one done", cleanupReplay: "matrix", edges: 1},
		{name: "several open older open", cleanupReplay: "matrix", edges: 2, open: true, olderOpen: true},
		{name: "several open older done", cleanupReplay: "matrix", edges: 2, open: true},
		{name: "several done older open", cleanupReplay: "matrix", edges: 2, olderOpen: true},
		{name: "several done older done", cleanupReplay: "matrix", edges: 2},
		{name: "cleanup replay rejects new human evidence", cleanupReplay: "human", edges: 1},
		{name: "cleanup replay rejects changed source", cleanupReplay: "source", edges: 1},
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
			scope := analyticsTestScope(t, f.nativeFixture)
			admin := hostedPostgresAdmin(t, f.service)
			event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("finish")}, Type: "run.finished", SchemaVersion: 1, Data: tracker.NativeRunData{
				Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "failed",
				TerminalFailure: &tracker.NativeTerminalFailure{Error: "422: landing requires security audit evidence for its exact base and head", ErrorClass: "publication", Stage: "publication", ObservedAt: f.now},
			}}
			if test.cleanupReplay != "" {
				event.Data.TerminalFailure = &tracker.NativeTerminalFailure{Stage: "implement", Attribution: "instance", ErrorClass: "protocol", Error: "Backend protocol unavailable", ObservedAt: f.now}
			}
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
			var incident tracker.NativeIssue
			const defectKey = "backend-protocol"
			if test.success {
				checkpoint := event
				checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Outcome = "run.checkpointed", hubapi.NewID("checkpoint"), ""
				checkpoint.Data.Finalization, checkpoint.Data.Disposition = nil, nil
				checkpoint.Data.Handoff = nativeTestCheckpoint()
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, checkpoint), http.StatusOK)
				event.Data.Sequence++
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, event), http.StatusOK)
			f.release(t)
			claim := func() {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, tracker.NativeClaim{PolicyID: f.policy, WorkItemID: f.issue.WorkItemID, MachineID: f.runner.binding.MachineID, SessionID: hubapi.NewID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &f.lease)
			}
			claim()
			var olderRepair tracker.NativeWorkItemID
			if test.cleanupReplay != "" {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), scope.Project)
					if err != nil {
						return err
					}
					project.States = append(project.States, tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo"}})
					_, err = w.UpdateProjectRecord(t.Context(), project)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				incidentScope := scope
				actor := scope.Actor()
				actor.Kind, actor.Agent = "machine", "progress-check"
				incidentScope.SourceActor = &actor
				raw, err := f.service.createNativeIssueCommand(t.Context(), incidentScope, tracker.CreateIssue{
					Mutation: tracker.Mutation{IdempotencyKey: "legacy-incident"}, Title: "Backend protocol unavailable", Body: issueContractTestSections, State: "Backlog",
					Provenance: &tracker.Provenance{Provider: "progress-check", ExternalID: defectKey},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &incident); err != nil || incident.WorkItemID == "" {
					t.Fatalf("legacy incident ID=%s: %v", incident.WorkItemID, err)
				}
				setState := func(target *tracker.NativeIssue, state string) {
					response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(target.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: string(target.WorkItemID) + "-" + state}, ExpectedRevision: target.Revision, State: state, Reason: "user_requested"})
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, target)
				}
				addEdge := func(target tracker.NativeWorkItemID, at time.Time) {
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.SetRelationship(t.Context(), corestore.Relationship{
							OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: f.issue.WorkItemID,
							RelatedProjectID: scope.Project, RelatedWorkItemID: target, Type: "dependency", Provenance: "progress_check",
						}, "add", at)
					}); err != nil {
						t.Fatal(err)
					}
				}
				if test.edges == 2 {
					prior := f.createIndependent(t, "Older repair")
					if !test.olderOpen {
						setState(&prior, "Done")
					}
					olderRepair = prior.WorkItemID
					addEdge(olderRepair, f.now.Add(-time.Minute))
				}
				if !test.open {
					setState(&incident, "Todo")
					setState(&incident, "Done")
				}
				if test.edges > 0 {
					addEdge(incident.WorkItemID, f.now)
				}
			}
			params := url.Values{"policy": {f.policy}, "instance": {string(f.lease.MachineID)}}
			read := func() nativeProgressView {
				response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/progress?"+params.Encode(), f.worker, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var view nativeProgressView
				decodeHubResponse(t, response, &view)
				return view
			}
			started := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("start")}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: hubapi.NewID("run"), AttemptID: hubapi.NewID("attempt"), PolicyID: f.policy}}
			if test.active {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, started), http.StatusOK)
			}
			longBody := strings.Repeat("Acceptance context. ", 200) + strings.Replace(issueContractTestSections, "Complete the requested work.", "Required result: original", 1)
			if test.evidenceChange == "issue.edited" {
				response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.worker, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var current tracker.NativeIssue
				decodeHubResponse(t, response, &current)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path, testHubAdminToken, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("long-body")}, ExpectedRevision: current.Revision, Body: &longBody}), http.StatusOK)
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
			if len(view.Request.Attempts) != 1 || view.Request.Attempts[0].ID != f.attempt || (!test.success && !test.emptyLanding && view.Request.Attempts[0].FailureClass != event.Data.TerminalFailure.ErrorClass) {
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
			if test.cleanupReplay != "" {
				doneID := string(incident.WorkItemID) + ":done"
				hasDone := slices.ContainsFunc(view.Request.Receipts, func(receipt progresscheck.Receipt) bool { return receipt.ID == doneID })
				if view.Request.Attempts[0].Attribution != "instance" || view.Record != nil || hasDone != (test.edges > 0 && !test.open) {
					t.Fatalf("unexpected initial evidence: %+v", view)
				}
				evidence := []string{f.attempt}
				if hasDone {
					evidence = append(evidence, doneID)
				}
				input.Decision = progresscheck.Decision{Outcome: progresscheck.SystemDefect, Diagnosis: f.attempt + ": backend protocol remains unavailable", Reason: "The runner could not start implementation", Evidence: evidence, DefectKey: defectKey, DefectTitle: incident.Title, DefectBody: incident.Body}
			}
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
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path, testHubAdminToken, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("edited-body")}, ExpectedRevision: current.Revision, Body: &longBody}), http.StatusOK)
					fresh := read()
					freshID, err := fresh.Request.ID()
					if err != nil || fresh.Request.Context != view.Request.Context || freshID == id {
						t.Fatalf("truncated edit lost invalidation: same context=%v same ID=%v err=%v", fresh.Request.Context == view.Request.Context, freshID == id, err)
					}
				} else if test.evidenceChange == "change.created" {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/changes", testHubAdminToken, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("source")}, Title: "Updated source"}), http.StatusOK)
				} else if strings.HasPrefix(test.evidenceChange, "run.") {
					started.Type, started.IdempotencyKey, started.Data.Sequence = test.evidenceChange, hubapi.NewID("evidence"), 2
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
			post := func(input progresscheck.Publication) *httptest.ResponseRecorder {
				return performHubAPIRequest(t, f.service, http.MethodPost, path+"/progress", f.worker, input)
			}
			response := post(input)
			if test.changed || test.released || test.staleRevision || test.legacy {
				if response.Code == http.StatusOK {
					t.Fatal("stale evidence or released authority published decision")
				}
				var count int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE organization_id=$1 AND actor_id=$2 AND operation=$3", scope.Organization, nativeProgressReceiptActor, nativeProgressReceiptOperation(scope, string(f.issue.WorkItemID))).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatal("rejected decision persisted")
				}
				return
			}
			requireNativeStatus(t, response, http.StatusOK)
			replayDuring := func(deliver func()) *httptest.ResponseRecorder {
				t.Helper()
				currentID, err := read().Request.ID()
				if err != nil {
					t.Fatal(err)
				}
				repository := f.service.config.TenantRepository.(*postgres.Repository)
				guard := &progressPublicationRepository{Repository: repository, observe: func() error {
					deliver()
					return nil
				}}
				if currentID == id {
					guard.skipWrites = 1
				}
				f.service.config.TenantRepository = guard
				defer func() { f.service.config.TenantRepository = repository }()
				request := newHubAPIRequest(t, http.MethodPost, path+"/progress", f.worker, input)
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, request.WithContext(context.WithValue(t.Context(), progressPublicationContextKey{}, new(int))))
				if !guard.fired {
					t.Fatal("event was not delivered at replay authorization")
				}
				return response
			}
			var second *httptest.ResponseRecorder
			if test.cleanupReplay == "usage" {
				second = replayDuring(func() {
					observed := started
					observed.IdempotencyKey, observed.Type, observed.Data.Sequence = hubapi.NewID("usage"), "run.observed", 2
					observed.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: f.now}
					observed.Data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "code-model", Input: 10, Currency: "USD"}}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, observed), http.StatusOK)
				})
			} else {
				second = post(input)
			}
			requireNativeStatus(t, second, http.StatusOK)
			if test.cleanupReplay != "" {
				var first, replay progresscheck.Record
				decodeHubResponse(t, response, &first)
				decodeHubResponse(t, second, &replay)
				if first.DefectIssueID != string(incident.WorkItemID) || first.CommentID == "" || !reflect.DeepEqual(first, replay) {
					t.Fatalf("replay=%+v, want original incident and comment from %+v", replay, first)
				}
				item := f.reload(t)
				var wantEdges []tracker.NativeWorkItemID
				if olderRepair != "" {
					wantEdges = append(wantEdges, olderRepair)
				}
				if !slices.Equal(item.Dependencies, wantEdges) {
					t.Fatalf("dependencies = %v, want %v", item.Dependencies, wantEdges)
				}
				var completionIDs []string
				for _, receipt := range read().Request.Receipts {
					if receipt.Step == "system_defect" {
						completionIDs = append(completionIDs, receipt.ID)
					}
				}
				var wantCompletions []string
				if olderRepair != "" && !test.olderOpen {
					wantCompletions = append(wantCompletions, string(olderRepair)+":done")
				}
				if !slices.Equal(completionIDs, wantCompletions) {
					t.Fatalf("completion evidence = %v, want %v", completionIDs, wantCompletions)
				}

			} else {
				replay := read()
				replayID, err := replay.Request.ID()
				if err != nil || replayID != id || replay.Record == nil {
					t.Fatalf("decision self-invalidated: id=%s want=%s record=%+v err=%v", replayID, id, replay.Record, err)
				}
			}
			comments := 0
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
				rows, err := reader.SelectComments(t.Context(), scope.Project, f.issue.WorkItemID, int64(1<<63-1), true, corestore.MaxPageSize)
				comments = len(rows)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			wantComments := 1
			if test.fullContext {
				wantComments += progresscheck.MaxComments
			}
			if comments != wantComments {
				t.Fatalf("replay created %d comments", comments)
			}
			switch test.cleanupReplay {
			case "matrix":
				changed := input
				changed.Decision.Reason = "Different publication"
				t.Run("live changed", func(t *testing.T) {
					requireNativeError(t, post(changed), http.StatusConflict, "revision_conflict")
				})
				f.release(t)
				for _, state := range []string{"released", "replaced"} {
					if state == "replaced" {
						if olderRepair != "" && test.olderOpen {
							f.lease = nativeAttemptFixtureLease(t, f.runner, f.issue, "replacement")
						} else {
							claim()
						}
					}
					t.Run(state+" identical", func(t *testing.T) {
						requireNativeError(t, post(input), http.StatusConflict, "stale_fencing_token")
					})
					t.Run(state+" changed", func(t *testing.T) {
						requireNativeStatus(t, post(changed), http.StatusConflict)
					})
				}
				input.LeaseID, input.FencingToken = f.lease.ID, f.lease.FencingToken
				requireNativeError(t, post(input), http.StatusConflict, "revision_conflict")
			case "human":
				during := replayDuring(func() {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "new-evidence-after-cleanup"}, Body: "New failure details from the operator"}), http.StatusOK)
				})
				requireNativeError(t, during, http.StatusConflict, "revision_conflict")
				requireNativeError(t, post(input), http.StatusConflict, "revision_conflict")
			case "source":
				during := replayDuring(func() {
					response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/changes", testHubAdminToken, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "new-change-after-cleanup"}, Title: "Updated source"})
					requireNativeStatus(t, response, http.StatusOK)
					var change tracker.ChangeRequest
					decodeHubResponse(t, response, &change)
					version := changeTestInput()
					bundle := changeTestSource(t, &version)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/changes/"+change.ID+"/versions", testHubAdminToken, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "new-version-after-cleanup"}, ChangeVersionInput: version, SourceBundle: bundle}), http.StatusOK)
				})
				requireNativeError(t, during, http.StatusConflict, "revision_conflict")
				requireNativeError(t, post(input), http.StatusConflict, "revision_conflict")
			}
			if test.cleanupReplay != "" {
				var count int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE organization_id=$1 AND actor_id=$2 AND operation=$3", scope.Organization, nativeProgressReceiptActor, nativeProgressReceiptOperation(scope, string(f.issue.WorkItemID))).Scan(&count); err != nil || count != 1 {
					t.Fatalf("publication count=%d, want 1: %v", count, err)
				}
			}
		})
	}
}
