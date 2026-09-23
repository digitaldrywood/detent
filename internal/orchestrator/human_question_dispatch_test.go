package orchestrator

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestHumanQuestionCandidateEligibility(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                              string
		changeBase, answered, unavailable bool
		wantReason                        string
	}{
		{name: "unanswered", wantReason: "human_question_wait"},
		{name: "changed work fingerprint", changeBase: true},
		{name: "authorized reply", answered: true},
		{name: "store unavailable", unavailable: true, wantReason: "human_question_unavailable"},
	} {
		for _, retry := range []bool{false, true} {
			name := tt.name + "/candidate"
			if retry {
				name = tt.name + "/retry"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				db := openWorkAttemptRecoveryStore(t, t.Context())
				tracker := &questionTracker{Connector: memory.New(memory.Config{})}
				o := newWorkAttemptRecoveryOrchestratorWithConnector(t, db, nil, tracker)
				issue := dispatchTestIssue("2995", "In Progress")
				issue.PullRequest = &connector.PullRequest{HeadSHA: "head", BaseSHA: "base", MergeableState: "dirty"}
				request := RunRequest{Issue: issue}
				o.attachHumanQuestionTool(&request)
				result, err := request.AgentToolHandler(t.Context(), runner.AgentToolCall{Name: "ask_human_question", Arguments: json.RawMessage(`{"key":"target","question":"Which target?"}`)})
				if err != nil || !result.Success {
					t.Fatalf("question = %+v, %v", result, err)
				}
				if tt.changeBase {
					issue.PullRequest.BaseSHA = "new-base"
				}
				if tt.answered {
					at := tracker.comments[0].CreatedAt.Add(time.Minute)
					tracker.comments = append(tracker.comments, connector.IssueComment{ID: "reply", Body: "Use target A", CreatedAt: &at, AuthorAuthorized: true})
				}
				if tt.unavailable {
					o.workAttempts = unavailableHumanQuestionStore{Store: db, HumanQuestionStore: db.(store.HumanQuestionStore)}
				}
				now := time.Now()
				state := newState(o.cfg)
				if retry {
					state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 2, DueAt: now.Add(-time.Minute)}
				}
				for cycle := range 2 {
					var decisions []dispatchPlanDecision
					calls := 0
					o.liveDispatchPlanner(t.Context()).plan(&state, []connector.Issue{issue}, now.Add(time.Duration(cycle)*8*time.Hour), dispatchPlanHooks{
						decision: func(d dispatchPlanDecision) { decisions = append(decisions, d) },
						dispatch: func(action dispatchAction) bool {
							calls++
							if tt.answered {
								found := false
								for _, comment := range action.issue.Comments {
									if comment.ID == "reply" {
										found = true
									}
								}
								if !found {
									t.Error("authorized reply missing from dispatched issue")
								}
							}
							return true
						},
					})
					if len(decisions) != 1 {
						t.Fatalf("decisions = %+v", decisions)
					}
					d := decisions[0]
					if d.SkipReason != tt.wantReason || d.Selected != (tt.wantReason == "") {
						t.Fatalf("decision = %+v, want reason %q", d, tt.wantReason)
					}
					if tt.wantReason != "" && calls != 0 {
						t.Fatal("waiting issue reached post-selection dispatch/refusal path")
					}
					if retry && tt.wantReason != "" {
						if _, ok := state.Retry[issue.ID]; !ok {
							t.Fatal("question wait lost retry")
						}
					}
					status := projectDispatchStatusFromCycle(state.DispatchStatus, o.cfg.Project.ID, []connector.Issue{issue}, decisions, nil, now.Add(time.Duration(cycle)*8*time.Hour))
					state.DispatchStatus = status
					if tt.wantReason == "human_question_wait" && (dispatchStatusSnapshot(status, time.Hour, now.Add(time.Duration(cycle)*8*time.Hour)).Stalled || status.CandidateCount != 0) {
						t.Fatalf("waiting-only dispatch status = %+v", status)
					}
				}
			})
		}
	}
}

