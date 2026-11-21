package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const nativeLaneEntryHistoryPages = 50

type nativeLaneEntry struct {
	revision   string
	transition connector.IssueStateTransition
	found      bool
}

type nativeLaneEntries struct {
	mu      sync.Mutex
	entries map[tracker.NativeWorkItemID]nativeLaneEntry
}

// CurrentLaneEntry reads the item's latest workflow transition into its
// current lane. Lane moves change the item revision, so a revision-keyed cache
// avoids rereading history on every dispatch tick.
func (c *NativeConnector) CurrentLaneEntry(ctx context.Context, issue connector.Issue) (connector.IssueStateTransition, bool, error) {
	id := tracker.NativeWorkItemID(strings.TrimSpace(issue.ID))
	if c == nil || c.client == nil || id == "" {
		return connector.IssueStateTransition{}, false, nil
	}
	revision := issue.Metadata["hub_revision"]
	if cached, ok := c.laneEntries.lookup(id, revision); ok {
		return cached.transition, cached.found, nil
	}
	events, err := c.client.RecentWorkflowTransitions(ctx, id, 1)
	if err != nil {
		return connector.IssueStateTransition{}, false, err
	}
	var latest *tracker.CollaborationEvent
	if len(events) != 0 {
		latest = &events[0]
	}
	entry := nativeLaneEntry{revision: revision}
	if latest != nil && strings.EqualFold(strings.TrimSpace(latest.Data.ToState), strings.TrimSpace(issue.State)) {
		entry.found = true
		entry.transition = connector.IssueStateTransition{
			EnteredAt: latest.RecordedAt.UTC(),
			Actor:     connector.IssueActor{Kind: latest.Actor.Kind, Login: latest.Actor.PrincipalID},
		}
	}
	c.laneEntries.store(id, entry)
	return entry.transition, entry.found, nil
}

func (c *NativeClient) RecentWorkflowTransitions(ctx context.Context, id tracker.NativeWorkItemID, limit int) ([]tracker.CollaborationEvent, error) {
	path, err := NativeItemPath(id)
	if err != nil {
		return nil, err
	}
	var transitions []tracker.CollaborationEvent
	err = c.eachWorkflowTransition(ctx, path, func(event tracker.CollaborationEvent) bool {
		transitions = append(transitions, event)
		return len(transitions) == limit
	})
	return transitions, err
}

func (c *NativeClient) planAnswer(ctx context.Context, id tracker.NativeWorkItemID, attemptID string) (tracker.CollaborationEvent, error) {
	path, err := NativeItemPath(id)
	if err != nil {
		return tracker.CollaborationEvent{}, err
	}
	params := url.Values{"blocker_attempt_id": {attemptID}}
	var page tracker.Page[tracker.CollaborationEvent]
	err = c.client.request(ctx, http.MethodGet, c.base()+path+"/history?"+params.Encode(), nil, &page)
	for _, event := range page.Items {
		if event.Actor.Kind == "human" && event.Type == "workflow.transitioned" && event.Data.BlockerAttemptID == attemptID && event.Data.ToState != "" && strings.EqualFold(event.Data.FromState, "Blocked") && !strings.EqualFold(event.Data.ToState, "Blocked") {
			return event, err
		}
	}
	return tracker.CollaborationEvent{}, err
}

