package nativechange

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/digitaldrywood/detent/internal/workpad"
)

func (e *nativeExecution) publicationEvidence(ctx context.Context) (string, []string, error) {
	if !e.ownsIssueCompletion() || e.preparedOutcome != "succeeded" {
		return "", nil, nil
	}
	client, lease := e.claim.source.NativeClient(), e.claim.lease
	attempt, err := client.NativeAttempt(ctx, lease.WorkItemID, e.data.AttemptID)
	if err != nil {
		return "", nil, fmt.Errorf("read publication attempt evidence: %w", err)
	}
	if attempt.AttemptID != e.data.AttemptID || attempt.LeaseID != lease.ID || attempt.FencingToken != lease.FencingToken {
		return "", nil, errors.New("publication evidence attempt authority mismatch")
	}
	var ids []string
	for _, evidence := range attempt.Evidence {
		ids = append(ids, evidence.AttachmentID)
	}
	var outcome string
	if d := e.preparedDisposition; d != nil && d.Status == workpad.StatusComplete && !d.Blockers && !d.HumanAction {
		outcome = d.FinalSummary
	}
	return outcome, mergeEvidenceIDs(nil, ids), nil
}

func mergeEvidenceIDs(previous, supplied []string) []string {
	ids := append(slices.Clone(previous), supplied...)
	slices.Sort(ids)
	return slices.Compact(ids)
}
