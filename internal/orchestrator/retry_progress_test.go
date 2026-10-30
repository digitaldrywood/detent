package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type progressScheduling struct {
	hubSchedulingSource
	request        progresscheck.Request
	records        map[string]progresscheck.Record
	comments       int
	defects        map[string]string
	admissions     int
	admissionError error
	requestError   error
	missingOwner   bool
}

type progressExecution struct {
	planDispatchExecution
	finished     string
	observations []tracker.NativeRuntimeObservation
}

func (e *progressExecution) Finish(ctx context.Context, outcome string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.finished = outcome
	return nil
}

func (e *progressExecution) ObserveRuntime(ctx context.Context, observation tracker.NativeRuntimeObservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.observations = append(e.observations, observation)
	return nil
}

func (s *progressScheduling) ReadmitRetryProgress(ctx context.Context, run RunRequest, _ runpkg.ProviderCapacityResolver) (runpkg.Execution, *isolation.Policy, error) {
	s.admissions++
	if run.Execution != nil {
		if err := run.Execution.Finish(ctx, "cancelled"); err != nil {
			return nil, nil, err
		}
	}
	if s.admissionError != nil {
		return nil, nil, s.admissionError
	}
	return &progressExecution{}, run.Issue.IsolationPolicy, nil
}

func (s *progressScheduling) RetryProgress(_ context.Context, issue connector.Issue) (progresscheck.Request, progresscheck.Owner, error) {
	if s.requestError != nil {
		return progresscheck.Request{}, nil, s.requestError
	}
	if s.missingOwner {
		return s.request, nil, nil
	}
	if issue.ID != s.request.IssueID {
		return progresscheck.Request{}, s, nil
	}
	return s.request, s, nil
}
func (s *progressScheduling) Load(_ context.Context, _, _, id string) (progresscheck.Record, bool, error) {
	r, ok := s.records[id]
	return r, ok, nil
}
func (s *progressScheduling) Publish(_ context.Context, r progresscheck.Record) (progresscheck.Record, error) {
	if old, ok := s.records[r.RequestID]; ok {
		return old, nil
	}
	r.CommentID = "comment"
	if r.Decision.Outcome == progresscheck.SystemDefect {
		if s.defects[r.Decision.DefectKey] == "" {
			s.defects[r.Decision.DefectKey] = "defect-issue"
		}
		r.DefectIssueID = s.defects[r.Decision.DefectKey]
	}
	s.comments++
	s.records[r.RequestID] = r
	return r, nil
}

type progressRunner struct {
	evidence progresscheck.Request
	decision progresscheck.Decision
	calls    int
	err      error
	started  chan RunRequest
	release  <-chan struct{}
	timeout  bool
}

func (r *progressRunner) CheckProgress(ctx context.Context, req RunRequest, evidence progresscheck.Request) (progresscheck.Decision, error) {
	r.evidence = evidence
	r.calls++
	if r.timeout {
		<-ctx.Done()
		return progresscheck.Decision{}, ctx.Err()
	}
	if r.started != nil {
		r.started <- req
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return progresscheck.Decision{}, ctx.Err()
		}
	}
	return r.decision, r.err
}

type progressCompletionExecution struct {
	planDispatchExecution
	outcomes []string
}

func (e *progressCompletionExecution) Finish(_ context.Context, outcome string) error {
	e.outcomes = append(e.outcomes, outcome)
	return nil
}

