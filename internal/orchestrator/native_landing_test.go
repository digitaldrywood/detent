package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativeLandingRunCompletion(t *testing.T) {
	hosted := []connector.WorkflowState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Blocked", "Human Review", "Merging", "Done"}},
		{Name: "Blocked", Transitions: []string{"Todo", "In Progress", "Done"}},
		{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Blocked", "Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Blocked", "Human Review", "In Progress"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	unreachableRework := append(append([]connector.WorkflowState(nil), hosted...), connector.WorkflowState{Name: "Rework", Dispatchable: true})
	undispatchedRework := append(append([]connector.WorkflowState(nil), hosted...), connector.WorkflowState{Name: "Rework"})
	undispatchedRework[4].Transitions = append(append([]string(nil), hosted[4].Transitions...), "Rework")
	fallback := []connector.WorkflowState{
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Merging", "Private", "Private Review", "Working", "Awaiting Review"}},
		{Name: "Done", Terminal: true, Dispatchable: true},
		{Name: "Private", Dispatchable: true, OperatorOnly: true},
		{Name: "Private Review", OperatorOnly: true},
		{Name: "Working", Dispatchable: true},
		{Name: "Awaiting Review"},
	}
	workflow := []connector.WorkflowState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
		{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Human Review", "Blocked", "Rework", "Refresh"}},
		{Name: "Rework", Dispatchable: true, Transitions: []string{"Human Review", "Merging"}},
		{Name: "Refresh", Dispatchable: true, Transitions: []string{"Human Review", "Merging"}},
		{Name: "Blocked", Transitions: []string{"Todo"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	head := strings.Repeat("c", 40)
	landed := &runpkg.NativeLanding{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Landed: true, MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "squash"}
	refused := &runpkg.NativeLanding{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, RefusalKind: "base_protected", Refusal: "the base branch main refused the push: GH006. Allow the runner to push to main, or enable GitHub pull request mode for this project."}
	conflict := &runpkg.NativeLanding{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, RefusalKind: workspace.LandRefusalConflict, Refusal: "GitHub refused the merge (HTTP 405); source merge of exact reviewed head and current base conflicts"}
	unproven := &runpkg.NativeLanding{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head}
	waiting := *unproven
	waiting.RefusalKind = workspace.LandRefusalBaseMoved
	waiting.Refusal = "GitHub refused PUT repos/example/repo/pulls/7/merge: status 405: Pull Request has merge conflicts; current source is unproven"
	for _, test := range []struct {
		absorbed           bool
		name               string
		finalMessage       string
		landing            *runpkg.NativeLanding
		hubState           string
		states             []connector.WorkflowState
		statesErr          error
		updateErr          error
		plain              bool
		noHumanReview      bool
		reworkState        string
		wantState          string
		wantMoves          int
		wantComment        string
		wantDeferred       bool
		wantError          string
		wantContinue       bool
		err                error
		wantInfrastructure bool
		mergeMessage       string
		mergeStatus        int
		sourceConflict     bool
		wantLandingWait    bool
		priorOutage        string
	}{
		{name: "absorbed Rework uses the existing landing completion owner", absorbed: true, landing: landed, hubState: "Done", states: workflow, wantState: "Done", wantComment: "Landed Change Request change_1", wantMoves: 0},
		{name: "a landed version is finished by the hub", landing: landed, hubState: "Done", states: workflow, wantState: "Done", wantComment: "Landed Change Request change_1", wantMoves: 0},
		{name: "a refused landing returns to review with the reason", landing: refused, hubState: "Merging", states: workflow, wantState: "Human Review", wantComment: "enable GitHub pull request mode", wantMoves: 1},
		{name: "a conflict enters rework without human review", landing: conflict, hubState: "Merging", states: workflow, noHumanReview: true, wantState: "Rework", wantComment: "was not landed", wantMoves: 1},
		{name: "a conflict enters configured rework with human review", landing: conflict, hubState: "Merging", states: workflow, reworkState: "Refresh", wantState: "Refresh", wantComment: "was not landed", wantMoves: 1},
		{name: "a hosted conflict returns to In Progress", landing: conflict, hubState: "Merging", states: hosted, wantState: "In Progress", wantComment: "was not landed", wantMoves: 1},
		{name: "a hosted conflict returns to In Progress without human review", landing: conflict, hubState: "Merging", states: hosted, noHumanReview: true, wantState: "In Progress", wantComment: "was not landed", wantMoves: 1},
		{name: "a conflict skips unreachable configured rework", landing: conflict, hubState: "Merging", states: unreachableRework, wantState: "In Progress", wantComment: "was not landed", wantMoves: 1},
		{name: "a conflict skips non-dispatchable configured rework", landing: conflict, hubState: "Merging", states: undispatchedRework, wantState: "In Progress", wantComment: "was not landed", wantMoves: 1},
		{name: "a conflict skips operator-only configured rework", landing: conflict, hubState: "Merging", states: fallback, reworkState: "Private", wantState: "Working", wantComment: "was not landed", wantMoves: 1},
		{name: "a conflict uses the workflow coding lane", landing: conflict, hubState: "Merging", states: fallback, wantState: "Working", wantComment: "was not landed", wantMoves: 1},
		{name: "a refusal uses the workflow review lane", landing: refused, hubState: "Merging", states: fallback, wantState: "Awaiting Review", wantComment: "was not landed", wantMoves: 1},
		{name: "a refusal uses the workflow review lane without human review", landing: refused, hubState: "Merging", states: fallback, noHumanReview: true, wantState: "Awaiting Review", wantComment: "was not landed", wantMoves: 1},
		{name: "a protected refusal prefers configured review without human review", landing: refused, hubState: "Merging", states: workflow, noHumanReview: true, wantState: "Human Review", wantComment: "was not landed", wantMoves: 1},
		{name: "unproven conflict retains landing retry without coding rework", landing: &waiting, hubState: "Merging", states: workflow, noHumanReview: true, wantLandingWait: true},
		{name: "stale base projection with current conflict enters configured rework", mergeMessage: "Pull Request has merge conflicts", sourceConflict: true, hubState: "Merging", states: workflow, reworkState: "Refresh", wantState: "Refresh", wantComment: "was not landed", wantMoves: 1},
		{name: "stale base projection with current clean source retains landing wait", mergeMessage: "Pull Request has merge conflicts", hubState: "Merging", states: workflow, noHumanReview: true, wantLandingWait: true},
		{name: "responsive clean projection clears only old synthetic outage", mergeMessage: "Pull Request has merge conflicts", hubState: "Merging", states: workflow, wantLandingWait: true, priorOutage: "projection"},
		{name: "proven conflict clears only old synthetic outage", mergeMessage: "Pull Request has merge conflicts", sourceConflict: true, hubState: "Merging", states: workflow, wantState: "Rework", wantComment: "was not landed", wantMoves: 1, priorOutage: "projection"},
		{name: "responsive clean projection preserves genuine server outage", mergeMessage: "Pull Request has merge conflicts", hubState: "Merging", states: workflow, wantLandingWait: true, priorOutage: forgeavailability.ClassServer},
		{name: "responsive clean projection preserves genuine transport outage", mergeMessage: "Pull Request has merge conflicts", hubState: "Merging", states: workflow, wantLandingWait: true, priorOutage: forgeavailability.ClassTransport},
		{name: "base race retains reviewed landing without human review", mergeMessage: "Base branch was modified. Review and try the merge again.", landing: unproven, hubState: "Merging", states: workflow, noHumanReview: true, wantLandingWait: true},
		{name: "base race retains reviewed landing with human review", mergeMessage: "Base branch was modified. Review and try the merge again.", landing: unproven, hubState: "Merging", states: workflow, wantLandingWait: true},
		{name: "genuine server outage retains native version and host backoff", mergeMessage: "Service Unavailable", mergeStatus: http.StatusServiceUnavailable, landing: unproven, hubState: "Merging", states: workflow, wantInfrastructure: true},
		{name: "strict head protection returns to review", mergeMessage: "Head branch is out of date. Review and try the merge again.", hubState: "Merging", states: workflow, wantState: "Human Review", wantComment: "Head branch is out of date", wantMoves: 1},
		{name: "strict head protection blocks without human review", mergeMessage: "Head branch is out of date. Review and try the merge again.", hubState: "Merging", states: workflow, noHumanReview: true, wantState: "Human Review", wantComment: "Head branch is out of date", wantMoves: 1},
		{name: "a conflict with absent configured rework uses reachable rework", landing: conflict, hubState: "Merging", states: workflow, reworkState: "Missing", wantState: "Rework", wantComment: "was not landed", wantMoves: 1},
		{name: "a conflict with neither coding lane is handed off", landing: conflict, hubState: "Merging", states: []connector.WorkflowState{{Name: "Merging", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true}}, wantDeferred: true, wantError: "native workflow allows no move from Merging to the landing refusal lane Rework"},
		{name: "a conflict with disallowed rework is handed off", landing: conflict, hubState: "Merging", states: []connector.WorkflowState{{Name: "Merging", Dispatchable: true, Transitions: []string{"Human Review", "Done"}}, {Name: "Human Review"}, {Name: "Rework", Dispatchable: true}, {Name: "Done", Terminal: true}}, wantDeferred: true},
		{name: "a conflict cannot enter operator-only rework", landing: conflict, hubState: "Merging", states: []connector.WorkflowState{{Name: "Merging", Dispatchable: true, Transitions: []string{"Rework"}}, {Name: "Rework", Dispatchable: true, OperatorOnly: true}}, wantDeferred: true},
		{name: "a conflict cannot complete through terminal rework", landing: conflict, hubState: "Merging", states: []connector.WorkflowState{{Name: "Merging", Dispatchable: true, Transitions: []string{"Rework"}}, {Name: "Rework", Terminal: true}}, wantDeferred: true},
		{name: "typed success retains landing authority over final question", landing: landed, hubState: "Done", states: workflow, finalMessage: "May I merge?", wantState: "Done", wantComment: "Landed Change Request change_1"},
		{name: "typed refusal retains review destination over final question", landing: refused, hubState: "Merging", states: workflow, finalMessage: "May I merge?", wantState: "Human Review", wantComment: "enable GitHub pull request mode", wantMoves: 1},
		{name: "typed refusal is not replaced by prose success", landing: refused, hubState: "Merging", states: workflow, finalMessage: "Landed the change successfully.", wantState: "Human Review", wantComment: "was not landed", wantMoves: 1},
		{name: "prose landing without typed evidence never lands", hubState: "Merging", states: workflow, finalMessage: "Landed the change successfully.", wantContinue: true},
		{name: "a refused landing with no review lane is handed off", landing: refused, hubState: "Merging", states: []connector.WorkflowState{{Name: "Merging", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true}}, wantDeferred: true, wantError: "native workflow allows no move from Merging to the landing refusal lane Human Review"},
		{name: "an unreadable workflow is handed off", landing: refused, hubState: "Merging", statesErr: errors.New("hub unavailable"), wantDeferred: true},
		{name: "a refused lane write is handed off", landing: refused, hubState: "Merging", states: workflow, updateErr: errors.New("stale fencing token"), wantDeferred: true},
		{name: "no landing keeps the ordinary path", hubState: "Merging", states: workflow, wantContinue: true},
		{name: "a connector without a workflow keeps the ordinary path", landing: landed, hubState: "Done", plain: true, wantContinue: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := completionTransitionIssue("Merging", "")
			var journey *nativeLandingJourney
			var result runpkg.RunResult
			runErr := test.err
			branch := "detent/land"
			landingHead := head
			landed := *landed
			if test.mergeMessage != "" {
				journey = newNativeLandingJourney(t, issue, test.mergeMessage, test.mergeStatus, test.sourceConflict)
				result, runErr = journey.run(t)
				branch, landingHead = result.WorkspaceBranch, journey.target.HeadSHA
				landed.HeadSHA = landingHead
				if test.wantInfrastructure {
					var status *github.StatusError
					wantError := connector.ErrPullRequestBaseOutOfDate
					base := journey.base
					statusCode := http.StatusMethodNotAllowed
					if test.mergeStatus >= http.StatusInternalServerError {
						wantError = forgeavailability.ErrUnavailable
						statusCode = test.mergeStatus
					}
					if test.mergeMessage == "Pull Request has merge conflicts" {
						wantError = forgeavailability.ErrUnavailable
						base = nativeLandingGit(t, t.Context(), journey.info.Path, "rev-parse", "refs/remotes/origin/main")
						if base == journey.base || base != nativeLandingGit(t, t.Context(), journey.remote, "rev-parse", "refs/heads/main") {
							t.Fatal("conflict verification did not refresh the advanced base")
						}
					}
					if !errors.Is(runErr, wantError) || !errors.As(runErr, &status) || status.StatusCode != statusCode || !strings.Contains(status.Body, test.mergeMessage) || test.mergeStatus < 500 && (!strings.Contains(runErr.Error(), "PUT repos/example/repo/pulls/7/merge") || !strings.Contains(runErr.Error(), base) || !strings.Contains(runErr.Error(), landingHead)) || len(journey.execution.recorded) != 0 {
						t.Fatalf("landing wait lost refusal evidence or fabricated landing: result %#v, error %v", result, runErr)
					}
				} else {
					wantRefusal := workspace.LandRefusalProtected
					if test.wantLandingWait {
						wantRefusal = workspace.LandRefusalBaseMoved
						base := nativeLandingGit(t, t.Context(), journey.remote, "rev-parse", "refs/heads/main")
						recordedBase := base
						if strings.HasPrefix(test.mergeMessage, "Base branch was modified") {
							recordedBase = journey.base
						}
						if base == journey.base || !strings.Contains(result.NativeLanding.Refusal, recordedBase) || !strings.Contains(result.NativeLanding.Refusal, landingHead) {
							t.Fatalf("landing wait lost source and refusal evidence: %#v", result.NativeLanding)
						}
					}
					if test.sourceConflict {
						wantRefusal = workspace.LandRefusalConflict
						base := nativeLandingGit(t, t.Context(), journey.remote, "rev-parse", "refs/heads/main")
						if base == journey.base || !strings.Contains(result.NativeLanding.Refusal, base) || !strings.Contains(result.NativeLanding.Refusal, landingHead) {
							t.Fatalf("conflict lost current Git base or reviewed head: %#v, %v", result.NativeLanding, runErr)
						}
					}
					if runErr != nil || result.Output != runpkg.RunOutputNativeLandingRefused || result.NativeLanding.RefusalKind != wantRefusal || !strings.Contains(result.NativeLanding.Refusal, test.mergeMessage) || !strings.Contains(result.NativeLanding.Refusal, "status 405") || result.NativeLanding.Landed || result.NativeLanding.MergeSHA != "" || len(journey.execution.recorded) != 0 || journey.provider.calls.Load() != 0 || journey.execution.started != 1 {
						t.Fatalf("landing lost refusal ownership or fabricated landing: landing %#v, error %v", result.NativeLanding, runErr)
					}
					test.landing = result.NativeLanding
				}
			}
			hubIssue := cloneIssue(issue)
			hubIssue.State = test.hubState
			tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{hubIssue}, updateErr: test.updateErr}
			var tracker connector.Connector = &nativeWorkflowConnector{autoPromoteTickConnector: tick, states: test.states, statesErr: test.statesErr}
			if test.plain {
				tracker = tick
			}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Merging"}, TerminalStates: []string{"Done"}})
			cfg.AutoPromote.SourceState = "Human Review"
			if test.noHumanReview {
				humanReview := false
				cfg.AutoPromote.HumanReview = &humanReview
			}
			cfg.AutoPromote.ReworkState = test.reworkState
			cfg.Policy.Gates.GitHubPullRequest = test.wantInfrastructure || test.wantLandingWait || journey != nil
			attempts := &recordingWorkAttemptStore{}
			scheduling := &hubSchedulingSource{}
			var logs strings.Builder
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, scheduling: scheduling, logger: slog.New(slog.NewTextHandler(&logs, nil))}
			state := newState(cfg)
			now := time.Date(2026, 9, 29, 1, 0, 0, 0, time.UTC)
			state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, WorkAttemptID: 42, Mode: runpkg.RunModeMerge, DispatchSourceState: "Merging", StartedAt: now.Add(-time.Minute)}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
			state.RepeatedFailures[issue.ID] = RepeatedFailure{Issue: issue, Count: 2}
			var priorCondition ForgeCondition
			if test.priorOutage != "" {
				class := test.priorOutage
				cause := errors.New("connection reset by peer")
				switch class {
				case "projection":
					class = forgeavailability.ClassServer
					cause = fmt.Errorf("GitHub refused PUT repos/example/repo/pulls/7/merge: %w", &github.StatusError{Err: github.ErrUnexpectedStatus, StatusCode: 405, Body: `{"message":"Pull Request has merge conflicts"}`})
				case forgeavailability.ClassServer:
					cause = &github.StatusError{Err: github.ErrUnexpectedStatus, StatusCode: 503, Body: `{"message":"Service Unavailable"}`}
				}
				priorCondition = orch.registerForgeUnavailable(&state, forgeavailability.NewError(forgeavailability.Scope{Host: "github.com", Operation: "github.update_pull_request repos/example/repo/pulls/7/merge"}, class, cause), state.Running[issue.ID], now.Add(-time.Minute))
				priorCondition.ProbeIssueID = issue.ID
				priorCondition.NextProbeAt = time.Time{}
				priorCondition.ProbeAttempts = 1
				state.ForgeUnavailable["github.com"] = priorCondition
				running := state.Running[issue.ID]
				running.ForgeProbeHost = "github.com"
				state.Running[issue.ID] = running
			}
			output := runpkg.RunOutputNativeLanded
			if test.landing != nil && !test.landing.Landed {
				output = runpkg.RunOutputNativeLandingRefused
			}
			if journey == nil {
				result = runpkg.RunResult{FinalState: FinalStateCompleted, FinalMessage: test.finalMessage, Output: output, NativeLanding: test.landing, WorkspaceBranch: branch}
				if test.absorbed {
					result.NativeLanding = nil
					result.NativeChange = &runpkg.NativeChange{Changed: true, ChangeID: landed.ChangeID, VersionID: landed.VersionID, HeadSHA: landed.HeadSHA, Reviewed: true, Landing: &landed}
				}
			}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: issue.ID, CompletedAt: now,
				Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge},
				Result:  result,
				Err:     runErr,
			})
			retry, retried := state.Retry[issue.ID]
			_, deferred := state.deferredCompletions[issue.ID]
			if test.wantLandingWait {
				if !retried || retry.ForgeUnavailable || retry.ForgeRetry != nil || retry.Attempt != 1 || retry.Issue.State != "Merging" || !retry.DueAt.Equal(now.Add(cfg.ContinuationRetryDelay)) || orch.dispatchMode(t.Context(), &state, retry.Issue) != runpkg.RunModeMerge || len(tick.updates) != 0 || len(tick.comments) != 0 || len(state.Completed) != 0 || len(state.Blocked) != 0 || len(state.InstantFailures) != 0 || scheduling.releases != 1 {
					t.Fatalf("projection wait lost item-local continuation: retry %#v, state %#v", retry, state)
				}
				peer := completionTransitionIssue("Merging", "")
				peer.ID = "unrelated-merge"
				peerBlocked := forgeAvailabilityBlocks(&state, peer, Retry{}, "github.com", now)
				genuineOutage := test.priorOutage != "" && test.priorOutage != "projection"
				if peerBlocked != genuineOutage || (len(state.ForgeUnavailable) != 0) != genuineOutage {
					t.Fatalf("projection changed unrelated merge admission: blocked %v, conditions %#v", peerBlocked, state.ForgeUnavailable)
				}
				if genuineOutage {
					condition := state.ForgeUnavailable["github.com"]
					if condition.ErrorClass != priorCondition.ErrorClass || condition.LastError != priorCondition.LastError || !condition.LastObservedAt.Equal(priorCondition.LastObservedAt) || condition.ProbeIssueID != "" {
						t.Fatalf("responsive refusal replaced genuine outage authority: %#v", condition)
					}
				}
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalSuccess || attempts.completions[0].Phase != "waiting" || attempts.completions[0].ErrorClass != "" {
					t.Fatalf("projection wait consumed failure/capacity: %#v", attempts.completions)
				}
				var metadata map[string]any
				if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &metadata); err != nil {
					t.Fatal(err)
				}
				if metadata["native_landed"] != false || metadata["native_version_id"] != unproven.VersionID || metadata["native_head_sha"] != landingHead || metadata["native_landing_refusal"] != workspace.LandRefusalBaseMoved || metadata["forge_wait"] != nil || metadata["native_merge_sha"] != nil {
					t.Fatalf("continuation lost immutable reviewed identity: %#v", metadata)
				}
				if journey != nil && !genuineOutage {
					retryResult, err := journey.run(t)
					if err != nil || !retryResult.NativeLanding.Landed || journey.execution.target.HeadSHA != landingHead || len(journey.execution.recorded) != 1 || journey.provider.calls.Load() != 0 {
						t.Fatalf("same-version continuation failed to land: %#v, %v", retryResult, err)
					}
					tick.stateIssues[0].State = "Done"
					state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, WorkAttemptID: 43, Mode: runpkg.RunModeMerge, DispatchSourceState: "Merging", StartedAt: retry.DueAt}
					state.Claimed[issue.ID] = Claimed{Issue: issue}
					orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: retry.DueAt.Add(time.Second), Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge}, Result: retryResult})
					if len(state.ForgeUnavailable) != 0 || state.Completed[issue.ID].Issue.State != "Done" || len(attempts.completions) != 2 || len(tick.updates) != 0 {
						t.Fatalf("same-version continuation failed to settle: %#v", state)
					}
				}
				return
			}
			if test.wantInfrastructure {
				if !retried || !retry.ForgeUnavailable || retry.Attempt != 1 || retry.Issue.State != "Merging" || orch.dispatchMode(t.Context(), &state, retry.Issue) != runpkg.RunModeMerge || len(tick.updates) != 0 || len(tick.comments) != 0 || len(state.Completed) != 0 || len(state.Blocked) != 0 || state.RepeatedFailures[issue.ID].Count != 2 || len(state.InstantFailures) != 0 || scheduling.releases != 1 {
					t.Fatalf("unproven conflict changed source ownership: retry %#v, updates %#v, state %#v", retry, tick.updates, state)
				}
				message := "Pull Request has merge conflicts"
				if test.mergeMessage != "" {
					message = test.mergeMessage
				}
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalCapacity || !strings.Contains(attempts.completions[0].ErrorMessage, message) {
					t.Fatalf("original refusal not preserved: %#v", attempts.completions)
				}
				var metadata map[string]any
				if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &metadata); err != nil {
					t.Fatal(err)
				}
				if metadata["native_landed"] != false || metadata["native_change_id"] != unproven.ChangeID || metadata["native_version_id"] != unproven.VersionID || metadata["native_head_sha"] != landingHead || metadata["native_merge_sha"] != nil || metadata["native_landing_refusal"] != nil {
					t.Fatalf("landing deferral lost identity or manufactured source conflict: %#v", metadata)
				}
				completion := attempts.completions[0]
				receipt := store.WorkAttempt{ID: 42, IssueID: issue.ID, Identifier: issue.Identifier, Lane: "Merging", AttemptNumber: 1, Status: store.WorkAttemptStatusTerminal, TerminalState: completion.TerminalState, ErrorClass: completion.ErrorClass, ErrorMessage: completion.ErrorMessage, WorkerMetadataJSON: completion.WorkerMetadataJSON, CompletedAt: now}
				wait, valid := forgeWaitMetadataFromAttempt(receipt)
				if !valid || wait.Branch != branch {
					t.Fatalf("landing wait cannot survive restart: %#v", wait)
				}
				restarted := newState(cfg)
				orch.recoverForgeAvailabilityWaits(t.Context(), &restarted, []store.WorkAttempt{receipt}, now.Add(time.Second))
				if restored := restarted.Retry[issue.ID]; !restored.ForgeUnavailable || restored.Attempt != 1 || restored.Issue.State != "Merging" || !restored.DueAt.Equal(retry.DueAt) {
					t.Fatalf("restart lost landing retry ownership: %#v", restored)
				}
				if _, reserved := reserveForgeAvailabilityProbe(&state, issue.ID, retry, retry.DueAt); !reserved {
					t.Fatal("landing retry cannot use the existing forge probe")
				}
				retryResult := runpkg.RunResult{FinalState: FinalStateCompleted, Output: runpkg.RunOutputNativeLanded, NativeLanding: &landed, ForgeWriteCompleted: true}
				if journey != nil {
					var err error
					retryResult, err = journey.run(t)
					if err != nil || retryResult.NativeLanding == nil || !retryResult.NativeLanding.Landed || len(journey.execution.recorded) != 1 || !reflect.DeepEqual(journey.execution.target, journey.target) || journey.provider.calls.Load() != 0 || journey.execution.started != 2 {
						t.Fatalf("fresh-base retry did not land the unchanged reviewed version: result %#v, error %v", retryResult, err)
					}
				}
				tick.stateIssues[0].State = "Done"
				state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, WorkAttemptID: 43, Mode: runpkg.RunModeMerge, DispatchSourceState: "Merging", ForgeProbeHost: "github.com", StartedAt: retry.DueAt}
				state.Claimed[issue.ID] = Claimed{Issue: issue}
				orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: retry.DueAt.Add(time.Second), Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge}, Result: retryResult})
				if len(state.ForgeUnavailable) != 0 || state.Completed[issue.ID].Issue.State != "Done" || len(attempts.completions) != 2 || attempts.completions[1].TerminalState != store.WorkAttemptTerminalSuccess || len(tick.updates) != 0 {
					t.Fatalf("same-version landing retry failed to settle: conditions %#v, completions %#v, updates %#v", state.ForgeUnavailable, attempts.completions, tick.updates)
				}
				return
			}
			if deferred != test.wantDeferred || test.wantDeferred && !retry.CompletionDeferred {
				t.Fatalf("deferred = %t (retry %#v), want %t", deferred, retry, test.wantDeferred)
			}
			if test.wantContinue {
				if _, completedByLanding := state.Completed[issue.ID]; completedByLanding && retried && !retry.CompletionDeferred {
					t.Fatalf("the landing path completed an ordinary run")
				}
				for _, comment := range tick.comments {
					if strings.Contains(comment.body, "Landed Change Request") {
						t.Fatalf("the landing path commented on an ordinary run: %#v", tick.comments)
					}
				}
				return
			}
			if test.wantDeferred {
				if test.wantError != "" && (state.deferredCompletions[issue.ID].Availability.Message != test.wantError || strings.Count(logs.String(), "native completion not applied") != 1) {
					t.Fatalf("handoff error = %q, logs = %s; want one warning for %q", state.deferredCompletions[issue.ID].Availability.Message, logs.String(), test.wantError)
				}
				if test.updateErr == nil && len(tick.updates) != 0 || len(attempts.completions) != 0 {
					t.Fatalf("a handed-off landing moved or completed: updates %#v, attempts %#v", tick.updates, attempts.completions)
				}
				if len(tick.comments) != 0 {
					t.Fatalf("a handed-off item was commented on: %#v", tick.comments)
				}
				return
			}
			if journey != nil && (retried || len(state.ForgeUnavailable) != 0 || nativeLandingGit(t, t.Context(), journey.remote, "rev-parse", "refs/heads/main") == journey.merge) {
				t.Fatalf("strict protection retained a base wait or landed without a receipt: retry %#v, state %#v", retry, state)
			}
			if len(tick.updates) != test.wantMoves || test.wantMoves == 1 && tick.updates[0].state != test.wantState {
				t.Fatalf("lane updates = %#v, want %d to %s", tick.updates, test.wantMoves, test.wantState)
			}
			if len(tick.comments) != 1 || !strings.Contains(tick.comments[0].body, test.wantComment) {
				t.Fatalf("comments = %#v, want one containing %q", tick.comments, test.wantComment)
			}
			if _, claimed := state.Claimed[issue.ID]; claimed || scheduling.releases != 1 {
				t.Fatalf("claim retained = %t, releases = %d", claimed, scheduling.releases)
			}
			completed, ok := state.Completed[issue.ID]
			if !ok || completed.Issue.State != test.wantState {
				t.Fatalf("completed = %#v, present = %t", completed, ok)
			}
			if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalSuccess {
				t.Fatalf("attempt completions = %#v", attempts.completions)
			}
			var metadata map[string]any
			if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata["native_landed"] != test.landing.Landed || metadata["native_version_id"] != test.landing.VersionID || metadata["native_change_id"] != test.landing.ChangeID {
				t.Fatalf("landing identity = %#v", metadata)
			}
			if journey != nil && metadata["native_head_sha"] != landingHead {
				t.Fatalf("protection refusal lost reviewed head: %#v", metadata)
			}
			if !test.landing.Landed && (metadata["native_landing_refusal"] != test.landing.RefusalKind || metadata["native_merge_sha"] != nil) {
				t.Fatalf("refusal became landing evidence: %#v", metadata)
			}
		})
	}
}

