package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type nativeWorkflowConnector struct {
	*autoPromoteTickConnector
	states       []connector.WorkflowState
	statesErr    error
	beforeStates func()
	reasons      []string
	reasonCodes  []string
	// reviewed is the change's review state when the completion is applied;
	// nil answers as a failed read.
	reviewed    *bool
	reviewState *connector.ChangeReview
	reviewErr   error
}

func (c *nativeWorkflowConnector) UpdateIssueState(ctx context.Context, id, state string) error {
	c.reasons = append(c.reasons, connector.LaneTransitionReasonDetail(ctx))
	c.reasonCodes = append(c.reasonCodes, connector.LaneTransitionReason(ctx))
	return c.autoPromoteTickConnector.UpdateIssueState(ctx, id, state)
}

func (c *nativeWorkflowConnector) ReadChangeReview(context.Context, string, string, string, string) (connector.ChangeReview, error) {
	if c.reviewErr != nil {
		return connector.ChangeReview{}, c.reviewErr
	}
	if c.reviewState != nil {
		return *c.reviewState, nil
	}
	return connector.ChangeReview{Current: true, Reviewed: c.reviewed != nil && *c.reviewed}, nil
}

func (c *nativeWorkflowConnector) WorkflowStates(context.Context) ([]connector.WorkflowState, error) {
	if c.beforeStates != nil {
		c.beforeStates()
	}
	return c.states, c.statesErr
}

func TestNativeInstanceBlockerCompletion(t *testing.T) {
	t.Parallel()
	const report = "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - owner: instance\n    reason: Supply the CI job logs through the native evidence path\n    unverifiable: true\nhuman_action: null\n```"
	const predicateReport = "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - owner: instance\n    reason: Wait for the instance configuration to change\n    predicate:\n      type: config_fingerprint\n      fingerprint: previous-config\nhuman_action: null\n```"
	benchBlocker := "  - owner: instance\n    reason: Pair the staging bench\n"
	multiple := strings.Replace(report, "human_action: null", benchBlocker+"human_action: null", 1)
	reordered := strings.Replace(report, "  - owner: instance", benchBlocker+"  - owner: instance", 1)
	for _, tt := range []struct {
		name       string
		first      string
		second     string
		diff       DiffStats
		wantHuman  bool
		historyErr error
	}{
		{name: "repeated blocker on unchanged source", wantHuman: true},
		{name: "multiple blockers reordered", first: multiple, second: reordered, wantHuman: true},
		{name: "typed instance predicate keeps retry owner", first: predicateReport, second: predicateReport},
		{name: "changed head", diff: DiffStats{Status: "clean", HeadSHA: "new-head", TreeSHA: "tree"}},
		{name: "changed tree", diff: DiffStats{Status: "clean", HeadSHA: "head", TreeSHA: "new-tree"}},
		{name: "changed workspace diff", diff: DiffStats{Status: "changed", HeadSHA: "head", TreeSHA: "tree", FilesChanged: 1, Fingerprint: "new-diff"}},
		{name: "unavailable source", diff: DiffStats{Status: "clean"}},
		{name: "changed blocker", second: strings.ReplaceAll(report, "CI job logs", "paired bench")},
		{name: "history unavailable", historyErr: errors.New("history unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := completionTransitionIssue("In Progress", "")
			tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			tracker := &nativeWorkflowConnector{autoPromoteTickConnector: tick, states: []connector.WorkflowState{
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Rework", "Blocked"}},
				{Name: "Rework", Dispatchable: true, Transitions: []string{"Blocked"}},
				{Name: "Blocked", Transitions: []string{"Rework"}},
			}}
			cfg := normalizeConfig(Config{ActiveStates: []string{"In Progress", "Rework"}})
			cfg.AutoPromote.HumanReview = new(false)
			attempts := &recordingWorkAttemptStore{}
			o := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
			state := newState(cfg)
			now := time.Now()
			for turn := range 2 {
				diff := DiffStats{Status: "clean", HeadSHA: "head", TreeSHA: "tree"}
				message := firstNonBlank(tt.first, report)
				if turn == 1 {
					if diffStatsPresent(tt.diff) {
						diff = tt.diff
					}
					message = firstNonBlank(tt.second, report)
					prior := attempts.completions[0]
					attempts.history = []store.WorkAttempt{{ID: prior.AttemptID, TerminalState: prior.TerminalState, WorkerMetadataJSON: prior.WorkerMetadataJSON}}
					attempts.historyErr = tt.historyErr
				}
				state.Running[issue.ID] = Running{Issue: issue, Mode: runpkg.RunModeImplement, WorkAttemptID: int64(turn + 1), TurnCount: 1, Attempt: turn + 1}
				state.Claimed[issue.ID] = Claimed{Issue: issue}
				o.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: now.Add(time.Duration(turn) * time.Minute), Result: runpkg.RunResult{FinalState: FinalStateCompleted, FinalMessage: message, DiffStats: diff}})
				if turn == 1 && tt.historyErr != nil {
					if _, deferred := state.deferredCompletions[issue.ID]; !deferred || len(attempts.completions) != 1 {
						t.Fatalf("history failure lost completion: deferred=%v receipts=%v", state.deferredCompletions, attempts.completions)
					}
					return
				}
				wantHuman := turn == 1 && tt.wantHuman
				blocked, held := state.Blocked[issue.ID]
				_, retry := state.Retry[issue.ID]
				if held != wantHuman || retry == wantHuman {
					t.Fatalf("turn %d: blocked=%+v retry=%t updates=%+v", turn, blocked, retry, tick.updates)
				}
				if wantHuman {
					if explicitHumanAction(blocked.Issue) == "" || !strings.Contains(explicitHumanAction(blocked.Issue), "CI job logs") || len(tick.updates) != 1 || tick.updates[0].state != "Blocked" {
						t.Fatalf("human action lost: blocked=%+v updates=%+v", blocked, tick.updates)
					}
					decision := o.liveDispatchPlanner(t.Context(), nil).dispatchableIssueDecision(blocked.Issue, &state, false, now.Add(time.Hour), "")
					if decision.dispatchable {
						t.Fatal("human action dispatched another provider turn")
					}
				} else if len(tick.updates) != 0 {
					t.Fatalf("instance retry changed lane: %+v", tick.updates)
				}
				if len(attempts.completions) != turn+1 {
					t.Fatalf("completion receipts = %+v", attempts.completions)
				}
			}
		})
	}
}