func TestEmptyLandingProgressCompletesOrImplements(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		complete bool
	}{
		{name: "progress completion advice requires implementation gates", complete: true},
		{name: "unmet acceptance implements with refusal feedback"},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := dispatchTestIssue("example-issue", "Rework")
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"In Progress", "Rework", "Merging"}, TerminalStates: []string{"Done"}, Project: scheduler.ProjectCandidate{ID: "example-project"}, WorkerHosts: []string{"worker-a"}})
			recovery := tracker.NativeRecovery{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "example-project", WorkItemID: "example-issue", Revision: 1}}, ChangeDetail: &tracker.ChangeDetail{Change: tracker.ChangeRequest{CurrentVersion: "empty-version"}, Summary: tracker.ChangeSummary{Status: "reviewed"}, Versions: []tracker.ChangeVersion{{ID: "empty-version", ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: "base", HeadSHA: "empty-head", Source: &tracker.ChangeSource{DiffSHA256: tracker.ChangeSourceDigest(nil)}}}}}, Discussion: []tracker.NativeComment{{ID: "base-evidence", Body: "Acceptance regression passes on base; prior reviewed fix is incorporated in base"}}}
			if !test.complete {
				recovery.Discussion[0].Body = "Acceptance regression still fails on base; the required behavior is missing"
			}
			for i := range 3 {
				recovery.Attempts = append(recovery.Attempts,
					tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{AttemptID: fmt.Sprintf("rework-%d", i), Outcome: "succeeded", Identity: &tracker.NativeExecutionIdentity{Role: "rework"}}},
					tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{AttemptID: fmt.Sprintf("merge-%d", i), Outcome: "succeeded", Identity: &tracker.NativeExecutionIdentity{Role: "merge"}, Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "nothing_to_land", Refusal: "the published source has no deliverable to verify", HeadSHA: "empty-head", BaseSHA: "base"}}}},
				)
			}
			execution := &progressCompletionExecution{}
			source := &progressScheduling{hubSchedulingSource: hubSchedulingSource{execution: execution}, request: progresscheck.NativeRequest(recovery, "policy", "instance"), records: map[string]progresscheck.Record{}}
			decision := progresscheck.Decision{Outcome: progresscheck.Reroute, Step: RunModeImplement, Diagnosis: "merge-2:landing contains no acceptance regression evidence", Instruction: "Add and run the missing acceptance regression", Reason: "Produce the missing acceptance regression", Evidence: []string{"merge-2:landing"}}
			if test.complete {
				decision = progresscheck.Decision{Outcome: progresscheck.Reroute, Step: progresscheck.CompleteStep, Diagnosis: "base-evidence confirms the reviewed fix and passing acceptance regression are already on base", Instruction: "Complete the issue using the verified base acceptance evidence", Reason: "The reviewed fix and passing acceptance regression are already on base", Evidence: []string{"merge-2:landing", "base-evidence"}}
			}
			checker := &progressRunner{decision: decision}
			worker := newWorkerHostRunner()
			tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			workflow := &nativeWorkflowConnector{autoPromoteTickConnector: tick, states: []connector.WorkflowState{{Name: "Rework", Dispatchable: true, Transitions: []string{"Merging", "Done"}}, {Name: "Done", Terminal: true}}}
			o := &Orchestrator{cfg: cfg, connector: workflow, scheduling: source, progressRunner: checker, supervisor: newTestSupervisor(t, worker, cfg), runResults: make(chan runpkg.Completion, 1)}
			state := newState(cfg)
			if !o.dispatchIssue(t.Context(), &state, issue, 7, time.Now(), "worker-a") {
				t.Fatal("progress check was not admitted")
			}
			running := state.Running[issue.ID]
			t.Cleanup(func() { running.cancel(); <-running.done })
			var request RunRequest
			select {
			case request = <-worker.started:
			case completion := <-o.runResults:
				t.Fatalf("progress advice bypassed required gates: %+v", completion)
			}
			state.Running[issue.ID].cancel()
			<-o.runResults
			<-state.Running[issue.ID].done
			if request.Mode != RunModeImplement || !strings.Contains(request.PriorAttempt.Reason, "nothing_to_land") || !strings.Contains(request.PriorAttempt.Reason, "no deliverable to verify") || !strings.Contains(request.PriorAttempt.Reason, decision.Reason) {
				t.Fatalf("implementation lost refusal feedback: %+v", request)
			}
			if len(tick.updates) != 0 || len(state.Completed) != 0 {
				t.Fatalf("progress advice completed without required gates: updates=%+v completed=%+v", tick.updates, state.Completed)
			}
			if checker.calls != 1 || source.comments != 1 || len(checker.evidence.Attempts) != 6 || checker.evidence.Attempts[5].Outcome != "success" || !checker.evidence.EmptyDelta {
				t.Fatalf("history/check publication lost: calls=%d comments=%d request=%+v", checker.calls, source.comments, checker.evidence)
			}
		})
	}
}

