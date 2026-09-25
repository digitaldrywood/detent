package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestCompletedActiveReviewRequiresFinishedWork(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		status     string
		prose      string
		draft      bool
		wantReview bool
	}{
		{name: "unfinished workpad", status: workpad.StatusInProgress},
		{name: "unfinished workpad with appended report", status: workpad.StatusInProgress, prose: "Rebased and lease-pushed draft PR. No production implementation added in this pass. Final validation remains outstanding."},
		{name: "completed workpad with draft PR", status: workpad.StatusComplete, draft: true},
		{name: "draft PR without workpad", draft: true},
		{name: "completed ready PR", status: workpad.StatusComplete, wantReview: true},
		{name: "legacy ready PR", wantReview: true},
	} {
		for _, lane := range []string{"In Progress", "Rework"} {
			t.Run(tt.name+"/"+lane, func(t *testing.T) {
				t.Parallel()
				issue := completionTransitionIssue(lane, "OPEN")
				issue.PullRequest.Draft = tt.draft
				if tt.status != "" {
					issue.Comments = []connector.IssueComment{{Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: " + tt.status + "\nblockers: []\nhuman_action: null\n```\n\n" + tt.prose}}
				}
				cfg := normalizeConfig(Config{ActiveStates: []string{"In Progress", "Rework"}, TerminalStates: []string{"Done"}})
				tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
				orch := &Orchestrator{cfg: cfg, connector: tracker}
				state := newState(cfg)
				now := time.Date(2026, 9, 20, 15, 36, 5, 0, time.UTC)
				state.Completed[issue.ID] = Completed{Issue: issue, FinalState: FinalStateCompleted, CompletedAt: now.Add(-24 * time.Hour), successfulAttemptPersisted: true}
				gateCfg := cfg
				gateCfg.AutoPromote.Enabled = true
				gateCfg.AutoPromote.Gate = gate.Config{Kind: gate.KindCommand}
				wantGateWait := tt.wantReview && lane == "In Progress"
				if got := autoPromoteActiveGatePendingIssue(issue, &state, gateCfg, gateCfg.AutoPromote); got != wantGateWait {
					t.Fatalf("completed gate wait = %t, want %t", got, wantGateWait)
				}
				result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)
				if got := len(result.transitioned) > 0; got != tt.wantReview {
					t.Fatalf("review transition = %t, want %t", got, tt.wantReview)
				}
				if !tt.wantReview && len(tracker.updates) != 0 {
					t.Fatalf("unfinished work changed lanes: %#v", tracker.updates)
				}
				if tt.wantReview {
					return
				}
				// Exercise the successful-session boundary as well as tick recovery.
				issue.PullRequest.Number = 17
				issue.PullRequest.HeadSHA = "rebased-head"
				tracker.stateIssues = []connector.Issue{issue}
				attempts := &recordingWorkAttemptStore{}
				orch.workAttempts = attempts
				orch.scheduling = &hubSchedulingSource{}
				state = newState(cfg)
				state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, WorkAttemptID: 42, Mode: runpkg.RunModeImplement, DispatchSourceState: lane, StartedAt: now.Add(-time.Minute), DiffStats: DiffStats{Status: "clean"}}
				state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
				orch.handleRunResult(t.Context(), &state, runpkg.Completion{
					IssueID: issue.ID, CompletedAt: now,
					Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement},
					Result:  runpkg.RunResult{FinalState: FinalStateCompleted, PullRequestUpdated: true, DiffStats: DiffStats{Status: "clean"}},
				})
				if len(tracker.updates) != 0 {
					t.Fatalf("successful unfinished session changed lanes: %#v", tracker.updates)
				}
				if retry, ok := state.Retry[issue.ID]; !ok || retry.Issue.State != lane {
					t.Fatalf("continuation = %#v, present=%t; want implementation in %s", retry, ok, lane)
				}
			})
		}
	}
}

func TestCompletedActiveReviewTargetState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		issue          connector.Issue
		finalState     string
		completionKind string
		cfg            AutoPromoteConfig
		want           string
	}{
		{
			name:       "todo completed with open pull request advances to human review when disabled",
			issue:      completionTransitionIssue("Todo", "OPEN"),
			finalState: FinalStateCompleted,
			want:       autoPromoteSourceState,
		},
		{
			name:       "in progress completed with open pull request advances to human review when disabled",
			issue:      completionTransitionIssue("In Progress", "OPEN"),
			finalState: FinalStateCompleted,
			want:       autoPromoteSourceState,
		},
		{
			name:       "artifact todo completed without pull request advances to configured review",
			issue:      completionTransitionIssue("Todo", ""),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				SourceState: "Review",
				Gate:        gate.Config{Kind: gate.KindArtifact},
			},
			want: "Review",
		},
		{
			name:       "artifact production completed with source gate wait advances to configured review",
			issue:      completionTransitionIssue("Production", ""),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:     true,
				SourceState: "Review",
				PassState:   "Ready for Pickup",
				ReworkState: "Rework",
				Gate: gate.Config{
					Kind: gate.KindArtifact,
					Artifact: gate.ArtifactConfig{
						StatusField:    "render_status",
						PassStatuses:   []string{"approved", "valid"},
						WaitStatuses:   []string{"queued", "rendering", "pending_review"},
						ReworkStatuses: []string{"recut", "invalid", "missing_assets"},
					},
				},
			},
			want: "Review",
		},
		{
			name:       "rework completed with open pull request advances to human review",
			issue:      completionTransitionIssue("Rework", "OPEN"),
			finalState: FinalStateCompleted,
			want:       autoPromoteSourceState,
		},
		{
			name:           "operational rework completion advances to review",
			completionKind: workpad.CompletionOperational,
			issue: func() connector.Issue {
				issue := completionTransitionIssue("Rework", "")
				issue.Description = operationalCompletionAuthorizationBody()
				issue.Comments = []connector.IssueComment{{
					Body: operationalCompletionWorkpadBody("Runner service is healthy and accepting jobs."),
				}}
				return issue
			}(),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate:    gate.Config{Kind: gate.KindCommand},
			},
			want: autoPromoteSourceState,
		},
		{
			name: "unaccepted operational completion stays active",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "")
				issue.Description = operationalCompletionAuthorizationBody()
				issue.Comments = []connector.IssueComment{{
					Body: operationalCompletionWorkpadBody("Runner service is healthy and accepting jobs."),
				}}
				return issue
			}(),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate:    gate.Config{Kind: gate.KindCommand},
			},
		},
		{
			name:       "artifact rework completed without pull request advances to configured review",
			issue:      completionTransitionIssue("Rework", ""),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:     true,
				SourceState: "Review",
				PassState:   "Ready for Pickup",
				ReworkState: "Rework",
				Gate:        artifactCompletionTestGate(),
			},
			want: "Review",
		},
		{
			name:       "merging completed with open pull request waits for merge lifecycle",
			issue:      completionTransitionIssue("Merging", "OPEN"),
			finalState: FinalStateCompleted,
		},
		{
			name:       "todo completed without pull request waits when pull request required",
			issue:      completionTransitionIssue("Todo", ""),
			finalState: FinalStateCompleted,
		},
		{
			name:       "zero quiet command gate with source wait skips human review target",
			issue:      completionTransitionIssue("In Progress", "OPEN"),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate:    gate.Config{Kind: gate.KindCommand},
			},
		},
		{
			name:       "command gate with quiet window and source wait skips human review target",
			issue:      completionTransitionIssue("In Progress", "OPEN"),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
				Gate:          gate.Config{Kind: gate.KindCommand},
			},
		},
		{
			name: "unresolved review threads bypass command gate wait",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "OPEN")
				issue.PullRequest.UnresolvedReviewThreads = []connector.PullRequestReviewThread{{Path: "internal/orchestrator/state.go", Line: 42}}
				return issue
			}(),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate:    gate.Config{Kind: gate.KindCommand},
			},
			want: autoPromoteSourceState,
		},
		{
			name:       "zero quiet command gate with review wait advances to human review",
			issue:      completionTransitionIssue("In Progress", "OPEN"),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:       true,
				GateWaitState: autoPromoteGateWaitReview,
				Gate:          gate.Config{Kind: gate.KindCommand},
			},
			want: autoPromoteSourceState,
		},
		{
			name:       "human review gate keeps human review target",
			issue:      completionTransitionIssue("In Progress", "OPEN"),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled: true,
				Gate:    gate.Config{Kind: gate.KindHumanReview},
			},
			want: autoPromoteSourceState,
		},
		{
			name: "opt out label keeps human review target",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "OPEN")
				issue.Labels = []string{"requires-human-review"}
				return issue
			}(),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:     true,
				OptoutLabel: "requires-human-review",
				Gate:        gate.Config{Kind: gate.KindCommand},
			},
			want: autoPromoteSourceState,
		},
		{
			name: "allowlist miss keeps human review target",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "OPEN")
				issue.Labels = []string{"bug"}
				return issue
			}(),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:            true,
				AllowedIssueLabels: []string{"release"},
				Gate:               gate.Config{Kind: gate.KindCommand},
			},
			want: autoPromoteSourceState,
		},
		{
			name: "allowlist hit skips human review target",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "OPEN")
				issue.Labels = []string{"release"}
				return issue
			}(),
			finalState: FinalStateCompleted,
			cfg: AutoPromoteConfig{
				Enabled:            true,
				AllowedIssueLabels: []string{"release"},
				Gate:               gate.Config{Kind: gate.KindCommand},
			},
		},
	}

	activeStates := normalizedStates([]string{"Todo", "In Progress", "Production", "Rework", "Merging"})
	terminalStates := normalizedStates([]string{"Ready for Pickup", "Done", "Cancelled"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := completedActiveReviewTargetState(
				tt.issue,
				tt.finalState,
				tt.completionKind,
				activeStates,
				terminalStates,
				tt.cfg,
			)
			if got != tt.want {
				t.Fatalf("completedActiveReviewTargetState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestActiveArtifactGateWaitReviewTargetState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		issue connector.Issue
		want  string
	}{
		{
			name:  "production pending review moves to review",
			issue: artifactCompletionTransitionIssue("Production", "pending_review"),
			want:  "Review",
		},
		{
			name:  "rework rendering moves to review",
			issue: artifactCompletionTransitionIssue("Rework", "rendering"),
			want:  "Review",
		},
		{
			name:  "todo queued does not move to review",
			issue: artifactCompletionTransitionIssue("Todo", "queued"),
		},
		{
			name:  "production pass status does not move to review",
			issue: artifactCompletionTransitionIssue("Production", "approved"),
		},
	}

	activeStates := normalizedStates([]string{"Todo", "Production", "Rework"})
	terminalStates := normalizedStates([]string{"Ready for Pickup", "Done", "Cancelled"})
	cfg := AutoPromoteConfig{
		Enabled:     true,
		SourceState: "Review",
		PassState:   "Ready for Pickup",
		ReworkState: "Rework",
		Gate:        artifactCompletionTestGate(),
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := activeArtifactGateWaitReviewTargetState(tt.issue, activeStates, terminalStates, cfg)
			if got != tt.want {
				t.Fatalf("activeArtifactGateWaitReviewTargetState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAutoPromoteActiveGatePendingIssueIncludesCompletedArtifact(t *testing.T) {
	t.Parallel()

	issue := artifactCompletionTransitionIssue("Production", "pending_review")
	cfg := normalizeConfig(Config{
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Backlog", "Review", "Blocked"},
		TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			SourceState:   "Review",
			PassState:     "Ready for Pickup",
			ReworkState:   "Rework",
			GateWaitState: autoPromoteGateWaitSource,
			Gate:          artifactCompletionTestGate(),
		},
	})
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:      issue,
		FinalState: FinalStateCompleted,
	}

	if !autoPromoteActiveGatePendingIssue(issue, &state, cfg, cfg.AutoPromote) {
		t.Fatal("completed artifact gate wait was not recognized without a pull request")
	}
}

func TestAutoPromoteActiveGatePendingIssueRequiresAcceptedOperationalCompletion(t *testing.T) {
	t.Parallel()

	issue := completionTransitionIssue("In Progress", "")
	issue.Description = operationalCompletionAuthorizationBody()
	issue.Comments = []connector.IssueComment{{
		Body: operationalCompletionWorkpadBody("Runner service is healthy and accepting jobs."),
	}}
	cfg := normalizeConfig(Config{
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates: []string{"Human Review", "Blocked"},
		TerminalStates: []string{"Done", "Cancelled"},
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			GateWaitState: autoPromoteGateWaitSource,
			Gate:          gate.Config{Kind: gate.KindCommand},
		},
	})
	tests := []struct {
		name           string
		completionKind string
		want           bool
	}{
		{name: "current declaration without accepted attempt"},
		{name: "accepted operational completion", completionKind: workpad.CompletionOperational, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newState(cfg)
			state.Completed[issue.ID] = Completed{
				Issue:          issue,
				FinalState:     FinalStateCompleted,
				CompletionKind: tt.completionKind,
			}
			if got := autoPromoteActiveGatePendingIssue(issue, &state, cfg, cfg.AutoPromote); got != tt.want {
				t.Fatalf("autoPromoteActiveGatePendingIssue() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestTransitionActiveArtifactGateWaitIssuesReconcilesToReviewAfterRestart(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 15, 35, 0, 0, time.UTC)
	issue := artifactCompletionTransitionIssue("Production", "pending_review")
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Backlog", "Review", "Blocked"},
		TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
		AutoPromote: AutoPromoteConfig{
			Enabled:     true,
			SourceState: "Review",
			PassState:   "Ready for Pickup",
			ReworkState: "Rework",
			Gate:        artifactCompletionTestGate(),
		},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)

	result := orch.transitionActiveArtifactGateWaitIssuesToReview(context.Background(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Review"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if _, ok := state.Completed[issue.ID]; ok {
		t.Fatalf("Completed[%q] present after restart reconciliation", issue.ID)
	}
	if len(result.dispatchCandidates) != 0 {
		t.Fatalf("dispatchCandidates = %#v, want none", result.dispatchCandidates)
	}
	if len(state.RecentEvents) != 1 || state.RecentEvents[0].Event != "artifact_gate_wait_review_reconciliation" {
		t.Fatalf("RecentEvents = %#v, want artifact gate wait reconciliation event", state.RecentEvents)
	}
}

func TestTransitionCompletedActiveIssuesLeavesAutoPromoteIssueActive(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 14, 0, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "OPEN")
	issue.PullRequest = &connector.PullRequest{
		Number:           17,
		URL:              "https://github.test/digitaldrywood/detent/pull/17",
		State:            "OPEN",
		MergeableState:   "unknown",
		CIStatus:         "pending",
		CodexReviewState: "COMMENTED",
	}
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 0,
			Gate:          gate.Config{Kind: gate.KindCommand},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:       issue,
		CompletedAt: now.Add(-time.Minute),
		FinalState:  FinalStateCompleted,
	}

	result := orch.transitionCompletedActiveIssuesToReview(context.Background(), &state, []connector.Issue{issue}, now)

	if len(result.transitioned) != 0 {
		t.Fatalf("transitioned = %#v, want none", result.transitioned)
	}
	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want no backend write", tracker.updates)
	}
	if len(result.dispatchCandidates) != 0 {
		t.Fatalf("dispatchCandidates = %#v, want none", result.dispatchCandidates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want none", tracker.comments)
	}
	if got := state.Completed[issue.ID].Issue.State; got != "In Progress" {
		t.Fatalf("Completed issue state = %q, want In Progress", got)
	}
	if len(state.RecentEvents) != 0 {
		t.Fatalf("RecentEvents = %#v, want none", state.RecentEvents)
	}
}

func TestCompletedReadyPullRequestEntersMergeGate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 25, 12, 2, 0, 0, time.UTC)
	for _, tt := range []struct {
		name     string
		ciStatus string
	}{
		{name: "CI pending", ciStatus: "pending"},
		{name: "CI passed", ciStatus: "pass"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			completedAt := now.Add(-25 * time.Minute)
			issue := completionTransitionIssue("In Progress", "OPEN")
			issue.PullRequest.Number = 3074
			issue.PullRequest.URL = "https://github.test/digitaldrywood/detent/pull/3074"
			issue.PullRequest.HeadSHA = "published-head"
			issue.PullRequest.MergeableState = "clean"
			issue.PullRequest.CIStatus = tt.ciStatus
			issue.PullRequest.CodexReviewState = "COMMENTED"
			issue.Comments = []connector.IssueComment{{Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"}}
			cfg := normalizeConfig(Config{
				AutoPromote:  AutoPromoteConfig{Enabled: true, GateWaitState: autoPromoteGateWaitSource, Gate: gate.Config{Kind: gate.KindCommand}},
				ActiveStates: []string{"Todo", "In Progress", "Rework", "Merging"}, TerminalStates: []string{"Done", "Cancelled"},
			})
			entered := now.Add(-time.Hour)
			issue.StageUpdatedAt = &entered
			baseTracker := &autoPromoteTickConnector{
				stateIssues:   []connector.Issue{issue},
				issueComments: map[string][]connector.IssueComment{issue.ID: issue.Comments},
			}
			tracker := &autoPromoteTickMergeConnector{autoPromoteTickConnector: baseTracker}
			attempts := &recordingWorkAttemptStore{}
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, recoveryInspector: strandedActiveRecoveryInspector{snapshot: runpkg.BlockedRecoverySnapshot{WorkspaceStatus: "missing"}}}
			state := newState(cfg)
			state.StrandedActiveThreshold = 10 * time.Minute
			state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, WorkAttemptID: 42, Mode: runpkg.RunModeImplement, DispatchSourceState: "In Progress", StartedAt: completedAt.Add(-time.Minute), DiffStats: DiffStats{Status: "clean"}}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: completedAt.Add(-time.Minute)}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: issue.ID, CompletedAt: completedAt,
				Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement},
				Result:  runpkg.RunResult{FinalState: FinalStateCompleted, PullRequestUpdated: true, PullRequestHeadPushed: true, CITriggerLabelReapplied: true, DiffStats: DiffStats{Status: "clean"}},
			})
			if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalSuccess {
				t.Fatalf("completions = %#v, want one successful attempt", attempts.completions)
			}
			if completed := state.Completed[issue.ID]; !completed.successfulAttemptPersisted {
				t.Fatalf("completed = %#v, want persisted success", completed)
			}
			state.WorkAttempts = []telemetry.WorkAttempt{{IssueID: issue.ID, Status: "completed", CompletedAt: &completedAt}}

			orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)
			if !autoPromoteActiveGatePendingIssue(issue, &state, cfg, cfg.AutoPromote) {
				t.Fatal("completed ready PR did not enter the existing gate wait")
			}
			if diagnostics := strandedActiveIssueSnapshots(state, issueSnapshots([]connector.Issue{issue}, 0, 0, now, state.laneEntries), now); len(diagnostics) != 1 || diagnostics[0].DurationSeconds != int64((25*time.Minute)/time.Second) {
				t.Fatalf("stranded diagnostics = %#v, want the recorded 25-minute completion-to-recovery gap", diagnostics)
			}
			if recovered := orch.recoverStrandedActiveIssues(t.Context(), &state, []connector.Issue{issue}, now); len(recovered) != 0 {
				t.Fatalf("completed issue recovered as stranded: %#v", recovered)
			}
			if len(baseTracker.updates) != 0 {
				t.Fatalf("gate wait changed lanes before promotion: %#v", baseTracker.updates)
			}
			issue.PullRequest.CIStatus = "pass"
			promoted := orch.autoPromoteHumanReviewIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Minute))
			if _, ok := promoted.transitioned[issue.ID]; !ok {
				t.Fatalf("green current head did not advance from the gate: decision = %#v", EvaluateAutoPromote(issue, AutoPromoteSummaryFromIssue(issue), cfg.AutoPromote, now.Add(time.Minute)))
			}
			if got, want := baseTracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Merging"}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("updates = %#v, want %#v", got, want)
			}
		})
	}
}

func TestTransitionCompletedActiveIssuesRoutesUnresolvedReviewThreadsToRework(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "OPEN")
	issue.PullRequest = &connector.PullRequest{
		Number:   2104,
		URL:      "https://github.test/digitaldrywood/detent/pull/2104",
		State:    "OPEN",
		CIStatus: "pass",
		UnresolvedReviewThreads: []connector.PullRequestReviewThread{{
			Path: "internal/orchestrator/autopromote.go",
			Line: 181,
		}},
	}
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate:          gate.Config{Kind: gate.KindHumanReview},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:       issue,
		CompletedAt: now.Add(-time.Minute),
		FinalState:  FinalStateCompleted,
	}

	result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Rework"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one rework comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Auto-promote routed this issue from In Progress to Rework: linked PR has 1 unresolved review thread.",
		"reason: unresolved_review_threads",
		"unresolved_review_threads: 1",
		"first_unresolved_review_thread: internal/orchestrator/autopromote.go:181",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
}

// Reproduce #2721: completed Rework cards with unresolved threads were removed
// on every refresh, until an operator label edit cleared their completion state.
func TestCompletedReworkCandidatesRemainVisible(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ number, pr, threads int }{
		{2659, 2677, 2}, {2660, 2668, 1}, {2663, 2673, 2},
	} {
		t.Run(strconv.Itoa(tt.number), func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 9, 15, 2, 21, 0, 0, time.UTC)
			issue := completionTransitionIssue("Rework", "OPEN")
			issue.ID = strconv.Itoa(tt.number)
			issue.Identifier = fmt.Sprintf("digitaldrywood/detent#%d", tt.number)
			issue.PullRequest.Number = tt.pr
			issue.PullRequest.MergeableState = "blocked"
			issue.PullRequest.CIStatus = "pass"
			issue.PullRequest.UnresolvedReviewThreads = make([]connector.PullRequestReviewThread, tt.threads)
			cfg := normalizeConfig(Config{
				AutoPromote:  AutoPromoteConfig{Enabled: true, QuietDuration: 10 * time.Minute, Gate: gate.Config{Kind: gate.KindCommand}},
				ActiveStates: []string{"Todo", "In Progress", "Rework", "Merging"}, TerminalStates: []string{"Done", "Cancelled"},
			})
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			orch := &Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			state := newState(cfg)
			slot := dispatchTestIssue("occupied-slot", "Merging")
			state.Running[slot.ID] = Running{Issue: slot}
			state.Completed[issue.ID] = Completed{Issue: issue, CompletedAt: now.Add(-5 * time.Hour), FinalState: FinalStateCompleted}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-5 * time.Hour)}
			for refresh := range 2 {
				orch.tick(t.Context(), &state, now.Add(time.Duration(refresh)*time.Minute))
				if len(state.BoardIssues) != 1 || state.BoardIssues[0].ID != issue.ID {
					t.Fatalf("refresh %d board = %#v, want %s", refresh, state.BoardIssues, issue.Identifier)
				}
				if state.DispatchStatus.CandidateCount != 1 {
					t.Fatalf("refresh %d candidate count = %d", refresh, state.DispatchStatus.CandidateCount)
				}
				if state.CandidatesMissingVsTracker == nil || *state.CandidatesMissingVsTracker != 0 {
					t.Fatalf("missing candidates = %v", state.CandidatesMissingVsTracker)
				}

				if _, ok := state.Completed[issue.ID]; ok {
					t.Fatal("completion still prevents Rework dispatch")
				}
				if _, ok := state.Claimed[issue.ID]; ok {
					t.Fatal("completed claim retained")
				}
			}
			if len(tracker.updates) != 0 || len(tracker.comments) != 0 {
				t.Fatal("same-state handoff wrote tracker")
			}
		})
	}
}

func TestTransitionCompletedActiveIssuesWaitsWhenReviewThreadsUnavailable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 15, 2, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "OPEN")
	issue.PullRequest.HydrationUnavailableReason = connector.PullRequestHydrationReasonRateLimited
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled: true,
			Gate:    gate.Config{Kind: gate.KindHumanReview},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:       issue,
		CompletedAt: now.Add(-time.Minute),
		FinalState:  FinalStateCompleted,
	}

	result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok || len(tracker.updates) != 0 || len(tracker.comments) != 0 {
		t.Fatalf("result = %#v updates = %#v comments = %#v, want parked without backend transition", result, tracker.updates, tracker.comments)
	}
	if got := state.Completed[issue.ID].Issue.State; got != "In Progress" {
		t.Fatalf("Completed issue state = %q, want In Progress", got)
	}
}

func TestHandleRunResultReleasesClaimWhenReviewThreadsUnavailable(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name          string
		reason        string
		completionErr error
		wantClaimed   bool
		wantReleases  int
	}{
		{name: "REST budget reserved", reason: connector.PullRequestHydrationReasonRESTBudgetReserved, wantReleases: 1},
		{name: "rate limited", reason: connector.PullRequestHydrationReasonRateLimited, wantReleases: 1},
		{name: "persistence failed", reason: connector.PullRequestHydrationReasonRESTBudgetReserved, completionErr: errors.New("attempt store unavailable"), wantClaimed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 9, 13, 17, 51, 0, 0, time.UTC)
			issue := completionTransitionIssue("Rework", "OPEN")
			issue.PullRequest.Number = 2104
			issue.PullRequest.HeadSHA = "head-sha"
			issue.PullRequest.CIStatus = "pass"
			issue.PullRequest.HydrationUnavailableReason = tt.reason
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			attempts := &recordingWorkAttemptStore{completionErrors: []error{tt.completionErr}}
			scheduling := &hubSchedulingSource{}
			cfg := normalizeConfig(Config{
				Project: scheduler.ProjectCandidate{ID: "detent"},
				AutoPromote: AutoPromoteConfig{
					Enabled: true,
					Gate:    gate.Config{Kind: gate.KindHumanReview},
				},
				ActiveStates:           []string{"Todo", "In Progress", "Rework", "Merging"},
				TerminalStates:         []string{"Done", "Cancelled"},
				ContinuationRetryDelay: time.Minute,
			})
			orch := &Orchestrator{cfg: cfg, connector: tracker, scheduling: scheduling, workAttempts: attempts}
			state := newState(cfg)
			state.Running[issue.ID] = Running{
				Issue:               issue,
				Attempt:             1,
				WorkAttemptID:       42,
				Mode:                runpkg.RunModeImplement,
				DispatchSourceState: "Rework",
				StartedAt:           now.Add(-time.Minute),
				DiffStats:           DiffStats{Status: "clean"},
			}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}

			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID:     issue.ID,
				CompletedAt: now,
				Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
				Result: runpkg.RunResult{
					FinalState:              FinalStateCompleted,
					PullRequestHeadPushed:   true,
					PullRequestUpdated:      true,
					CITriggerLabelReapplied: true,
					DiffStats:               DiffStats{Status: "clean"},
				},
			})

			if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalSuccess {
				t.Fatalf("completions = %#v, want one successful persisted attempt", attempts.completions)
			}
			if _, ok := state.Running[issue.ID]; ok {
				t.Fatalf("Running[%q] present after completion", issue.ID)
			}
			if _, ok := state.Retry[issue.ID]; ok {
				t.Fatalf("Retry[%q] present while review-thread hydration is pending", issue.ID)
			}
			if _, ok := state.Completed[issue.ID]; !ok {
				t.Fatalf("Completed[%q] missing while review-thread hydration is pending", issue.ID)
			}
			_, claimed := state.Claimed[issue.ID]
			if claimed != tt.wantClaimed {
				t.Fatalf("Claimed[%q] present = %t, want %t", issue.ID, claimed, tt.wantClaimed)
			}
			if scheduling.releases != tt.wantReleases {
				t.Fatalf("scheduling claim releases = %d, want %d", scheduling.releases, tt.wantReleases)
			}
			if len(tracker.updates) != 0 {
				t.Fatalf("updates = %#v, want no backend transition", tracker.updates)
			}
			if got := tracker.reviewThreadHydrations; !reflect.DeepEqual(got, []string{issue.ID}) {
				t.Fatalf("review thread hydrations = %#v, want one for %s", got, issue.ID)
			}
			laterIssue := cloneIssue(issue)
			laterIssue.PullRequest.HydrationUnavailableReason = ""
			delete(state.Completed, issue.ID)
			decision := orch.dispatchPlanner().dispatchableIssueDecision(laterIssue, &state, false, now.Add(time.Minute), "")
			if tt.wantClaimed {
				if decision.reason != dispatchSkipAlreadyClaimed {
					t.Fatalf("dispatch decision = %#v, want retained claim after persistence failure", decision)
				}
			} else if !decision.dispatchable {
				t.Fatalf("dispatch decision = %#v, want eligibility after existing gates clear", decision)
			}
		})
	}
}

func TestHydrationUnavailableDoesNotReleaseReplacementTrackerClaim(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 13, 18, 0, 0, 0, time.UTC)
	issue := completionTransitionIssue("Rework", "OPEN")
	issue.PullRequest.HydrationUnavailableReason = connector.PullRequestHydrationReasonRESTBudgetReserved
	issue.Fields["Detent Lease"] = now.Add(-time.Minute).Format(time.RFC3339Nano)
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled: true,
			Gate:    gate.Config{Kind: gate.KindHumanReview},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
		Claiming: ClaimingConfig{
			Enabled:    true,
			LeaseField: "Detent Lease",
		},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:                      issue,
		CompletedAt:                now,
		FinalState:                 FinalStateCompleted,
		successfulAttemptPersisted: true,
	}
	state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}

	orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)

	if _, ok := state.Claimed[issue.ID]; ok {
		t.Fatalf("Claimed[%q] present after completed attempt release", issue.ID)
	}
	if got := tracker.stateIssues[0].Fields["Detent Lease"]; got != "" {
		t.Fatalf("Detent Lease after completed attempt release = %q, want empty", got)
	}

	replacementLease := now.Add(time.Minute).Format(time.RFC3339Nano)
	tracker.stateIssues[0].Fields["Detent Lease"] = replacementLease
	replacement := cloneIssue(tracker.stateIssues[0])
	orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{replacement}, now.Add(time.Minute))

	if got := tracker.stateIssues[0].Fields["Detent Lease"]; got != replacementLease {
		t.Fatalf("replacement Detent Lease = %q, want %q", got, replacementLease)
	}
	if got := len(tracker.setFields); got != 1 {
		t.Fatalf("claim field writes = %d, want one completed-attempt release", got)
	}
}

func TestHydrationUnavailablePreservesPersistedUnsuccessfulAttemptRetry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 13, 17, 52, 0, 0, time.UTC)
	issue := completionTransitionIssue("Rework", "OPEN")
	issue.PullRequest.Number = 2104
	issue.PullRequest.HeadSHA = "head-sha"
	issue.PullRequest.HydrationUnavailableReason = connector.PullRequestHydrationReasonRESTBudgetReserved
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	scheduling := &hubSchedulingSource{}
	cfg := normalizeConfig(Config{
		Project: scheduler.ProjectCandidate{ID: "detent"},
		AutoPromote: AutoPromoteConfig{
			Enabled: true,
			Gate:    gate.Config{Kind: gate.KindHumanReview},
		},
		ActiveStates:           []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:         []string{"Done", "Cancelled"},
		ContinuationRetryDelay: time.Minute,
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker, scheduling: scheduling}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:       issue,
		CompletedAt: now,
		FinalState:  runpkg.FinalStateFailed,
	}
	state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
	state.Retry[issue.ID] = Retry{Issue: issue, Attempt: 2, DueAt: now.Add(time.Minute)}

	result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Second))

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing for hydration deferral", issue.ID)
	}
	if _, ok := state.Retry[issue.ID]; !ok {
		t.Fatalf("Retry[%q] removed by hydration deferral", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; !ok {
		t.Fatalf("Claimed[%q] removed by hydration deferral", issue.ID)
	}
	if scheduling.releases != 0 {
		t.Fatalf("scheduling claim releases = %d, want 0", scheduling.releases)
	}
}

func TestTransitionCompletedReworkIssueReturnsToHumanReviewAfterThreadsResolve(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 15, 5, 0, 0, time.UTC)
	issue := completionTransitionIssue("Rework", "OPEN")
	issue.PullRequest.Number = 2104
	issue.PullRequest.URL = "https://github.test/digitaldrywood/detent/pull/2104"
	issue.PullRequest.CIStatus = "pass"
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate:          gate.Config{Kind: gate.KindHumanReview},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:       issue,
		CompletedAt: now.Add(-time.Minute),
		FinalState:  FinalStateCompleted,
	}

	result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Human Review"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want no rework comment after threads resolve", tracker.comments)
	}
}

func TestTransitionCompletedActiveIssuesCompletesOperationalWorkWithoutPullRequest(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 18, 18, 0, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "")
	issue.Description = operationalCompletionAuthorizationBody()
	issue.Comments = []connector.IssueComment{{
		Body: operationalCompletionWorkpadBody("Runner service is healthy and accepting jobs."),
		URL:  "https://github.test/comment/operational-completion",
	}}
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled: true,
			Gate:    gate.Config{Kind: gate.KindCommand},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:          issue,
		CompletedAt:    now.Add(-time.Minute),
		FinalState:     FinalStateCompleted,
		CompletionKind: workpad.CompletionOperational,
	}

	result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Done"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if got := state.Completed[issue.ID].Issue.State; got != "Done" {
		t.Fatalf("Completed issue state = %q, want Done", got)
	}
	if len(result.dispatchCandidates) != 0 {
		t.Fatalf("dispatchCandidates = %#v, want none", result.dispatchCandidates)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one audit comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Completed this issue operationally",
		"reason: operational_completion",
		"completion_kind: operational",
		"operational_evidence: Runner service is healthy and accepting jobs.",
		"workpad_comment: https://github.test/comment/operational-completion",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
}

func TestTransitionCompletedActiveIssuesHandlesArtifactReworkNoop(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 12, 13, 0, 0, 0, time.UTC)
	issue := artifactCompletionTransitionIssue("Rework", "recut")
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled:     true,
			SourceState: "Review",
			PassState:   "Ready for Pickup",
			ReworkState: "Rework",
			Gate:        artifactCompletionTestGate(),
		},
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Review"},
		TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:      issue,
		FinalState: FinalStateCompleted,
	}
	state.Retry[issue.ID] = Retry{Issue: issue}
	state.BudgetRefusals[issue.ID] = BudgetRefusal{Issue: issue, Code: "per_issue_max_usd"}

	result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none", tracker.updates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want none", tracker.comments)
	}
	if len(result.dispatchCandidates) != 1 || result.dispatchCandidates[0].ID != issue.ID || result.dispatchCandidates[0].State != "Rework" {
		t.Fatalf("dispatchCandidates = %#v, want same-state Rework issue", result.dispatchCandidates)
	}
	if _, ok := state.Completed[issue.ID]; ok {
		t.Fatalf("Completed[%q] present after same-state auto-promote", issue.ID)
	}
	if _, ok := state.Retry[issue.ID]; !ok {
		t.Fatalf("Retry[%q] missing after same-state auto-promote", issue.ID)
	}
	if _, ok := state.BudgetRefusals[issue.ID]; !ok {
		t.Fatalf("BudgetRefusals[%q] missing after same-state auto-promote", issue.ID)
	}
	if len(state.RecentEvents) != 0 {
		t.Fatalf("RecentEvents = %#v, want none", state.RecentEvents)
	}
}

func TestTransitionCompletedActiveIssuesRoutesInvalidWorkpadStatusToRework(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 9, 14, 10, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "OPEN")
	issue.PullRequest = &connector.PullRequest{
		Number:                 20,
		URL:                    "https://github.test/digitaldrywood/detent/pull/20",
		State:                  "OPEN",
		MergeableState:         "clean",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: timePointer(now.Add(-20 * time.Minute)),
	}
	issue.Comments = []connector.IssueComment{{
		Body: "## Codex Workpad\n\n```detent-status\nschema: 1\nstatus: human-review\nblockers: []\nhuman_action: null\n```",
		URL:  "https://github.test/comment/completed-invalid-workpad",
	}}
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 0,
			GateWaitState: autoPromoteGateWaitReview,
			Gate:          gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:       issue,
		CompletedAt: now.Add(-time.Minute),
		FinalState:  FinalStateCompleted,
	}

	result := orch.transitionCompletedActiveIssuesToReview(context.Background(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Rework"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if got := state.Completed[issue.ID].Issue.State; got != "Rework" {
		t.Fatalf("Completed issue state = %q, want Rework", got)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one rework comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Auto-promote routed this issue from In Progress to Rework",
		"reason: workpad_status_invalid",
		`status "human-review"`,
		"in_progress, blocked, complete",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
}

func TestTransitionCompletedActiveIssuesEscalatesGateWaitTimeout(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 15, 0, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "OPEN")
	issue.PullRequest = &connector.PullRequest{
		Number:         19,
		URL:            "https://github.test/digitaldrywood/detent/pull/19",
		State:          "OPEN",
		MergeableState: "unknown",
		CIStatus:       "pending",
	}
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled:         true,
			QuietDuration:   0,
			GateWaitTimeout: 15 * time.Minute,
			Gate:            gate.Config{Kind: gate.KindCommand},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:       issue,
		CompletedAt: now.Add(-16 * time.Minute),
		FinalState:  FinalStateCompleted,
	}

	result := orch.transitionCompletedActiveIssuesToReview(context.Background(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Human Review"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one timeout comment", tracker.comments)
	}
	for _, fragment := range []string{
		"Auto-promote gate wait timed out",
		"reason: auto_promote_gate_wait_timeout",
		"waited: 16m0s",
		"timeout: 15m0s",
		"https://github.test/digitaldrywood/detent/pull/19",
		"ci_status: pending",
	} {
		if !strings.Contains(tracker.comments[0].body, fragment) {
			t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
		}
	}
	if got := state.Completed[issue.ID].Issue.State; got != "Human Review" {
		t.Fatalf("Completed issue state = %q, want Human Review", got)
	}
	if len(result.dispatchCandidates) != 0 {
		t.Fatalf("dispatchCandidates = %#v, want none", result.dispatchCandidates)
	}
	if len(state.RecentEvents) != 1 || state.RecentEvents[0].Event != "completed_active_gate_wait_timeout" {
		t.Fatalf("RecentEvents = %#v, want timeout event", state.RecentEvents)
	}
}

func TestTransitionCompletedActiveIssuesKeepsHumanReviewWhenRequired(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 14, 0, 0, 0, time.UTC)
	reviewedAt := now.Add(-20 * time.Minute)
	issue := completionTransitionIssue("In Progress", "OPEN")
	issue.PullRequest = &connector.PullRequest{
		Number:                 18,
		URL:                    "https://github.test/digitaldrywood/detent/pull/18",
		State:                  "OPEN",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &reviewedAt,
	}
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	cfg := normalizeConfig(Config{
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate:          gate.Config{Kind: gate.KindHumanReview},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:      issue,
		FinalState: FinalStateCompleted,
	}

	result := orch.transitionCompletedActiveIssuesToReview(context.Background(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing", issue.ID)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Human Review"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if len(result.dispatchCandidates) != 0 {
		t.Fatalf("dispatchCandidates = %#v, want none", result.dispatchCandidates)
	}
	if len(tracker.comments) != 0 {
		t.Fatalf("comments = %#v, want no auto-promote comment for Human Review transition", tracker.comments)
	}
	if got := state.Completed[issue.ID].Issue.State; got != "Human Review" {
		t.Fatalf("Completed issue state = %q, want Human Review", got)
	}
}

func TestTransitionCompletedActiveIssuesRespectsRunningWorker(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 18, 19, 10, 0, 0, time.UTC)
	tests := []struct {
		name           string
		running        bool
		wantTransition bool
	}{
		{name: "completed issue without running worker transitions", wantTransition: true},
		{name: "completed issue with running worker stays active", running: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := completionTransitionIssue("In Progress", "OPEN")
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			cfg := normalizeConfig(Config{
				AutoPromote:    AutoPromoteConfig{Gate: gate.Config{Kind: gate.KindHumanReview}},
				ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
				TerminalStates: []string{"Done", "Cancelled"},
			})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			state.Completed[issue.ID] = Completed{
				Issue:       issue,
				CompletedAt: now.Add(-time.Minute),
				FinalState:  FinalStateCompleted,
			}
			if tt.running {
				state.Running[issue.ID] = Running{Issue: issue, StartedAt: now.Add(-30 * time.Second)}
			}

			result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)

			_, transitioned := result.transitioned[issue.ID]
			if transitioned != tt.wantTransition {
				t.Fatalf("transitioned[%q] present = %v, want %v", issue.ID, transitioned, tt.wantTransition)
			}
			if got := len(tracker.updates); got != boolInt(tt.wantTransition) {
				t.Fatalf("tracker updates = %d, want %d", got, boolInt(tt.wantTransition))
			}
			if tt.running {
				if _, ok := state.Running[issue.ID]; !ok {
					t.Fatalf("Running[%q] missing after blocked transition", issue.ID)
				}
				if got := state.Completed[issue.ID].Issue.State; got != "In Progress" {
					t.Fatalf("Completed issue state = %q, want In Progress", got)
				}
			}
		})
	}
}

func TestTransitionCompletedActiveIssuesAutomatedReviewTimeoutModes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 13, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		mode         string
		timeout      string
		ciStatus     string
		reviewState  string
		wantState    string
		wantDispatch bool
	}{
		{name: "required absent holds for human", mode: gate.AutomatedReviewRequired, ciStatus: "success", wantState: "Human Review"},
		{name: "optional absent merges", mode: gate.AutomatedReviewOptional, ciStatus: "success", wantState: "Merging", wantDispatch: true},
		{name: "optional present merges", mode: gate.AutomatedReviewOptional, ciStatus: "success", reviewState: "COMMENTED", wantState: "Merging", wantDispatch: true},
		{name: "optional late p1 reworks", mode: gate.AutomatedReviewOptional, ciStatus: "success", reviewState: "P1", wantState: "Rework"},
		{name: "optional pending ci keeps waiting", mode: gate.AutomatedReviewOptional, ciStatus: "pending"},
		{name: "optional can hold for human", mode: gate.AutomatedReviewOptional, timeout: autoPromoteGateWaitTimeoutHumanReview, ciStatus: "success", wantState: "Human Review"},
		{name: "off promotes without review", mode: gate.AutomatedReviewOff, ciStatus: "success", wantState: "Merging", wantDispatch: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issue := completionTransitionIssue("In Progress", "OPEN")
			issue.PullRequest.URL = "https://github.test/digitaldrywood/detent/pull/1297"
			issue.PullRequest.MergeableState = "clean"
			issue.PullRequest.CIStatus = tt.ciStatus
			issue.PullRequest.CodexReviewState = tt.reviewState
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			cfg := normalizeConfig(Config{
				AutoPromote: AutoPromoteConfig{
					Enabled:               true,
					GateWaitTimeout:       15 * time.Minute,
					GateWaitTimeoutAction: tt.timeout,
					Gate: gate.Config{
						Kind:            gate.KindCommand,
						AutomatedReview: tt.mode,
					},
				},
				ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
				TerminalStates: []string{"Done", "Cancelled"},
			})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			state.Completed[issue.ID] = Completed{
				Issue:       issue,
				CompletedAt: now.Add(-16 * time.Minute),
				FinalState:  FinalStateCompleted,
			}

			result := orch.transitionCompletedActiveIssuesToReview(context.Background(), &state, []connector.Issue{issue}, now)

			if tt.wantState == "" {
				if len(result.transitioned) != 0 || len(tracker.updates) != 0 {
					t.Fatalf("transitioned = %#v, updates = %#v, want continued wait", result.transitioned, tracker.updates)
				}
				return
			}
			if got := state.Completed[issue.ID].Issue.State; got != tt.wantState {
				t.Fatalf("Completed issue state = %q, want %q", got, tt.wantState)
			}
			if got := len(result.dispatchCandidates) == 1; got != tt.wantDispatch {
				t.Fatalf("dispatch candidate = %t, want %t: %#v", got, tt.wantDispatch, result.dispatchCandidates)
			}
		})
	}
}

func TestTransitionCompletedActiveIssuesHandlesFailedReviewUpdate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 7, 14, 0, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "OPEN")
	trackerErr := errors.New("tracker unavailable")
	tracker := &autoPromoteTickConnector{
		stateIssues: []connector.Issue{issue},
		updateErr:   trackerErr,
	}
	cfg := normalizeConfig(Config{
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{
		Issue:      issue,
		FinalState: FinalStateCompleted,
	}

	result := orch.transitionCompletedActiveIssuesToReview(context.Background(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned[%q] missing after failed update", issue.ID)
	}
	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: issue.ID, state: "Human Review"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want attempted update %#v", got, want)
	}
	if got := state.Completed[issue.ID].Issue.State; got != "In Progress" {
		t.Fatalf("Completed issue state = %q, want original In Progress after failed update", got)
	}
	if len(result.dispatchCandidates) != 0 {
		t.Fatalf("dispatchCandidates = %#v, want none after failed update", result.dispatchCandidates)
	}
	if len(state.RecentEvents) != 0 {
		t.Fatalf("RecentEvents = %#v, want no transition event after failed update", state.RecentEvents)
	}
}

func completionTransitionIssue(state string, pullRequestState string) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = "issue-1"
	issue.Identifier = "digitaldrywood/detent#1"
	issue.Title = "Transition completion"
	issue.State = state
	if pullRequestState != "" {
		issue.PullRequest = &connector.PullRequest{State: pullRequestState}
	}
	return issue
}

func operationalCompletionWorkpadBody(evidence string) string {
	return "## Codex Workpad\n\n```detent-status\n" +
		"schema: 1\n" +
		"status: complete\n" +
		"fields:\n" +
		"  completion_kind: operational\n" +
		"  completion_evidence: \"" + evidence + "\"\n" +
		"blockers: []\n" +
		"human_action: null\n" +
		"```"
}

func operationalCompletionAuthorizationBody() string {
	return "## Completion contract\n\n```detent-completion\n" +
		"schema: 1\n" +
		"completion_kind: operational\n" +
		"```"
}

func artifactCompletionTransitionIssue(state string, status string) connector.Issue {
	issue := completionTransitionIssue(state, "")
	issue.Deliverable = &connector.Deliverable{Kind: "artifact"}
	issue.Fields = map[string]string{"render_status": status}
	return issue
}

func artifactCompletionTestGate() gate.Config {
	return gate.Config{
		Kind: gate.KindArtifact,
		Artifact: gate.ArtifactConfig{
			StatusField:    "render_status",
			PassStatuses:   []string{"approved", "valid"},
			WaitStatuses:   []string{"queued", "rendering", "pending_review"},
			ReworkStatuses: []string{"recut", "invalid", "missing_assets"},
		},
	}
}

func mergedCompletionWorkpadBody() string {
	return strings.Replace(operationalCompletionWorkpadBody("Acceptance regression passed 100 repetitions; git merge-base --is-ancestor merge head exited 0."), "  completion_kind: operational", `  completion_kind: operational
  completion_merged_pr: https://github.com/example/repo/pull/12
  completion_merge_commit: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  completion_branch: origin/main
  completion_branch_head: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  completion_ancestry: verified`, 1)
}

func TestTransitionAlreadyMergedCompletion(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"In Progress", "Rework"} {
		t.Run(lane, func(t *testing.T) {
			t.Parallel()
			now := time.Now()
			issue := completionTransitionIssue(lane, "")
			issue.Description = "Bug already fixed; no operational pre-authorization."
			issue.Comments = []connector.IssueComment{{Body: mergedCompletionWorkpadBody(), URL: "https://github.test/comment/merge-evidence"}}
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			cfg := normalizeConfig(Config{AutoPromote: AutoPromoteConfig{Enabled: true, Gate: gate.Config{Kind: gate.KindCommand}}, ActiveStates: []string{"Todo", "In Progress", "Rework", "Merging"}, TerminalStates: []string{"Done", "Cancelled"}})
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			state.Completed[issue.ID] = Completed{Issue: issue, CompletedAt: now.Add(-time.Minute), FinalState: FinalStateCompleted, CompletionKind: workpad.CompletionOperational}
			result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)
			if len(result.dispatchCandidates) != 0 || len(tracker.updates) != 1 || tracker.updates[0].state != "Done" {
				t.Fatalf("updates=%+v candidates=%+v", tracker.updates, result.dispatchCandidates)
			}
			if len(tracker.comments) != 1 {
				t.Fatalf("comments=%+v", tracker.comments)
			}
			for _, evidence := range []string{"pull/12", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "origin/main", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "100 repetitions", "exited 0"} {
				if !strings.Contains(tracker.comments[0].body, evidence) {
					t.Fatalf("audit missing %q: %s", evidence, tracker.comments[0].body)
				}
			}
		})
	}
}