func TestNativeLandingComment(t *testing.T) {
	t.Parallel()
	landed := nativeLandingComment(&runpkg.NativeLanding{ChangeID: "change_1", HeadSHA: "0123456789abcdef", Landed: true, MergeSHA: "fedcba9876543210", BaseRef: "main", Method: "squash"}, "Merging", "Done")
	for _, want := range []string{"change_1", "head 0123456789ab", "on main as fedcba987654", "by squash", "Merging to Done"} {
		if !strings.Contains(landed, want) {
			t.Fatalf("landed comment %q does not contain %q", landed, want)
		}
	}
	refused := nativeLandingComment(&runpkg.NativeLanding{ChangeID: "change_1", HeadSHA: "0123456789abcdef", RefusalKind: "conflict", Refusal: "squashing conflicts."}, "Merging", "Human Review")
	for _, want := range []string{"was not landed: squashing conflicts.", "Merging to Human Review"} {
		if !strings.Contains(refused, want) {
			t.Fatalf("refused comment %q does not contain %q", refused, want)
		}
	}
}

func TestNativeCandidateStatesIncludeTheLandingLane(t *testing.T) {
	t.Parallel()
	cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}})
	tick := &autoPromoteTickConnector{}
	for _, test := range []struct {
		name      string
		connector connector.Connector
		want      []string
	}{
		{name: "native adds Merging", connector: &nativeWorkflowConnector{autoPromoteTickConnector: tick}, want: []string{"todo", "in progress", "merging"}},
		{name: "other trackers keep their configuration", connector: tick, want: []string{"todo", "in progress"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			orch := &Orchestrator{cfg: cfg, connector: test.connector}
			state := newState(cfg)
			got := orch.candidateFetchStatesForTick(&state)
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("candidate states = %v, want %v", got, test.want)
			}
			issue := completionTransitionIssue("Merging", "")
			mode := orch.dispatchMode(t.Context(), &state, issue)
			wantMode := runpkg.RunModeImplement
			if test.name == "native adds Merging" {
				wantMode = runpkg.RunModeMerge
			}
			if mode != wantMode {
				t.Fatalf("dispatchMode(Merging) = %q, want %q", mode, wantMode)
			}
		})
	}
}