func TestRetryProgressDispatch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		decision     progresscheck.Decision
		source       string
		outcome      string
		want         string
		err          error
		reason       string
		requestError error
		missingOwner bool
	}{
		{name: "changed source dispatches same step", source: "new-head", decision: progresscheck.Decision{Outcome: progresscheck.Progressing, Diagnosis: "attempt-2" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Convergence: "The failing validation now has a matching successful command receipt after the assertion was repaired", Reason: "Source changed", Evidence: []string{"attempt-2"}}, want: RunModeImplement},
		{name: "1748 unchanged 422 refusal reroutes to the audit pipeline", decision: progresscheck.Decision{Outcome: progresscheck.Reroute, Step: RunModeMerge, Diagnosis: "attempt-1" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Run the merge pipeline to produce the missing audit receipt", Evidence: []string{"attempt-1", "attempt-2"}}, want: RunModeMerge},
		{name: "Detent refusal files one dependency and retained decision is advisory", decision: progresscheck.Decision{Outcome: progresscheck.SystemDefect, Diagnosis: "attempt-1" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Reason: "Protocol cannot carry the audit receipt", Evidence: []string{"attempt-1", "attempt-2"}, DefectKey: "audit-publication-protocol", DefectTitle: "Audit receipt cannot be published", DefectBody: "Repeated 422 on unchanged base and head"}},
		{name: "success pays no AI turn", outcome: "success", want: RunModeImplement},
		{name: "backend failure permits ordinary retry", err: errors.New("provider unavailable"), reason: "provider unavailable", want: RunModeImplement},
		{name: "timeout permits ordinary retry", err: context.DeadlineExceeded, reason: "context deadline exceeded", want: RunModeImplement},
		{name: "invalid decision permits ordinary retry", decision: progresscheck.Decision{Outcome: progresscheck.Progressing, Step: RunModeMerge, Reason: "Continue", Evidence: []string{"unavailable"}}, reason: "progress decision cites unavailable evidence", want: RunModeImplement},
		{name: "missing owner permits ordinary retry", missingOwner: true, reason: "progress check requires backend and tracker owner", want: RunModeImplement},
		{name: "evidence failure permits ordinary retry", requestError: errors.New("progress evidence unavailable"), reason: "progress evidence unavailable", want: RunModeImplement},
		{name: "progressing ignores suggested step", decision: progresscheck.Decision{Outcome: progresscheck.Progressing, Step: RunModeMerge, Diagnosis: "attempt-2" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Convergence: "The failing validation now has a matching successful command receipt after the assertion was repaired", Reason: "Continue", Evidence: []string{"attempt-2"}}, want: RunModeImplement},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}, Project: scheduler.ProjectCandidate{ID: "example-project"}, WorkerHosts: []string{"worker-a"}})
				issue := dispatchTestIssue("example-issue", "In Progress")
				source := &progressScheduling{request: progresscheck.Request{ProjectID: "example-project", IssueID: issue.ID, PolicyID: "policy", EvidenceRevision: "1", SourceSHA: "head", BaseSHA: "base", AvailableSteps: []string{RunModeImplement, RunModePlan, RunModeMerge}, Attempts: []progresscheck.Attempt{
					{ID: "attempt-1", Step: "publication", Outcome: "failed", SourceSHA: "head", BaseSHA: "base", Error: "422: landing requires security audit evidence for its exact base and head"},
					{ID: "attempt-2", Step: "publication", Outcome: "failed", SourceSHA: "head", BaseSHA: "base", Error: "422: landing requires security audit evidence for its exact base and head"},
				}}, records: map[string]progresscheck.Record{}, defects: map[string]string{}, requestError: tt.requestError, missingOwner: tt.missingOwner}
				if tt.source != "" {
					source.request.SourceSHA = tt.source
				}
				if tt.outcome != "" {
					source.request.Attempts[1].Outcome = tt.outcome
				}
				checker := &progressRunner{decision: tt.decision, err: tt.err, timeout: errors.Is(tt.err, context.DeadlineExceeded)}
				worker := newWorkerHostRunner()
				o := &Orchestrator{cfg: cfg, scheduling: source, progressRunner: checker, supervisor: newTestSupervisor(t, worker, cfg), runResults: make(chan runpkg.Completion, 1)}
				state := newState(cfg)
				now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
				execution := &progressExecution{}
				req := RunRequest{Issue: issue, Mode: RunModeImplement, Execution: execution}
				err := o.prepareRetryProgress(t.Context(), &req)
				wantError := tt.decision.Outcome == progresscheck.SystemDefect
				if (err != nil) != wantError || !wantError && req.Mode != tt.want {
					t.Fatalf("progress request mode=%q error=%v", req.Mode, err)
				}
				if wantError && execution.finished != "failed" {
					t.Fatalf("failed progress check retained its execution: outcome=%s", execution.finished)
				}
				if tt.reason != "" {
					wantAdmissions, wantFinished := 1, "cancelled"
					if tt.missingOwner || tt.requestError != nil {
						wantAdmissions, wantFinished = 0, ""
						if checker.calls != 0 {
							t.Fatal("unavailable progress dependency started a provider")
						}
					}
					if req.ProgressDecision.RequestID != "" || len(execution.observations) != 1 || execution.observations[0].Recovery == nil || !strings.Contains(execution.observations[0].Recovery.Reason, tt.reason) || strings.Count(req.PriorAttempt.Reason, tt.reason) != 1 || source.comments != 0 || source.admissions != wantAdmissions || execution.finished != wantFinished {
						t.Fatalf("advisory failure missing: request=%+v observations=%+v admissions=%d", req, execution.observations, source.admissions)
					}
				}
				if tt.reason == "" {
					replay := RunRequest{Issue: issue, Mode: RunModeImplement, Execution: req.Execution}
					replayErr := o.prepareRetryProgress(t.Context(), &replay)
					if replayErr != nil {
						t.Fatal(replayErr)
					}
					wantCalls := 1
					if tt.outcome == "success" {
						wantCalls = 0
					}
					if checker.calls != wantCalls || source.comments != wantCalls {
						t.Fatalf("replay calls=%d comments=%d", checker.calls, source.comments)
					}
				}
				if !o.dispatchIssue(t.Context(), &state, issue, 3, now, "worker-a") {
					t.Fatal("worker was not admitted")
				}
				{
					request := <-worker.started
					state.Running[issue.ID].cancel()
					<-o.runResults
					<-state.Running[issue.ID].done
					if tt.reason != "" && (request.ProgressDecision.RequestID != "" || strings.Count(request.PriorAttempt.Reason, tt.reason) != 1) {
						t.Fatalf("ordinary retry lost failure reason: %+v", request)
					}
					if tt.want != "" && request.Mode != tt.want {
						t.Fatalf("dispatched mode=%q want %q", request.Mode, tt.want)
					}
					if tt.decision.Outcome == progresscheck.Reroute && (request.ProgressDecision.Decision.Step != tt.want || !strings.Contains(request.PriorAttempt.Reason, tt.decision.Reason) || !strings.Contains(request.PriorAttempt.Reason, tt.decision.Diagnosis) || !strings.Contains(request.PriorAttempt.Reason, tt.decision.Instruction)) {
						t.Fatalf("reroute feedback missing: %+v", request.PriorAttempt)
					}
				}
				if tt.decision.Outcome == progresscheck.SystemDefect {
					if len(source.defects) != 1 || source.comments != 1 || checker.calls != 1 {
						t.Fatalf("defects=%v comments=%d calls=%d", source.defects, source.comments, checker.calls)
					}
					source.request.Comments = []progresscheck.Comment{{ID: "human-input", Human: true, Revision: "1", Body: "The audit protocol is repaired"}}
					checker.decision = progresscheck.Decision{Outcome: progresscheck.Progressing, Diagnosis: "human-input" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Convergence: "The failing validation now has a matching successful command receipt after the assertion was repaired", Reason: "New human input", Evidence: []string{"human-input"}}
					delete(state.Running, issue.ID)
					if !o.dispatchIssue(t.Context(), &state, issue, 3, now, "worker-a") {
						t.Fatal("new human input did not reopen dispatch")
					}
					<-worker.started
					state.Running[issue.ID].cancel()
					<-o.runResults
					<-state.Running[issue.ID].done
					if checker.calls != 2 || len(source.defects) != 1 {
						t.Fatalf("calls=%d defects=%v", checker.calls, source.defects)
					}
				}
			})
		})
	}
}