// TestNativeChangeRunCompletion drives a successful native run's completion
// through the success path: the orchestrator moves the item along the hub
// workflow through the lane ledger and releases the claim instead of
// continuing the item in its active lane.
func TestNativeChangeRunCompletion(t *testing.T) {
	t.Parallel()
	workflow := []connector.WorkflowState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Blocked", "In Review", "Done", "Todo"}},
		{Name: "Blocked", Transitions: []string{"In Progress"}},
		{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
		{Name: "Done", Terminal: true},
	}
	hosted := []connector.WorkflowState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	landing := []connector.WorkflowState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "In Review", "Merging", "Done"}},
		{Name: "In Review", Transitions: []string{"Done", "In Progress", "Merging"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "In Review", "In Progress"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	fourLanes := []connector.WorkflowState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Human Review", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
		{Name: "Human Review", Transitions: []string{"In Progress", "Done"}},
		{Name: "Done", Terminal: true},
	}
	planRecovery := append([]connector.WorkflowState(nil), workflow...)
	planRecovery[0].Transitions = append([]string{"Blocked"}, planRecovery[0].Transitions...)
	dispatchableBlocked := append([]connector.WorkflowState(nil), planRecovery...)
	dispatchableBlocked[2].Dispatchable = true
	operatorBlocked := append([]connector.WorkflowState(nil), planRecovery...)
	operatorBlocked[2].OperatorOnly = true
	undispatched := append([]connector.WorkflowState(nil), landing...)
	undispatched[3].Dispatchable = false
	rework := append(append([]connector.WorkflowState(nil), landing...), connector.WorkflowState{Name: "Rework", Dispatchable: true, Transitions: []string{"In Review", "Merging", "Blocked"}})
	reworkRetry := append(append([]connector.WorkflowState(nil), landing...), connector.WorkflowState{Name: "Rework", Dispatchable: true, Transitions: []string{"Todo", "In Review", "Merging", "Blocked"}})
	blockedLanding := append([]connector.WorkflowState(nil), landing...)
	blockedLanding[1].Transitions = append([]string{"Blocked"}, blockedLanding[1].Transitions...)
	blockedLanding = append(blockedLanding, connector.WorkflowState{Name: "Blocked", Transitions: []string{"In Progress"}})
	blockedExecution := append([]connector.WorkflowState(nil), blockedLanding...)
	blockedExecution[1].Dispatchable = false
	unfinishedWorkflow := append(append([]connector.WorkflowState(nil), blockedLanding...), connector.WorkflowState{Name: "Rework", Dispatchable: true, Transitions: []string{"In Review", "Merging", "Blocked"}})
	unfinishedWorkflow[1].Transitions = append([]string{"Rework"}, unfinishedWorkflow[1].Transitions...)
	blockedRecovery := append([]connector.WorkflowState(nil), unfinishedWorkflow...)
	blockedRecovery[1].Dispatchable = false
	noDispatchRework := append([]connector.WorkflowState(nil), unfinishedWorkflow...)
	noDispatchRework[len(noDispatchRework)-1].Dispatchable = false
	operatorRework := append([]connector.WorkflowState(nil), unfinishedWorkflow...)
	operatorRework[len(operatorRework)-1].OperatorOnly = true
	customRework := append([]connector.WorkflowState(nil), unfinishedWorkflow...)
	customRework[1].Transitions = append([]string{"Fixing"}, customRework[1].Transitions...)
	customRework[len(customRework)-1].Name = "Fixing"
	unfinishedReport := "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```"
	instanceReport := "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:worker-loopback\n    reason: sandbox refused the mock listener with EPERM\nhuman_action: null\n```"
	prerequisiteReport := "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: prj_6d4919bebd73446798e6cd807feda10e#411\n    reason: prerequisite must finish\nhuman_action: null\n```"
	noRecoveryLane := []connector.WorkflowState{
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Done", Terminal: true},
	}
	checkpoint := DiffStats{Status: "clean", HeadSHA: strings.Repeat("c", 40), RecoveryStateExpected: true, RecoveryStateAvailable: true}
	checkpointDeadline := fmt.Errorf("worker cancellation: deadline_exceeded (source: orchestrator.parent_context)\nrun agent turn: %w\nresolve cleanup ownership source: git rev-parse --git-common-dir failed: %w", runpkg.ErrNativeRecoveryRequired, context.DeadlineExceeded)
	yes, no := true, false
	head := strings.Repeat("c", 40)
	opened := &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, Files: 2}
	waiting := &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Files: 2}
	accepted := &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", Reviewed: true, HeadSHA: head, Files: 2}
	validatorRework := &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Files: 2, Validator: &gate.ValidatorResult{VersionID: "version_1", Submitted: true, Verdict: gate.ValidatorVerdictRework, Score: 1}}
	auditRework := *waiting
	auditRework.SecurityAudit = &tracker.SecurityAuditReview{Verdict: "rework", Run: securityaudit.Run{Findings: []securityaudit.Finding{{ID: "audit-finding-1", Severity: "p1", Body: "Handle missing authority"}, {ID: "audit-finding-2", Severity: "p2", Body: "Preserve the request boundary"}, {ID: "resolved-finding", Status: "resolved"}}}}
	attributed := func(role, value string) *runpkg.NativeChange {
		base := strings.Repeat("a", 40)
		diagnostic := "source.go:4: invalid input"
		reproduction := failureattribution.Observation{Command: "check-input", HeadSHA: base, ExitCode: 1, Output: diagnostic, Complete: true}
		if value == "change" {
			reproduction.ExitCode, reproduction.Output = 0, ""
		}
		if value == "unknown" {
			reproduction.Complete = false
		}
		receipt := gate.CommandResult{Command: "check-input", HeadSHA: head, ExitCode: 1, Output: diagnostic, FinishedAt: time.Now(), BaseReproduction: &reproduction}
		review := &gate.ValidatorResult{BaseSHA: base, HeadSHA: head, Verdict: "rework", Findings: []gate.Finding{{Severity: "p1", Body: diagnostic, CheckCommand: receipt.Command, CheckDiagnostic: diagnostic}}, Commands: []gate.CommandResult{receipt}}
		gate.AttributeFindings(base, head, review.Findings, review.Commands)
		gate.NormalizeAttribution(review)
		change := *waiting
		change.BaseSHA = base
		switch role {
		case "validator":
			change.Validator = review
		case "implementation_review":
			change.ImplementationReview = review
		case "security_audit":
			change.SecurityAudit = &tracker.SecurityAuditReview{Verdict: "rework", Run: securityaudit.Run{BaseSHA: base, HeadSHA: head, Findings: []securityaudit.Finding{{ID: "input", Severity: "p1", Body: diagnostic, CheckCommand: receipt.Command, CheckDiagnostic: diagnostic}}, Checks: []securityaudit.CheckComparison{{Head: gate.CommandObservation(receipt), Base: reproduction}}}}
		}
		return &change
	}
	sourceWait := &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Files: 2, ImplementationReview: &gate.ValidatorResult{VersionID: "version_1", HeadSHA: head, Verdict: "wait", Commands: []gate.CommandResult{{Command: "unavailable-host-tool", HeadSHA: head, ExitCode: 127}}}}
	chatGPTCriterion := "a real ChatGPT custom connector signs in against production"
	claudeCriterion := "a real Claude connector signs in against production"
	criterionChange := func(entries ...gate.CriterionEvidence) *runpkg.NativeChange {
		change := *accepted
		var criteria []string
		for _, entry := range entries {
			criteria = append(criteria, entry.Criterion)
		}
		change.Validator = &gate.ValidatorResult{VersionID: "version_1", Submitted: true, Verdict: gate.ValidatorVerdictPass, Score: 1, CriteriaEvidence: entries}
		gate.NormalizeCriterionEvidence(change.Validator, criteria, "")
		cfg := gate.ValidatorConfig{Enabled: true, UnverifiedCriteria: gate.UnverifiedCriteriaRework}
		gate.ApplyCriterionPolicy(cfg, change.Validator)
		if decision, stopped := gate.EvaluateValidator(cfg, *change.Validator); stopped {
			change.Validator.Verdict = string(decision.Action)
		}
		return &change
	}
	criterionReview := func(verdict string, score float64, findings ...gate.Finding) *runpkg.NativeChange {
		change := criterionChange(gate.CriterionEvidence{Criterion: chatGPTCriterion, Kind: gate.EvidenceNotVerified, UnverifiedClass: gate.CriterionUnsatisfiable})
		change.Validator.Verdict = verdict
		change.Validator.Score = score
		change.Validator.Findings = findings
		gate.ApplyCriterionPolicy(gate.ValidatorConfig{}, change.Validator)
		return change
	}
	standingReviews := &tracker.NativeRecovery{ChangeDetail: &tracker.ChangeDetail{
		Change: tracker.ChangeRequest{ID: "change_1", CurrentVersion: "version_1"},
		Reviews: []tracker.ChangeReview{
			{ID: "finding-1", VersionID: "version_1", Decision: "changes_requested"},
			{ID: "finding-2", VersionID: "version_1", Decision: "changes_requested"},
			{ID: "old-finding", VersionID: "version_0", Decision: "changes_requested"},
		},
	}}
	reviewConversations := workspace.ReviewConversationsRefusalText + " Address each finding in Rework, then resolve its conversation with evidence of the fix or dismissal.\n- review-bot on internal/app.go:12: handle the empty case"
	refusedVersion := func(version string) *tracker.NativeRecovery {
		return &tracker.NativeRecovery{
			Change:   &tracker.NativeChangeReference{ChangeID: "change_1", VersionID: version, HeadSHA: head},
			Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{ChangeID: "change_1", VersionID: version, HeadSHA: head, RefusalKind: workspace.LandRefusalProtected}}}}},
			History:  []tracker.CollaborationEvent{{Type: "workflow.transitioned", Data: tracker.CollaborationData{FromState: "Merging", ToState: "Rework", ReasonDetail: reviewConversations}}},
		}
	}
	connectorAction := "Rewrite each of these acceptance criteria as a pre-merge check or remove it; no repository change can satisfy them before merge:\n- " + chatGPTCriterion + "\n- " + claudeCriterion
	deliveryErr := &runpkg.DeliverableRecoveryError{Err: &runpkg.DeliverableCommandError{OperationClass: "pull_request", Message: "pull request publication failed"}}
	for _, test := range []struct {
		machineAttempt   int
		progressCycles   bool
		optout           bool
		name             string
		republish        bool
		finalState       string
		finalMessage     string
		runErr           error
		wantHuman        bool
		wantHumanDetails []string
		delegated        bool
		wantQuestion     string
		wantAction       string
		wantWaitKind     string
		noUsage          bool
		change           *runpkg.NativeChange
		states           []connector.WorkflowState
		statesErr        error
		reviewed         *bool
		reviewState      *connector.ChangeReview
		startHead        string
		updateErr        error
		plain            bool
		quotaWait        bool
		draining         bool
		sourceState      string
		reworkState      string
		wantSameState    bool
		retryCycles      bool
		wantDirect       bool
		diffStats        DiffStats
		wantOrdinary     bool
		wantInstance     bool
		wantSettled      bool
		wantParked       bool
		wantFindings     bool
		auditFindings    bool
		humanReview      *bool
		wantAttribution  string
		wantState        string
		wantComment      string
		wantReason       string
		laneReason       string
		details          []string
		wantSummary      string
		wantDeferred     bool
		wantContinue     bool
		wantAbandoned    bool
		wantTerminal     store.WorkAttemptTerminalState
		roundTrip        bool
		lifecycle        string
		validationErr    error
		wantRecovery     bool
		blockerState     string
		handoffState     string
		interrupted      bool
		turnStarted      bool
		mode             string
		preProvider      bool
		preInstance      bool
		releaseDelivery  bool
		wantAuthority    bool
		recovery         *tracker.NativeRecovery
		cleared          bool
		reusedAttempt    string
	}{
		{name: "PLAN ordinary provider failure retains ordinary completion", mode: runpkg.RunModePlan, sourceState: "Todo", states: planRecovery, runErr: errors.New("provider failed"), turnStarted: true, wantOrdinary: true, wantContinue: true},
		{name: "PLAN post-provider recovery error retains ordinary completion", mode: runpkg.RunModePlan, sourceState: "Todo", states: planRecovery, runErr: runpkg.ErrNativeRecoveryRequired, turnStarted: true, wantOrdinary: true, wantContinue: true},
		{name: "release delivery repository setup failure retains Rework", sourceState: "Rework", preProvider: true, states: reworkRetry, humanReview: &no, runErr: fmt.Errorf("%w: create workspace: invalid pull request repository", runpkg.ErrWorkspacePreparation), preInstance: true, releaseDelivery: true, roundTrip: true},
		{name: "release delivery unavailable base retains Rework", sourceState: "Rework", preProvider: true, states: reworkRetry, humanReview: &no, runErr: fmt.Errorf("%w: create workspace: resolve recovery base: local progress base branch is unavailable", runpkg.ErrWorkspacePreparation), preInstance: true, releaseDelivery: true},
		{name: "PLAN pre-provider checkpoint recovery retries the instance", mode: runpkg.RunModePlan, sourceState: "Todo", preProvider: true, states: planRecovery, runErr: fmt.Errorf("%w: checkpoint_requires_recovery", runpkg.ErrNativeRecoveryRequired), preInstance: true},
		{name: "CODE pre-provider checkpoint recovery retries the instance", preProvider: true, states: planRecovery, humanReview: &no, runErr: fmt.Errorf("%w: checkpoint_requires_recovery", runpkg.ErrNativeRecoveryRequired), preInstance: true},
		{name: "CODE pre_provider external effect uncertainty retries the instance", preProvider: true, states: planRecovery, humanReview: &no, runErr: fmt.Errorf("%w: external_effect_uncertain", runpkg.ErrNativeRecoveryRequired), preInstance: true},
		{name: "PLAN pre_provider external effect uncertainty replays as instance failure", mode: runpkg.RunModePlan, sourceState: "Todo", preProvider: true, states: planRecovery, runErr: fmt.Errorf("%w: external_effect_uncertain", runpkg.ErrNativeRecoveryRequired), preInstance: true, roundTrip: true},
		{name: "CODE unavailable retained checkpoint retries the instance", preProvider: true, states: planRecovery, humanReview: &no, runErr: fmt.Errorf("%w: checkpoint_unavailable", runpkg.ErrNativeRecoveryRequired), preInstance: true},
		{name: "PLAN unacknowledged terminal outcome retries the instance after replay", mode: runpkg.RunModePlan, sourceState: "Todo", preProvider: true, states: planRecovery, runErr: fmt.Errorf("%w: session_restart_required", runpkg.ErrNativeRecoveryRequired), preInstance: true, roundTrip: true},
		{name: "CODE changed local checkpoint retries the instance", preProvider: true, states: planRecovery, humanReview: &no, runErr: fmt.Errorf("%w: local_checkpoint_changed", runpkg.ErrNativeRecoveryRequired), preInstance: true},
		{name: "CODE post-provider external effect uncertainty returns to retry", states: planRecovery, humanReview: &yes, turnStarted: true, runErr: fmt.Errorf("%w: external_effect_uncertain", runpkg.ErrNativeRecoveryRequired), wantContinue: true, wantState: "Todo", wantTerminal: store.WorkAttemptTerminalCancelled, wantRecovery: true},
		{name: "PLAN persisted policy refusal retries the instance", mode: runpkg.RunModePlan, sourceState: "Todo", preProvider: true, states: planRecovery, runErr: fmt.Errorf("%w: policy_mismatch", runpkg.ErrNativeRecoveryRequired), preInstance: true},
		{name: "PLAN policy refusal denied lane retries unchanged lane after replay", mode: runpkg.RunModePlan, sourceState: "Todo", preProvider: true, states: planRecovery, updateErr: connector.ErrStateUpdateBlocked, runErr: fmt.Errorf("%w: policy_mismatch", runpkg.ErrNativeRecoveryRequired), preInstance: true, roundTrip: true},
		{name: "PLAN recovery refusal unavailable workflow remains deferred", mode: runpkg.RunModePlan, sourceState: "Todo", preProvider: true, states: planRecovery, statesErr: errors.New("native workflow unavailable"), runErr: fmt.Errorf("%w: checkpoint_requires_recovery", runpkg.ErrNativeRecoveryRequired), wantDeferred: true},
		{name: "PLAN policy refusal stale lane fencing needs no lane write", mode: runpkg.RunModePlan, sourceState: "Todo", preProvider: true, states: planRecovery, updateErr: tracker.ErrStaleFencingToken, runErr: fmt.Errorf("%w: policy_mismatch", runpkg.ErrNativeRecoveryRequired), preInstance: true},
		{name: "PLAN policy refusal unavailable lane write needs no lane write", mode: runpkg.RunModePlan, sourceState: "Todo", preProvider: true, states: planRecovery, updateErr: errors.New("native lane write unavailable"), runErr: fmt.Errorf("%w: policy_mismatch", runpkg.ErrNativeRecoveryRequired), preInstance: true},
		{name: "PLAN instance recovery ignores dispatchable Blocked", mode: runpkg.RunModePlan, sourceState: "Todo", preProvider: true, states: dispatchableBlocked, runErr: fmt.Errorf("%w: checkpoint_requires_recovery", runpkg.ErrNativeRecoveryRequired), preInstance: true},
		{name: "PLAN instance recovery ignores operator Blocked", mode: runpkg.RunModePlan, sourceState: "Todo", preProvider: true, states: operatorBlocked, runErr: fmt.Errorf("%w: checkpoint_requires_recovery", runpkg.ErrNativeRecoveryRequired), preInstance: true},
		{name: "PLAN instance recovery needs no Blocked destination", mode: runpkg.RunModePlan, sourceState: "Todo", preProvider: true, states: fourLanes, runErr: fmt.Errorf("%w: checkpoint_requires_recovery", runpkg.ErrNativeRecoveryRequired), preInstance: true},
		{name: "PLAN recovery refusal with expired authority preserves issue", mode: runpkg.RunModePlan, sourceState: "Todo", preProvider: true, states: planRecovery, runErr: errors.Join(runpkg.ErrNativeRecoveryRequired, runpkg.ErrExecutionAuthorityUnavailable, tracker.ErrStaleFencingToken), wantAuthority: true},
		{name: "PLAN policy refusal requires no lane write", mode: runpkg.RunModePlan, sourceState: "Todo", preProvider: true, states: planRecovery, updateErr: errors.Join(runpkg.ErrExecutionAuthorityUnavailable, tracker.ErrStaleFencingToken), runErr: fmt.Errorf("%w: policy_mismatch", runpkg.ErrNativeRecoveryRequired), preInstance: true},
		{name: "source free completion without Change returns to Todo", states: fourLanes, humanReview: &no, wantSettled: true, wantContinue: true, wantState: "Todo"},
		{name: "unfinished completion without Change returns checkpoint to Rework", states: unfinishedWorkflow, humanReview: &no, finalMessage: unfinishedReport, diffStats: checkpoint, wantSettled: true, wantContinue: true, wantState: "Rework"},
		{name: "typed instance blockers retry with instance reason code", states: blockedLanding, humanReview: &no, finalMessage: strings.Replace(instanceReport, "status: blocked", "status: blocked\nreason_code: instance_limitation", 1), wantInstance: true},
		{name: "instance blocked completion without Change retries", states: blockedLanding, humanReview: &no, finalMessage: instanceReport, wantInstance: true},
		{name: "released instance blocked completion leaves nondispatchable execution lane", states: blockedExecution, humanReview: &no, finalMessage: instanceReport, wantInstance: true, wantState: "Todo", roundTrip: true},
		{name: "released instance blocked checkpoint recovers through Rework", states: blockedRecovery, humanReview: &no, finalMessage: instanceReport, diffStats: checkpoint, wantInstance: true, wantState: "Rework", roundTrip: true},
		{name: "instance blocked settlement failure retains completion owner", states: blockedExecution, humanReview: &no, finalMessage: instanceReport, updateErr: connector.ErrStateUpdateBlocked, wantDeferred: true, wantState: "Todo"},
		{name: "instance infrastructure restoration retains completion on workflow outage", states: blockedExecution, humanReview: &no, runErr: runpkg.ErrValidatorInfrastructure, statesErr: errors.New("workflow unavailable"), wantDeferred: true},
		{name: "instance blocked completion preserves active landing handoff", states: blockedExecution, humanReview: &no, finalMessage: instanceReport, handoffState: "Merging", wantInstance: true},
		{name: "blocked unchanged Rework preserves instance checkpoint without Change", states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, finalMessage: instanceReport, diffStats: checkpoint, wantInstance: true, roundTrip: true},
		{name: "blocked unchanged Rework preserves unmet prerequisite without Change", states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, finalMessage: prerequisiteReport, blockerState: "Backlog", diffStats: checkpoint, wantState: "Rework", wantSameState: true, wantComment: "detent-status blocked", roundTrip: true},
		{name: "instance blocker lane refusal still retries", states: blockedLanding, humanReview: &no, finalMessage: instanceReport, updateErr: connector.ErrStateUpdateBlocked, wantInstance: true},
		{name: "instance blocker without Blocked lane retries", states: fourLanes, humanReview: &no, finalMessage: instanceReport, wantInstance: true},
		{name: "Rework completion leaving requested changes on its starting head has no work product", change: waiting, recovery: standingReviews, wantFindings: true, startHead: head, reviewState: &connector.ChangeReview{Current: true, ChangesRequested: true}, states: reworkRetry, sourceState: "Rework", humanReview: &no, finalMessage: "```detent-status\\nschema: 1\\nstatus: complete\\nblockers: []\\nhuman_action: null\\n```", wantSettled: true, wantContinue: true, wantSameState: true, wantState: "Rework"},
		{name: "Rework completion leaving a validator rework verdict on its starting head has no work product", change: validatorRework, startHead: head, states: reworkRetry, sourceState: "Rework", humanReview: &no, finalMessage: "```detent-status\\nschema: 1\\nstatus: complete\\nblockers: []\\nhuman_action: null\\n```", wantSettled: true, wantContinue: true, wantSameState: true, wantState: "Rework"},
		{name: "unchanged audited Rework parks without an allowed Todo retry", change: &auditRework, startHead: head, states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantSettled: true, wantContinue: true, wantSameState: true, auditFindings: true, wantState: "Rework"},
		{name: "Rework publishing a new head with standing findings remains in Rework", change: validatorRework, startHead: strings.Repeat("d", 40), states: reworkRetry, sourceState: "Rework", humanReview: &no, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantState: "Rework", wantSameState: true, wantComment: "validator requested changes"},
		{name: "standing native findings take the retry brake before a review-conversation refusal", change: validatorRework, startHead: head, recovery: refusedVersion("version_1"), states: reworkRetry, sourceState: "Rework", humanReview: &no, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantSettled: true, wantContinue: true, wantSameState: true, wantState: "Rework"},
		{name: "an unchanged reviewed head with only a review-conversation refusal holds for review", change: accepted, startHead: head, recovery: refusedVersion("version_1"), states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, reviewed: &yes, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantState: "Rework", wantSameState: true, wantComment: "resolved none of the review conversations"},
		{name: "Rework completion with new commits under requested changes stays in Rework", change: waiting, startHead: strings.Repeat("e", 40), reviewState: &connector.ChangeReview{Current: true, ChangesRequested: true}, states: reworkRetry, sourceState: "Rework", humanReview: &no, finalMessage: "```detent-status\\nschema: 1\\nstatus: complete\\nblockers: []\\nhuman_action: null\\n```", wantState: "Rework", wantSameState: true},
		{name: "unfinished source free completion with checkpoint returns to Rework", states: unfinishedWorkflow, change: &runpkg.NativeChange{}, humanReview: &no, diffStats: checkpoint, wantSettled: true, wantContinue: true, wantState: "Rework"},
		{name: "recorded failed completion with retained checkpoint returns to Rework", states: unfinishedWorkflow, humanReview: &no, runErr: checkpointDeadline, diffStats: checkpoint, turnStarted: true, wantContinue: true, wantState: "Rework", wantTerminal: store.WorkAttemptTerminalCancelled, wantRecovery: true},
		{name: "recorded failed completion without checkpoint returns to Todo", states: fourLanes, humanReview: &no, runErr: checkpointDeadline, turnStarted: true, wantContinue: true, wantState: "Todo", wantTerminal: store.WorkAttemptTerminalCancelled, wantRecovery: true},
		{name: "retained failed checkpoint uses configured Rework", states: customRework, reworkState: "Fixing", humanReview: &no, runErr: checkpointDeadline, diffStats: checkpoint, wantContinue: true, wantState: "Fixing", wantTerminal: store.WorkAttemptTerminalCancelled, wantRecovery: true},
		{name: "retained failed checkpoint refuses nondispatchable Rework", states: noDispatchRework, humanReview: &no, runErr: checkpointDeadline, diffStats: checkpoint, wantDeferred: true},
		{name: "retained failed checkpoint refuses operator Rework", states: operatorRework, humanReview: &no, runErr: checkpointDeadline, diffStats: checkpoint, wantDeferred: true},
		{name: "retained checkpoint routing survives deferred replay", states: unfinishedWorkflow, humanReview: &no, runErr: checkpointDeadline, diffStats: checkpoint, roundTrip: true, wantContinue: true, wantState: "Rework", wantTerminal: store.WorkAttemptTerminalCancelled, wantRecovery: true},
		{name: "failed Rework checkpoint stays in Rework", states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, runErr: checkpointDeadline, diffStats: checkpoint, wantContinue: true, wantState: "Rework", wantSameState: true, wantTerminal: store.WorkAttemptTerminalCancelled, wantRecovery: true},
		{name: "retained failed checkpoint lane refusal preserves authority", states: unfinishedWorkflow, humanReview: &no, runErr: checkpointDeadline, diffStats: checkpoint, updateErr: connector.ErrStateUpdateBlocked, wantDeferred: true},
		{name: "failed provider with checkpoint returns to Rework", states: unfinishedWorkflow, humanReview: &no, runErr: errors.New("provider failed"), diffStats: checkpoint, wantContinue: true, wantState: "Rework"},
		{name: "completed checkpoint ownership deadline is instance owned with human review", states: workflow, humanReview: &yes, runErr: errors.Join(runpkg.ErrWorkspacePreparation, runpkg.ErrNativeRecoveryRequired, context.DeadlineExceeded), wantContinue: true},
		{name: "completed checkpoint ownership deadline is instance owned without human review", states: workflow, humanReview: &no, runErr: errors.Join(runpkg.ErrWorkspacePreparation, runpkg.ErrNativeRecoveryRequired, context.DeadlineExceeded), wantContinue: true},
		{name: "checkpoint mismatch without retained source returns to Todo", states: fourLanes, humanReview: &no, runErr: fmt.Errorf("%w: local_checkpoint_changed", runpkg.ErrNativeRecoveryRequired), wantContinue: true, wantState: "Todo", wantTerminal: store.WorkAttemptTerminalCancelled, wantRecovery: true},
		{name: "checkpoint mismatch without human review returns to Todo", states: workflow, humanReview: &no, runErr: fmt.Errorf("%w: local_checkpoint_changed", runpkg.ErrNativeRecoveryRequired), wantContinue: true, wantState: "Todo", wantTerminal: store.WorkAttemptTerminalCancelled, wantRecovery: true},
		{name: "checkpoint mismatch optout returns to retry", states: workflow, humanReview: &no, optout: true, runErr: fmt.Errorf("%w: local_checkpoint_changed", runpkg.ErrNativeRecoveryRequired), wantContinue: true, wantState: "Todo", wantTerminal: store.WorkAttemptTerminalCancelled, wantRecovery: true},
		{name: "checkpoint mismatch is not human review ready", states: workflow, runErr: fmt.Errorf("%w: local_checkpoint_changed", runpkg.ErrNativeRecoveryRequired), wantContinue: true, wantState: "Todo", wantTerminal: store.WorkAttemptTerminalCancelled, wantRecovery: true},
		{name: "missing legacy source is not human review ready", states: workflow, runErr: fmt.Errorf("%w: missing_head: legacy Change has no retained source", runpkg.ErrNativeRecoveryRequired), wantContinue: true, wantState: "Todo", wantTerminal: store.WorkAttemptTerminalCancelled, wantRecovery: true},
		{name: "deferred checkpoint mismatch retains refusal after restart", states: fourLanes, humanReview: &no, runErr: fmt.Errorf("%w: local_checkpoint_changed", runpkg.ErrNativeRecoveryRequired), wantContinue: true, wantState: "Todo", wantTerminal: store.WorkAttemptTerminalCancelled, wantRecovery: true, roundTrip: true},
		{name: "persisted policy mismatch leaves dispatch", states: fourLanes, humanReview: &no, runErr: fmt.Errorf("%w: policy_mismatch: persisted session policy changed", runpkg.ErrNativeRecoveryRequired), wantContinue: true, wantState: "Todo", wantTerminal: store.WorkAttemptTerminalCancelled, wantRecovery: true},
		{name: "checkpoint mismatch without destination retains completion", states: noRecoveryLane, humanReview: &no, runErr: fmt.Errorf("%w: local_checkpoint_changed", runpkg.ErrNativeRecoveryRequired), wantDeferred: true, roundTrip: true},
		{name: "checkpoint mismatch lane write refusal retains completion", states: fourLanes, humanReview: &no, runErr: fmt.Errorf("%w: local_checkpoint_changed", runpkg.ErrNativeRecoveryRequired), updateErr: connector.ErrStateUpdateBlocked, wantDeferred: true},
		{name: "permanent excluded source refusal settles without tracker outage", change: &runpkg.NativeChange{VersionError: `checkpoint could not be safely published: excluded source path ".detent/skills/split-issue.md"`}, states: fourLanes, humanReview: &no, wantSettled: true, wantContinue: true, wantState: "Todo", roundTrip: true},
		{name: "permanent source refusal cannot accept a complete report", change: &runpkg.NativeChange{VersionError: `checkpoint could not be safely published: excluded source path ".detent/skills/split-issue.md"`}, states: workflow, finalMessage: strings.Replace(unfinishedReport, "in_progress", "complete", 1), wantState: "In Review", wantComment: "excluded source path"},
		{name: "settled head refusal retires deferred completion authority", lifecycle: "refusal", republish: true, change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, VersionError: "the final attempt diff does not identify the current Change Request head"}, states: fourLanes, statesErr: errors.New("workflow temporarily unavailable"), humanReview: &no, wantSettled: true, roundTrip: true},
		{name: "settled policy refusal retires deferred completion authority", lifecycle: "refusal", republish: true, change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", Reviewed: true, HeadSHA: head, VersionError: "policy mismatch", VersionCode: "policy_mismatch"}, states: landing, statesErr: errors.New("workflow temporarily unavailable"), reviewed: &yes, humanReview: &no, wantSettled: true},
		{name: "no review missing report returns source free completion to Todo", machineAttempt: 3, lifecycle: "settle", change: &runpkg.NativeChange{}, states: fourLanes, humanReview: &no, wantSettled: true, wantContinue: true, wantState: "Todo", roundTrip: true},
		{name: "no review failed final state returns to Todo", diffStats: DiffStats{Status: "clean", HeadSHA: head, TreeSHA: "changed-tree"}, machineAttempt: 4, change: accepted, states: fourLanes, humanReview: &no, finalState: runpkg.FinalStateFailed, wantContinue: true, wantState: "Todo", wantTerminal: store.WorkAttemptTerminalFailure},
		{name: "refusal without a safe workflow destination settles without relaning", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, VersionError: "head mismatch"}, states: hosted, humanReview: &no, wantSettled: true, roundTrip: true},
		{name: "native release outage keeps completion ownership", lifecycle: "release", change: accepted, states: landing, reviewed: &yes, wantState: "Merging"},
		{name: "normal provider exit renews through delayed native settlement", lifecycle: "settle", change: accepted, states: landing, reviewed: &yes, wantState: "Merging"},
		{name: "normal provider exit renews through deferred native publication", lifecycle: "defer", republish: true, change: &runpkg.NativeChange{Changed: true, Error: "native publication unavailable"}, states: landing, reviewed: &yes, wantState: "Merging"},
		{name: "canceled validation retains completed native result", lifecycle: "interrupted", validationErr: context.Canceled, republish: true, change: &runpkg.NativeChange{Changed: true, Error: "native publication unavailable"}, states: landing, reviewed: &yes, wantState: "Merging"},
		{name: "timed out validation retains completed native result", lifecycle: "interrupted", validationErr: context.DeadlineExceeded, republish: true, change: &runpkg.NativeChange{Changed: true, Error: "native publication unavailable"}, states: landing, reviewed: &yes, wantState: "Merging"},
		{name: "deferred native publication loses fencing authority", lifecycle: "lost", republish: true, change: &runpkg.NativeChange{Changed: true, Error: "native publication unavailable"}, states: landing, reviewed: &yes},
		{name: "true provider cancellation retires renewal", lifecycle: "cancel", runErr: context.Canceled, states: workflow, wantState: "In Review", wantTerminal: store.WorkAttemptTerminalCancelled},
		{name: "four lane reviewed change lands from current lane", change: accepted, states: fourLanes, humanReview: &no, wantDirect: true},
		{name: "four lane reviewed change lands during drain", change: accepted, states: fourLanes, humanReview: &no, draining: true, wantDirect: true},
		{name: "four lane reviewed optout waits for human review", change: accepted, states: fourLanes, reviewed: &yes, optout: true, humanReview: &no, wantState: "Human Review"},
		{name: "native token ceiling uses growing retry", machineAttempt: 8, change: accepted, states: fourLanes, humanReview: &no, runErr: runpkg.ErrSessionTokenCeilingExceeded, turnStarted: true, wantContinue: true, wantState: "Todo"},
		{name: "four lane failed run returns to Todo", diffStats: DiffStats{Status: "clean", HeadSHA: head, TreeSHA: "changed-tree"}, machineAttempt: 5, lifecycle: "settle", roundTrip: true, change: accepted, states: fourLanes, humanReview: &no, runErr: errors.New("provider failed"), wantContinue: true, wantState: "Todo"},
		{name: "four lane unreviewed version retains completion", change: waiting, states: fourLanes, humanReview: &no, wantDeferred: true},
		{name: "four lane unfinished source settles without a Rework move", change: accepted, states: fourLanes, humanReview: &no, finalMessage: unfinishedReport, wantSettled: true},
		{name: "reason only report settles without acceptance", change: accepted, states: unfinishedWorkflow, humanReview: &no, finalMessage: "Source conflicts remain unresolved.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: merge_conflict\nblockers: []\nhuman_action: null\n```", wantSettled: true},
		{name: "human action comment retains agent reason and summary", change: accepted, states: workflow, finalMessage: "The operator must approve the migration.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: permission_wait\nblockers: []\nhuman_action: Approve the migration\n```", wantHuman: true, wantAction: "Approve the migration", wantReason: "permission_wait", wantSummary: "The operator must approve the migration."},
		{name: "untyped instance limitation settles without acceptance", change: accepted, states: unfinishedWorkflow, humanReview: &no, finalMessage: "Sandbox forbids TCP listeners; upstream fetch returned HTTP 403.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: instance_limitation\nblockers: []\nhuman_action: null\n```", wantSettled: true},
		{name: "deferred cleanup deadline retains instance attribution", states: workflow, runErr: errors.Join(runpkg.ErrWorkerProcessReap, context.DeadlineExceeded), roundTrip: true, wantContinue: true},
		{name: "deferred provider cancellation retains its outcome", states: workflow, runErr: context.Canceled, wantState: "In Review", wantTerminal: store.WorkAttemptTerminalCancelled, roundTrip: true},
		{name: "failed native stale authority is rejected", states: workflow, runErr: errors.New("provider failed"), updateErr: errors.Join(runpkg.ErrExecutionAuthorityUnavailable, errors.New("final diff unavailable")), wantAbandoned: true},
		{name: "failed provider without native change settles for review", states: workflow, runErr: errors.New("provider failed"), wantState: "In Review"},
		{name: "failed provider optout prefers review over Blocked", states: workflow, humanReview: &no, optout: true, runErr: errors.New("provider failed"), wantState: "In Review"},
		{name: "failed provider lane refusal retains authority", states: workflow, runErr: errors.New("provider failed"), updateErr: errors.New("stale fencing token"), wantDeferred: true},
		{name: "failed provider missing allowed lane retains authority", states: hosted, runErr: errors.New("provider failed"), wantDeferred: true},
		{name: "failed outcome without error cannot publish", states: workflow, finalState: runpkg.FinalStateFailed, change: accepted, wantState: "In Review", wantTerminal: store.WorkAttemptTerminalFailure},
		{name: "successful native coding completes during drain", change: accepted, states: landing, draining: true, wantState: "Merging", wantComment: "runner lands it next"},
		{name: "successful native rework retains current version during drain", change: accepted, states: rework, sourceState: "Rework", draining: true, wantState: "Merging", wantComment: "runner lands it next"},
		{name: "native publication authority failure during drain is instance owned", states: rework, sourceState: "Rework", draining: true, runErr: errors.Join(runpkg.ErrExecutionAuthorityUnavailable, errors.New("final diff unavailable")), wantAbandoned: true},
		{name: "missing native result during drain keeps ordinary cleanup", states: rework, sourceState: "Rework", draining: true, wantOrdinary: true},
		{name: "refused native lane write during drain stays with native completion", change: waiting, states: rework, sourceState: "Rework", draining: true, updateErr: errors.New("stale fencing token"), wantDeferred: true},
		{name: "failed native rework during drain cannot complete a published version", change: accepted, states: rework, sourceState: "Rework", draining: true, runErr: errors.New("backend failed during drain"), wantState: "In Review"},
		{name: "interrupted native rework during drain cannot complete a published version", change: accepted, states: rework, sourceState: "Rework", draining: true, runErr: context.Canceled, wantTerminal: store.WorkAttemptTerminalCancelled, wantState: "In Review"},
		{name: "successful coding publishes during landing quota wait", change: accepted, states: landing, quotaWait: true, wantState: "Merging", wantComment: "runner lands it next"},
		{name: "commits move to the configured review lane", change: waiting, states: workflow, wantState: "In Review", wantComment: "opened Change Request change_1"},
		{name: "a version that needs no reviewer goes straight to landing", change: accepted, states: landing, wantState: "Merging", wantComment: "runner lands it next"},
		{name: "MergeResolution changed source returns from merge to review", mode: runpkg.RunModeMerge, sourceState: "Merging", change: &runpkg.NativeChange{MergeResolution: true, Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: head}, states: landing, wantState: "In Review", wantComment: "opened Change Request change_1"},
		{name: "a version waiting for a reviewer goes to review", change: waiting, states: landing, wantState: "In Review", wantComment: "opened Change Request change_1"},
		{name: "published passing version waits for review without implementation redispatch", lifecycle: "review_wait", change: waiting, states: unfinishedWorkflow, humanReview: &no, reviewed: &no, finalMessage: strings.Replace(unfinishedReport, "in_progress", "complete", 1)},
		{name: "published version retains audit infrastructure failure", lifecycle: "review_audit", runErr: fmt.Errorf("%w: landing security audit: unavailable", runpkg.ErrValidatorInfrastructure), finalState: runpkg.FinalStateFailed, change: waiting, states: unfinishedWorkflow, humanReview: &no, reviewed: &no, finalMessage: strings.Replace(unfinishedReport, "in_progress", "complete", 1)},
		{name: "published version retains unparseable validator output", lifecycle: "review_validator", runErr: fmt.Errorf("%w: parse validator result: invalid character", runpkg.ErrValidatorInfrastructure), finalState: runpkg.FinalStateFailed, change: waiting, states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, reviewed: &no, finalMessage: strings.Replace(unfinishedReport, "in_progress", "complete", 1)},
		{name: "published version retries unparseable validator without completion report", runErr: fmt.Errorf("%w: parse validator result: invalid character", runpkg.ErrValidatorInfrastructure), finalState: runpkg.FinalStateFailed, change: waiting, states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, wantState: "Rework", wantSameState: true, wantContinue: true},
		{name: "published review wait survives retained source in Rework", lifecycle: "review_wait", change: waiting, states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, reviewed: &no, finalMessage: strings.Replace(unfinishedReport, "in_progress", "complete", 1)},
		{name: "published review wait resumes after Hub timeout", lifecycle: "review_timeout", change: waiting, states: unfinishedWorkflow, sourceState: "In Progress", humanReview: &no, reviewed: &no, finalMessage: strings.Replace(unfinishedReport, "in_progress", "complete", 1)},
		{name: "published review wait receives actionable findings", lifecycle: "review_rework", change: waiting, states: unfinishedWorkflow, sourceState: "In Progress", humanReview: &no, reviewed: &no, finalMessage: strings.Replace(unfinishedReport, "in_progress", "complete", 1)},
		{name: "published review wait refuses changed head", lifecycle: "review_head", change: waiting, states: unfinishedWorkflow, sourceState: "In Progress", humanReview: &no, reviewed: &no, finalMessage: strings.Replace(unfinishedReport, "in_progress", "complete", 1)},
		{name: "published review wait rejects expired authority", lifecycle: "review_expired", change: waiting, states: unfinishedWorkflow, sourceState: "In Progress", humanReview: &no, reviewed: &no, finalMessage: strings.Replace(unfinishedReport, "in_progress", "complete", 1)},
		{name: "published review wait preserves operator hold", lifecycle: "review_operator", change: waiting, states: unfinishedWorkflow, sourceState: "In Progress", humanReview: &no, reviewed: &no, finalMessage: strings.Replace(unfinishedReport, "in_progress", "complete", 1)},
		{name: "disabled review lands accepted change", change: accepted, states: blockedLanding, humanReview: &no, wantState: "Merging", wantComment: "runner lands it next"},
		{name: "disabled review complete source settles publication refusal", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, VersionID: "version_1", Reviewed: true, VersionError: "the final attempt diff does not identify the current Change Request head", VersionCode: "policy_mismatch"}, states: unfinishedWorkflow, humanReview: &no, reviewed: &yes, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantSettled: true, roundTrip: true},
		{name: "an accepted version without a landing move lands directly", change: accepted, states: workflow, wantDirect: true},
		{name: "an approval that arrived after the publish lands", change: waiting, states: landing, reviewed: &yes, wantState: "Merging", wantComment: "runner lands it next"},
		{name: "reused completion names its source attempt in the item comment", change: waiting, states: rework, sourceState: "Rework", reviewed: &yes, reusedAttempt: "attempt_source", wantState: "Merging", wantComment: "Detent reused the completed disposition from attempt attempt_source to resume review without rerunning implementation."},
		{name: "reused completion discloses its source when acceptance requests rework", change: criterionReview("rework", 1, gate.Finding{Severity: "p2", Body: "Source defect"}), states: unfinishedWorkflow, humanReview: &no, reusedAttempt: "attempt_source", wantState: "Rework", wantComment: "Detent reused the completed disposition from attempt attempt_source to resume review without rerunning implementation."},
		{name: "a run that published no version cannot succeed on an earlier reviewed one", change: opened, states: landing, reviewed: &yes, wantDeferred: true},
		{name: "unavailable version evidence retains publication owner", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, Error: "publication unavailable"}, states: landing, wantDeferred: true},
		{name: "unproven absorbed rework hands off the actual mismatch without publication replay", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, VersionError: "the final attempt diff does not identify the current Change Request head"}, finalMessage: unfinishedReport, states: landing, humanReview: &no, wantSettled: true},
		{name: "refused policy cannot inherit earlier version approval", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, VersionID: "version_1", Reviewed: true, VersionError: "policy mismatch", VersionCode: "policy_mismatch"}, reviewed: &yes, states: rework, sourceState: "Rework", wantState: "In Review", wantComment: "policy mismatch"},
		{name: "PR publication head refusal retries Rework", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, VersionError: "required GitHub PR is not ready; preserve source and resolve publication blocker: head_moved: PR publication identity mismatch; preserve source and reconcile the recorded PR before retrying", VersionCode: "head_moved"}, states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, wantState: "Rework", wantSameState: true, wantComment: "reconcile the recorded PR"},
		{name: "PR publication head refusal moves to Rework", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, VersionError: "required GitHub PR is not ready; preserve source and resolve publication blocker: head_moved: PR publication identity mismatch; preserve source and reconcile the recorded PR before retrying", VersionCode: "head_moved"}, states: unfinishedWorkflow, sourceState: "In Progress", humanReview: &no, wantState: "Rework", wantComment: "reconcile the recorded PR"},
		{name: "publication refusal preserves review owner during drain", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, VersionError: "producer allowance exhausted", VersionCode: "allowance_exhausted"}, states: rework, sourceState: "Rework", draining: true, wantState: "In Review", wantComment: "allowance exhausted"},
		{name: "native publication authority failure is instance owned", states: workflow, runErr: errors.Join(runpkg.ErrExecutionAuthorityUnavailable, errors.New("final diff unavailable")), wantAbandoned: true},
		{name: "a version that lost its acceptance goes to review", change: accepted, states: landing, reviewed: &no, wantState: "In Review", wantComment: "opened Change Request change_1"},
		{name: "an accepted version lands directly without a dispatchable landing lane", change: accepted, states: undispatched, wantDirect: true},
		{name: "unsatisfiable criterion preserves blocking finding", change: criterionReview("wait", 1, gate.Finding{Severity: "p1", Body: "Source defect"}), states: unfinishedWorkflow, humanReview: &no, wantState: "Rework", wantComment: "validator requested changes", laneReason: "validator_rework", details: []string{"Source defect"}},
		{name: "unsatisfiable criterion preserves minimum score", change: criterionReview("wait", .2), states: unfinishedWorkflow, humanReview: &no, wantState: "Rework", wantComment: "validator requested changes"},
		{name: "unsatisfiable criterion preserves explicit source rework", change: criterionReview("rework", 1), states: unfinishedWorkflow, humanReview: &no, wantState: "Rework", wantComment: "validator requested changes"},
		{name: "unsatisfiable criterion preserves published review findings", change: criterionReview("wait", 1), reviewState: &connector.ChangeReview{Current: true, ChangesRequested: true}, states: unfinishedWorkflow, humanReview: &no, wantState: "Rework", wantComment: "opened Change Request"},
		{name: "undecidable security audit uses existing human park", wantHuman: true, wantAction: "Decide the security audit leads from the supplied evidence: auth: first verdict undetermined; higher-model verdict undetermined; unresolved fact: Whether caller authenticates", wantHumanDetails: []string{"auth", "first verdict undetermined", "higher-model verdict undetermined", "Whether caller authenticates"}, change: waiting, states: unfinishedWorkflow, humanReview: &no, finalMessage: (securityaudit.Run{Findings: []securityaudit.Finding{{ID: "auth", Certainty: securityaudit.CertaintyUndetermined, Body: "Potential p2 access", UnresolvedFact: "Whether caller authenticates"}}}).CompletionMessage("Source complete"), wantState: "Blocked", wantComment: "Whether caller authenticates"},
		{name: "unsatisfiable criterion preserves reported human action", change: criterionReview("wait", 1), states: unfinishedWorkflow, humanReview: &no, finalMessage: strings.ReplaceAll(unfinishedReport, "human_action: null", "human_action: Approve the consumer rollout"), wantState: "Blocked", wantComment: "issue acceptance is not recorded"},
		{name: "unsatisfiable criterion preserves prerequisite owner", change: criterionReview("wait", 1), states: unfinishedWorkflow, humanReview: &no, finalMessage: prerequisiteReport, wantState: "Rework", wantComment: "prerequisite must finish"},
		{name: "fixable criterion keeps Rework", change: criterionChange(gate.CriterionEvidence{Criterion: "Missing screenshot", Kind: gate.EvidenceNotVerified, UnverifiedClass: gate.CriterionReworkFixable}), states: unfinishedWorkflow, humanReview: &no, wantState: "Rework", wantComment: "validator requested changes"},
		{name: "delegated external verification returns to source rework", delegated: true, change: criterionChange(gate.CriterionEvidence{Criterion: chatGPTCriterion, Kind: gate.EvidenceNotVerified, UnverifiedClass: gate.CriterionUnsatisfiable}), states: unfinishedWorkflow, humanReview: &no, wantState: "Rework", laneReason: "validator_rework", wantComment: "Record equivalent pre-merge criteria", details: []string{chatGPTCriterion}},
		{name: "delegated policy preserves actual human action", delegated: true, change: criterionReview("wait", 1), states: unfinishedWorkflow, humanReview: &no, finalMessage: strings.ReplaceAll(unfinishedReport, "human_action: null", "human_action: Approve the consumer rollout"), wantState: "Blocked", wantComment: "issue acceptance is not recorded"},
		{name: "1245 production connectors block on first validator run", change: criterionChange(gate.CriterionEvidence{Criterion: chatGPTCriterion, Kind: gate.EvidenceNotVerified, UnverifiedClass: gate.CriterionUnsatisfiable}, gate.CriterionEvidence{Criterion: claudeCriterion, Kind: gate.EvidenceNotVerified, UnverifiedClass: gate.CriterionUnsatisfiable}), states: unfinishedWorkflow, humanReview: &no, wantState: "Rework", roundTrip: true},
		{name: "criteria gate lane outage defers without a generic permission wait", change: criterionChange(gate.CriterionEvidence{Criterion: chatGPTCriterion, Kind: gate.EvidenceNotVerified, UnverifiedClass: gate.CriterionUnsatisfiable}, gate.CriterionEvidence{Criterion: claudeCriterion, Kind: gate.EvidenceNotVerified, UnverifiedClass: gate.CriterionUnsatisfiable}), states: unfinishedWorkflow, humanReview: &no, updateErr: errors.New("native lane write unavailable"), wantDeferred: true, wantState: "Blocked", wantQuestion: connectorAction},
		{name: "mixed criteria block with only unsatisfiable actions", change: criterionChange(gate.CriterionEvidence{Criterion: "Missing regression test", Kind: gate.EvidenceNotVerified, UnverifiedClass: gate.CriterionReworkFixable}, gate.CriterionEvidence{Criterion: chatGPTCriterion, Kind: gate.EvidenceNotVerified, UnverifiedClass: gate.CriterionUnsatisfiable}, gate.CriterionEvidence{Criterion: claudeCriterion, Kind: gate.EvidenceNotVerified, UnverifiedClass: gate.CriterionUnsatisfiable}), states: unfinishedWorkflow, humanReview: &no, finalMessage: unfinishedReport, wantState: "Rework"},
		{name: "rewritten connector criterion reuses retained source and passes", change: criterionChange(gate.CriterionEvidence{Criterion: "Connector sign-in covered by local regression", Kind: gate.EvidenceTest, Reference: "connector_test.go:TestSignIn", Behavior: "The local client signs in successfully"}), states: rework, sourceState: "Rework", humanReview: &no, wantState: "Merging", wantComment: "runner lands it next"},
		{name: "IndependentFailure repair dependency retains dispatchable lane", change: attributed("validator", "independent"), reviewState: &connector.ChangeReview{Current: true, ChangesRequested: true, RepairPending: true}, states: unfinishedWorkflow, humanReview: &no, wantState: "In Progress", wantSameState: true, wantContinue: true},
		{name: "Attribution validator independent retains Rework", change: attributed("validator", "independent"), wantAttribution: "independent", states: unfinishedWorkflow, humanReview: &no, wantState: "Rework"},
		{name: "Attribution validator change retains Rework", change: attributed("validator", "change"), wantAttribution: "change", states: unfinishedWorkflow, humanReview: &no, wantState: "Rework"},
		{name: "Attribution validator unknown retains Rework", change: attributed("validator", "unknown"), wantAttribution: "unknown", states: unfinishedWorkflow, humanReview: &no, wantState: "Rework"},
		{name: "Attribution implementation_review independent retains Rework", change: attributed("implementation_review", "independent"), wantAttribution: "independent", states: unfinishedWorkflow, humanReview: &no, wantState: "Rework"},
		{name: "Attribution implementation_review change retains Rework", change: attributed("implementation_review", "change"), wantAttribution: "change", states: unfinishedWorkflow, humanReview: &no, wantState: "Rework"},
		{name: "Attribution implementation_review unknown retains Rework", change: attributed("implementation_review", "unknown"), wantAttribution: "unknown", states: unfinishedWorkflow, humanReview: &no, wantState: "Rework"},
		{name: "Attribution security_audit independent retains Rework", change: attributed("security_audit", "independent"), wantAttribution: "independent", states: unfinishedWorkflow, humanReview: &no, wantState: "Rework"},
		{name: "Attribution security_audit change retains Rework", change: attributed("security_audit", "change"), wantAttribution: "change", states: unfinishedWorkflow, humanReview: &no, wantState: "Rework"},
		{name: "Attribution security_audit unknown retains Rework", change: attributed("security_audit", "unknown"), wantAttribution: "unknown", states: unfinishedWorkflow, humanReview: &no, wantState: "Rework"},
		{name: "validator rework routes to Rework", diffStats: DiffStats{Status: "clean", HeadSHA: head, TreeSHA: "changed-tree"}, retryCycles: true, change: &runpkg.NativeChange{Changed: true, ChangeID: "change", VersionID: "version", HeadSHA: head, Validator: &gate.ValidatorResult{Verdict: "rework", Summary: "Changes requested", NotVerified: []string{"Reject invalid input"}, Findings: []gate.Finding{{Severity: "p1", Path: "input.go", Body: "Invalid input is accepted"}}}}, states: unfinishedWorkflow, wantState: "Rework", wantComment: "validator requested changes", laneReason: "validator_rework", details: []string{"Reject invalid input", "p1 input.go: Invalid input is accepted", "Changes requested"}},
		{name: "exhausted source review budget retains completion routing", change: &runpkg.NativeChange{Changed: true, ChangeID: "change", VersionID: "version", HeadSHA: head, Validator: &gate.ValidatorResult{Verdict: "rework", Summary: "Shared implementation and review turn budget exhausted", NotVerified: []string{"Reject invalid input"}, Findings: []gate.Finding{{Severity: "p1", Path: "input.go", Body: "Invalid input is accepted"}}}}, states: unfinishedWorkflow, finalMessage: unfinishedReport, wantState: "Rework", wantComment: "validator requested changes", laneReason: "validator_rework", details: []string{"Reject invalid input", "p1 input.go: Invalid input is accepted", "Shared implementation and review turn budget exhausted"}},
		{name: "validator rework bounds long summary and retains criteria", change: &runpkg.NativeChange{Changed: true, ChangeID: "change", VersionID: "version", HeadSHA: head, Validator: &gate.ValidatorResult{Verdict: "rework", Summary: strings.Repeat("界", workpad.MaxFinalSummaryBytes), NotVerified: []string{"Reject invalid input"}}}, states: customRework, reworkState: "Fixing", humanReview: &no, wantState: "Fixing", wantComment: "validator requested changes", laneReason: "validator_rework", details: []string{"Reject invalid input", "…"}},
		{name: "validator rework without findings explains gate", change: &runpkg.NativeChange{Changed: true, ChangeID: "change", VersionID: "version", HeadSHA: head, Validator: &gate.ValidatorResult{Verdict: "rework"}}, states: unfinishedWorkflow, finalMessage: unfinishedReport, humanReview: &no, wantState: "Rework", wantComment: "validator requested changes", laneReason: "validator_rework", details: []string{"The validator requested changes"}},
		{name: "source review rework routes to Rework", diffStats: DiffStats{Status: "clean", HeadSHA: head, TreeSHA: "changed-tree"}, change: &runpkg.NativeChange{Changed: true, ChangeID: "change", VersionID: "version", HeadSHA: head, ImplementationReview: &gate.ValidatorResult{Verdict: "rework"}}, states: unfinishedWorkflow, humanReview: &no, wantState: "Rework"},
		{name: "acknowledged current source rework outranks summary evidence error", change: &runpkg.NativeChange{Changed: true, ChangeID: "change", VersionID: "version", HeadSHA: head, ImplementationReview: &gate.ValidatorResult{VersionID: "version", HeadSHA: head, Verdict: "rework"}, ImplementationReviewError: "summary unavailable"}, states: unfinishedWorkflow, humanReview: &no, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantState: "Rework"},
		{name: "source review wait retains completion ownership", change: sourceWait, states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantDeferred: true, roundTrip: true},
		{name: "source review producer failure resumes from retained completion", lifecycle: "review_source", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, ImplementationReviewError: "required source review backend unavailable"}, states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", runErr: runpkg.NewSSHRunResponse(runpkg.RunResult{}, &runpkg.NativeImplementationReviewError{Err: errors.New("required source review backend unavailable")}).Err(), wantDeferred: true, roundTrip: true},
		{name: "source review evidence hold publication refusal retains completion", change: sourceWait, states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", updateErr: errors.New("tracker unavailable"), wantDeferred: true},
		{name: "source review evidence hold needs an executable recovery destination", change: sourceWait, states: append(append([]connector.WorkflowState(nil), unfinishedWorkflow[:len(unfinishedWorkflow)-2]...), connector.WorkflowState{Name: "Blocked"}, unfinishedWorkflow[len(unfinishedWorkflow)-1]), sourceState: "Rework", humanReview: &no, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantDeferred: true},
		{name: "source review wait respects custom Rework", change: sourceWait, states: customRework, reworkState: "Fixing", humanReview: &no, wantDeferred: true},
		{name: "source review wait with disabled Rework settles", change: sourceWait, states: noDispatchRework, humanReview: &no, wantDeferred: true},
		{name: "source review wait without Rework settles", change: sourceWait, states: fourLanes, humanReview: &no, wantDeferred: true},
		{name: "source review wait retains human review lane", change: sourceWait, states: unfinishedWorkflow, wantState: "In Review"},
		{name: "source review wait without review destination defers", change: sourceWait, states: hosted, wantDeferred: true},
		{name: "source review wait preserves prerequisite authority", change: sourceWait, states: unfinishedWorkflow, humanReview: &no, finalMessage: prerequisiteReport, wantState: "Rework", wantComment: "prerequisite must finish"},
		{name: "source review wait preserves human action", change: sourceWait, states: unfinishedWorkflow, humanReview: &no, finalMessage: strings.ReplaceAll(unfinishedReport, "human_action: null", "human_action: Approve the consumer rollout"), wantState: "Blocked", wantComment: "issue acceptance is not recorded"},
		{name: "source review rework preserves prerequisite authority", change: &runpkg.NativeChange{Changed: true, ChangeID: "change", VersionID: "version", HeadSHA: head, ImplementationReview: &gate.ValidatorResult{Verdict: "rework"}}, states: unfinishedWorkflow, humanReview: &no, finalMessage: prerequisiteReport, wantState: "Rework", wantComment: "prerequisite must finish"},
		{name: "source review rework preserves human action", change: &runpkg.NativeChange{Changed: true, ChangeID: "change", VersionID: "version", HeadSHA: head, ImplementationReview: &gate.ValidatorResult{Verdict: "rework"}}, states: unfinishedWorkflow, humanReview: &no, finalMessage: strings.ReplaceAll(unfinishedReport, "human_action: null", "human_action: Approve the consumer rollout"), wantState: "Blocked", wantComment: "issue acceptance is not recorded"},
		{name: "reviewed validator rework cannot land", change: &runpkg.NativeChange{Changed: true, ChangeID: "change", VersionID: "version", HeadSHA: head, Reviewed: true, Validator: &gate.ValidatorResult{Verdict: "rework", Summary: "Missing validation evidence"}}, states: unfinishedWorkflow, humanReview: &no, wantState: "Rework", wantComment: "validator requested changes", laneReason: "validator_rework", details: []string{"Missing validation evidence"}},
		{name: "validator rework preserves prerequisite authority", change: &runpkg.NativeChange{Changed: true, ChangeID: "change", VersionID: "version", HeadSHA: head, Validator: &gate.ValidatorResult{Verdict: "rework"}}, states: unfinishedWorkflow, humanReview: &no, finalMessage: prerequisiteReport, wantState: "Rework", wantComment: "prerequisite must finish"},
		{name: "validator rework preserves reported human action", change: &runpkg.NativeChange{Changed: true, ChangeID: "change", VersionID: "version", HeadSHA: head, Validator: &gate.ValidatorResult{Verdict: "rework"}}, states: unfinishedWorkflow, humanReview: &no, finalMessage: strings.ReplaceAll(unfinishedReport, "human_action: null", "human_action: Approve the consumer rollout"), wantState: "Blocked", wantComment: "issue acceptance is not recorded"},
		{name: "validated optout remains in human review", change: accepted, states: landing, reviewed: &yes, optout: true, humanReview: &no, wantState: "In Review"},
		{name: "terminal human question keeps action reason", change: &runpkg.NativeChange{}, states: workflow, finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Choose the storage architecture\n```", wantHuman: true, wantAction: "Choose the storage architecture", roundTrip: true},
		{name: "permission wording in structured human action keeps action reason", change: &runpkg.NativeChange{}, states: workflow, finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: May I implement the storage architecture?\n```", wantHuman: true, wantAction: "May I implement the storage architecture?"},
		{name: "implementation permission retains permission reason", change: &runpkg.NativeChange{}, states: workflow, finalMessage: "May I implement the assigned fix?", wantHuman: true, wantWaitKind: "implementation_permission"},
		{name: "unchanged work with final approval question needs human", change: &runpkg.NativeChange{}, states: workflow, finalMessage: "May I merge?", wantHuman: true},
		{name: "unchanged work with final structured blocker needs human", change: &runpkg.NativeChange{}, states: workflow, finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve the migration\n```", wantHuman: true, wantAction: "Approve the migration"},
		{name: "human attention without a produced change", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Choose the storage architecture\n```", wantAction: "Choose the storage architecture", wantHuman: true},
		{name: "human attention without final text", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, wantContinue: true, noUsage: true},
		{name: "human attention with synthetic unchanged change", change: &runpkg.NativeChange{}, states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve the migration\n```", wantAction: "Approve the migration", wantHuman: true},
		{name: "human attention with failed producer and no change", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve the migration\n```", wantAction: "Approve the migration", runErr: deliveryErr, wantHuman: true},
		{name: "human attention retains instance workspace failure", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", runErr: errors.Join(deliveryErr, runpkg.ErrWorkspacePreparation), wantContinue: true},
		{name: "human attention retains checkpoint failure", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", runErr: errors.Join(deliveryErr, errors.New("checkpoint persistence failed")), wantState: "In Review"},
		{name: "human attention retains lease failure", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", runErr: errors.Join(deliveryErr, errors.New("native execution lease lost")), wantState: "In Review"},
		{name: "human attention retains session failure", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", runErr: errors.Join(deliveryErr, errors.New("session persistence failed")), wantState: "In Review"},
		{name: "human attention defers a refused lane write", states: workflow, finalMessage: "May I merge?", updateErr: errors.New("stale fencing token"), wantDeferred: true},
		{name: "unchanged inspection cannot complete on a provider claim", change: &runpkg.NativeChange{BaseSHA: head}, states: workflow, finalMessage: "Inspection verified the requested behavior.\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantState: "In Review", wantComment: "issue acceptance is not recorded"},
		{name: "NoWorkProduct complete claim without delivery retries implementation", change: &runpkg.NativeChange{BaseSHA: head}, states: unfinishedWorkflow, humanReview: &no, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantSettled: true, wantContinue: true, wantState: "Todo"},
		{name: "native26 missing acceptance report preserves review", change: &runpkg.NativeChange{BaseSHA: head, HeadSHA: head}, states: workflow, finalMessage: "Full parity remains blocked. No source changes were made or staged.\n\n- Five capabilities remain pending: credit checkout, auto-funding, invitation grants, invitation editing, and resend.\n- Inventory validation fails on 37 uncovered and 13 stale source sites, including Cloud attachments.\n- Focused MCP, operator, and dashboard regressions passed. Hosted tests were blocked by sandbox loopback restrictions.\n- Node 24 make app and the configured true gate passed.\n\nThe native owner needs focused follow-ups for these gaps before final parent conformance can pass.", wantState: "In Review", wantComment: "no valid complete detent-status disposition", roundTrip: true},
		{name: "empty legacy unchanged report preserves review", change: &runpkg.NativeChange{BaseSHA: head}, states: workflow, wantState: "In Review", wantComment: "issue acceptance is not recorded"},
		{name: "typed unfinished unchanged work preserves review", change: &runpkg.NativeChange{BaseSHA: head}, states: workflow, finalMessage: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```", wantState: "In Review", wantComment: "detent-status in_progress"},
		{name: "native272 clean instance report reuses completion owner", change: &runpkg.NativeChange{BaseSHA: head, HeadSHA: head}, states: blockedLanding, humanReview: &no, finalMessage: instanceReport, wantInstance: true, roundTrip: true},
		{name: "native instance report preserves authentic published source", change: accepted, states: unfinishedWorkflow, sourceState: "Rework", finalMessage: instanceReport, wantInstance: true},
		{name: "native instance report retains publication refusal with instance owner", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionError: "policy mismatch"}, states: blockedLanding, humanReview: &no, finalMessage: instanceReport, wantInstance: true, roundTrip: true},
		{name: "native273 malformed predicate settles without acceptance with disabled review", change: &runpkg.NativeChange{BaseSHA: head}, states: blockedLanding, humanReview: &no, finalMessage: strings.Replace(instanceReport, "    reason:", "    predicate: instance_available\n    reason:", 1), wantSettled: true, wantContinue: true, wantState: "Todo"},
		{name: "instance and human action retain human owner", change: accepted, states: blockedLanding, humanReview: &no, finalMessage: strings.Replace(instanceReport, "human_action: null", "human_action: Approve the exception", 1), wantState: "Blocked", wantHuman: true, wantAction: "Approve the exception"},
		{name: "instance and external blockers settle without acceptance with disabled review", change: accepted, states: blockedLanding, humanReview: &no, finalMessage: strings.Replace(instanceReport, "human_action: null", "  - ref: '#42'\n    reason: Await dependency\nhuman_action: null", 1), wantSettled: true},
		{name: "malformed status report cannot accept unchanged work", change: &runpkg.NativeChange{BaseSHA: head}, lifecycle: "settle", states: fourLanes, humanReview: &no, roundTrip: true, finalMessage: "```detent-status\nschema: 99\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantSettled: true, wantContinue: true, wantState: "Todo"},
		{name: "ordinary code fence cannot accept unchanged work", change: &runpkg.NativeChange{BaseSHA: head}, states: workflow, finalMessage: "```text\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantState: "In Review", wantComment: "no valid complete detent-status disposition"},
		{name: "current typed claim still requires delivery evidence", change: &runpkg.NativeChange{BaseSHA: head}, states: workflow, finalMessage: "The previous run was blocked; inspection now verifies all acceptance.\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantState: "In Review", wantComment: "issue acceptance is not recorded"},
		{name: "native26 unfinished reviewed source continues implementation", progressCycles: true, change: accepted, states: unfinishedWorkflow, humanReview: &no, finalMessage: unfinishedReport, wantState: "Rework", wantComment: "implementation remains unfinished", roundTrip: true},
		{name: "unfinished source continues before native review", progressCycles: true, change: waiting, states: unfinishedWorkflow, finalMessage: unfinishedReport, wantState: "Rework", wantComment: "issue acceptance is not recorded"},
		{name: "unfinished unchanged turn uses allowed implementation lane", change: &runpkg.NativeChange{BaseSHA: head}, states: unfinishedWorkflow, finalMessage: unfinishedReport, wantState: "Rework", wantComment: "further work"},
		{name: "unfinished Rework stays runnable without a self transition", progressCycles: true, change: accepted, states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, finalMessage: unfinishedReport, wantState: "Rework", wantSameState: true, wantComment: "implementation remains unfinished"},
		{name: "unfinished source respects configured rework name", change: accepted, states: customRework, reworkState: "Fixing", humanReview: &no, finalMessage: unfinishedReport, wantState: "Fixing", wantComment: "implementation remains unfinished"},
		{name: "disabled Rework settles without acceptance with disabled review", change: accepted, states: noDispatchRework, humanReview: &no, finalMessage: unfinishedReport, wantSettled: true},
		{name: "operator only Rework settles without acceptance with disabled review", change: accepted, states: operatorRework, humanReview: &no, finalMessage: unfinishedReport, wantSettled: true},
		{name: "disallowed Rework keeps configured review", change: accepted, states: rework, finalMessage: unfinishedReport, wantState: "In Review", wantComment: "issue acceptance is not recorded"},
		{name: "in progress external blocker cannot dispatch Rework", change: accepted, states: unfinishedWorkflow, humanReview: &no, finalMessage: strings.ReplaceAll(unfinishedReport, "blockers: []", "blockers:\n  - ref: '#42'\n    reason: Await the dependent project"), wantSettled: true},
		{name: "explicit external blocked report cannot dispatch Rework", change: accepted, states: unfinishedWorkflow, humanReview: &no, finalMessage: strings.ReplaceAll(unfinishedReport, "status: in_progress\nblockers: []", "status: blocked\nblockers:\n  - reason: Await external acceptance"), wantSettled: true},
		{name: "in progress human action remains held", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", Reviewed: true, HeadSHA: head, VersionError: "head mismatch"}, states: unfinishedWorkflow, humanReview: &no, finalMessage: strings.ReplaceAll(unfinishedReport, "human_action: null", "human_action: Approve the consumer rollout"), wantState: "Blocked", wantComment: "issue acceptance is not recorded"},
		{name: "unfinished policy refusal settles with source owner without relaning", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, VersionError: "policy mismatch", VersionCode: "policy_mismatch"}, states: unfinishedWorkflow, humanReview: &no, finalMessage: unfinishedReport, wantSettled: true, roundTrip: true},
		{name: "unfinished refused Rework retains authentic source owner without relaning", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionError: "policy mismatch", VersionCode: "policy_mismatch"}, states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, finalMessage: unfinishedReport, wantSettled: true},
		{name: "typed unfinished published source cannot auto land", change: accepted, states: landing, finalMessage: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```", wantState: "In Review", wantComment: "detent-status in_progress"},
		{name: "malformed published report cannot auto land", change: accepted, states: landing, finalMessage: "```detent-status\nschema: 99\nstatus: complete\n```", wantState: "In Review", wantComment: "no valid complete detent-status disposition"},
		{name: "typed accepted current version lands normally", change: accepted, states: landing, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantState: "Merging", wantComment: "runner lands it next"},
		{name: "completed reviewed Rework reaches landing", change: accepted, states: rework, sourceState: "Rework", humanReview: &no, reviewed: &yes, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantState: "Merging", wantComment: "runner lands it next"},
		{name: "unchanged Rework after a review-conversation refusal goes to human review", change: accepted, states: rework, sourceState: "Rework", reviewed: &yes, recovery: refusedVersion("version_1"), wantState: "Rework", wantSameState: true, wantComment: "resolved none of the review conversations"},
		{name: "unchanged Rework after a review-conversation refusal holds without human review", retryCycles: true, change: accepted, states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, reviewed: &yes, recovery: refusedVersion("version_1"), wantState: "Rework", wantSameState: true, wantComment: "internal/app.go:12"},
		{name: "opted-out unchanged Rework after a review-conversation refusal keeps its review lane", change: accepted, states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, optout: true, reviewed: &yes, recovery: refusedVersion("version_1"), wantState: "Rework", wantSameState: true, wantComment: "resolved none of the review conversations"},
		{name: "Rework whose refused conversations are cleared lands", change: accepted, states: rework, sourceState: "Rework", humanReview: &no, reviewed: &yes, recovery: refusedVersion("version_1"), cleared: true, wantState: "Merging", wantComment: "runner lands it next. " + workspace.ReviewConversationsResolvedText},
		{name: "unreadable review conversation state retries as an instance failure", change: accepted, states: rework, sourceState: "Rework", reviewed: &yes, recovery: refusedVersion("version_1"), runErr: fmt.Errorf("%w: bad gateway", runpkg.ErrReviewConversationsUnavailable), turnStarted: true, wantSameState: true, wantContinue: true},
		{name: "Rework that published a new version after the refusal lands", change: accepted, states: rework, sourceState: "Rework", humanReview: &no, reviewed: &yes, recovery: refusedVersion("version_0"), wantState: "Merging", wantComment: "runner lands it next"},
		{name: "refused completed Rework landing retains completion ownership", change: accepted, states: rework, sourceState: "Rework", humanReview: &no, reviewed: &yes, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", updateErr: errors.New("stale fencing token"), wantState: "Merging", wantDeferred: true, roundTrip: true},
		{name: "publication outage retries current publisher", reviewed: &yes, republish: true, change: &runpkg.NativeChange{Error: "hub unavailable"}, states: landing, wantDeferred: true},
		{name: "an unopened change is handed off, not reviewed", change: &runpkg.NativeChange{Changed: true, Error: "hub unavailable", HeadSHA: head, Files: 1}, states: workflow, wantDeferred: true},
		{name: "a workflow without the review lane is handed off, never ended", change: waiting, states: hosted, wantDeferred: true},
		{name: "a workflow without a terminal move is handed off", change: &runpkg.NativeChange{}, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", states: []connector.WorkflowState{{Name: "In Progress", Dispatchable: true, Transitions: []string{"Blocked"}}, {Name: "Blocked"}}, wantDeferred: true},
		{name: "an unreadable workflow is handed off", change: waiting, statesErr: errors.New("hub unavailable"), wantDeferred: true},
		{name: "a refused lane write is handed off", change: waiting, states: workflow, updateErr: errors.New("stale fencing token"), wantDeferred: true},
		{name: "missing native result keeps ordinary continuation", states: workflow, wantOrdinary: true, wantContinue: true},
		{name: "dirty1067 prerequisite report retains native blocked handoff", states: workflow, finalMessage: prerequisiteReport, wantState: "Todo", wantComment: "detent-status blocked", diffStats: DiffStats{Status: "changed", FilesChanged: 2, TrackedPaths: []string{"inventory.go", "inventory_test.go"}}},
		{name: "clean prerequisite report retains the same blocked handoff", change: &runpkg.NativeChange{}, states: workflow, finalMessage: prerequisiteReport, wantState: "Todo", wantComment: "detent-status blocked"},
		{name: "terminal prerequisite is evaluated before blocked write", states: workflow, finalMessage: prerequisiteReport, blockerState: "Done", wantState: "Todo", wantComment: "detent-status blocked"},
		{name: "unfinished prerequisite still writes blocked", states: workflow, finalMessage: prerequisiteReport, blockerState: "In Progress", wantState: "Todo", wantComment: "detent-status blocked"},
		{name: "interrupted attempt then satisfied prerequisite does not reapply blocked", states: workflow, humanReview: &no, finalMessage: prerequisiteReport, blockerState: "Done", interrupted: true, wantState: "Todo", wantComment: "detent-status blocked"},
		{name: "interrupted attempt then unmet prerequisite retains blocked", states: workflow, humanReview: &no, finalMessage: prerequisiteReport, blockerState: "In Progress", interrupted: true, wantState: "Todo", wantComment: "detent-status blocked"},
		{name: "prerequisite handoff respects a workflow without Blocked", states: fourLanes, finalMessage: prerequisiteReport, wantState: "Todo"},
		{name: "prerequisite retains blocked owner and publication refusal", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionError: "policy mismatch"}, states: workflow, finalMessage: prerequisiteReport, humanReview: &no, wantState: "Todo", wantComment: "policy mismatch", roundTrip: true},
		{name: "no review Rework prerequisite retains readiness owner despite refusal", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", Reviewed: true, VersionError: "unrelated source provenance", VersionCode: "invalid_request"}, states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, finalMessage: prerequisiteReport, wantState: "Rework", wantSameState: true, wantComment: "unrelated source provenance"},
		{name: "human hold retains authority despite publication refusal", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionError: "policy mismatch"}, states: blockedLanding, humanReview: &no, finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve the migration\n```", wantHuman: true, wantAction: "Approve the migration"},
		{name: "malformed dirty prerequisite retains ordinary path", states: workflow, finalMessage: strings.Replace(prerequisiteReport, "schema: 1", "schema: 99", 1), wantOrdinary: true, wantContinue: true},
		{name: "human owned dirty prerequisite retains ordinary path", states: workflow, finalMessage: strings.Replace(prerequisiteReport, "    reason:", "    owner: human\n    reason:", 1), wantOrdinary: true, wantContinue: true},
		{name: "dirty tracked native Code keeps ordinary continuation", states: workflow, wantOrdinary: true, wantContinue: true, diffStats: DiffStats{Status: "changed", FilesChanged: 1, TrackedPaths: []string{"source.go"}}},
		{name: "dirty untracked native Rework keeps ordinary continuation", states: rework, sourceState: "Rework", wantOrdinary: true, wantContinue: true, diffStats: DiffStats{Status: "changed", FilesChanged: 1, UntrackedPaths: []string{"source.go"}}},
		{name: "late host source conflict preserves progress", states: rework, sourceState: "Rework", wantOrdinary: true, wantContinue: true, diffStats: DiffStats{Status: "changed", FilesChanged: 1, TrackedPaths: []string{"docs/invariants.md"}, TreeSHA: "committed-tree", Fingerprint: "late-host-conflict", RecoveryStateExpected: true, RecoveryStateAvailable: true}},
		{name: "late host conflict after replay preserves unpublished progress", states: rework, sourceState: "Rework", wantOrdinary: true, wantContinue: true, diffStats: DiffStats{Status: "changed", FilesChanged: 1, TrackedPaths: []string{"docs/invariants.md"}, UnpushedCommits: 1, TreeSHA: "committed-tree", Fingerprint: "late-host-conflict", RecoveryStateExpected: true, RecoveryStateAvailable: true}},
		{name: "already paused conflict left unresolved remains failure", states: rework, sourceState: "Rework", runErr: fmt.Errorf("finalize native work: %w: unresolved source conflicts: docs/invariants.md", workspace.ErrMergeResolutionInvalid), wantState: "In Review", diffStats: DiffStats{Status: "changed", FilesChanged: 1, TrackedPaths: []string{"docs/invariants.md"}, Fingerprint: "existing-conflict", RecoveryStateExpected: true, RecoveryStateAvailable: true}},
		{name: "non-native final question keeps the ordinary path", finalMessage: "May I merge?", plain: true, wantContinue: true},
		{name: "a connector without a workflow keeps the ordinary path", change: &runpkg.NativeChange{}, plain: true, wantContinue: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !test.wantDirect {
				t.Parallel()
			}
			issue := completionTransitionIssue(firstNonBlank(test.sourceState, "In Progress"), "")
			if test.releaseDelivery {
				issue.PRRepository = "https://github.com/example-org/example-project"
				issue.BranchName = "operator/release-pick"
				issue.PullRequest = &connector.PullRequest{Number: 7, State: "OPEN", BranchName: issue.BranchName, HeadSHA: head, BaseRef: "release/v1.2", BaseSHA: strings.Repeat("a", 40)}
			}
			if test.blockerState != "" {
				issue.DependencySource = connector.BlockedRefSourceNative
				issue.Metadata = map[string]string{"hub_profile": "native", "hub_revision": "6"}
			}
			if test.optout {
				issue.Labels = append(issue.Labels, "requires-human-review")
			}
			tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}, updateErr: test.updateErr}
			if test.handoffState != "" {
				tick.stateIssues[0].State = test.handoffState
			}
			if test.blockerState != "" {
				tick.resolvedIssues = []connector.Issue{{ID: "prerequisite", Identifier: "prj_6d4919bebd73446798e6cd807feda10e#411", State: test.blockerState, Closed: test.blockerState == "Done"}}
			}
			var tracker connector.Connector = &nativeWorkflowConnector{autoPromoteTickConnector: tick, states: test.states, statesErr: test.statesErr, reviewed: test.reviewed, reviewState: test.reviewState}
			if test.plain {
				tracker = tick
			}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}, TerminalStates: []string{"Done"}})
			if (test.wantInstance || test.wantDeferred) && len(test.states) > 0 && !dispatchableState(test.states, "In Progress") {
				cfg.ActiveStates = []string{"Todo", "Rework"}
			}
			if test.handoffState != "" {
				cfg.ActiveStates = append(cfg.ActiveStates, test.handoffState)
			}
			if test.delegated {
				cfg.IssueContract = &workflowconfig.IssueContract{Context: "## Issue Contract\n- Acceptance criteria\n\nAgent-authored criteria do not require human confirmation.\n"}
			}
			cfg.AutoPromote.SourceState = "In Review"
			cfg.AutoPromote.HumanReview = test.humanReview
			if test.reworkState != "" {
				cfg.AutoPromote.ReworkState = test.reworkState
			}
			attempts := &recordingWorkAttemptStore{}
			scheduling := &nativeCompletionScheduling{hubSchedulingSource: &hubSchedulingSource{}}
			publisher := &nativeCompletionPublisher{nativeLandingJourneyExecution: nativeLandingJourneyExecution{}, change: accepted}
			if test.republish {
				scheduling.execution = publisher
				if test.lifecycle == "refusal" {
					publisher.change = test.change
				}
			}
			scheduling.release = func() {
				for _, update := range tick.updates {
					if test.wantInstance && update.state == "Blocked" {
						t.Fatalf("instance claim released after a Blocked handoff: %v", tick.updates)
					}
				}
				if test.wantState != "" && !test.wantSameState && !test.wantDirect && (len(tick.updates) != 1 || tick.updates[0].state != test.wantState) {
					t.Fatalf("claim released before lane settlement: updates=%v, want=%s", tick.updates, test.wantState)
				}
			}
			metrics := &workflowMetricsRecorderSpy{}
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, scheduling: scheduling, workflowMetrics: metrics}
			if test.machineAttempt > 0 {
				db, err := store.Open(t.Context(), store.Config{Backend: store.BackendSQLite, Path: filepath.Join(t.TempDir(), "retry.db")})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				orch.workflowMetrics = db
			}
			if native, ok := tracker.(*nativeWorkflowConnector); ok && test.reviewed == nil && test.change != nil {
				native.reviewed = &test.change.Reviewed
			}
			var journey *nativeLandingJourney
			landingGate := &countingProjectDispatchGate{}
			if test.wantDirect {
				journey = newNativeLandingJourney(t, issue, "", 200, false, false)
				var err error
				orch.supervisor, err = runpkg.NewSupervisor(journey.runner, runpkg.SupervisorConfig{})
				if err != nil {
					t.Fatal(err)
				}
				orch.runResults = make(chan runpkg.Completion, 1)
				orch.globalDispatchGate = landingGate
				scheduling.execution = journey.execution
			}
			state := newState(cfg)
			state.Draining = test.draining
			now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
			if test.quotaWait {
				orch.setGitHubRESTCapacityOutage(&state, githubRESTBudgetEvidence{Consumer: "worker", CredentialIdentity: "runner", RateLimitKind: "primary_exhausted", ObservedAt: now, ResetAt: now.Add(time.Hour)}, now)
			}
			mode := firstNonBlank(test.mode, runpkg.RunModeImplement)
			runningTokens := TokenTotals{TotalTokens: 42}
			if test.preProvider {
				runningTokens = TokenTotals{}
			}
			state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, WorkAttemptID: 42, Generation: 7, SessionID: "native-session", Tokens: runningTokens, Mode: mode, DispatchSourceState: issue.State, DispatchTargetState: issue.State, StartedAt: now.Add(-time.Minute)}
			if test.diffStats.TreeSHA != "" {
				running := state.Running[issue.ID]
				running.CompletionSourceStart = completionSourceStartRecord{Captured: true, Fingerprint: completionSourceFingerprint{WorkspaceTree: "dispatch-tree"}}
				state.Running[issue.ID] = running
			}
			if test.startHead != "" {
				running := state.Running[issue.ID]
				running.CompletionSourceStart = completionSourceStartRecord{Captured: true, WorkspaceHeadAvailable: true, Fingerprint: completionSourceFingerprint{WorkspaceHead: test.startHead}}
				state.Running[issue.ID] = running
			}
			if test.machineAttempt > 0 {
				running := state.Running[issue.ID]
				running.Attempt = test.machineAttempt
				state.Running[issue.ID] = running
			}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
			if test.interrupted {
				previous := cloneIssue(issue)
				previous.WorkpadSignal, _ = workpad.SignalFromComment(prerequisiteReport, "", "")
				previous.Comments = []connector.IssueComment{{Body: "## Codex Workpad\n\n" + prerequisiteReport}}
				running := state.Running[issue.ID]
				running.Issue = previous
				interrupted := newState(cfg)
				interrupted.Running[issue.ID] = running
				interrupted.Claimed[issue.ID] = state.Claimed[issue.ID]
				settlement := *scheduling
				settlement.hubSchedulingSource = &hubSchedulingSource{}
				settlement.release = nil
				owner := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: &recordingWorkAttemptStore{}, scheduling: &settlement}
				event := runpkg.Completion{IssueID: issue.ID, Err: context.Canceled, CompletedAt: now, Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement}, Result: runpkg.RunResult{TurnStarted: true}}
				if !owner.completeNativeChangeRun(t.Context(), &interrupted, event, running, runpkg.FinalStateFailed) || len(tick.updates) != 1 || tick.updates[0].state != "Todo" {
					t.Fatalf("interrupted attempt did not return to Todo: %v", tick.updates)
				}
				tick.updates = nil
				tick.stateIssues[0] = issue
				tracker.(*nativeWorkflowConnector).reasons = nil
				tracker.(*nativeWorkflowConnector).reasonCodes = nil
				state.Running[issue.ID] = running
			}
			finalState := test.finalState
			if finalState == "" {
				finalState = FinalStateCompleted
			}
			tokens := TokenTotals{TotalTokens: 42}
			if test.noUsage || test.preProvider {
				tokens = TokenTotals{}
			}
			diffStats := test.diffStats
			if !diffStatsPresent(diffStats) {
				diffStats = DiffStats{Status: "clean", HeadSHA: head}
			}
			var advance func()
			if strings.HasPrefix(test.lifecycle, "review_") {
				orch.now = func() time.Time { return now }
			}
			if test.lifecycle != "" && !strings.HasPrefix(test.lifecycle, "review_") {
				cfg.Claiming.LeaseTTL = 90 * time.Second
				cfg.Claiming.HeartbeatInterval = 30 * time.Second
				orch.cfg = cfg
				orch.now = func() time.Time { return now }
				orch.heartbeats = newHeartbeatManager(cfg, tracker, attempts, orch.now, nil, scheduling)
				expires := now.Add(cfg.Claiming.LeaseTTL)
				released := false
				scheduling.renew = func() (Claimed, error) {
					if released || !now.Before(expires) {
						return Claimed{}, ErrSchedulingClaimLost
					}
					expires = now.Add(cfg.Claiming.LeaseTTL)
					return Claimed{Issue: issue, Owner: "machine-a", LeaseRenewedAt: now, LeaseExpiresAt: expires}, nil
				}
				running := state.Running[issue.ID]
				running.WorkerProcess = startHeartbeatWorkerProcess(t, true)
				running.progress = newWorkerProgress(running, store.WorkAttemptHeartbeat{AttemptID: 42}, attempts, 1024)
				state.Running[issue.ID] = running
				state.Claimed[issue.ID] = Claimed{Issue: issue, Owner: "machine-a", LeaseExpiresAt: expires}
				orch.trackRunningHeartbeat(&state, running, state.Claimed[issue.ID], now)
				advance = func() {
					for range 7 {
						now = now.Add(cfg.Claiming.HeartbeatInterval)
						due := orch.heartbeats.due(now)
						if len(due) != 1 {
							t.Fatalf("host completion lost heartbeat owner: %d targets", len(due))
						}
						orch.heartbeats.execute(t.Context(), due[0])
						result := <-orch.heartbeats.results
						if !result.claimRenewed || !result.workAttemptRenewed || !result.workerChecked || result.workerAlive || result.heartbeat.AttemptID != 42 {
							t.Fatalf("exited provider lost existing ownership or appeared healthy: %#v", result)
						}
						orch.handleHeartbeatResult(&state, result)
					}
				}
				advance()
				tracker.(*nativeWorkflowConnector).beforeStates = advance
				publisher.prepare = advance
				release := scheduling.release
				scheduling.release = func() {
					advance()
					release()
					if !now.Before(expires) {
						t.Fatal("completion released an expired existing lease")
					}
					released = scheduling.releaseError == nil
				}
				if test.lifecycle == "release" {
					scheduling.releaseError = errors.Join(ErrSchedulingUnavailable, errors.New("native Finish publication unavailable"))
				}
				if test.lifecycle == "cancel" {
					orch.cancelRunning(&state, issue.ID, "test.cancellation")
					if len(orch.heartbeats.due(now.Add(time.Hour))) != 0 {
						t.Fatal("true cancellation retained renewal ownership")
					}
					tracker.(*nativeWorkflowConnector).beforeStates = nil
					scheduling.release = release
				}
			}
			event := runpkg.Completion{
				IssueID: issue.ID, CompletedAt: now, Err: test.runErr,
				Request: runpkg.RunRequest{Mode: mode, WorkAttemptID: 42, Generation: 7},
				Result:  runpkg.RunResult{FinalState: finalState, FinalMessage: test.finalMessage, NativeChange: test.change, Tokens: tokens, DiffStats: diffStats, TurnStarted: test.turnStarted || test.wantInstance || test.wantSettled || test.diffStats.Fingerprint != "", ReviewConversationsCleared: test.cleared},
			}
			if test.reusedAttempt != "" {
				copy := *event.Result.NativeChange
				copy.ReusedDispositionAttemptID = test.reusedAttempt
				event.Result.NativeChange = &copy
			}
			if test.recovery != nil {
				event.Request.Execution = &nativeLandingJourneyExecution{recovery: *test.recovery}
			}
			if test.wantDirect {
				change := *test.change
				change.HeadSHA = journey.target.HeadSHA
				event.Result.NativeChange = &change
				event.Result.TokenUSD = 0.25
				event.Request.OnActivityUpdate = func(runpkg.AgentActivityUpdate) error { return context.Canceled }
			}
			if test.roundTrip {
				data, err := json.Marshal(newDeferredCompletion(event, state.Running[issue.ID], errors.New("lane unavailable"), now))
				if err != nil {
					t.Fatal(err)
				}
				var restored deferredCompletion
				if err := json.Unmarshal(data, &restored); err != nil {
					t.Fatal(err)
				}
				event = restored.completion()
			}
			if test.wantOrdinary && orch.completeNativeChangeRun(t.Context(), &state, event, state.Running[issue.ID], finalState) {
				t.Fatal("nil native result bypassed ordinary continuation ownership")
			}
			orch.handleRunResult(t.Context(), &state, event)
			if change := event.Result.NativeChange; change != nil && change.VersionID != "" {
				if native, ok := tracker.(*nativeWorkflowConnector); ok {
					for _, reason := range native.reasonCodes {
						if reason == terminalAttemptWithoutWorkProductReason {
							t.Fatal("published version was classified as without work product")
						}
					}
				}
				for _, receipt := range attempts.completions {
					if receipt.ErrorClass == terminalAttemptWithoutWorkProductReason {
						t.Fatal("published version receipt was classified as without work product")
					}
				}
			}
			if test.machineAttempt > 0 {
				wantDelay := map[int]time.Duration{3: 80 * time.Second, 4: 160 * time.Second, 5: 300 * time.Second, 8: 300 * time.Second}[test.machineAttempt]
				assertCompletionMachineRetry(t, orch, state, issue.ID, event.CompletedAt, test.machineAttempt+1, wantDelay)
				if scheduling.releases != 1 {
					t.Fatalf("lease releases = %d, want 1", scheduling.releases)
				}
			}
			if strings.HasPrefix(test.lifecycle, "review_") {
				if len(state.deferredCompletions) != 1 || !state.Retry[issue.ID].CompletionDeferred || scheduling.releases != 0 || len(attempts.completions) != 0 || tick.stateIssues[0].State != issue.State {
					t.Fatalf("published unreviewed version lost completion ownership: deferred=%d releases=%d completions=%d lane=%s", len(state.deferredCompletions), scheduling.releases, len(attempts.completions), tick.stateIssues[0].State)
				}
				native := tracker.(*nativeWorkflowConnector)
				for range 3 {
					now = now.Add(time.Minute)
					if orch.retryDeferredCompletions(t.Context(), &state, now) {
						t.Fatal("unreviewed version settled")
					}
					if reason := knownDispatchWaitReason(issue, &state, nil, orch.cfg.TerminalStates); reason != dispatchSkipRetryPending {
						t.Fatalf("review wait allowed implementation: %q", reason)
					}
				}
				last := attempts.heartbeats[len(attempts.heartbeats)-1]
				if last.WaitReason != string(gate.ReasonAutomatedReviewMissing) || !strings.Contains(last.StatusMessage, "waiting on review evidence") || strings.Contains(last.StatusMessage, "without work product") {
					t.Fatalf("missing review cause: %+v", last)
				}
				record, err := decodeDeferredCompletion(store.WorkAttempt{ID: 42, WorkerMetadataJSON: last.WorkerMetadataJSON})
				if err != nil {
					t.Fatal(err)
				}
				if record.Result.NativeChange.VersionID != "version_1" || record.Result.NativeChange.HeadSHA != head {
					t.Fatal("review wait lost immutable source")
				}
				state = newState(cfg)
				orch.recoverDeferredCompletions(t.Context(), &state, []store.WorkAttempt{{ID: 42, Phase: deferredCompletionPhase, WorkerMetadataJSON: last.WorkerMetadataJSON}}, now)
				now = now.Add(time.Minute)
				native.reviewed = &yes
				expected := "Merging"
				switch test.lifecycle {
				case "review_timeout":
					native.reviewErr = context.DeadlineExceeded
					if orch.retryDeferredCompletions(t.Context(), &state, now) || scheduling.releases != 0 {
						t.Fatal("Hub timeout lost completion")
					}
					now = now.Add(time.Minute)
					native.reviewErr = nil
					tick.updateErr = context.DeadlineExceeded
					if orch.retryDeferredCompletions(t.Context(), &state, now) || scheduling.releases != 0 {
						t.Fatal("lane timeout lost completion")
					}
					now = now.Add(time.Minute)
					tick.updateErr = nil
				case "review_rework":
					native.reviewState = &connector.ChangeReview{Current: true, ChangesRequested: true}
					expected = "Rework"
				case "review_head":
					native.reviewState = &connector.ChangeReview{}
					expected = issue.State
				case "review_operator":
					tick.stateIssues[0].State = "Blocked"
					expected = "Blocked"
				case "review_expired":
					orch.scheduling = &completionRestartScheduling{hubSchedulingSource: &hubSchedulingSource{}, restoreError: runpkg.ErrExecutionAuthorityUnavailable}
					record := state.deferredCompletions[issue.ID]
					record.AuthorityRestored = false
					state.deferredCompletions[issue.ID] = record
					expected = issue.State
				}
				if !orch.retryDeferredCompletions(t.Context(), &state, now) || len(state.deferredCompletions) != 0 {
					t.Fatal("genuine review result did not settle")
				}
				orch.retryDeferredCompletions(t.Context(), &state, now.Add(time.Minute))
				if tick.stateIssues[0].State != expected || len(attempts.completions) != 1 {
					t.Fatalf("review replay: lane=%s want=%s completions=%d", tick.stateIssues[0].State, expected, len(attempts.completions))
				}
				return
			}

			if forwarded, ok := tracker.(*nativeWorkflowConnector); ok {
				for i, code := range forwarded.reasonCodes {
					if code == "" || code == "worker_progress" || strings.TrimSpace(forwarded.reasons[i]) == "" {
						t.Fatalf("native completion lost its transition reason: code=%q detail=%q", code, forwarded.reasons[i])
					}
				}
			}
			if test.preProvider {
				if len(state.Completed) != 0 || state.FailureBreaker.Count != 0 {
					t.Fatalf("unexpected state after completion: retry=%+v blocked=%+v breaker=%+v", state.Retry, state.Blocked, state.FailureBreaker)
				}
				if test.preInstance {
					retry, retried := state.Retry[issue.ID]
					if tick.stateIssues[0].State != issue.State || len(tick.updates) != 0 || len(tick.comments) != 0 || len(state.Blocked) != 0 || len(state.Claimed) != 0 || scheduling.releases != 1 || !retried || retry.Attempt != 2 || !retry.DueAt.After(now) || !dispatchableState(test.states, retry.Issue.State) || len(state.FailureBreaker.Failures) != 0 {
						t.Fatalf("pre-provider recovery did not retry the instance in its lane: updates=%v retry=%+v releases=%d", tick.updates, retry, scheduling.releases)
					}
					if len(attempts.completions) != 1 {
						t.Fatalf("instance completion receipts = %v", attempts.completions)
					}
					receipt := attempts.completions[0]
					if receipt.TerminalState != store.WorkAttemptTerminalFailure || receipt.ErrorClass != workAttemptErrorWorkspace || !strings.Contains(receipt.ErrorMessage, test.runErr.Error()) || !allowanceInfrastructureAttempt(store.WorkAttempt{TerminalState: receipt.TerminalState, ErrorClass: receipt.ErrorClass, MetricsJSON: receipt.MetricsJSON, WorkerMetadataJSON: receipt.WorkerMetadataJSON}) {
						t.Fatalf("pre-provider recovery lost infrastructure attribution: %+v", receipt)
					}
					return
				}
				if test.wantAuthority {
					if tick.stateIssues[0].State != issue.State || len(tick.comments) != 0 || len(state.deferredCompletions) != 0 || len(state.Retry) != 0 || len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalAbandoned {
						t.Fatal("lost native authority fabricated lane settlement or retained obsolete replay")
					}
					return
				}
				if test.wantDeferred {
					record, retained := state.deferredCompletions[issue.ID]
					if !retained || !record.NativeRecoveryRequired || record.Request.Mode != mode || !state.Retry[issue.ID].CompletionDeferred || scheduling.releases != 0 || len(attempts.completions) != 0 || tick.stateIssues[0].State != issue.State || len(tick.comments) != 0 {
						t.Fatal("refused native handoff lost durable refusal, authority, or lane")
					}
					return
				}
				if len(state.Retry) != 0 || scheduling.releases != 1 || len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalCancelled || attempts.completions[0].ErrorClass != workAttemptErrorWorkspace || tick.stateIssues[0].State != "Blocked" || len(tick.comments) != 0 {
					t.Fatal("permanent native refusal did not settle Blocked without coding retry")
				}
				return
			}
			if test.lifecycle == "refusal" {
				if !state.Retry[issue.ID].CompletionDeferred || scheduling.releases != 0 || len(attempts.completions) != 0 {
					t.Fatal("workflow outage lost retained refusal authority")
				}
				tracker.(*nativeWorkflowConnector).statesErr = nil
				advance()
				if !orch.retryDeferredCompletions(t.Context(), &state, now) || publisher.prepared != 1 {
					t.Fatal("unchanged settled refusal replayed publication instead of settling")
				}
				for range 3 {
					now = now.Add(cfg.Claiming.LeaseTTL)
					orch.retryDeferredCompletions(t.Context(), &state, now)
				}
				if publisher.prepared != 1 || len(state.deferredCompletions) != 0 || len(state.Retry) != 0 || len(state.Claimed) != 0 || len(orch.heartbeats.due(now)) != 0 {
					t.Fatal("settled refusal retained publication replay or lease renewal")
				}
			}
			if test.lifecycle == "release" {
				if !state.Retry[issue.ID].CompletionDeferred || len(attempts.completions) != 0 || len(state.Completed) != 0 {
					t.Fatal("release outage retired the completed source owner")
				}
				advance()
				scheduling.releaseError = nil
				if !orch.retryDeferredCompletions(t.Context(), &state, now) {
					t.Fatal("retained release did not settle")
				}
			}
			if test.lifecycle == "defer" || test.lifecycle == "lost" || test.lifecycle == "interrupted" {
				advance()
				last := attempts.heartbeats[len(attempts.heartbeats)-1]
				record, err := decodeDeferredCompletion(store.WorkAttempt{ID: 42, WorkerMetadataJSON: last.WorkerMetadataJSON})
				if err != nil || record.Result.NativeChange == nil || record.Running.Generation != 7 || record.Running.WorkAttemptID != 42 {
					t.Fatalf("renewal lost deferred source evidence: %#v, %v", record, err)
				}
				if test.lifecycle == "lost" {
					scheduling.renew = func() (Claimed, error) { return Claimed{}, ErrSchedulingClaimLost }
					now = now.Add(cfg.Claiming.HeartbeatInterval)
					due := orch.heartbeats.due(now)
					orch.heartbeats.execute(t.Context(), due[0])
					orch.handleHeartbeatResult(&state, <-orch.heartbeats.results)
					retained, ok := state.deferredCompletions[issue.ID]
					if !ok || !reflect.DeepEqual(retained.Result, record.Result) || !state.Retry[issue.ID].CompletionDeferred || len(orch.heartbeats.due(now.Add(time.Hour))) != 0 || publisher.prepared != 0 || len(attempts.completions) != 0 {
						t.Fatal("lost fence discarded deferred source or retained renewal ownership")
					}
					publisher.validationErr = runpkg.ErrExecutionAuthorityUnavailable
					scheduling.release = nil
					orch.retryDeferredCompletions(t.Context(), &state, now.Add(time.Hour))
					if len(state.deferredCompletions) != 0 || len(state.Claimed) != 0 || len(orch.heartbeats.due(now.Add(time.Hour))) != 0 || publisher.prepared != 0 || len(tick.updates) != 0 || len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalAbandoned {
						t.Fatal("lost fence revived deferred publication")
					}
					return
				}
				if test.lifecycle == "interrupted" {
					publisher.validationErr = test.validationErr
					if orch.retryDeferredCompletions(t.Context(), &state, now) {
						t.Fatal("interrupted validation settled completed provider result")
					}
					retained, ok := state.deferredCompletions[issue.ID]
					if !ok || retained.Error != "" || !reflect.DeepEqual(retained.Result, record.Result) || len(state.Claimed) != 1 || scheduling.releases != 0 || len(attempts.completions) != 0 || publisher.prepared != 0 || len(state.Running) != 0 {
						t.Fatal("interrupted validation abandoned authority or changed completed result")
					}
					last = attempts.heartbeats[len(attempts.heartbeats)-1]
					persisted, err := decodeDeferredCompletion(store.WorkAttempt{ID: 42, WorkerMetadataJSON: last.WorkerMetadataJSON})
					if err != nil || persisted.Error != "" || !reflect.DeepEqual(persisted.Result, record.Result) {
						t.Fatalf("interrupted validation lost durable completed result: %+v, %v", persisted, err)
					}
					publisher.validationErr = nil
					now = state.Retry[issue.ID].DueAt
				}
				if !orch.retryDeferredCompletions(t.Context(), &state, now) || publisher.prepared != 1 {
					t.Fatal("retained completion did not publish exactly once")
				}
				test.change = accepted
			}
			if test.lifecycle != "" {
				if len(orch.heartbeats.due(now.Add(time.Hour))) != 0 {
					t.Fatal("renewal continued after terminal release")
				}
			}
			if test.wantDirect {
				if state.Running[issue.ID].Mode != runpkg.RunModeMerge || len(tick.updates) != 0 || len(attempts.completions) != 0 || scheduling.releases != 0 || len(state.deferredCompletions) != 0 || landingGate.tryAcquireCalls != 1 {
					t.Fatalf("direct landing lost source authority: running=%v moves=%v attempts=%v releases=%d", state.Running, tick.updates, attempts.completions, scheduling.releases)
				}
				select {
				case landed := <-orch.runResults:
					if landed.Err != nil || landed.Result.NativeLanding == nil || !landed.Result.NativeLanding.Landed || journey.execution.started != 1 || len(journey.execution.recorded) != 1 || journey.provider.calls.Load() != 0 {
						t.Fatalf("direct landing failed or restarted provider: %+v, err=%v", landed.Result, landed.Err)
					}
					if nativeLandingGit(t, t.Context(), journey.remote, "rev-parse", "refs/heads/main") != landed.Result.NativeLanding.MergeSHA {
						t.Fatal("direct landing did not push the recorded merge")
					}
					tick.stateIssues[0].State = "Done"
					orch.handleRunResult(t.Context(), &state, landed)
				case <-t.Context().Done():
					t.Fatal(t.Context().Err())
				}
				if len(state.deferredCompletions) != 0 || len(state.Retry) != 0 || len(state.Running) != 0 || len(state.Claimed) != 0 || scheduling.releases != 1 || len(attempts.completions) != 1 || tick.stateIssues[0].State != "Done" {
					t.Fatalf("direct landing did not settle: deferred=%d retry=%d running=%d claims=%d releases=%d attempts=%d", len(state.deferredCompletions), len(state.Retry), len(state.Running), len(state.Claimed), scheduling.releases, len(attempts.completions))
				}
				var metrics struct {
					TokenUSD float64 `json:"token_usd"`
				}
				if err := json.Unmarshal([]byte(attempts.completions[0].MetricsJSON), &metrics); err != nil || metrics.TokenUSD != 0.25 {
					t.Fatalf("landing lost source usage: %s, %v", attempts.completions[0].MetricsJSON, err)
				}
				return
			}

			if test.wantSettled {
				if test.republish && test.runErr == nil && finalState == runpkg.FinalStateCompleted {
					if len(publisher.observations) == 0 || publisher.observations[len(publisher.observations)-1].Completion == nil || publisher.observations[len(publisher.observations)-1].Completion.AcceptanceRecorded || publisher.observations[len(publisher.observations)-1].Completion.ObservedAt.IsZero() {
						t.Fatalf("settled refusal omitted host nonacceptance observation: %+v", publisher.observations)
					}
				}
				wantTransitions := 0
				if test.wantState != "" && !test.wantSameState {
					wantTransitions = 1
				}
				if len(tick.updates) != wantTransitions || test.wantState != "" && tick.stateIssues[0].State != test.wantState || (!test.wantParked && (len(tick.comments) != 0 || len(state.Blocked) != 0)) || len(state.Completed) != 0 || (len(state.Retry) > 0) != test.wantContinue || len(state.deferredCompletions) != 0 || len(state.Claimed) != 0 || scheduling.releases != 1 || len(attempts.completions) != 1 {
					t.Fatalf("native settlement accepted, relaned, or replayed immutable outcome: updates=%v completed=%v retry=%v releases=%d attempts=%v", tick.updates, state.Completed, state.Retry, scheduling.releases, attempts.completions)
				}
				if test.wantParked {
					park := state.Blocked[issue.ID]
					if park.Reason != workAttemptErrorRunner || park.Recovery == nil || park.Recovery.Owner != blockedRecoveryOwnerOperator || !strings.Contains(park.Recovery.OperatorRemedy, head) {
						t.Fatalf("unchanged Rework lost its durable operator hold: %+v", park)
					}
					if test.wantFindings && (!strings.Contains(park.Recovery.OperatorRemedy, "finding-1") || !strings.Contains(park.Recovery.OperatorRemedy, "finding-2") || strings.Contains(park.Recovery.OperatorRemedy, "old-finding")) {
						t.Fatalf("park named the wrong standing findings: %s", park.Recovery.OperatorRemedy)
					}
					if test.auditFindings && (!strings.Contains(park.Recovery.OperatorRemedy, "audit-finding-1") || !strings.Contains(park.Recovery.OperatorRemedy, "audit-finding-2") || strings.Contains(park.Recovery.OperatorRemedy, "resolved-finding")) {
						t.Fatalf("park named the wrong standing audit findings: %s", park.Recovery.OperatorRemedy)
					}
				}
				if test.wantFindings && (!strings.Contains(state.Retry[issue.ID].Error, "finding-1") || !strings.Contains(state.Retry[issue.ID].Error, "finding-2") || strings.Contains(state.Retry[issue.ID].Error, "old-finding")) {
					t.Fatalf("retry omitted current rejection evidence: %+v", state.Retry[issue.ID])
				}
				if test.auditFindings && (!strings.Contains(state.Retry[issue.ID].Error, "audit-finding-1") || !strings.Contains(state.Retry[issue.ID].Error, "audit-finding-2") || strings.Contains(state.Retry[issue.ID].Error, "resolved-finding")) {
					t.Fatalf("retry omitted current audit evidence: %+v", state.Retry[issue.ID])
				}
				wantTerminal := test.wantTerminal
				if wantTerminal == "" {
					wantTerminal = terminalStateForRun(test.runErr, finalState)
				}
				if attempts.completions[0].TerminalState != wantTerminal || wantTerminal == store.WorkAttemptTerminalSuccess && attempts.completions[0].ErrorClass != "" && attempts.completions[0].ErrorClass != terminalAttemptWithoutWorkProductReason || state.TokenTotals.TotalTokens != 42 {
					t.Fatalf("settlement lost provider accounting: %+v", attempts.completions[0])
				}
				var metadata struct {
					ChangeID                  string `json:"native_change_id"`
					VersionID                 string `json:"native_version_id"`
					HeadSHA                   string `json:"native_head_sha"`
					VersionError              string `json:"native_version_error"`
					VersionCode               string `json:"native_version_code"`
					ValidatorAttribution      string `json:"validator_attribution"`
					ImplementationAttribution string `json:"implementation_review_attribution"`
					AuditAttribution          string `json:"security_audit_attribution"`
				}
				if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &metadata); err != nil {
					t.Fatal(err)
				}
				if test.change != nil && (metadata.ChangeID != test.change.ChangeID || metadata.VersionID != test.change.VersionID || metadata.HeadSHA != test.change.HeadSHA || metadata.VersionError != test.change.VersionError || metadata.VersionCode != test.change.VersionCode) {
					t.Fatalf("settlement lost genuine source evidence: %+v", metadata)
				}
				for range 3 {
					orch.retryDeferredCompletions(t.Context(), &state, now.Add(time.Hour))
				}
				if len(attempts.completions) != 1 || scheduling.releases != 1 || len(state.deferredCompletions) != 0 {
					t.Fatal("settled immutable outcome replayed completion")
				}
				if wantTerminal == store.WorkAttemptTerminalSuccess && len(state.FailureBreaker.Failures) != 0 {
					t.Fatal("publication refusal consumed issue failure allowance")
				}
				return
			}
			if test.handoffState != "" {
				if tick.stateIssues[0].State != test.handoffState || len(tick.updates) != 0 || len(state.Running) != 0 || len(state.Claimed) != 0 || len(state.Retry) != 0 || len(state.deferredCompletions) != 0 || scheduling.releases != 1 || len(attempts.completions) != 1 {
					t.Fatalf("finished attempt replaced active handoff: lane=%s updates=%v releases=%d", tick.stateIssues[0].State, tick.updates, scheduling.releases)
				}
				return
			}
			if test.wantInstance {
				lane := issue.State
				for _, update := range tick.updates {
					if update.state == "Blocked" {
						t.Fatalf("instance report parked the issue in Blocked: updates=%v", tick.updates)
					}
					if test.updateErr == nil {
						lane = update.state
					}
				}
				if _, retried := state.Retry[issue.ID]; retried != stateIn(lane, cfg.ActiveStates) || len(tick.comments) != 0 || len(state.Blocked) != 0 || len(state.Completed) != 0 || len(state.Claimed) != 0 || scheduling.releases != 1 || len(state.FailureBreaker.Failures) != 0 {
					t.Fatalf("instance report did not retry in its lane: lane=%s updates=%v blocked=%v retry=%v completed=%v claims=%v releases=%d", lane, tick.updates, state.Blocked, state.Retry, state.Completed, state.Claimed, scheduling.releases)
				}
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalSuccess || attempts.completions[0].ErrorClass != "" {
					t.Fatalf("instance report lost provider success: %#v", attempts.completions)
				}
				var metadata struct {
					Evidence     []telemetry.BlockerEvidence `json:"blocker_evidence"`
					ChangeID     string                      `json:"native_change_id"`
					VersionError string                      `json:"native_version_error"`
					VersionCode  string                      `json:"native_version_code"`
				}
				if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &metadata); err != nil {
					t.Fatal(err)
				}

				if len(metadata.Evidence) != 1 || metadata.Evidence[0].Owner != workpad.BlockerOwnerInstance || metadata.Evidence[0].Reference != "instance:worker-loopback" || !strings.Contains(metadata.Evidence[0].Reason, "EPERM") || metadata.Evidence[0].RecordedAt == nil || !metadata.Evidence[0].RecordedAt.Equal(now) || test.change != nil && metadata.ChangeID != test.change.ChangeID {
					t.Fatalf("instance or source evidence lost: %#v", metadata)
				}
				if test.change != nil && (metadata.VersionError != test.change.VersionError || metadata.VersionCode != test.change.VersionCode) {
					t.Fatalf("instance completion lost publication refusal: %#v", metadata)
				}
				var metrics struct {
					Tokens int `json:"total_tokens"`
					Turns  int `json:"turns"`
				}
				if err := json.Unmarshal([]byte(attempts.completions[0].MetricsJSON), &metrics); err != nil || metrics.Tokens != 42 || metrics.Turns != 1 || state.TokenTotals.TotalTokens != 42 || state.DiffStats[issue.ID].HeadSHA != head {
					t.Fatalf("instance report lost genuine usage: %#v, %v", metrics, err)
				}
				for range 3 {
					orch.retryDeferredCompletions(t.Context(), &state, now.Add(time.Hour))
				}
				if len(attempts.completions) != 1 || scheduling.releases != 1 || len(state.deferredCompletions) != 0 {
					t.Fatal("instance completion replayed after settlement")
				}
				return
			}
			if test.wantAbandoned {
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalAbandoned || !strings.Contains(attempts.completions[0].ErrorMessage, "final diff unavailable") {
					t.Fatalf("native authority failure = %#v", attempts.completions)
				}
				if test.updateErr == nil && len(tick.updates) != 0 || tick.stateIssues[0].State != issue.State || len(state.Blocked) != 0 || len(state.Completed) != 0 || len(state.deferredCompletions) != 0 || len(state.FailureBreaker.Failures) != 0 {
					t.Fatal("native authority failure changed the issue or repeated an obsolete completion")
				}
				return
			}
			retry, retried := state.Retry[issue.ID]
			_, deferred := state.deferredCompletions[issue.ID]
			if deferred != test.wantDeferred || test.wantDeferred && !retry.CompletionDeferred {
				t.Fatalf("deferred = %t (retry %#v), want %t", deferred, retry, test.wantDeferred)
			}
			if test.republish && test.lifecycle == "" {
				if len(tick.updates) != 0 || len(attempts.completions) != 0 {
					t.Fatal("publication outage completed or changed issue")
				}
				if !orch.retryDeferredCompletions(t.Context(), &state, state.Retry[issue.ID].DueAt.Add(time.Second)) {
					t.Fatal("publisher retry remained deferred")
				}
				if publisher.prepared != 1 || len(state.deferredCompletions) != 0 || len(tick.updates) != 1 || tick.updates[0].state != "Merging" || len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalSuccess {
					t.Fatalf("retry did not publish same completed result: publisher=%d updates=%+v attempts=%+v", publisher.prepared, tick.updates, attempts.completions)
				}
				return
			}
			machineRetry := !test.wantContinue && !test.wantDeferred && test.wantState != "" && (test.wantState == "Rework" || test.wantState == "Fixing" || test.wantState == "Todo") && test.runErr == nil && !test.wantHuman
			if continued := retried && !retry.CompletionDeferred; continued != (test.wantContinue || machineRetry) {
				t.Fatalf("continuation scheduled = %t, want %t", continued, test.wantContinue)
			}
			if test.wantOrdinary {
				if len(tick.updates) != 0 || len(state.Blocked) != 0 || len(state.deferredCompletions) != 0 {
					t.Fatal("ordinary continuation acquired native completion effects")
				}
				if test.diffStats.Fingerprint != "" {
					if len(attempts.completions) != 1 {
						t.Fatalf("continuation lost attempt receipt: %#v", attempts.completions)
					}
					receipt := attempts.completions[0]
					var metadata struct {
						Progress implementProgressRecord `json:"completion_progress"`
					}
					if err := json.Unmarshal([]byte(receipt.WorkerMetadataJSON), &metadata); err != nil {
						t.Fatal(err)
					}
					if receipt.TerminalState != store.WorkAttemptTerminalSuccess || receipt.ErrorClass != "" || metadata.Progress.Reason != "signature_changed" || metadata.Progress.WorkspaceDiffStats.Fingerprint != test.diffStats.Fingerprint {
						t.Fatalf("host integration conflict lost successful source progress: receipt=%#v metadata=%#v", receipt, metadata)
					}
					if terminalAttemptRetryableFailure(telemetry.WorkAttempt{TerminalState: string(receipt.TerminalState), ErrorClass: receipt.ErrorClass}) || retry.Attempt != 1 {
						t.Fatal("successful host-conflict continuation consumed failure allowance")
					}
					var metrics struct {
						Tokens int `json:"total_tokens"`
						Turns  int `json:"turns"`
					}
					if err := json.Unmarshal([]byte(receipt.MetricsJSON), &metrics); err != nil || metrics.Tokens != 42 || metrics.Turns != 1 || state.TokenTotals.TotalTokens != 42 {
						t.Fatalf("host-conflict continuation lost actual session cost: metrics=%+v error=%v", metrics, err)
					}
				}
				return
			}
			if test.wantReason != "" || test.wantSummary != "" {
				found := false
				for _, comment := range tick.comments {
					found = found || strings.Contains(comment.body, test.wantReason) && strings.Contains(comment.body, test.wantSummary)
				}
				if !found {
					t.Fatalf("comments lost reason %q or summary %q: %#v", test.wantReason, test.wantSummary, tick.comments)
				}
			}
			if test.wantHuman {
				cause := permissionWaitReason
				if test.wantAction != "" {
					cause = "human_action"
				}
				blocked, ok := state.Blocked[issue.ID]
				if !ok || blocked.Reason != cause || blocked.Recovery.HoldReason != cause || blocked.Recovery.Owner != blockedRecoveryOwnerHuman || blocked.Recovery.WorkAttemptID != 42 {
					t.Fatalf("human outcome = %#v, present = %t", blocked, ok)
				}
				if _, completed := state.Completed[issue.ID]; completed || len(tick.updates) != 1 || tick.updates[0].state != "Blocked" {
					t.Fatalf("human attention completed = %t; updates = %#v", completed, tick.updates)
				}
				if _, claimed := state.Claimed[issue.ID]; claimed || scheduling.releases < 1 {
					t.Fatalf("claim retained = %t, releases = %d", claimed, scheduling.releases)
				}
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalNoProgress || attempts.completions[0].ErrorClass != cause {
					t.Fatalf("attempt completions = %#v", attempts.completions)
				}
				var records map[string]json.RawMessage
				if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &records); err != nil {
					t.Fatal(err)
				}
				var wait permissionWaitRecord
				if err := json.Unmarshal(records[cause], &wait); err != nil {
					t.Fatal(err)
				}
				if test.wantAction != "" && (wait.Question != test.wantAction || blocked.Recovery.OperatorRemedy != test.wantAction || wait.Source != "terminal_detent_status" || len(records[permissionWaitReason]) != 0) {
					t.Fatalf("human action = %#v, remedy = %q", wait, blocked.Recovery.OperatorRemedy)
				}
				for _, detail := range test.wantHumanDetails {
					if !strings.Contains(wait.Question, detail) || !strings.Contains(blocked.Recovery.OperatorRemedy, detail) {
						t.Fatalf("human remedy lost %q: %+v", detail, blocked.Recovery)
					}
				}
				if test.wantWaitKind != "" && wait.Kind != test.wantWaitKind {
					t.Fatalf("wait kind = %q, want %q", wait.Kind, test.wantWaitKind)
				}
				if wait.WorkAttemptID != 42 || wait.SessionID != "native-session" || wait.Question == "" {
					t.Fatalf("human outcome provenance = %#v", wait)
				}
				if test.wantQuestion != "" && (wait.Question != test.wantQuestion || wait.Kind != permissionWaitCriteriaUnsatisfiable || wait.Source != "acceptance_validator" || len(wait.Criteria) == 0 || blocked.Recovery.OperatorRemedy != test.wantQuestion+"\nThen move the issue to Rework.") {
					t.Fatalf("criterion wait = %#v, remedy = %q, want %q", wait, blocked.Recovery.OperatorRemedy, test.wantQuestion)
				}
				if test.wantQuestion == "" && wait.Kind == permissionWaitCriteriaUnsatisfiable {
					t.Fatalf("worker permission wait recorded as criteria gate: %#v", wait)
				}
				if state.TokenTotals.TotalTokens != 42 || state.DiffStats[issue.ID].HeadSHA != head {
					t.Fatalf("telemetry lost: tokens = %#v, diff = %#v", state.TokenTotals, state.DiffStats[issue.ID])
				}
				if len(state.Retry) != 0 {
					t.Fatalf("human outcome scheduled retry: %#v", state.Retry)
				}
				orch.retryDeferredCompletions(t.Context(), &state, now.Add(time.Hour))
				if len(attempts.completions) != 1 || scheduling.releases != 1 {
					t.Fatal("human completion replayed after settlement")
				}
				if orch.recoverCauseBlockedIssue(t.Context(), &state, blocked.Issue, now.Add(24*time.Hour)) {
					t.Fatal("human outcome automatically recovered")
				}
				return
			}
			if test.wantDeferred && test.runErr != nil {
				if scheduling.releases != 0 || len(attempts.completions) != 0 || len(state.Completed) != 0 {
					t.Fatal("failed native completion refusal released authority or fabricated completion")
				}
				return
			}
			if (test.runErr != nil || finalState == runpkg.FinalStateFailed) && !test.wantHuman {
				wantTerminal := test.wantTerminal
				if wantTerminal == "" {
					wantTerminal = store.WorkAttemptTerminalFailure
				}
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != wantTerminal || attempts.completions[0].ErrorClass == permissionWaitReason || test.runErr != nil && !strings.Contains(attempts.completions[0].ErrorMessage, test.runErr.Error()) {
					t.Fatalf("failure outcome = %#v", attempts.completions)
				}
				if errors.Is(test.runErr, workspace.ErrMergeResolutionInvalid) {
					receipt := attempts.completions[0]
					if allowanceInfrastructureAttempt(store.WorkAttempt{TerminalState: receipt.TerminalState, ErrorClass: receipt.ErrorClass, MetricsJSON: `{"turns":1,"total_tokens":42}`}) || !terminalAttemptRetryableFailure(telemetry.WorkAttempt{TerminalState: string(receipt.TerminalState), ErrorClass: receipt.ErrorClass}) {
						t.Fatal("unresolved model conflict escaped genuine failure accounting")
					}
				}
				if errors.Is(test.runErr, runpkg.ErrReviewConversationsUnavailable) {
					if _, retried := state.Retry[issue.ID]; attempts.completions[0].ErrorClass != workAttemptErrorRunner || len(tick.updates) != 0 || !retried {
						t.Fatalf("unreadable review conversations did not retry as an instance failure: class=%s updates=%v", attempts.completions[0].ErrorClass, tick.updates)
					}
				}
				if errors.Is(test.runErr, runpkg.ErrWorkerProcessReap) || errors.Is(test.runErr, runpkg.ErrWorkspacePreparation) {
					receipt := attempts.completions[0]
					if receipt.ErrorClass != workAttemptErrorWorkspace || !allowanceInfrastructureAttempt(store.WorkAttempt{TerminalState: receipt.TerminalState, ErrorClass: receipt.ErrorClass, MetricsJSON: receipt.MetricsJSON}) {
						t.Fatal("native cleanup failure consumed issue failure allowance")
					}
				}
				wantTransitions := 1
				if test.wantSameState {
					wantTransitions = 0
				}
				if test.wantRecovery {
					receipt := attempts.completions[0]
					reasons := tracker.(*nativeWorkflowConnector).reasons
					if len(reasons) != wantTransitions || wantTransitions > 0 && reasons[0] != "terminal attempt without work product" || receipt.ErrorClass != workAttemptErrorWorkspace || !allowanceInfrastructureAttempt(store.WorkAttempt{TerminalState: receipt.TerminalState, ErrorClass: receipt.ErrorClass, MetricsJSON: receipt.MetricsJSON}) || len(state.FailureBreaker.Failures) != 0 || !test.optout && test.humanReview != nil && !*test.humanReview && !dispatchableState(test.states, tick.stateIssues[0].State) {
						t.Fatalf("checkpoint recovery lost mismatch or instance attribution: reasons=%v receipt=%+v state=%s", reasons, receipt, tick.stateIssues[0].State)
					}
					for range 3 {
						orch.retryDeferredCompletions(t.Context(), &state, now.Add(time.Hour))
					}
					if len(tick.updates) != wantTransitions || scheduling.releases != 1 || (len(state.Retry) > 0) != test.wantContinue || len(state.deferredCompletions) != 0 || len(attempts.completions) != 1 {
						t.Fatal("checkpoint refusal repeated completion or lost its retry")
					}
				}
				if test.wantState != "" {
					if len(tick.updates) != wantTransitions || tick.stateIssues[0].State != test.wantState || retried != test.wantContinue || scheduling.releases != 1 || len(tick.comments) != 0 {
						t.Fatalf("failed native handoff: updates=%v retry=%t releases=%d comments=%v", tick.updates, retried, scheduling.releases, tick.comments)
					}
				}
				if _, blocked := state.Blocked[issue.ID]; blocked {
					t.Fatal("mixed failure became a human park")
				}
				if _, completed := state.Completed[issue.ID]; completed {
					t.Fatal("mixed failure completed")
				}
				for _, update := range tick.updates {
					if update.state != firstNonBlank(test.wantState, "Todo") {
						t.Fatalf("failure transition = %#v", update)
					}
				}
				return
			}
			if test.wantContinue || test.wantDeferred {
				if test.wantDeferred && len(attempts.completions) != 0 {
					t.Fatalf("deferred native completion recorded a terminal outcome: %#v", attempts.completions)
				}
				if test.wantDeferred && (state.deferredCompletions[issue.ID].Result.FinalMessage != event.Result.FinalMessage || state.deferredCompletions[issue.ID].Result.FinalState != event.Result.FinalState) {
					t.Fatal("deferred completion lost the worker disposition")
				}
				if test.wantDeferred && test.wantQuestion != "" {
					if orch.handlePermissionWaitCompletion(t.Context(), nil, state.deferredCompletions[issue.ID].completion(), Running{}) {
						t.Fatal("deferred criteria gate replays as a generic permission wait")
					}
					return
				}
				if test.wantDeferred && test.change != nil && test.change.VersionError != "" {
					preserved := state.deferredCompletions[issue.ID].Result.NativeChange
					if preserved == nil || preserved.VersionError != test.change.VersionError || preserved.VersionCode != test.change.VersionCode || preserved.ChangeID != test.change.ChangeID || preserved.HeadSHA != test.change.HeadSHA || len(state.FailureBreaker.Failures) != 0 || len(state.Completed) != 0 || len(state.Blocked) != 0 || scheduling.releases != 0 {
						t.Fatalf("publication refusal lost its existing completion owner: %#v", preserved)
					}
				}
				for _, update := range tick.updates {
					if test.updateErr == nil {
						t.Fatalf("the item was moved: %#v", tick.updates)
					}
					if update.state != "In Review" && update.state != test.wantState && (test.finalMessage == "" || update.state != "Blocked") {
						t.Fatalf("refused write targeted %s", update.state)
					}
				}
				if test.wantDeferred && test.finalMessage == "" && len(tick.comments) != 0 {
					t.Fatalf("a handed-off item was commented on: %#v", tick.comments)
				}
				return
			}
			if test.reviewState != nil && test.reviewState.RepairPending {
				if len(tick.updates) != 0 || len(tick.comments) != 0 || len(state.Blocked) != 0 || !retry.DueAt.After(now) || retry.DueAt.After(now.Add(cfg.MaxRetryBackoff)) || retry.Attempt != 2 || len(attempts.completions) != 1 {
					t.Fatalf("repair wait lost retry owner: retry=%+v updates=%v blocked=%v completions=%v", retry, tick.updates, state.Blocked, attempts.completions)
				}
				return
			}
			if test.wantSameState {
				if len(tick.updates) != 0 {
					t.Fatalf("same-state continuation wrote a lane: %#v", tick.updates)
				}
			} else if len(tick.updates) != 1 || tick.updates[0].state != test.wantState {
				t.Fatalf("lane updates = %#v, want one to %s", tick.updates, test.wantState)
			}
			if len(tick.comments) != 1 || !strings.Contains(strings.ToLower(tick.comments[0].body), strings.ToLower(test.wantComment)) {
				t.Fatalf("comments = %#v, want one containing %q", tick.comments, test.wantComment)
			}
			if test.laneReason != "" {
				if len(metrics.events) < 2 || metrics.events[0].Reason != test.laneReason || metrics.events[1].Reason != test.laneReason {
					t.Fatalf("lane events lost transition reason %q", test.laneReason)
				}
				var transition workflowLaneMetadata
				if err := json.Unmarshal([]byte(metrics.events[0].MetadataJSON), &transition); err != nil {
					t.Fatal(err)
				}
				if len(transition.ReasonDetail) > workpad.MaxFinalSummaryBytes {
					t.Fatalf("transition detail exceeds the summary bound: %d bytes", len(transition.ReasonDetail))
				}
				for _, detail := range test.details {
					if !strings.Contains(transition.ReasonDetail, detail) {
						t.Fatalf("transition detail = %q, want %q", transition.ReasonDetail, detail)
					}
				}
				forwarded := tracker.(*nativeWorkflowConnector)
				if len(forwarded.reasonCodes) != 1 || forwarded.reasonCodes[0] != test.laneReason || forwarded.reasons[0] != transition.ReasonDetail {
					t.Fatalf("connector lost transition reason: codes=%v details=%v", forwarded.reasonCodes, forwarded.reasons)
				}
				if !strings.Contains(tick.comments[0].body, transition.ReasonDetail) || strings.Contains(tick.comments[0].body, "The run succeeded") {
					t.Fatalf("comment lost the review reason: %q", tick.comments[0].body)
				}
			}
			wantReleases := 1
			if test.lifecycle == "release" {
				wantReleases = 2
			}
			if _, claimed := state.Claimed[issue.ID]; claimed || scheduling.releases != wantReleases {
				t.Fatalf("claim retained = %t, releases = %d", claimed, scheduling.releases)
			}
			completed, ok := state.Completed[issue.ID]
			if !machineRetry && !test.wantSameState && (!ok || completed.Issue.State != test.wantState) || (test.wantSameState || machineRetry) && ok {
				t.Fatalf("completed = %#v, present = %t", completed, ok)
			}
			if machineRetry && (!retry.DueAt.After(now) || retry.DueAt.After(now.Add(cfg.MaxRetryBackoff)) || retry.Attempt != 2) {
				t.Fatalf("machine disposition lost capped retry: %+v", retry)
			}
			if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalSuccess {
				t.Fatalf("attempt completions = %#v", attempts.completions)
			}
			if attempts.completions[0].ErrorClass != "" || len(state.FailureBreaker.Failures) != 0 || terminalAttemptRetryableFailure(telemetry.WorkAttempt{TerminalState: string(attempts.completions[0].TerminalState), ErrorClass: attempts.completions[0].ErrorClass}) {
				t.Fatal("successful native review consumed failure allowance")
			}
			var metadata struct {
				ChangeID                  string `json:"native_change_id"`
				VersionID                 string `json:"native_version_id"`
				HeadSHA                   string `json:"native_head_sha"`
				VersionError              string `json:"native_version_error"`
				VersionCode               string `json:"native_version_code"`
				ValidatorAttribution      string `json:"validator_attribution"`
				ImplementationAttribution string `json:"implementation_review_attribution"`
				AuditAttribution          string `json:"security_audit_attribution"`
			}
			if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &metadata); err != nil {
				t.Fatal(err)
			}
			if test.wantAttribution != "" {
				got := metadata.ValidatorAttribution
				if got == "" {
					got = metadata.ImplementationAttribution
				}
				if got == "" {
					got = metadata.AuditAttribution
				}
				if got != test.wantAttribution {
					t.Fatalf("outcome attribution = %q, want %q", got, test.wantAttribution)
				}
			}
			wantChange := test.change
			if wantChange == nil {
				wantChange = &runpkg.NativeChange{}
			}
			if metadata.ChangeID != wantChange.ChangeID || metadata.VersionID != wantChange.VersionID || metadata.HeadSHA != wantChange.HeadSHA {
				t.Fatalf("completion lost the published native identity: %+v, want %+v", metadata, test.change)
			}
			if metadata.VersionError != wantChange.VersionError || metadata.VersionCode != wantChange.VersionCode {
				t.Fatalf("completion lost publication diagnostic: %+v", metadata)
			}
			if state.Draining != test.draining || len(state.Running) != 0 {
				t.Fatalf("completion changed drain or started work: draining=%t, running=%+v", state.Draining, state.Running)
			}
			if test.progressCycles {
				scheduling.release = nil
				beforeTree := "dispatch-tree"
				for _, step := range []struct {
					tree string
					want int
				}{
					{"tree-1", 1},
					{"tree-2", 1},
					{"tree-2", 2},
					{"tree-2", 3},
					{"tree-3", 1},
				} {
					current := state.Retry[issue.ID]
					run := Running{Issue: current.Issue, Attempt: current.Attempt, Mode: mode, StartedAt: now,
						CompletionSourceStart: completionSourceStartRecord{Captured: true, Fingerprint: completionSourceFingerprint{WorkspaceTree: beforeTree}}}
					event.CompletedAt = current.DueAt
					event.Result.DiffStats = DiffStats{Status: "clean", HeadSHA: head, TreeSHA: step.tree}
					state.Claimed[issue.ID] = Claimed{Issue: current.Issue}
					releases := scheduling.releases
					if !orch.completeNativeChangeRun(t.Context(), &state, event, run, FinalStateCompleted) {
						t.Fatal("native continuation was not handled")
					}
					next, ok := state.Retry[issue.ID]
					if !ok || next.Attempt != step.want || scheduling.releases != releases+1 {
						t.Fatalf("tree %s to %s: retry=%+v, want attempt %d and one release", beforeTree, step.tree, next, step.want)
					}
					beforeTree = step.tree
				}
			}
			if test.retryCycles {
				scheduling.release = nil
				for _, attempt := range []int{2, 3, 50} {
					current := state.Retry[issue.ID].Issue
					run := Running{Issue: current, Attempt: attempt, Mode: mode, StartedAt: now,
						CompletionSourceStart: completionSourceStartRecord{Captured: true, Fingerprint: completionSourceFingerprint{WorkspaceTree: "dispatch-tree"}}}
					event.Result.DiffStats = DiffStats{Status: "clean", HeadSHA: head, TreeSHA: fmt.Sprintf("changed-tree-%d", attempt)}
					event.CompletedAt = state.Retry[issue.ID].DueAt
					if !orch.completeNativeChangeRun(t.Context(), &state, event, run, FinalStateCompleted) {
						t.Fatal("native retry was not handled")
					}
					next := state.Retry[issue.ID]
					if next.Attempt != attempt+1 || !next.DueAt.After(event.CompletedAt) || next.DueAt.After(event.CompletedAt.Add(cfg.MaxRetryBackoff)) {
						t.Fatalf("repeated disposition lost backoff: %+v", next)
					}
				}
				current := state.Retry[issue.ID].Issue
				event.Result.NativeChange = accepted
				event.Result.FinalMessage = "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
				event.Result.ReviewConversationsCleared = true
				tracker.(*nativeWorkflowConnector).reviewed = &yes
				state.PriorAttempts[issue.ID] = runpkg.PriorAttempt{}
				if !orch.completeNativeChangeRun(t.Context(), &state, event, Running{Issue: current, Attempt: 51, Mode: mode}, FinalStateCompleted) {
					t.Fatal("successful native completion was not handled")
				}
				if _, retry := state.Retry[issue.ID]; retry {
					t.Fatal("successful completion retained retry")
				}
				if _, prior := state.PriorAttempts[issue.ID]; prior {
					t.Fatal("successful completion retained prior attempts")
				}
			}

		})
	}
}

