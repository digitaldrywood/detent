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
	for _, retry := range []bool{false, true} {
		name := "candidate"
		if retry {
			name = "retry"
		}
		t.Run(name, func(t *testing.T) {
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
			if retry {
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
