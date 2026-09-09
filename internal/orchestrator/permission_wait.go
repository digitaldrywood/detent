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

const permissionWaitReason = "permission_wait"

type permissionWaitRecord struct {
	Question      string `json:"question"`
	Kind          string `json:"kind"`
	Source        string `json:"source"`
	WorkAttemptID int64  `json:"work_attempt_id"`
	SessionID     string `json:"session_id,omitempty"`
}

func terminalPermissionWait(output string) (permissionWaitRecord, bool) {
	if signal, ok := workpad.SignalFromComment(output, "", ""); ok && signal != nil && signal.Invalid == nil && signal.Status == workpad.StatusBlocked && strings.TrimSpace(signal.HumanAction) != "" {
		return permissionWaitRecord{Question: strings.TrimSpace(signal.HumanAction), Kind: "approval_or_input", Source: "terminal_detent_status"}, true
	}
	inFence := false
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		lower := strings.ToLower(line)
		questionEnd := strings.IndexByte(line, '?')
		if questionEnd < 0 {
			continue
		}
		request := false
		for _, prefix := range []string{"may i ", "can i ", "do you authorize ", "do you approve ", "will you approve "} {
			request = request || strings.HasPrefix(lower, prefix)
		}
		if !request {
			continue
		}
		question := line[:questionEnd+1]
		kind := "approval_or_input"
		lower = strings.ToLower(question)
		if (strings.HasPrefix(lower, "may i ") || strings.HasPrefix(lower, "can i ")) &&
			(strings.Contains(lower, "implement") || strings.Contains(lower, "edit") || strings.Contains(lower, "changes for #")) {
			kind = "implementation_permission"
		}
		for _, gate := range []string{"product", "architecture", "access", "withdraw", "deploy", "publish", "production", "delete", "credential", "merge"} {
			if strings.Contains(lower, gate) {
				kind = "approval_or_input"
			}
		}
		return permissionWaitRecord{Question: question, Kind: kind, Source: "terminal_assistant_question"}, true
	}
	return permissionWaitRecord{}, false
}

func (o *Orchestrator) handlePermissionWaitCompletion(ctx context.Context, state *State, event runpkg.Completion, running Running) bool {
	if event.Err != nil || (event.Result.FinalState != "" && event.Result.FinalState != runpkg.FinalStateCompleted && event.Result.FinalState != runpkg.FinalStateNeedsHumanAttention) {
		return false
	}
	wait, ok := terminalPermissionWait(event.Result.FinalMessage)
	if !ok {
		return false
	}
	wait.Question = o.operatorText(wait.Question)
	wait.WorkAttemptID = running.WorkAttemptID
	wait.SessionID = running.SessionID
	detail := fmt.Sprintf("%s (source: %s, work attempt: %d, session: %s, kind: %s)", wait.Question, wait.Source, wait.WorkAttemptID, wait.SessionID, wait.Kind)
	remedy := "Record direction for this question, then move the issue to Rework: " + wait.Question
	if wait.Kind == "implementation_permission" {
		remedy = "The worker requested implementation permission. Check the assigned scope and record direction, preserving explicit approval gates, then move the issue to Rework: " + wait.Question
	}
	parkEvent := event
	parkEvent.Err = errors.New(detail)
	if !o.blockHumanOwnedWorkerFailure(ctx, state, parkEvent, running, permissionWaitReason, detail, remedy, "worker_permission_wait") {
		o.deferTrackerUnavailableCompletion(ctx, state, event, running, errors.New("persist permission-wait hold failed"))
		return true
	}
	o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalNoProgress, nil, permissionWaitReason, detail)
	o.completeDurableWorkAttemptWithMetadata(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalNoProgress, permissionWaitReason, detail, "blocked", wait.Question, map[string]any{permissionWaitReason: wait})
	return true
}
