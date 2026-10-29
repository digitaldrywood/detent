package orchestrator

import (
	"context"
	"errors"
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
	decision progresscheck.Decision
	calls    int
	err      error
	started  chan RunRequest
	release  <-chan struct{}
	timeout  bool
}

func (r *progressRunner) CheckProgress(ctx context.Context, req RunRequest, _ progresscheck.Request) (progresscheck.Decision, error) {
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

func TestRetryProgressDispatch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		decision progresscheck.Decision
		source   string
		outcome  string
		want     string
		err      error
		reason   string
	}{
		{name: "changed source dispatches same step", source: "new-head", decision: progresscheck.Decision{Outcome: progresscheck.Progressing, Diagnosis: "attempt-2" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Convergence: "The failing validation now has a matching successful command receipt after the assertion was repaired", Reason: "Source changed", Evidence: []string{"attempt-2"}}, want: RunModeImplement},
		{name: "1748 unchanged 422 refusal reroutes to the audit pipeline", decision: progresscheck.Decision{Outcome: progresscheck.Reroute, Step: RunModeMerge, Diagnosis: "attempt-1" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Instruction: "Repair the missing validation evidence in the source finalization path and verify the previously failing check on the pinned source", Reason: "Run the merge pipeline to produce the missing audit receipt", Evidence: []string{"attempt-1", "attempt-2"}}, want: RunModeMerge},
		{name: "Detent refusal files one dependency and retained decision is advisory", decision: progresscheck.Decision{Outcome: progresscheck.SystemDefect, Diagnosis: "attempt-1" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Reason: "Protocol cannot carry the audit receipt", Evidence: []string{"attempt-1", "attempt-2"}, DefectKey: "audit-publication-protocol", DefectTitle: "Audit receipt cannot be published", DefectBody: "Repeated 422 on unchanged base and head"}},
		{name: "success pays no AI turn", outcome: "success", want: RunModeImplement},
		{name: "backend failure permits ordinary retry", err: errors.New("provider unavailable"), reason: "provider unavailable", want: RunModeImplement},
		{name: "timeout permits ordinary retry", err: context.DeadlineExceeded, reason: "context deadline exceeded", want: RunModeImplement},
		{name: "progressing ignores suggested step", decision: progresscheck.Decision{Outcome: progresscheck.Progressing, Step: RunModeMerge, Diagnosis: "attempt-2" + ": the retained failure identifies missing validation evidence; the failing step has not produced its required receipt", Convergence: "The failing validation now has a matching successful command receipt after the assertion was repaired", Reason: "Continue", Evidence: []string{"attempt-2"}}, want: RunModeImplement},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}, Project: scheduler.ProjectCandidate{ID: "example-project"}, WorkerHosts: []string{"worker-a"}})
				issue := dispatchTestIssue("example-issue", "In Progress")
				source := &progressScheduling{request: progresscheck.Request{ProjectID: "example-project", IssueID: issue.ID, PolicyID: "policy", EvidenceRevision: "1", SourceSHA: "head", BaseSHA: "base", AvailableSteps: []string{RunModeImplement, RunModePlan, RunModeMerge}, Attempts: []progresscheck.Attempt{
					{ID: "attempt-1", Step: "publication", Outcome: "failed", SourceSHA: "head", BaseSHA: "base", Error: "422: landing requires security audit evidence for its exact base and head"},
					{ID: "attempt-2", Step: "publication", Outcome: "failed", SourceSHA: "head", BaseSHA: "base", Error: "422: landing requires security audit evidence for its exact base and head"},
				}}, records: map[string]progresscheck.Record{}, defects: map[string]string{}}
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
					if req.ProgressDecision.RequestID != "" || len(execution.observations) != 1 || execution.observations[0].Recovery == nil || !strings.Contains(execution.observations[0].Recovery.Reason, tt.reason) || strings.Count(req.PriorAttempt.Reason, tt.reason) != 1 || source.comments != 0 || source.admissions != 1 || execution.finished != "cancelled" {
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