func TestNativeCompletionComment(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change runpkg.NativeChange
		want   []string
	}{
		{name: "opened", change: runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: "0123456789abcdef", Files: 3}, want: []string{"change_1", "3 files", "head 0123456789ab)", "In Progress to In Review"}},
		{name: "opened and needing no reviewer", change: runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", Reviewed: true, HeadSHA: "0123456789abcdef", Files: 3}, want: []string{"needs no further review", "runner lands it next"}},
		{name: "unchanged", change: runpkg.NativeChange{BaseSHA: "abc"}, want: []string{"against abc", "nothing to review"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			to := "In Review"
			if test.change.Reviewed {
				to = "Merging"
			}
			got := nativeCompletionComment(&test.change, "In Progress", to)
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Fatalf("comment %q does not contain %q", got, want)
				}
			}
		})
	}
}

func TestNativeCompletionNoWorkProductRejection(t *testing.T) {
	issue := completionTransitionIssue("Rework", "")
	journey := newNativeLandingJourney(t, issue, "", 200, false, false)
	head := journey.target.HeadSHA
	result, err := (FakeRunner{}).Run(t.Context(), RunRequest{Issue: issue, Mode: RunModeImplement})
	if err != nil {
		t.Fatal(err)
	}
	result.FinalMessage = "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
	result.DiffStats = DiffStats{Status: "clean", HeadSHA: head}
	result.NativeChange = &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: head}
	tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	native := &nativeWorkflowConnector{autoPromoteTickConnector: tick, states: []connector.WorkflowState{{Name: "Rework", Dispatchable: true, Transitions: []string{"Done", "Blocked"}}, {Name: "Done", Terminal: true}, {Name: "Blocked"}}, reviewState: &connector.ChangeReview{Current: true, ChangesRequested: true}}
	cfg := normalizeConfig(Config{ActiveStates: []string{"Rework"}, TerminalStates: []string{"Done"}})
	cfg.AutoPromote.HumanReview = new(false)
	attempts := &recordingWorkAttemptStore{}
	scheduling := &nativeCompletionScheduling{hubSchedulingSource: &hubSchedulingSource{}}
	orch := &Orchestrator{cfg: cfg, connector: native, workAttempts: attempts, scheduling: scheduling}
	state := newState(cfg)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	state.Running[issue.ID] = Running{Issue: issue, Mode: RunModeImplement, Attempt: 3, WorkAttemptID: 42, Generation: 7, TurnCount: 1, CompletionSourceStart: completionSourceStartRecord{Captured: true, WorkspaceHeadAvailable: true, Fingerprint: completionSourceFingerprint{WorkspaceHead: head}}}
	state.Claimed[issue.ID] = Claimed{Issue: issue}
	journey.execution.recovery.ChangeDetail = &tracker.ChangeDetail{Change: tracker.ChangeRequest{ID: "change_1", CurrentVersion: "version_1"}, Reviews: []tracker.ChangeReview{{ID: "review_1", VersionID: "version_1", Decision: "changes_requested", Body: "Preserve concurrent updates"}}}
	orch.handleRunResult(t.Context(), &state, runpkg.Completion{IssueID: issue.ID, CompletedAt: now, Request: RunRequest{Mode: RunModeImplement, Execution: journey.execution}, Result: result})
	if len(state.Blocked) != 0 || len(tick.updates) != 0 || len(state.Claimed) != 0 || len(attempts.completions) != 1 {
		t.Fatalf("no-change turn lost completion ownership: state=%+v moves=%+v receipts=%+v", state, tick.updates, attempts.completions)
	}
	if retry := state.Retry[issue.ID]; len(state.Completed) != 0 || retry.Issue.State != "Rework" || retry.Attempt != 4 || !retry.DueAt.After(now) || result.NativeChange.Landing != nil || !strings.Contains(retry.Error, "Preserve concurrent updates") {
		t.Fatalf("undelivered source did not resume implementation: retry=%+v completed=%+v", retry, state.Completed)
	}
}