func (c *NativeClient) eachWorkflowTransition(ctx context.Context, path string, found func(tracker.CollaborationEvent) bool) error {
	cursor := ""
	for range nativeLaneEntryHistoryPages {
		params := url.Values{"view": {"timeline"}, "direction": {"newest"}, "limit": {"200"}, "cursor": {cursor}}
		var page tracker.Page[tracker.CollaborationEvent]
		if err := c.client.request(ctx, http.MethodGet, c.base()+path+"/history?"+params.Encode(), nil, &page); err != nil {
			return err
		}
		for _, event := range page.Items {
			if event.Type == "workflow.transitioned" && found(event) {
				return nil
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
	return errors.New("current lane entry history is incomplete")
}

func (e *nativeLaneEntries) lookup(id tracker.NativeWorkItemID, revision string) (nativeLaneEntry, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.entries[id]
	return entry, ok && revision != "" && entry.revision == revision
}

func (e *nativeLaneEntries) store(id tracker.NativeWorkItemID, entry nativeLaneEntry) {
	if entry.revision == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.entries == nil {
		e.entries = make(map[tracker.NativeWorkItemID]nativeLaneEntry)
	}
	e.entries[id] = entry
}

func (c *NativeClient) ResolvePlan(ctx context.Context, recovery tracker.NativeRecovery, pinnedPolicy policy.Descriptor, receipts []tracker.NativePhaseReceipt) (*tracker.NativePlanState, error) {
	item := recovery.Issue.WorkItemID
	result := &tracker.NativePlanState{}
	humanReview := pinnedPolicy.Gates.PlanReview == gate.PlanReviewHuman || pinnedPolicy.Gates.PlanReview == gate.PlanReviewBoth
	index := slices.IndexFunc(receipts, func(receipt tracker.NativePhaseReceipt) bool { return tracker.IsPlanPhase(receipt.Phase) })
	if index < 0 {
		if humanReview {
			return result, nil
		}
		if len(receipts) != 0 || recovery.Change != nil {
			return nil, nil
		}
		if source := recovery.SourceAttempt(); source != nil && source.Checkpoint.PreservesSource() && source.Identity != nil {
			switch source.Identity.Role {
			case runner.RoleCode, runner.RoleRework:
				return nil, nil
			}
		}
		return result, nil
	}
	pinned := receipts[index]
	if pinned.PolicyID != pinnedPolicy.ID {
		return result, nil
	}
	owner, err := c.planAttempt(ctx, recovery, pinned.AttemptID, pinned.Phase == "plan_draft")
	if err != nil {
		return nil, err
	}
	if owner.LeaseID != pinned.LeaseID || owner.FencingToken != pinned.FencingToken || owner.PolicyID != pinned.PolicyID {
		return nil, errors.New("plan receipt does not match its attempt")
	}
	direction := tracker.PlanIssueDigest(recovery)
	for _, step := range owner.Steps {
		if step.Step != "plan" || step.Source.HeadSHA != pinned.HeadSHA || step.Source.WorkspaceDigest != pinned.TreeSHA ||
			step.PolicyID != pinned.PolicyID || step.ReviewState != direction || step.CompletionBody == "" ||
			tracker.PlanPhaseDigest(pinned, step) != pinned.InputDigest {
			continue
		}
		if pinned.Phase == "plan" {
			var accepted bool
			switch pinnedPolicy.Gates.PlanReview {
			case gate.PlanReviewAutomated:
				accepted = strings.HasPrefix(step.PlanApproval, "automated:")
			case gate.PlanReviewHuman, gate.PlanReviewBoth:
				accepted, err = c.hasHumanPlanApproval(ctx, recovery, pinned, step, receipts[index+1:])
				if err != nil {
					return nil, err
				}
			}
			if accepted {
				result.Draft, result.InputDigest, result.Accepted = step.CompletionBody, pinned.InputDigest, true
			}
			return result, nil
		}
		result.Draft = step.CompletionBody
		if pinned.Phase == "plan_review" {
			result.ReviewPending, result.Findings = true, step.PlanApproval
			return result, nil
		}
		if step.PlanApproval != "" || !humanReview {
			return &tracker.NativePlanState{}, nil
		}
		if owner.CompletionBody == "" {
			return result, nil
		}
		if !isPlanApprovalQuestion(owner, step.CompletionBody) {
			return &tracker.NativePlanState{}, nil
		}
		answer, err := c.planAnswer(ctx, item, owner.AttemptID)
		if err != nil {
			return nil, fmt.Errorf("read plan approval answer: %w", err)
		}
		if answer.ID != "" {
			result.Answer, result.AnswerID, result.AnswerAttemptID = strings.TrimSpace(answer.Data.ReasonDetail), answer.ID, owner.AttemptID
		}
		return result, nil
	}
	return result, nil
}

func (c *NativeClient) planAttempt(ctx context.Context, recovery tracker.NativeRecovery, attemptID string, completion bool) (tracker.NativeAttempt, error) {
	for _, attempt := range recovery.Attempts {
		if attempt.AttemptID == attemptID && len(attempt.Steps) != 0 && (!completion || attempt.CompletionBody != "") {
			return attempt, nil
		}
	}
	attempt, err := c.NativeAttempt(ctx, recovery.Issue.WorkItemID, attemptID)
	if err != nil {
		return tracker.NativeAttempt{}, fmt.Errorf("read plan receipt attempt: %w", err)
	}
	if attempt.AttemptID != attemptID {
		return tracker.NativeAttempt{}, errors.New("plan receipt does not match its attempt")
	}
	return attempt, nil
}

func isPlanApprovalQuestion(attempt tracker.NativeAttempt, plan string) bool {
	return attempt.Status == "succeeded" &&
		attempt.CompletionBody == workpad.QuestionDetail(plan, tracker.PlanApprovalQuestion) &&
		workpad.PlainSummary(attempt.CompletionBody) == plan
}

func (c *NativeClient) hasHumanPlanApproval(ctx context.Context, recovery tracker.NativeRecovery, pinned tracker.NativePhaseReceipt, step tracker.NativeStepReceipt, earlier []tracker.NativePhaseReceipt) (bool, error) {
	provenance, ok := strings.CutPrefix(step.PlanApproval, "human:")
	attemptID, answerID, found := strings.Cut(provenance, ":")
	if !ok || !found || attemptID == "" || answerID == "" {
		return false, nil
	}
	legacy := strings.EqualFold(answerID, "approve plan")
	draftStep := step
	draftStep.PlanApproval = ""
	var receipt tracker.NativePhaseReceipt
	if legacy {
		answerID, attemptID = attemptID, ""
		for _, candidate := range earlier {
			if candidate.Phase == "plan_draft" && candidate.HeadSHA == pinned.HeadSHA && candidate.TreeSHA == pinned.TreeSHA &&
				candidate.PolicyID == pinned.PolicyID && candidate.InputDigest == tracker.PlanPhaseDigest(candidate, draftStep) {
				receipt, attemptID = candidate, candidate.AttemptID
				break
			}
		}
	}
	if attemptID == "" {
		return false, nil
	}
	draft, err := c.planAttempt(ctx, recovery, attemptID, true)
	if err != nil {
		var refusal *APIError
		if errors.Is(err, operatortool.ErrInvalidArguments) || errors.As(err, &refusal) && refusal.Status == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	if draft.PolicyID != pinned.PolicyID || !isPlanApprovalQuestion(draft, step.CompletionBody) ||
		legacy && (draft.LeaseID != receipt.LeaseID || draft.FencingToken != receipt.FencingToken) ||
		!slices.ContainsFunc(draft.Steps, func(candidate tracker.NativeStepReceipt) bool {
			return candidate.Step == draftStep.Step && candidate.Source.HeadSHA == draftStep.Source.HeadSHA &&
				candidate.Source.WorkspaceDigest == draftStep.Source.WorkspaceDigest && candidate.PolicyID == draftStep.PolicyID &&
				candidate.ReviewState == draftStep.ReviewState && candidate.CompletionBody == draftStep.CompletionBody && candidate.PlanApproval == ""
		}) {
		return false, nil
	}
	answer, err := c.planAnswer(ctx, recovery.Issue.WorkItemID, draft.AttemptID)
	if err != nil {
		return false, fmt.Errorf("read plan approval answer: %w", err)
	}
	return answer.ID == answerID && strings.EqualFold(strings.TrimSpace(answer.Data.ReasonDetail), "approve plan"), nil
}
