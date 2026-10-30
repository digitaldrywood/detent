package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const (
	permissionWaitReason         = "permission_wait"
	permissionWaitCriteriaDefect = "criteria_defect"
)

type permissionWaitRecord struct {
	Question      string                `json:"question"`
	Kind          string                `json:"kind"`
	Source        string                `json:"source"`
	WorkAttemptID int64                 `json:"work_attempt_id"`
	SessionID     string                `json:"session_id,omitempty"`
	Criterion     string                `json:"criterion,omitempty"`
	Options       []workpad.BlockOption `json:"options,omitempty"`
	BlockKind     string                `json:"block_kind"`
}

func terminalPermissionWait(output string) (permissionWaitRecord, bool) {
	if signal, ok := workpad.SignalFromComment(output, "", ""); ok && signal != nil && signal.Invalid == nil && signal.Status == workpad.StatusBlocked && strings.TrimSpace(signal.HumanAction) != "" {
		return permissionWaitRecord{Question: strings.TrimSpace(signal.HumanAction), Kind: signal.BlockKind, BlockKind: signal.BlockKind, Criterion: signal.Criterion, Options: signal.Options, Source: "terminal_detent_status"}, true
	}
	return permissionWaitRecord{}, false
}

func permissionWaitCompletionAllowed(event runpkg.Completion) bool {
	if event.Result.FinalState != "" && event.Result.FinalState != runpkg.FinalStateCompleted && event.Result.FinalState != runpkg.FinalStateNeedsHumanAttention {
		return false
	}
	if event.Err != nil {
		commandErr, deliveryFailure := runpkg.PullRequestDeliverableFailure(event.Err)
		if event.Result.FinalState != runpkg.FinalStateNeedsHumanAttention || !deliveryFailure || commandErr == nil {
			return false
		}
	}
	return true
}

func (o *Orchestrator) handlePermissionWaitCompletion(ctx context.Context, state *State, event runpkg.Completion, running Running) bool {
	if !permissionWaitCompletionAllowed(event) {
		return false
	}
	wait, ok := terminalPermissionWait(event.Result.FinalMessage)
	if !ok {
		return false
	}
	return o.parkPermissionWait(ctx, state, event, running, wait)
}

func (o *Orchestrator) parkPermissionWait(ctx context.Context, state *State, event runpkg.Completion, running Running, wait permissionWaitRecord) bool {
	wait.Question = o.operatorText(wait.Question)
	wait.WorkAttemptID = running.WorkAttemptID
	wait.SessionID = running.SessionID
	detail := fmt.Sprintf("%s (source: %s, work attempt: %d, session: %s, kind: %s)", wait.Question, wait.Source, wait.WorkAttemptID, wait.SessionID, wait.Kind)
	report, _ := workpad.SignalFromComment(event.Result.FinalMessage, "", "")
	detail = nativeCompletionReason(detail, report, event.Result.FinalMessage, nil)
	remedy := "Record direction for this question, then move the issue to Rework: " + wait.Question
	switch wait.Kind {
	case "missing_authorization":
		remedy = "The worker requested implementation permission. Check the assigned scope and record direction, preserving explicit approval gates, then move the issue to Rework: " + wait.Question
	case permissionWaitCriteriaDefect:
		remedy = wait.Question + "\nThen move the issue to Rework."
	}
	cause := permissionWaitReason
	if wait.Source == "terminal_detent_status" {
		cause = "human_action"
		remedy = wait.Question
	}
	if diffStatsPresent(event.Result.DiffStats) {
		running.DiffStats = event.Result.DiffStats
	}
	parkEvent := event
	parkEvent.Err = errors.New(detail)
	if !o.blockHumanOwnedWorkerFailure(ctx, state, parkEvent, running, cause, detail, remedy, "worker_permission_wait") {
		o.deferTrackerUnavailableCompletion(ctx, state, event, running, errors.New("persist permission-wait hold failed"))
		return true
	}
	tokens := event.Result.Tokens
	if tokens == (TokenTotals{}) {
		tokens = running.Tokens
	}
	state.TokenTotals = addTokenTotals(state.TokenTotals, tokens)
	if event.Result.RateLimits != nil {
		state.RateLimits = mergeRateLimits(state.RateLimits, event.Result.RateLimits)
	}
	if diffStatsPresent(running.DiffStats) {
		state.DiffStats[event.IssueID] = running.DiffStats
	}
	metadata := nativeLandingMetadata(event.Result.NativeLanding)
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata[cause] = wait
	o.completeDurableWorkAttemptWithMetadata(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalNoProgress, cause, detail, "blocked", wait.Question, metadata)
	return true
}
