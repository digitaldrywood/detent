package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type nativeWorkflowConnector struct {
	*autoPromoteTickConnector
	states    []connector.WorkflowState
	statesErr error
	// reviewed is the change's review state when the completion is applied;
	// nil answers as a failed read.
	reviewed *bool
}

func (c *nativeWorkflowConnector) ChangeReviewed(context.Context, string, string, string) (bool, error) {
	if c.reviewed == nil {
		return false, errors.New("hub unavailable")
	}
	return *c.reviewed, nil
}

func (c *nativeWorkflowConnector) WorkflowStates(context.Context) ([]connector.WorkflowState, error) {
	return c.states, c.statesErr
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
	undispatched := append([]connector.WorkflowState(nil), landing...)
	undispatched[3].Dispatchable = false
	rework := append(append([]connector.WorkflowState(nil), landing...), connector.WorkflowState{Name: "Rework", Dispatchable: true, Transitions: []string{"In Review", "Merging", "Blocked"}})
	blockedLanding := append([]connector.WorkflowState(nil), landing...)
	blockedLanding[1].Transitions = append([]string{"Blocked"}, blockedLanding[1].Transitions...)
	blockedLanding = append(blockedLanding, connector.WorkflowState{Name: "Blocked", Transitions: []string{"In Progress"}})
	unfinishedWorkflow := append(append([]connector.WorkflowState(nil), blockedLanding...), connector.WorkflowState{Name: "Rework", Dispatchable: true, Transitions: []string{"In Review", "Merging", "Blocked"}})
	unfinishedWorkflow[1].Transitions = append([]string{"Rework"}, unfinishedWorkflow[1].Transitions...)
	noDispatchRework := append([]connector.WorkflowState(nil), unfinishedWorkflow...)
	noDispatchRework[len(noDispatchRework)-1].Dispatchable = false
	operatorRework := append([]connector.WorkflowState(nil), unfinishedWorkflow...)
	operatorRework[len(operatorRework)-1].OperatorOnly = true
	customRework := append([]connector.WorkflowState(nil), unfinishedWorkflow...)
	customRework[1].Transitions = append([]string{"Fixing"}, customRework[1].Transitions...)
	customRework[len(customRework)-1].Name = "Fixing"
	unfinishedReport := "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```"
	instanceReport := "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:worker-loopback\n    reason: sandbox refused the mock listener with EPERM\nhuman_action: null\n```"
	yes, no := true, false
	head := strings.Repeat("c", 40)
	opened := &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, Files: 2}
	waiting := &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, Files: 2}
	accepted := &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", Reviewed: true, HeadSHA: head, Files: 2}
	deliveryErr := &runpkg.DeliverableRecoveryError{Err: &runpkg.DeliverableCommandError{OperationClass: "pull_request", Message: "pull request publication failed"}}
	for _, test := range []struct {
		name          string
		republish     bool
		finalState    string
		finalMessage  string
		runErr        error
		wantHuman     bool
		noUsage       bool
		change        *runpkg.NativeChange
		states        []connector.WorkflowState
		statesErr     error
		reviewed      *bool
		updateErr     error
		plain         bool
		quotaWait     bool
		draining      bool
		sourceState   string
		reworkState   string
		wantSameState bool
		wantDirect    bool
		diffStats     DiffStats
		wantOrdinary  bool
		wantInstance  bool
		humanReview   *bool
		wantState     string
		wantComment   string
		wantReason    string
		wantSummary   string
		wantDeferred  bool
		wantContinue  bool
		wantAbandoned bool
		wantTerminal  store.WorkAttemptTerminalState
		roundTrip     bool
	}{
		{name: "four lane reviewed change lands from current lane", change: accepted, states: fourLanes, humanReview: &no, wantDirect: true},
		{name: "four lane reviewed change lands during drain", change: accepted, states: fourLanes, humanReview: &no, draining: true, wantDirect: true},
		{name: "four lane failed run parks without retry", states: fourLanes, humanReview: &no, runErr: errors.New("provider failed"), wantState: "Human Review"},
		{name: "four lane unreviewed version parks", change: waiting, states: fourLanes, humanReview: &no, wantState: "Human Review", wantComment: "opened Change Request"},
		{name: "four lane unfinished source parks without landing", change: accepted, states: fourLanes, humanReview: &no, finalMessage: unfinishedReport, wantState: "Human Review", wantComment: "detent-status in_progress"},
		{name: "blocked comment retains agent reason and summary", change: accepted, states: unfinishedWorkflow, humanReview: &no, finalMessage: "Source conflicts remain unresolved.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: merge_conflict\nblockers: []\nhuman_action: null\n```", wantState: "In Review", wantComment: "detent-status blocked", wantReason: "merge_conflict", wantSummary: "Source conflicts remain unresolved."},
		{name: "human action comment retains agent reason and summary", change: accepted, states: workflow, finalMessage: "The operator must approve the migration.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: permission_wait\nblockers: []\nhuman_action: Approve the migration\n```", wantHuman: true, wantReason: "permission_wait", wantSummary: "The operator must approve the migration."},
		{name: "instance limitation comment retains agent reason and summary", change: accepted, states: unfinishedWorkflow, humanReview: &no, finalMessage: "Sandbox forbids TCP listeners; upstream fetch returned HTTP 403.\n```detent-status\nschema: 1\nstatus: blocked\nreason_code: instance_limitation\nblockers: []\nhuman_action: null\n```", wantState: "In Review", wantComment: "detent-status blocked", wantReason: "instance_limitation", wantSummary: "Sandbox forbids TCP listeners; upstream fetch returned HTTP 403."},
		{name: "deferred cleanup deadline retains instance attribution", states: workflow, runErr: errors.Join(runpkg.ErrWorkerProcessReap, context.DeadlineExceeded), wantState: "In Review", wantTerminal: store.WorkAttemptTerminalTimedOut, roundTrip: true},
		{name: "deferred provider cancellation retains its outcome", states: workflow, runErr: context.Canceled, wantState: "In Review", wantTerminal: store.WorkAttemptTerminalCancelled, roundTrip: true},
		{name: "failed native stale authority is rejected", states: workflow, runErr: errors.New("provider failed"), updateErr: errors.Join(runpkg.ErrExecutionAuthorityUnavailable, errors.New("final diff unavailable")), wantAbandoned: true},
		{name: "failed provider without native change settles for review", states: workflow, runErr: errors.New("provider failed"), wantState: "In Review"},
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
		{name: "a version waiting for a reviewer goes to review", change: waiting, states: landing, wantState: "In Review", wantComment: "opened Change Request change_1"},
		{name: "disabled review sends unaccepted change to configured review", change: waiting, states: blockedLanding, humanReview: &no, wantState: "In Review", wantComment: "opened Change Request change_1"},
		{name: "disabled review lands accepted change", change: accepted, states: blockedLanding, humanReview: &no, wantState: "Merging", wantComment: "runner lands it next"},
		{name: "an accepted version without a landing move lands directly", change: accepted, states: workflow, wantDirect: true},
		{name: "an approval that arrived after the publish lands", change: waiting, states: landing, reviewed: &yes, wantState: "Merging", wantComment: "runner lands it next"},
		{name: "a run that published no version cannot succeed on an earlier reviewed one", change: opened, states: landing, reviewed: &yes, wantDeferred: true},
		{name: "unavailable version evidence goes to review", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, VersionError: "publication unavailable"}, states: landing, wantState: "In Review", wantComment: "publication unavailable"},
		{name: "refused policy cannot inherit earlier version approval", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, VersionID: "version_1", Reviewed: true, VersionError: "policy mismatch", VersionCode: "policy_mismatch"}, reviewed: &yes, states: rework, sourceState: "Rework", wantState: "In Review", wantComment: "policy mismatch"},
		{name: "publication refusal reaches review during drain", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", HeadSHA: head, VersionError: "producer allowance exhausted", VersionCode: "allowance_exhausted"}, states: rework, sourceState: "Rework", draining: true, wantState: "In Review", wantComment: "producer allowance exhausted"},
		{name: "native publication authority failure is instance owned", states: workflow, runErr: errors.Join(runpkg.ErrExecutionAuthorityUnavailable, errors.New("final diff unavailable")), wantAbandoned: true},
		{name: "a version that lost its acceptance goes to review", change: accepted, states: landing, reviewed: &no, wantState: "In Review", wantComment: "opened Change Request change_1"},
		{name: "an accepted version lands directly without a dispatchable landing lane", change: accepted, states: undispatched, wantDirect: true},
		{name: "unchanged work with final approval question needs human", change: &runpkg.NativeChange{}, states: workflow, finalMessage: "May I merge?", wantHuman: true},
		{name: "unchanged work with final structured blocker needs human", change: &runpkg.NativeChange{}, states: workflow, finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve the migration\n```", wantHuman: true},
		{name: "human attention without a produced change", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Choose the storage architecture", wantHuman: true},
		{name: "human attention without final text", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, wantHuman: true, noUsage: true},
		{name: "human attention with synthetic unchanged change", change: &runpkg.NativeChange{}, states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", wantHuman: true},
		{name: "human attention with failed producer and no change", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", runErr: deliveryErr, wantHuman: true},
		{name: "human attention retains instance workspace failure", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", runErr: errors.Join(deliveryErr, runpkg.ErrWorkspacePreparation)},
		{name: "human attention retains checkpoint failure", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", runErr: errors.Join(deliveryErr, errors.New("checkpoint persistence failed")), wantState: "In Review"},
		{name: "human attention retains lease failure", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", runErr: errors.Join(deliveryErr, errors.New("native execution lease lost")), wantState: "In Review"},
		{name: "human attention retains session failure", states: workflow, finalState: runpkg.FinalStateNeedsHumanAttention, finalMessage: "Approve the migration", runErr: errors.Join(deliveryErr, errors.New("session persistence failed")), wantState: "In Review"},
		{name: "human attention defers a refused lane write", states: workflow, finalMessage: "May I merge?", updateErr: errors.New("stale fencing token"), wantDeferred: true},
		{name: "unchanged inspection with typed acceptance ends the work", change: &runpkg.NativeChange{BaseSHA: head}, states: workflow, finalMessage: "Inspection verified the requested behavior.\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantState: "Done", wantComment: "nothing to review"},
		{name: "native26 missing acceptance report preserves review", change: &runpkg.NativeChange{BaseSHA: head, HeadSHA: head}, states: workflow, finalMessage: "Full parity remains blocked. No source changes were made or staged.\n\n- Five capabilities remain pending: credit checkout, auto-funding, invitation grants, invitation editing, and resend.\n- Inventory validation fails on 37 uncovered and 13 stale source sites, including Cloud attachments.\n- Focused MCP, operator, and dashboard regressions passed. Hosted tests were blocked by sandbox loopback restrictions.\n- Node 24 make app and the configured true gate passed.\n\nThe native owner needs focused follow-ups for these gaps before final parent conformance can pass.", wantState: "In Review", wantComment: "no valid complete detent-status disposition", roundTrip: true},
		{name: "empty legacy unchanged report preserves review", change: &runpkg.NativeChange{BaseSHA: head}, states: workflow, wantState: "In Review", wantComment: "issue acceptance is not recorded"},
		{name: "typed unfinished unchanged work preserves review", change: &runpkg.NativeChange{BaseSHA: head}, states: workflow, finalMessage: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```", wantState: "In Review", wantComment: "detent-status in_progress"},
		{name: "native272 clean instance report reuses completion owner", change: &runpkg.NativeChange{BaseSHA: head, HeadSHA: head}, states: blockedLanding, humanReview: &no, finalMessage: instanceReport, wantInstance: true, roundTrip: true},
		{name: "native instance report preserves authentic published source", change: accepted, states: unfinishedWorkflow, sourceState: "Rework", finalMessage: instanceReport, wantInstance: true},
		{name: "native instance report cannot bypass publication refusal", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionError: "policy mismatch"}, states: blockedLanding, humanReview: &no, finalMessage: instanceReport, wantState: "In Review", wantComment: "for review"},
		{name: "native273 malformed predicate retains review", change: &runpkg.NativeChange{BaseSHA: head}, states: blockedLanding, humanReview: &no, finalMessage: strings.Replace(instanceReport, "    reason:", "    predicate: instance_available\n    reason:", 1), wantState: "In Review", wantComment: "no valid complete detent-status disposition"},
		{name: "instance and human action retain human owner", change: accepted, states: blockedLanding, humanReview: &no, finalMessage: strings.Replace(instanceReport, "human_action: null", "human_action: Approve the exception", 1), wantState: "Blocked", wantHuman: true},
		{name: "instance and external blocker retain review", change: accepted, states: blockedLanding, humanReview: &no, finalMessage: strings.Replace(instanceReport, "human_action: null", "  - ref: '#42'\n    reason: Await dependency\nhuman_action: null", 1), wantState: "In Review", wantComment: "for review"},
		{name: "malformed status report cannot accept unchanged work", change: &runpkg.NativeChange{BaseSHA: head}, states: workflow, finalMessage: "```detent-status\nschema: 99\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantState: "In Review", wantComment: "no valid complete detent-status disposition"},
		{name: "ordinary code fence cannot accept unchanged work", change: &runpkg.NativeChange{BaseSHA: head}, states: workflow, finalMessage: "```text\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantState: "In Review", wantComment: "no valid complete detent-status disposition"},
		{name: "past incident prose cannot override current typed acceptance", change: &runpkg.NativeChange{BaseSHA: head}, states: workflow, finalMessage: "The previous run was blocked; inspection now verifies all acceptance.\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantState: "Done", wantComment: "nothing to review"},
		{name: "native26 unfinished reviewed source continues implementation", change: accepted, states: unfinishedWorkflow, humanReview: &no, finalMessage: unfinishedReport, wantState: "Rework", wantComment: "implementation remains unfinished", roundTrip: true},
		{name: "unfinished source continues before native review", change: waiting, states: unfinishedWorkflow, finalMessage: unfinishedReport, wantState: "Rework", wantComment: "issue acceptance is not recorded"},
		{name: "unfinished unchanged turn uses allowed implementation lane", change: &runpkg.NativeChange{BaseSHA: head}, states: unfinishedWorkflow, finalMessage: unfinishedReport, wantState: "Rework", wantComment: "further work"},
		{name: "unfinished Rework stays runnable without a self transition", change: accepted, states: unfinishedWorkflow, sourceState: "Rework", humanReview: &no, finalMessage: unfinishedReport, wantState: "Rework", wantSameState: true, wantComment: "implementation remains unfinished"},
		{name: "unfinished source respects configured rework name", change: accepted, states: customRework, reworkState: "Fixing", humanReview: &no, finalMessage: unfinishedReport, wantState: "Fixing", wantComment: "implementation remains unfinished"},
		{name: "disabled Rework keeps configured review", change: accepted, states: noDispatchRework, humanReview: &no, finalMessage: unfinishedReport, wantState: "In Review", wantComment: "for review"},
		{name: "operator only Rework keeps configured review", change: accepted, states: operatorRework, humanReview: &no, finalMessage: unfinishedReport, wantState: "In Review", wantComment: "for review"},
		{name: "disallowed Rework keeps configured review", change: accepted, states: rework, finalMessage: unfinishedReport, wantState: "In Review", wantComment: "for review"},
		{name: "in progress external blocker cannot dispatch Rework", change: accepted, states: unfinishedWorkflow, humanReview: &no, finalMessage: strings.ReplaceAll(unfinishedReport, "blockers: []", "blockers:\n  - ref: '#42'\n    reason: Await the dependent project"), wantState: "In Review", wantComment: "for review"},
		{name: "explicit external blocked report cannot dispatch Rework", change: accepted, states: unfinishedWorkflow, humanReview: &no, finalMessage: strings.ReplaceAll(unfinishedReport, "status: in_progress\nblockers: []", "status: blocked\nblockers:\n  - reason: Await external acceptance"), wantState: "In Review", wantComment: "detent-status blocked"},
		{name: "in progress human action remains held", change: accepted, states: unfinishedWorkflow, humanReview: &no, finalMessage: strings.ReplaceAll(unfinishedReport, "human_action: null", "human_action: Approve the consumer rollout"), wantState: "In Review", wantComment: "for review"},
		{name: "unfinished policy refusal cannot enter Rework", change: &runpkg.NativeChange{Changed: true, ChangeID: "change_1", VersionID: "version_1", HeadSHA: head, VersionError: "policy mismatch", VersionCode: "policy_mismatch"}, states: unfinishedWorkflow, humanReview: &no, finalMessage: unfinishedReport, wantState: "In Review", wantComment: "for review"},
		{name: "typed unfinished published source cannot auto land", change: accepted, states: landing, finalMessage: "```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```", wantState: "In Review", wantComment: "detent-status in_progress"},
		{name: "malformed published report cannot auto land", change: accepted, states: landing, finalMessage: "```detent-status\nschema: 99\nstatus: complete\n```", wantState: "In Review", wantComment: "no valid complete detent-status disposition"},
		{name: "typed accepted current version lands normally", change: accepted, states: landing, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", wantState: "Merging", wantComment: "runner lands it next"},
		{name: "publication outage retries current publisher", republish: true, change: &runpkg.NativeChange{Error: "hub unavailable"}, states: landing, wantDeferred: true},
		{name: "an unopened change is handed off, not reviewed", change: &runpkg.NativeChange{Changed: true, Error: "hub unavailable", HeadSHA: head, Files: 1}, states: workflow, wantDeferred: true},
		{name: "a workflow without the review lane is handed off, never ended", change: waiting, states: hosted, wantDeferred: true},
		{name: "a workflow without a terminal move is handed off", change: &runpkg.NativeChange{}, finalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```", states: []connector.WorkflowState{{Name: "In Progress", Dispatchable: true, Transitions: []string{"Blocked"}}, {Name: "Blocked"}}, wantDeferred: true},
		{name: "an unreadable workflow is handed off", change: waiting, statesErr: errors.New("hub unavailable"), wantDeferred: true},
		{name: "a refused lane write is handed off", change: waiting, states: workflow, updateErr: errors.New("stale fencing token"), wantDeferred: true},
		{name: "missing native result keeps ordinary continuation", states: workflow, wantOrdinary: true, wantContinue: true},
		{name: "dirty tracked native Code keeps ordinary continuation", states: workflow, wantOrdinary: true, wantContinue: true, diffStats: DiffStats{Status: "changed", FilesChanged: 1, TrackedPaths: []string{"source.go"}}},
		{name: "dirty untracked native Rework keeps ordinary continuation", states: rework, sourceState: "Rework", wantOrdinary: true, wantContinue: true, diffStats: DiffStats{Status: "changed", FilesChanged: 1, UntrackedPaths: []string{"source.go"}}},
		{name: "late host source conflict preserves progress", states: rework, sourceState: "Rework", wantOrdinary: true, wantContinue: true, diffStats: DiffStats{Status: "changed", FilesChanged: 1, TrackedPaths: []string{"docs/invariants.md"}, Fingerprint: "late-host-conflict", RecoveryStateExpected: true, RecoveryStateAvailable: true}},
		{name: "late host conflict after replay preserves unpublished progress", states: rework, sourceState: "Rework", wantOrdinary: true, wantContinue: true, diffStats: DiffStats{Status: "changed", FilesChanged: 1, TrackedPaths: []string{"docs/invariants.md"}, UnpushedCommits: 1, Fingerprint: "late-host-conflict", RecoveryStateExpected: true, RecoveryStateAvailable: true}},
		{name: "already paused conflict left unresolved remains failure", states: rework, sourceState: "Rework", runErr: fmt.Errorf("finalize native work: %w: unresolved source conflicts: docs/invariants.md", workspace.ErrMergeResolutionInvalid), wantState: "In Review", diffStats: DiffStats{Status: "changed", FilesChanged: 1, TrackedPaths: []string{"docs/invariants.md"}, Fingerprint: "existing-conflict", RecoveryStateExpected: true, RecoveryStateAvailable: true}},
		{name: "non-native final question keeps the ordinary path", finalMessage: "May I merge?", plain: true, wantContinue: true},
		{name: "a connector without a workflow keeps the ordinary path", change: &runpkg.NativeChange{}, plain: true, wantContinue: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !test.wantDirect {
				t.Parallel()
			}
			issue := completionTransitionIssue(firstNonBlank(test.sourceState, "In Progress"), "")
			tick := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}, updateErr: test.updateErr}
			var tracker connector.Connector = &nativeWorkflowConnector{autoPromoteTickConnector: tick, states: test.states, statesErr: test.statesErr, reviewed: test.reviewed}
			if test.plain {
				tracker = tick
			}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo", "In Progress", "Rework"}, TerminalStates: []string{"Done"}})
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
			}
			scheduling.release = func() {
				if test.wantState != "" && !test.wantSameState && !test.wantDirect && (len(tick.updates) != 1 || tick.updates[0].state != test.wantState) {
					t.Fatalf("claim released before lane settlement: updates=%v, want=%s", tick.updates, test.wantState)
				}
			}
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts, scheduling: scheduling}
			var journey *nativeLandingJourney
			landingGate := &countingProjectDispatchGate{}
			if test.wantDirect {
				journey = newNativeLandingJourney(t, issue, "", 200, false)
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
			state.Running[issue.ID] = Running{Issue: issue, Attempt: 1, WorkAttemptID: 42, Generation: 7, SessionID: "native-session", Tokens: TokenTotals{TotalTokens: 42}, Mode: runpkg.RunModeImplement, DispatchSourceState: issue.State, StartedAt: now.Add(-time.Minute)}
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: now.Add(-time.Minute)}
			finalState := test.finalState
			if finalState == "" {
				finalState = FinalStateCompleted
			}
			tokens := TokenTotals{TotalTokens: 42}
			if test.noUsage {
				tokens = TokenTotals{}
			}
			diffStats := test.diffStats
			if !diffStatsPresent(diffStats) {
				diffStats = DiffStats{Status: "clean", HeadSHA: head}
			}
			event := runpkg.Completion{
				IssueID: issue.ID, CompletedAt: now, Err: test.runErr,
				Request: runpkg.RunRequest{Mode: runpkg.RunModeImplement, WorkAttemptID: 42, Generation: 7},
				Result:  runpkg.RunResult{FinalState: finalState, FinalMessage: test.finalMessage, NativeChange: test.change, Tokens: tokens, DiffStats: diffStats, TurnStarted: test.wantInstance || test.diffStats.Fingerprint != ""},
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
			if test.wantInstance {
				if len(tick.updates) != 0 || len(tick.comments) != 0 || len(state.Blocked) != 0 || len(state.Retry) != 0 || len(state.Completed) != 0 || len(state.Claimed) != 0 || scheduling.releases != 1 || len(state.FailureBreaker.Failures) != 0 {
					t.Fatalf("instance report acquired issue completion effects: updates=%v blocked=%v retry=%v completed=%v claims=%v releases=%d", tick.updates, state.Blocked, state.Retry, state.Completed, state.Claimed, scheduling.releases)
				}
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalSuccess || attempts.completions[0].ErrorClass != "" {
					t.Fatalf("instance report lost provider success: %#v", attempts.completions)
				}
				var metadata struct {
					Evidence []telemetry.BlockerEvidence `json:"blocker_evidence"`
					ChangeID string                      `json:"native_change_id"`
				}
				if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &metadata); err != nil {
					t.Fatal(err)
				}
				if len(metadata.Evidence) != 1 || metadata.Evidence[0].Owner != workpad.BlockerOwnerInstance || metadata.Evidence[0].Reference != "instance:worker-loopback" || !strings.Contains(metadata.Evidence[0].Reason, "EPERM") || metadata.Evidence[0].RecordedAt == nil || !metadata.Evidence[0].RecordedAt.Equal(now) || metadata.ChangeID != test.change.ChangeID {
					t.Fatalf("instance or source evidence lost: %#v", metadata)
				}
				var metrics struct {
					Tokens int `json:"total_tokens"`
					Turns  int `json:"turns"`
				}
				if err := json.Unmarshal([]byte(attempts.completions[0].MetricsJSON), &metrics); err != nil || metrics.Tokens != 42 || metrics.Turns != 1 || state.TokenTotals.TotalTokens != 42 || state.DiffStats[issue.ID].HeadSHA != head {
					t.Fatalf("instance report lost genuine usage: %#v, %v", metrics, err)
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
			if test.republish {
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
			if continued := retried && !retry.CompletionDeferred; continued != test.wantContinue {
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
					if receipt.TerminalState != store.WorkAttemptTerminalSuccess || receipt.ErrorClass != "" || metadata.Progress.Reason != "workspace_diff_present_without_pull_request" || metadata.Progress.WorkspaceDiffStats.Fingerprint != test.diffStats.Fingerprint {
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
					found = found || strings.Contains(comment.body, test.wantReason) && strings.Contains(comment.body, test.wantSummary) && !strings.Contains(comment.body, "```detent-status")
				}
				if !found {
					t.Fatalf("comments lost reason %q or summary %q: %#v", test.wantReason, test.wantSummary, tick.comments)
				}
			}
			if test.wantHuman {
				blocked, ok := state.Blocked[issue.ID]
				if !ok || blocked.Reason != permissionWaitReason || blocked.Recovery.Owner != blockedRecoveryOwnerHuman || blocked.Recovery.WorkAttemptID != 42 {
					t.Fatalf("human outcome = %#v, present = %t", blocked, ok)
				}
				if _, completed := state.Completed[issue.ID]; completed || len(tick.updates) != 1 || tick.updates[0].state != "Blocked" {
					t.Fatalf("human attention completed = %t; updates = %#v", completed, tick.updates)
				}
				if _, claimed := state.Claimed[issue.ID]; claimed || scheduling.releases < 1 {
					t.Fatalf("claim retained = %t, releases = %d", claimed, scheduling.releases)
				}
				if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalNoProgress || attempts.completions[0].ErrorClass != permissionWaitReason {
					t.Fatalf("attempt completions = %#v", attempts.completions)
				}
				var metadata struct {
					PermissionWait permissionWaitRecord `json:"permission_wait"`
				}
				if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &metadata); err != nil {
					t.Fatal(err)
				}
				if metadata.PermissionWait.WorkAttemptID != 42 || metadata.PermissionWait.SessionID != "native-session" || metadata.PermissionWait.Question == "" {
					t.Fatalf("human outcome provenance = %#v", metadata.PermissionWait)
				}
				if state.TokenTotals.TotalTokens != 42 || state.DiffStats[issue.ID].HeadSHA != head {
					t.Fatalf("telemetry lost: tokens = %#v, diff = %#v", state.TokenTotals, state.DiffStats[issue.ID])
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
				if errors.Is(test.runErr, runpkg.ErrWorkerProcessReap) {
					receipt := attempts.completions[0]
					if receipt.ErrorClass != workAttemptErrorWorkspace || !allowanceInfrastructureAttempt(store.WorkAttempt{TerminalState: receipt.TerminalState, ErrorClass: receipt.ErrorClass, MetricsJSON: receipt.MetricsJSON}) {
						t.Fatal("native cleanup failure consumed issue failure allowance")
					}
				}
				if test.wantState != "" {
					if len(tick.updates) != 1 || tick.updates[0].state != test.wantState || retried || scheduling.releases != 1 || len(tick.comments) != 0 {
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
				for _, update := range tick.updates {
					if test.updateErr == nil {
						t.Fatalf("the item was moved: %#v", tick.updates)
					}
					if update.state != "In Review" && (test.finalMessage == "" || update.state != "Blocked") {
						t.Fatalf("refused write targeted %s", update.state)
					}
				}
				if test.wantDeferred && test.finalMessage == "" && len(tick.comments) != 0 {
					t.Fatalf("a handed-off item was commented on: %#v", tick.comments)
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
			if _, claimed := state.Claimed[issue.ID]; claimed || scheduling.releases != 1 {
				t.Fatalf("claim retained = %t, releases = %d", claimed, scheduling.releases)
			}
			completed, ok := state.Completed[issue.ID]
			if !test.wantSameState && (!ok || completed.Issue.State != test.wantState) || test.wantSameState && ok {
				t.Fatalf("completed = %#v, present = %t", completed, ok)
			}
			if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalSuccess {
				t.Fatalf("attempt completions = %#v", attempts.completions)
			}
			if attempts.completions[0].ErrorClass != "" || len(state.FailureBreaker.Failures) != 0 || terminalAttemptRetryableFailure(telemetry.WorkAttempt{TerminalState: string(attempts.completions[0].TerminalState), ErrorClass: attempts.completions[0].ErrorClass}) {
				t.Fatal("successful native review consumed failure allowance")
			}
			var metadata struct {
				ChangeID     string `json:"native_change_id"`
				VersionID    string `json:"native_version_id"`
				HeadSHA      string `json:"native_head_sha"`
				VersionError string `json:"native_version_error"`
				VersionCode  string `json:"native_version_code"`
			}
			if err := json.Unmarshal([]byte(attempts.completions[0].WorkerMetadataJSON), &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata.ChangeID != test.change.ChangeID || metadata.VersionID != test.change.VersionID || metadata.HeadSHA != test.change.HeadSHA {
				t.Fatalf("completion lost the published native identity: %+v, want %+v", metadata, test.change)
			}
			if metadata.VersionError != test.change.VersionError || metadata.VersionCode != test.change.VersionCode {
				t.Fatalf("completion lost publication diagnostic: %+v", metadata)
			}
			if state.Draining != test.draining || len(state.Running) != 0 {
				t.Fatalf("completion changed drain or started work: draining=%t, running=%+v", state.Draining, state.Running)
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

type nativeCompletionScheduling struct {
	*hubSchedulingSource
	release func()
}

func (s *nativeCompletionScheduling) ReleaseClaim(ctx context.Context, issueID, reason string) error {
	if s.release != nil {
		s.release()
	}
	return s.hubSchedulingSource.ReleaseClaim(ctx, issueID, reason)
}

type nativeCompletionPublisher struct {
	nativeLandingJourneyExecution
	change   *runpkg.NativeChange
	prepared int
}

func (p *nativeCompletionPublisher) PrepareFinish(context.Context, string, string) error {
	p.prepared++
	return nil
}

func (p *nativeCompletionPublisher) NativeChange() *runpkg.NativeChange {
	return p.change
}