func TestRetryProgressDoesNotDelayHealthyDispatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := normalizeConfig(Config{MaxConcurrentAgents: 2, ActiveStates: []string{"Todo", "In Progress"}, Project: scheduler.ProjectCandidate{ID: "example-project"}, WorkerHosts: []string{"worker-a"}})
		issue := dispatchTestIssue("stuck", "In Progress")
		source := &progressScheduling{hubSchedulingSource: hubSchedulingSource{execution: &progressExecution{}}, request: progresscheck.Request{ProjectID: "example-project", IssueID: issue.ID, PolicyID: "policy", EvidenceRevision: "1", AvailableSteps: []string{RunModeImplement}, Attempts: []progresscheck.Attempt{{ID: "failed", Outcome: "failed"}}}, records: map[string]progresscheck.Record{}, defects: map[string]string{}}
		release := make(chan struct{})
		checker := &progressRunner{started: make(chan RunRequest, 1), release: release, decision: progresscheck.Decision{Outcome: progresscheck.Progressing, Diagnosis: "failed" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Convergence: "The failing validation now has a matching successful command receipt after the assertion was repaired", Reason: "Continue with new evidence", Evidence: []string{"failed"}}}
		worker := newWorkerHostRunner()
		o := &Orchestrator{cfg: cfg, scheduling: source, progressRunner: checker, supervisor: newTestSupervisor(t, worker, cfg), runResults: make(chan runpkg.Completion, 2)}
		state := newState(cfg)
		dispatched := make(chan bool, 1)
		go func() { dispatched <- o.dispatchIssue(t.Context(), &state, issue, 2, time.Now(), "worker-a") }()
		defer func() {
			close(release)
			synctest.Wait()
			for _, running := range state.Running {
				running.cancel()
			}
			for range state.Running {
				<-o.runResults
			}
		}()
		synctest.Wait()
		select {
		case ok := <-dispatched:
			if !ok {
				t.Fatal("progress candidate was not admitted")
			}
		default:
			t.Fatal("progress provider blocked the scheduler")
		}
		req := <-checker.started
		if req.Execution == nil {
			t.Fatal("progress check has no usage accounting execution")
		}
		healthy := dispatchTestIssue("healthy", "In Progress")
		if !o.dispatchIssue(t.Context(), &state, healthy, 1, time.Now(), "worker-a") {
			t.Fatal("healthy candidate was not dispatched during the check")
		}
		got := <-worker.started
		if got.Issue.ID != healthy.ID {
			t.Fatalf("started %q before progress decision completed", got.Issue.ID)
		}
	})
}