// TestNativeMergingLaneIsNotReconciledAsAStalePullRequest checks that a
// native item waiting to land is left to the landing run: the stale-Merging
// reconciliation reads pull request state, and a native item has none.
func TestNativeMergingLaneIsNotReconciledAsAStalePullRequest(t *testing.T) {
	t.Parallel()
	cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Merging"}, TerminalStates: []string{"Done"}})
	now := time.Date(2026, 9, 29, 2, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		native    bool
		wantMoves int
	}{
		{name: "native stays for the landing run", native: true, wantMoves: 0},
		{name: "a tracker with pull requests still reconciles", native: false, wantMoves: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			issue := completionTransitionIssue("Merging", "")
			tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			var tracker connector.Connector = tick
			if test.native {
				tracker = &nativeWorkflowConnector{autoPromoteTickConnector: tick}
			}
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			transitioned := orch.reconcileStaleMergingPullRequestIssues(t.Context(), &state, []connector.Issue{issue}, now)
			if len(transitioned) != test.wantMoves || len(tick.updates) != test.wantMoves {
				t.Fatalf("transitioned = %#v, updates = %#v, want %d", transitioned, tick.updates, test.wantMoves)
			}
		})
	}
}

func TestWithNativeLandingLane(t *testing.T) {
	t.Parallel()
	cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress"}, TerminalStates: []string{"Done"}})
	tick := &autoPromoteTickConnector{}
	native := withNativeLandingLane(cfg, &nativeWorkflowConnector{autoPromoteTickConnector: tick})
	if strings.Join(native.ActiveStates, ",") != "todo,in progress,merging" {
		t.Fatalf("native active states = %v", native.ActiveStates)
	}
	if again := withNativeLandingLane(native, &nativeWorkflowConnector{autoPromoteTickConnector: tick}); len(again.ActiveStates) != 3 {
		t.Fatalf("the lane was added twice: %v", again.ActiveStates)
	}
	if plain := withNativeLandingLane(cfg, tick); strings.Join(plain.ActiveStates, ",") != "todo,in progress" {
		t.Fatalf("a tracker with pull requests gained a lane: %v", plain.ActiveStates)
	}
	planner := dispatchPlanner{cfg: native}
	issue := completionTransitionIssue("Merging", "")
	state := newState(native)
	if decision := planner.dispatchableIssueDecisionForModelRequirement(issue, &state, false, time.Now(), "", false); decision.reason == dispatchSkipInactiveState {
		t.Fatalf("a native Merging item is skipped as inactive: %#v", decision)
	}
}