type nativeCompletionScheduling struct {
	*hubSchedulingSource
	release func()
	renew   func() (Claimed, error)
}

func (s *nativeCompletionScheduling) RenewClaim(ctx context.Context, issueID string, now time.Time) (Claimed, error) {
	if s.renew != nil {
		return s.renew()
	}
	return s.hubSchedulingSource.RenewClaim(ctx, issueID, now)
}

func (s *nativeCompletionScheduling) ReleaseClaim(ctx context.Context, issueID, reason string) error {
	if s.release != nil {
		s.release()
	}
	return s.hubSchedulingSource.ReleaseClaim(ctx, issueID, reason)
}

type nativeCompletionPublisher struct {
	nativeLandingJourneyExecution
	observations  []tracker.NativeRuntimeObservation
	change        *runpkg.NativeChange
	prepared      int
	prepare       func()
	validationErr error
}

func (*nativeCompletionPublisher) PrepareReview(context.Context, tracker.NativeChangeReference) error {
	return nil
}

func (p *nativeCompletionPublisher) ObserveRuntime(_ context.Context, observation tracker.NativeRuntimeObservation) error {
	p.observations = append(p.observations, observation)
	return nil
}

func (*nativeCompletionPublisher) FlushRuntime(context.Context) error { return nil }

func (p *nativeCompletionPublisher) Validate(context.Context) error { return p.validationErr }