func TestRetryProgressRespectsBackendCapacity(t *testing.T) {
	t.Parallel()
	cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"In Progress"}, Project: scheduler.ProjectCandidate{ID: "example-project"}, WorkerHosts: []string{"worker-a"}})
	issue := dispatchTestIssue("example", "In Progress")
	source := &progressScheduling{request: progresscheck.Request{ProjectID: "example-project", IssueID: issue.ID, PolicyID: "policy", EvidenceRevision: "1", Attempts: []progresscheck.Attempt{{ID: "failed", Outcome: "failed"}}}}
	checker := &progressRunner{}
	scope := backendcapacity.Scope{BackendID: "codex", BackendKind: "codex", Provider: "openai"}
	o := &Orchestrator{cfg: cfg, scheduling: source, progressRunner: checker, capacityController: backendCapacityTestController{scope: scope}}
	state := newState(cfg)
	state.BackendOutages[scope.Key()] = BackendOutage{Scope: scope, Kind: "http_503", NextProbeAt: time.Now().Add(time.Hour)}
	outcome := o.dispatchIssueWithOutcome(t.Context(), &state, issue, 2, time.Now(), "worker-a")
	if outcome.dispatched || outcome.reason != dispatchIssueFailureBackendCapacityPaused || checker.calls != 0 {
		t.Fatalf("capacity refusal ran a progress turn: %+v calls=%d", outcome, checker.calls)
	}
}