func TestHumanQuestionDispatchSkipsBeforeSelection(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		retry, full bool
	}{
		{name: "candidate"},
		{name: "retry", retry: true},
		{name: "candidate at capacity", full: true},
		{name: "retry at capacity", retry: true, full: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := openWorkAttemptRecoveryStore(t, t.Context())
			issue := dispatchTestIssue("2995", "In Progress")
			tracker := &questionTracker{Connector: memory.New(memory.Config{Issues: []connector.Issue{issue}})}
			o := newWorkAttemptRecoveryOrchestratorWithConnector(t, db, nil, tracker)
			request := RunRequest{Issue: issue}
			o.attachHumanQuestionTool(&request)
			result, err := request.AgentToolHandler(t.Context(), runner.AgentToolCall{Name: "ask_human_question", Arguments: json.RawMessage(`{"key":"target","question":"Which target?"}`)})
			if err != nil || !result.Success {
				t.Fatalf("question = %+v, %v", result, err)
			}
			now := time.Now()
			state := newState(o.cfg)
			if tt.full {
				busy := dispatchTestIssue("busy", "In Progress")
				state.Running[busy.ID] = Running{Issue: busy}
			}
			if tt.retry {
				state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 2, DueAt: now.Add(-time.Minute)}
			}
			for cycle := range 2 {
				o.dispatchReadyIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Duration(cycle)*8*time.Hour))
				if len(state.SchedulerDecisions) != cycle+1 {
					t.Fatalf("scheduler decisions = %+v; want one pre-selection skip per cycle", state.SchedulerDecisions)
				}
				for _, decision := range state.SchedulerDecisions {
					if decision.Selected || decision.Reason != "human_question_wait" {
						t.Fatalf("unexpected decision = %+v", decision)
					}
				}
				if got := dispatchStatusSnapshot(state.DispatchStatus, time.Hour, now.Add(time.Duration(cycle)*8*time.Hour)); got.Stalled || got.CandidateCount != 0 {
					t.Fatalf("dispatch status = %+v", got)
				}
			}
		})
	}
}

func TestHumanQuestionToolInfrastructureGuidance(t *testing.T) {
	t.Parallel()
	db := openWorkAttemptRecoveryStore(t, t.Context())
	tracker := &questionTracker{Connector: memory.New(memory.Config{})}
	o := newWorkAttemptRecoveryOrchestratorWithConnector(t, db, nil, tracker)
	request := RunRequest{Issue: dispatchTestIssue("2995", "In Progress")}
	o.attachHumanQuestionTool(&request)
	if len(request.AgentTools) != 1 {
		t.Fatalf("tools = %+v", request.AgentTools)
	}
	for _, phrase := range []string{"CI runners", "worker credentials", "host tools", "gate that fails on unchanged main", "belong to the instance", "record the exact failure in the Workpad and end the turn"} {
		t.Run(phrase, func(t *testing.T) {
			if !strings.Contains(request.AgentTools[0].Description, phrase) {
				t.Fatalf("tool description omits %q", phrase)
			}
		})
	}
}

func TestHumanQuestionAllowanceTriageEligibility(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, key string
		answered  bool
	}{
		{name: "ordinary unanswered question", key: "target"},
		{name: "migrated unanswered question", key: "migration:target"},
		{name: "answered question permits triage", key: "target", answered: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			db := openWorkAttemptRecoveryStore(t, t.Context())
			issue := dispatchTestIssue("2995", "In Progress")
			tracker := &questionTracker{Connector: memory.New(memory.Config{Issues: []connector.Issue{issue}})}
			o := newWorkAttemptRecoveryOrchestratorWithConnector(t, db, nil, tracker)
			o.cfg.Claiming.Enabled = false
			o.cfg.AutoPromote.Enabled = true
			o.cfg.AutoPromote.SourceState = issue.State
			o.supervisor = newTestSupervisor(t, attemptTriageRunner{}, o.cfg)
			o.runResults = make(chan runner.Completion, 1)
			now := time.Now()
			for i := range 3 {
				id := startRecoveryWorkAttempt(t, t.Context(), db, issue, store.WorkAttemptStatusActive, "", now.Add(-time.Duration(10-i)*time.Minute))
				if err := db.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: id, CompletedAt: now.Add(-time.Duration(9-i) * time.Minute), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalFailure, ErrorClass: "runner_error"}); err != nil {
					t.Fatal(err)
				}
			}
			allowance, err := o.issueAttemptAllowance(t.Context(), issue)
			if err != nil || !allowance.exhausted() {
				t.Fatalf("allowance = %+v, %v", allowance, err)
			}
			request := RunRequest{Issue: issue}
			o.attachHumanQuestionTool(&request)
			args, err := json.Marshal(map[string]string{"key": tt.key, "question": "Which target?"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := request.AgentToolHandler(t.Context(), runner.AgentToolCall{Name: "ask_human_question", Arguments: args})
			if err != nil || !result.Success {
				t.Fatalf("question = %+v, %v", result, err)
			}
			if tt.answered {
				at := tracker.comments[0].CreatedAt.Add(time.Minute)
				tracker.comments = append(tracker.comments, connector.IssueComment{ID: "reply", Body: "Use target A", CreatedAt: &at, AuthorAuthorized: true})
			}
			state := newState(o.cfg)
			o.autoPromoteHumanReviewIssues(t.Context(), &state, []connector.Issue{issue}, now)
			_, running := state.Running[issue.ID]
			if running != tt.answered {
				t.Fatalf("triage running = %v, want %v", running, tt.answered)
			}
		})
	}
}