func (p *nativeCompletionPublisher) PrepareFinish(context.Context, string, string, *tracker.NativeTerminalFailure) error {
	p.prepared++
	if p.prepare != nil {
		p.prepare()
	}
	return nil
}

func (p *nativeCompletionPublisher) NativeChange() *runpkg.NativeChange {
	return p.change
}

type nativeReviewHostChecker string

func (c nativeReviewHostChecker) WorkerHostAvailable(_ context.Context, host string) bool {
	return host != string(c)
}

func TestNativeReviewContinuationRespectsProviderCapacity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name              string
		running, capacity int
		blocked           bool
		hosts             []string
		previousHost      string
		unavailableHost   string
		wantHost          string
	}{
		{name: "scaled provider capacity", running: 3, capacity: 5, blocked: true},
		{name: "below provider capacity", running: 2, capacity: 5},
		{name: "one slot replaces restored owner", capacity: 1},
		{name: "hard capacity", running: 1, capacity: 1, blocked: true},
		{name: "unavailable saved host", capacity: 1, hosts: []string{"worker-a", "worker-b"}, previousHost: "worker-a", unavailableHost: "worker-a", wantHost: "worker-b"},
		{name: "removed saved host", capacity: 1, hosts: []string{"worker-b"}, previousHost: "worker-a", wantHost: "worker-b"},
		{name: "no live host", capacity: 1, hosts: []string{"worker-a"}, previousHost: "worker-a", unavailableHost: "worker-a", blocked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := providerWindowTestConfig()
			cfg.MaxConcurrentAgents = test.capacity
			cfg.WorkerHosts = test.hosts
			if len(test.hosts) > 0 {
				cfg.MaxConcurrentAgentsPerHost = 1
			}
			state := providerWindowState(cfg, test.running)
			issue := dispatchTestIssue("review-owner", "In Progress")
			running := Running{Issue: issue, Mode: runpkg.RunModeImplement, CompletionOwnershipReleased: true, WorkerHost: test.previousHost}
			state.Running[issue.ID] = running
			supervisor, err := runpkg.NewSupervisor(FakeRunner{}, runpkg.SupervisorConfig{})
			if err != nil {
				t.Fatal(err)
			}
			orch := &Orchestrator{cfg: cfg, supervisor: supervisor, runResults: make(chan runpkg.Completion, 1)}
			orch.workerHostChecker = nativeReviewHostChecker(test.unavailableHost)
			source := &runpkg.NativeChange{Changed: true, ChangeID: "change_example", VersionID: "version_example", HeadSHA: "head"}
			event := runpkg.Completion{IssueID: issue.ID, Request: runpkg.RunRequest{Execution: &nativeCompletionPublisher{}}, Result: runpkg.RunResult{NativeChange: source, FinalMessage: "completed source"}}
			err = orch.continueNativeRun(t.Context(), &state, event, running, true)
			if test.blocked {
				if !errors.Is(err, runpkg.ErrModelPermitUnavailable) || !state.Running[issue.ID].CompletionOwnershipReleased {
					t.Fatalf("capacity did not retain deferred owner: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			completion := <-orch.runResults
			<-state.Running[issue.ID].done
			defer state.Running[issue.ID].cancel()
			continuation := completion.Request.NativeReviewContinuation
			if completion.Request.WorkerHost != test.wantHost || state.Running[issue.ID].WorkerHost != test.wantHost {
				t.Fatalf("review dispatched on request host %q and running host %q, want %q", completion.Request.WorkerHost, state.Running[issue.ID].WorkerHost, test.wantHost)
			}
			if continuation == nil || continuation.Source.VersionID != source.VersionID || continuation.Source.HeadSHA != source.HeadSHA || continuation.FinalMessage != event.Result.FinalMessage || state.Running[issue.ID].ModelPermitExempt || state.Running[issue.ID].CompletionOwnershipReleased {
				t.Fatal("review continuation lost source or capacity ownership")
			}
			if err := orch.continueNativeLandingRun(t.Context(), &state, completion, state.Running[issue.ID]); err != nil {
				t.Fatal(err)
			}
			landing := <-orch.runResults
			<-state.Running[issue.ID].done
			defer state.Running[issue.ID].cancel()
			if landing.Request.NativeReviewContinuation != nil || landing.Request.Mode != runpkg.RunModeMerge {
				t.Fatal("accepted review continuation replaced native landing")
			}
		})
	}
}

func assertCompletionMachineRetry(t *testing.T, orch *Orchestrator, state State, issueID string, at time.Time, attempt int, upper time.Duration) {
	t.Helper()
	retry, ok := state.Retry[issueID]
	if !ok || retry.Attempt != attempt {
		t.Fatalf("retry = %+v, want attempt %d", retry, attempt)
	}
	if delay := retry.DueAt.Sub(at); delay < upper-upper/8 || delay > upper {
		t.Fatalf("delay = %v, want [%v, %v]", delay, upper-upper/8, upper)
	}
	if _, claimed := state.Claimed[issueID]; claimed {
		t.Fatal("retry retained lease")
	}
	restarted := &Orchestrator{cfg: orch.cfg, connector: orch.connector, workflowMetrics: orch.workflowMetrics}
	restored := newState(orch.cfg)
	restarted.restoreMachineRetry(t.Context(), &restored, retry.Issue)
	got, ok := restored.Retry[issueID]
	if !ok || got.Attempt != attempt || !got.DueAt.Equal(retry.DueAt) || got.Error != retry.Error || got.Issue.State != retry.Issue.State || got.WorkerHost != retry.WorkerHost {
		t.Fatalf("persisted retry restoration = %+v, want %+v", got, retry)
	}
}

func TestFindingAttributionSurvivesOutcomeAdapters(t *testing.T) {
	evidence := &failureattribution.Evidence{BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Command: "check-input", Diagnostic: "source.go:4: invalid input", BaseExitCode: 1}
	original := []gate.Finding{{Body: "Invalid input", Attribution: "independent", AttributionEvidence: evidence, CheckCommand: evidence.Command, CheckDiagnostic: evidence.Diagnostic}}
	for _, test := range []struct {
		name   string
		result []gate.Finding
	}{
		{"validator store", gateFindingsFromStore(storeFindingsFromGate(original))},
		{"promotion summary", gateFindings(autoPromoteFindingsFromGate(original))},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !reflect.DeepEqual(test.result, original) {
				t.Fatalf("attribution lost in outcome adapter: %#v", test.result)
			}
		})
	}
}