func TestRetryProgressReadmitsSelectedMode(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		mode     string
		retained bool
		refused  bool
	}{
		{name: "new decision"},
		{name: "retained decision", retained: true},
		{name: "new admission refused", refused: true},
		{name: "retained admission refused", retained: true, refused: true},
		{name: "new decision in the same mode", mode: RunModeImplement},
		{name: "retained decision in the same mode", mode: RunModeImplement, retained: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := dispatchTestIssue("example-issue", "Todo")
			source := &progressScheduling{request: progresscheck.Request{ProjectID: "example-project", IssueID: issue.ID, PolicyID: "policy", EvidenceRevision: "1", AvailableSteps: []string{RunModeImplement}, Attempts: []progresscheck.Attempt{{ID: "failed-plan", Step: runpkg.RolePlan, Outcome: "failed"}}}, records: map[string]progresscheck.Record{}}
			decision := progresscheck.Decision{Outcome: progresscheck.Reroute, Step: RunModeImplement, Diagnosis: "failed-plan" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Continue with implementation", Evidence: []string{"failed-plan"}}
			if tt.retained {
				id, err := source.request.ID()
				if err != nil {
					t.Fatal(err)
				}
				source.records[id] = progresscheck.Record{RequestID: id, ProjectID: source.request.ProjectID, IssueID: issue.ID, EvidenceRevision: "1", Decision: decision, CommentID: "retained-comment"}
			}
			if tt.refused {
				source.admissionError = errors.New("implementation capacity unavailable")
			}
			checker := &progressRunner{decision: decision}
			o := &Orchestrator{scheduling: source, progressRunner: checker}
			original := &progressExecution{}
			mode := tt.mode
			if mode == "" {
				mode = RunModePlan
			}
			request := RunRequest{Issue: issue, Mode: mode, Execution: original}
			err := o.prepareRetryProgress(t.Context(), &request)
			if !strings.Contains(request.PriorAttempt.Reason, decision.Instruction) || !strings.Contains(request.PriorAttempt.Reason, decision.Diagnosis) {
				t.Fatalf("implementation handoff lost diagnosis or instruction: %+v", request.PriorAttempt)
			}
			wantAdmissions := 1
			if tt.retained && mode == RunModeImplement {
				wantAdmissions = 0
			}
			if source.admissions != wantAdmissions || request.Mode != RunModeImplement || request.ProgressDecision.Decision.Step != RunModeImplement {
				t.Fatalf("selected step was not readmitted: admissions=%d mode=%s decision=%+v", source.admissions, request.Mode, request.ProgressDecision)
			}
			wantCalls := 1
			if tt.retained {
				wantCalls = 0
			}
			if checker.calls != wantCalls {
				t.Fatalf("progress calls=%d, want %d", checker.calls, wantCalls)
			}
			if tt.refused {
				if !errors.Is(err, source.admissionError) || request.Execution != original {
					t.Fatalf("admission refusal did not preserve its error and execution: %v", err)
				}
			} else if err != nil || (request.Execution == original) != (wantAdmissions == 0) {
				t.Fatalf("reroute retained its old execution: %v", err)
			}
		})
	}
}

type outageBackendError struct{ body string }

func (outageBackendError) Error() string { return "backend protocol unavailable" }
func (e outageBackendError) BackendErrorBody() string {
	if e.body != "" {
		return e.body
	}
	return `{"error":"protocol unavailable"}`
}

