package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

type progressScheduling struct {
	hubSchedulingSource
	request  progresscheck.Request
	records  map[string]progresscheck.Record
	comments int
	defects  map[string]string
}

func (s *progressScheduling) RetryProgress(context.Context, connector.Issue) (progresscheck.Request, progresscheck.Owner, error) {
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
}

func (r *progressRunner) CheckProgress(context.Context, RunRequest, progresscheck.Request) (progresscheck.Decision, error) {
	r.calls++
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
	}{
		{name: "changed source dispatches same step", source: "new-head", decision: progresscheck.Decision{Outcome: progresscheck.Progressing, Reason: "Source changed", Evidence: []string{"attempt-2"}}, want: RunModeImplement},
		{name: "1748 unchanged 422 refusal reroutes to the audit pipeline", decision: progresscheck.Decision{Outcome: progresscheck.Reroute, Step: RunModeMerge, Reason: "Run the merge pipeline to produce the missing audit receipt", Evidence: []string{"attempt-1", "attempt-2"}}, want: RunModeMerge},
		{name: "Detent refusal files one defect and does not dispatch", decision: progresscheck.Decision{Outcome: progresscheck.SystemDefect, Reason: "Protocol cannot carry the audit receipt", Evidence: []string{"attempt-1", "attempt-2"}, DefectKey: "audit-publication-protocol", DefectTitle: "Audit receipt cannot be published", DefectBody: "Repeated 422 on unchanged base and head"}},
		{name: "success pays no AI turn", outcome: "success", want: RunModeImplement},
		{name: "backend failure never permits blind retry", err: errors.New("provider unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
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
			checker := &progressRunner{decision: tt.decision, err: tt.err}
			worker := newWorkerHostRunner()
			o := &Orchestrator{cfg: cfg, scheduling: source, progressRunner: checker, supervisor: newTestSupervisor(t, worker, cfg), runResults: make(chan runpkg.Completion, 1)}
			state := newState(cfg)
			now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
			mode, allowed, err := o.retryProgress(t.Context(), issue, RunModeImplement, nil)
			if (err != nil) != (tt.err != nil) || allowed != (tt.want != "") || mode != tt.want {
				t.Fatalf("retry decision=%q,%v,%v", mode, allowed, err)
			}
			if tt.err == nil {
				_, _, err = o.retryProgress(t.Context(), issue, RunModeImplement, nil)
				if err != nil {
					t.Fatal(err)
				}
				wantCalls := 1
				if tt.outcome == "success" {
					wantCalls = 0
				}
				if checker.calls != wantCalls || source.comments != wantCalls {
					t.Fatalf("replay calls=%d comments=%d", checker.calls, source.comments)
				}
			}
			dispatched := o.dispatchIssue(t.Context(), &state, issue, 3, now, "worker-a")
			if dispatched != (tt.want != "") {
				if dispatched {
					state.Running[issue.ID].cancel()
					<-o.runResults
				}
				t.Fatalf("dispatched=%v", dispatched)
			}
			if dispatched {
				request := <-worker.started
				state.Running[issue.ID].cancel()
				<-o.runResults
				if request.Mode != tt.want {
					t.Fatalf("dispatched mode=%q want %q", request.Mode, tt.want)
				}
				if tt.decision.Outcome == progresscheck.Reroute && (request.ProgressDecision.Decision.Step != tt.want || request.PriorAttempt.Reason != tt.decision.Reason) {
					t.Fatalf("reroute feedback missing: %+v", request.PriorAttempt)
				}
			}
			if tt.decision.Outcome == progresscheck.SystemDefect {
				if len(source.defects) != 1 || source.comments != 1 || checker.calls != 1 {
					t.Fatalf("defects=%v comments=%d calls=%d", source.defects, source.comments, checker.calls)
				}
				source.request.Comments = []progresscheck.Comment{{ID: "human-input", Human: true, Revision: "1", Body: "The audit protocol is repaired"}}
				checker.decision = progresscheck.Decision{Outcome: progresscheck.Progressing, Reason: "New human input", Evidence: []string{"human-input"}}
				if !o.dispatchIssue(t.Context(), &state, issue, 3, now, "worker-a") {
					t.Fatal("new human input did not reopen dispatch")
				}
				<-worker.started
				state.Running[issue.ID].cancel()
				<-o.runResults
				if checker.calls != 2 || len(source.defects) != 1 {
					t.Fatalf("calls=%d defects=%v", checker.calls, source.defects)
				}
			}
		})
	}
}