func TestNativeLandingQuotaWait(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name              string
		status            int
		reset             bool
		retryAfter        string
		remaining         string
		finishUnavailable bool
		missingPublisher  bool
	}{
		{name: "primary 403", status: 403, reset: true},
		{name: "secondary 429", status: 429, retryAfter: "120"},
		{name: "secondary 429 with healthy primary reset", status: 429, retryAfter: "120", reset: true, remaining: "4990"},
		{name: "body-only 403", status: 403},
		{name: "native Finish outage retains completion", status: 429, retryAfter: "120", finishUnavailable: true},
		{name: "restart without native publisher rejects completion", status: 429, retryAfter: "120", finishUnavailable: true, missingPublisher: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			reset := now.Add(time.Hour).Truncate(time.Second)
			client, err := github.NewClient(github.ClientConfig{
				Endpoint:    "https://" + strings.ReplaceAll(test.name, " ", "-") + ".test/graphql",
				TokenSource: github.StaticTokenSource("native-landing"),
				HTTPClient: restRecoveryHTTPClient(func(*http.Request) (*http.Response, error) {
					headers := make(http.Header)
					if test.reset {
						headers.Set("X-RateLimit-Limit", "5000")
						if test.remaining == "" {
							headers.Set("X-RateLimit-Remaining", "0")
						} else {
							headers.Set("X-RateLimit-Remaining", test.remaining)
						}
						headers.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
					}
					if test.retryAfter != "" {
						headers.Set("Retry-After", test.retryAfter)
					}
					return &http.Response{StatusCode: test.status, Header: headers, Body: io.NopCloser(strings.NewReader(`{"message":"API rate limit exceeded for user585100"}`))}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			err = client.REST(t.Context(), "GET", "/repos/example/repo/pulls?state=all", nil, nil)
			if !errors.Is(err, github.ErrRateLimited) {
				t.Fatalf("response = %v", err)
			}
			usage := client.FlushRESTRateLimitUsage()
			issue := completionTransitionIssue("Merging", "")
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Merging"}, TerminalStates: []string{"Done"}, MaxConcurrentAgents: 4})
			cfg.Policy.Gates.GitHubPullRequest = true
			tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			tracker := &nativeWorkflowConnector{autoPromoteTickConnector: tick}
			attempts := &recordingWorkAttemptStore{}
			scheduling := &hubSchedulingSource{execution: &nativeLandingJourneyExecution{}}
			if test.finishUnavailable {
				scheduling.releaseError = errors.Join(ErrSchedulingUnavailable, errors.New("native Finish publication unavailable"))
			}
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, scheduling: scheduling}
			state := newState(cfg)
			head := strings.Repeat("c", 40)
			landing := &runpkg.NativeLanding{ChangeID: "change_1", VersionID: "version_1", HeadSHA: head}
			state.Running[issue.ID] = Running{Issue: issue, Attempt: 4, WorkAttemptID: 42, Mode: runpkg.RunModeMerge, DispatchSourceState: "Merging", StartedAt: now.Add(-time.Minute)}
			state.Claimed[issue.ID] = Claimed{Issue: issue}
			state.RepeatedFailures[issue.ID] = RepeatedFailure{Issue: issue, Count: 2}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: issue.ID, CompletedAt: now, Err: err,
				Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge},
				Result:  runpkg.RunResult{NativeLanding: landing, GitHubRESTUsage: &usage, GitHubRESTConsumer: telemetry.RESTConsumerWorker},
			})
			if test.finishUnavailable {
				deferred, retained := state.deferredCompletions[issue.ID]
				_, claimed := state.Claimed[issue.ID]
				if !retained || !state.Retry[issue.ID].CompletionDeferred || !claimed || len(attempts.completions) != 0 || scheduling.releases != 1 || deferred.Result.NativeLanding == nil || *deferred.Result.NativeLanding != *landing {
					t.Fatalf("Finish outage lost completion ownership: deferred=%t claim=%t retry=%#v completions=%#v releases=%d", retained, claimed, state.Retry[issue.ID], attempts.completions, scheduling.releases)
				}
				// Replay the persisted JSON, including typed response evidence.
				data, marshalErr := json.Marshal(deferred)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				var recovered deferredCompletion
				if unmarshalErr := json.Unmarshal(data, &recovered); unmarshalErr != nil {
					t.Fatal(unmarshalErr)
				}
				recovered.Persisted = true
				state.deferredCompletions[issue.ID] = recovered
				scheduling.releaseError = nil
				if test.missingPublisher {
					// A fresh scheduler has no original prepared execution.
					scheduling = &hubSchedulingSource{releaseError: runpkg.ErrExecutionAuthorityUnavailable}
					orch.scheduling = scheduling
				}
				if !orch.retryDeferredCompletions(t.Context(), &state, state.Retry[issue.ID].DueAt.Add(time.Second)) || len(state.deferredCompletions) != 0 || state.Retry[issue.ID].CompletionDeferred {
					t.Fatal("settled Finish did not resume the original quota completion")
				}
			}
			if test.missingPublisher {
				_, claimed := state.Claimed[issue.ID]
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalAbandoned || len(state.Retry) != 0 || claimed || len(state.Running) != 0 || len(tick.updates) != 0 || state.RepeatedFailures[issue.ID].Count != 2 {
					t.Fatalf("missing publisher fabricated settlement: attempts=%#v retry=%#v claimed=%t", attempts.completions, state.Retry, claimed)
				}
				return
			}
			if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalCapacity {
				t.Fatalf("completions = %#v", attempts.completions)
			}
			completion := attempts.completions[0]
			attempt := store.WorkAttempt{ID: 42, IssueID: issue.ID, Identifier: issue.Identifier, Lane: "Merging", AttemptNumber: 4, Status: store.WorkAttemptStatusTerminal, TerminalState: completion.TerminalState, ErrorClass: completion.ErrorClass, WorkerMetadataJSON: completion.WorkerMetadataJSON, CompletedAt: now}
			wait, ok := githubRESTWaitMetadataFromAttempt(attempt)
			if !ok || wait.Reserve != 0 || wait.NativeLanding == nil || *wait.NativeLanding != *landing || wait.RateLimitKind == "" {
				t.Fatalf("wait = %#v, valid %t", wait, ok)
			}
			if test.reset != !wait.ResetAt.IsZero() || test.retryAfter != "" && wait.RetryAfter != 120*time.Second {
				t.Fatalf("response evidence = %#v", wait)
			}
			retry, ok := state.Retry[issue.ID]
			if test.remaining != "" && !retry.DueAt.Before(reset) {
				t.Fatal("secondary wait used healthy primary reset")
			}
			if !ok || retry.Attempt != 4 || retry.Issue.State != "Merging" || len(tick.updates) != 0 || len(tick.comments) != 0 || len(state.Blocked) != 0 || len(state.Completed) != 0 {
				t.Fatalf("quota completion changed item: retry %#v state %#v", retry, state.Blocked)
			}
			// ReleaseClaim settles deferred native Finish and frees the durable
			// lease; a local retry cannot stand in for that completion.
			_, claimed := state.Claimed[issue.ID]
			wantReleases := 1
			if test.finishUnavailable {
				wantReleases++
			}
			if state.RepeatedFailures[issue.ID].Count != 2 || len(state.InstantFailures) != 0 || scheduling.releases != wantReleases || claimed {
				t.Fatalf("attempt allowance or claim changed: %#v releases %d", state.RepeatedFailures, scheduling.releases)
			}
			if orch.adaptivePollInterval(&state, now) > cfg.PollInterval {
				t.Fatal("landing quota lengthened native polling")
			}
			if _, signaled := orch.currentGitHubLookupSignal(&state, now); signaled {
				t.Fatal("landing quota paused native tracker reads")
			}
			if state.RateLimits.RESTUsage == nil || !state.RateLimits.RESTUsage.RateLimited || state.RateLimits.RESTUsage.TotalRequests != 1 {
				t.Fatalf("REST metrics = %#v", state.RateLimits)
			}
			restarted := newState(cfg)
			orch.recoverGitHubRESTCapacityWaits(t.Context(), &restarted, []store.WorkAttempt{attempt}, now.Add(time.Second))
			if restored := restarted.Retry[issue.ID]; restored.Attempt != 4 || restored.Issue.State != "Merging" || !restored.DueAt.Equal(retry.DueAt) {
				t.Fatalf("restart lost wait: %#v", restored)
			}
			restarted.RateLimits.GitHubRESTBudgets = append(restarted.RateLimits.GitHubRESTBudgets, telemetry.RESTBudget{
				Consumer: telemetry.RESTConsumerWorker, CredentialIdentity: wait.CredentialIdentity, EndpointFamily: "worker credential", Resource: "core", Remaining: 5000, Limit: 5000, MinRemainingReserve: 1000, ResetAt: &reset, ObservedAt: timePointer(now.Add(time.Second)),
			})
			orch.syncGitHubRESTCapacityOutage(&restarted, now.Add(time.Second))
			if _, active := activeGitHubRESTCapacityOutage(&restarted, now.Add(time.Second)); !active {
				t.Fatal("restart lost outage")
			}
			coding := completionTransitionIssue("In Progress", "")
			coding.ID = "coding"
			if !orch.dispatchPlanner().dispatchableIssueDecision(coding, &restarted, false, now.Add(time.Second), "").dispatchable {
				t.Fatal("quota outage blocked native coding dispatch")
			}
			if _, allowed, reason := orch.dispatchPlanner().retryAction(&restarted, coding, Retry{Issue: coding, DueAt: now}, now.Add(time.Second)); !allowed {
				t.Fatalf("quota outage blocked native coding retry: %s", reason)
			}
			if orch.handleGitHubRESTCapacityCompletion(t.Context(), &restarted, runpkg.Completion{CompletedAt: now.Add(time.Second)}, Running{Issue: coding}) {
				t.Fatal("quota outage absorbed successful coding completion")
			}
			if _, allowed, reason := orch.dispatchPlanner().retryAction(&restarted, issue, restarted.Retry[issue.ID], now.Add(time.Second)); allowed || reason != dispatchSkipGitHubRESTCapacity {
				t.Fatalf("landing did not retain wait: %t %s", allowed, reason)
			}
			if _, allowed, reason := orch.dispatchPlanner().retryAction(&restarted, issue, restarted.Retry[issue.ID], retry.DueAt); !allowed {
				t.Fatalf("landing retry did not resume: %s", reason)
			}
			freshAt := retry.DueAt.Add(time.Second)
			landed := *landing
			landed.Landed, landed.MergeSHA, landed.BaseRef = true, strings.Repeat("d", 40), "main"
			freshUsage := connector.RESTRateLimitUsage{HasRateLimit: true, Budgets: []connector.RESTRateLimitBudget{{CredentialIdentity: wait.CredentialIdentity, EndpointFamily: "pull requests", RateLimit: connector.RESTRateLimit{Remaining: 4999, Limit: 5000, UpdatedAt: freshAt}}}}
			tick.stateIssues[0].State = "Done"
			restarted.Running[issue.ID] = Running{Issue: issue, Attempt: 4, WorkAttemptID: 43, Mode: runpkg.RunModeMerge, StartedAt: retry.DueAt}
			restarted.Claimed[issue.ID] = Claimed{Issue: issue}
			orch.handleRunResult(t.Context(), &restarted, runpkg.Completion{IssueID: issue.ID, CompletedAt: freshAt, Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge}, Result: runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, NativeLanding: &landed, GitHubRESTUsage: &freshUsage}})
			if done := restarted.Completed[issue.ID]; done.Issue.State != "Done" || len(attempts.completions) != 2 || attempts.completions[1].TerminalState != store.WorkAttemptTerminalSuccess {
				t.Fatalf("retry completion = %#v / %#v", done, attempts.completions)
			}
			if _, _, exists := githubRESTCapacityOutage(restarted.BackendOutages); exists {
				t.Fatal("fresh same-credential landing retained outage")
			}
			if bytes, err := json.Marshal(wait); err != nil || !strings.Contains(string(bytes), head) {
				t.Fatalf("durable exact head = %s, %v", bytes, err)
			}
		})
	}
}