func TestRetryProgressSharedBackendOutage(t *testing.T) {
	t.Parallel()
	for _, instance := range []bool{false, true} {
		t.Run(fmt.Sprintf("instance=%t", instance), func(t *testing.T) {
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}, Project: scheduler.ProjectCandidate{ID: "example-project"}, WorkerHosts: []string{"worker-a"}, FailureRetryBaseDelay: time.Second, MaxRetryBackoff: time.Minute})
			source := &progressScheduling{records: map[string]progresscheck.Record{}, defects: map[string]string{}}
			checker := &progressRunner{decision: progresscheck.Decision{Outcome: progresscheck.SystemDefect, Diagnosis: "attempt-1 and attempt-2 record a shared backend protocol outage on worker-a across issues", Reason: "Shared backend protocol outage", Evidence: []string{"attempt-1", "attempt-2"}, DefectKey: "backend-protocol-unavailable", DefectTitle: "Backend protocol unavailable", DefectBody: "Multiple issues failed on the same instance"}}
			worker := newWorkerHostRunner()
			attempts := &recordingWorkAttemptStore{}
			tracker := &terminalRetryConnector{issues: map[string]connector.Issue{}}
			o := &Orchestrator{cfg: cfg, connector: tracker, progressRunner: checker, supervisor: newTestSupervisor(t, worker, cfg), runResults: make(chan runpkg.Completion, 1), workAttempts: attempts}
			state := newState(cfg)
			now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
			issues := make([]connector.Issue, 6)
			history := make(map[string][]progresscheck.Attempt)
			for index := range issues {
				issue := dispatchTestIssue(fmt.Sprintf("issue-%d", index), "In Progress")
				issues[index] = issue
				tracker.issues[issue.ID] = issue
				var failure error = outageBackendError{}
				if instance {
					failure = fmt.Errorf("%w: backend protocol unavailable", runpkg.ErrWorkspacePreparation)
				}
				failure = decidedTestFailure{failure, "instance"}
				for attempt := 1; attempt <= 2; attempt++ {
					state.Running[issue.ID] = Running{Issue: issue, Attempt: attempt, WorkAttemptID: int64(index*2 + attempt), WorkerHost: "worker-a", StartedAt: now.Add(-time.Second)}
					o.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Err: failure, Result: RunResult{TurnStarted: !instance}})
					retry, ok := state.Retry[issue.ID]
					if !ok || retry.Attempt != attempt+1 || !retry.DueAt.After(now) || retry.DueAt.After(now.Add(time.Minute)) {
						t.Fatalf("issue %s attempt %d retry=%+v present=%t", issue.ID, attempt, retry, ok)
					}
					if instance && attempts.completions[len(attempts.completions)-1].StatusMessage != "instance workspace or startup failure" {
						t.Fatalf("lost instance attribution: %+v", attempts.completions)
					}
					maximum := time.Duration(1<<attempt) * time.Second
					if delay := retry.DueAt.Sub(now); delay < maximum-maximum/8 || delay > maximum {
						t.Fatalf("attempt %d backoff=%v, want jittered backoff in [%v, %v]", attempt, delay, maximum-maximum/8, maximum)
					}
					completion := attempts.completions[len(attempts.completions)-1]
					if instance && completion.ErrorClass != workAttemptErrorWorkspace {
						t.Fatalf("instance outage recorded as issue failure: %+v", completion)
					}
					entry := progresscheck.Attempt{ID: fmt.Sprintf("attempt-%d", attempt), Step: RunModeImplement, Outcome: "failed", Error: completion.ErrorMessage, FailureClass: completion.ErrorClass, Instance: "worker-a"}
					if instance {
						entry.Attribution = "instance"
					}
					history[issue.ID] = append(history[issue.ID], entry)
				}
				issues[index] = state.Retry[issue.ID].Issue
			}
			o.scheduling = source
			for _, issue := range issues {
				source.request = progresscheck.Request{ProjectID: "example-project", IssueID: issue.ID, PolicyID: "policy", EvidenceRevision: "1", SourceSHA: "head", BaseSHA: "base", Instance: "worker-a", AvailableSteps: []string{RunModeImplement}, Attempts: history[issue.ID]}
				outcome := o.dispatchIssueWithOutcome(t.Context(), &state, issue, 3, now.Add(time.Minute), "worker-a")
				if !outcome.dispatched {
					t.Fatalf("outage prevented AI progress check: %+v", outcome)
				}
				running := state.Running[issue.ID]
				t.Cleanup(func() { running.cancel(); <-running.done })
				var completion runpkg.Completion
				select {
				case completion = <-o.runResults:
				case <-worker.started:
					t.Fatal("progress defect dispatched implementation instead of returning to the retry owner")
				}
				<-running.done
				if completion.Err == nil || !completion.Retryable {
					t.Fatalf("progress defect escaped retry owner: %+v", completion)
				}
				o.handleRunResult(t.Context(), &state, completion)
				retry, ok := state.Retry[issue.ID]
				if !ok || retry.Attempt != 4 {
					t.Fatalf("progress defect reset retry accounting: retry=%+v present=%t", retry, ok)
				}
				if delay := retry.DueAt.Sub(completion.CompletedAt); delay < 7*time.Second || delay > 8*time.Second {
					t.Fatalf("progress defect lost growing jittered backoff: %v", delay)
				}
				request := checker.evidence
				if len(request.Attempts) != 2 || request.Attempts[1].Error == "" || (instance && request.Attempts[1].Attribution != "instance") {
					t.Fatalf("progress check lost recorded outage evidence: %+v", request)
				}
			}
			if len(source.records) != len(issues) || checker.calls != len(issues) || len(state.Blocked) != 0 {
				t.Fatalf("records=%d checks=%d holds=%v", len(source.records), checker.calls, state.Blocked)
			}
			source.request = progresscheck.Request{}
			other := dispatchTestIssue("unrelated", "Todo")
			if !o.dispatchIssue(t.Context(), &state, other, 1, now.Add(time.Minute), "worker-a") {
				t.Fatal("backend outage paused unrelated project work")
			}
			running := state.Running[other.ID]
			t.Cleanup(func() { running.cancel(); <-running.done })
			<-worker.started
			running.cancel()
			<-o.runResults
			<-running.done
		})
	}
}
