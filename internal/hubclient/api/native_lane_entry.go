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
	var answer tracker.CollaborationEvent
	err = c.eachWorkflowTransition(ctx, path, func(event tracker.CollaborationEvent) bool {
		if event.Actor.Kind == "human" && event.Data.BlockerAttemptID == attemptID && event.Data.ToState != "" && strings.EqualFold(event.Data.FromState, "Blocked") && !strings.EqualFold(event.Data.ToState, "Blocked") {
			answer = event
			return true
		}
		return false
	})
	return answer, err
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

func (c *NativeClient) ResolvePlan(ctx context.Context, recovery tracker.NativeRecovery, policyID string, receipts []tracker.NativePhaseReceipt) (*tracker.NativePlanState, error) {
	item := recovery.Issue.WorkItemID
	if len(receipts) == 0 {
		if recovery.Change != nil {
			return nil, nil
		}
		if source := recovery.SourceAttempt(); source != nil {
			if source.LeaseID == recovery.Lease.ID && source.Runtime != nil && source.Runtime.Phase == "planning" {
				return &tracker.NativePlanState{}, nil
			}
			if source.Identity != nil && source.Identity.Role == "plan" && source.Status == "succeeded" {
				return &tracker.NativePlanState{}, nil
			}
			if source.Checkpoint.PreservesSource() {
				return nil, nil
			}
		}
		if !strings.EqualFold(recovery.Issue.State, "Todo") {
			workflow, err := c.RecentWorkflowTransitions(ctx, item, 1)
			if err != nil {
				return nil, err
			}
			if len(workflow) == 0 || workflow[0].Actor.Kind != "runner" || !strings.EqualFold(workflow[0].Data.FromState, "Todo") || !strings.EqualFold(workflow[0].Data.ToState, recovery.Issue.State) {
				return nil, nil
			}
		}
		return &tracker.NativePlanState{}, nil
	}

	index := slices.IndexFunc(receipts, func(receipt tracker.NativePhaseReceipt) bool { return tracker.IsPlanPhase(receipt.Phase) })
	if index < 0 {
		return nil, nil
	}
	pinned := receipts[index]
	result := &tracker.NativePlanState{}
	if pinned.PolicyID != policyID {
		return result, nil
	}
	var owner tracker.NativeAttempt
	for _, attempt := range recovery.Attempts {
		if attempt.AttemptID == pinned.AttemptID {
			owner = attempt
			break
		}
	}
	if len(owner.Steps) == 0 || pinned.Phase == "plan_draft" && owner.CompletionBody == "" {
		var err error
		owner, err = c.NativeAttempt(ctx, item, pinned.AttemptID)
		if err != nil {
			return nil, fmt.Errorf("read plan receipt attempt: %w", err)
		}
	}
	if owner.AttemptID != pinned.AttemptID || owner.LeaseID != pinned.LeaseID || owner.FencingToken != pinned.FencingToken || owner.PolicyID != pinned.PolicyID {
		return nil, errors.New("plan receipt does not match its attempt")
	}
	for _, step := range owner.Steps {
		if step.Step != "plan" || step.Source.HeadSHA != pinned.HeadSHA || step.Source.WorkspaceDigest != pinned.TreeSHA ||
			step.PolicyID != policyID || step.ReviewState != tracker.PlanIssueDigest(recovery) || step.CompletionBody == "" ||
			tracker.PlanPhaseDigest(pinned, step) != pinned.InputDigest {
			continue
		}
		if pinned.Phase == "plan" {
			if step.PlanApproval != "" {
				result.Draft, result.InputDigest, result.Accepted = step.CompletionBody, pinned.InputDigest, true
			}
			return result, nil
		}
		result.Draft = step.CompletionBody
		if pinned.Phase == "plan_review" {
			result.ReviewPending, result.Findings = true, step.PlanApproval
			return result, nil
		}
		signal, ok := workpad.SignalFromComment(owner.CompletionBody, "", "")
		if owner.Status != "succeeded" || !ok || signal.Invalid != nil || signal.HumanAction != tracker.PlanApprovalQuestion {
			return result, nil
		}
		answer, err := c.planAnswer(ctx, item, owner.AttemptID)
		if err != nil {
			return nil, fmt.Errorf("read plan approval answer: %w", err)
		}
		result.Answer, result.AnswerID = strings.TrimSpace(answer.Data.ReasonDetail), answer.ID
		return result, nil
	}
	return result, nil
}
