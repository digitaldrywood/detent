package scenariotest

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (h *nativeChangeHub) latestRuntimeEvidence(t *testing.T, item tracker.NativeWorkItemID) (tracker.NativeRuntimeEvidence, error) {
	t.Helper()
	recovery, err := h.admin.Recovery(t.Context(), item)
	if err != nil {
		return tracker.NativeRuntimeEvidence{}, err
	}
	if len(recovery.Attempts) == 0 {
		return tracker.NativeRuntimeEvidence{}, errors.New("fixture has no recorded attempt")
	}
	evidence, err := h.admin.RuntimeEvidence(t.Context(), item, recovery.Attempts[len(recovery.Attempts)-1].AttemptID)
	if err != nil {
		return evidence, err
	}
	evidence.Change = recovery.ChangeDetail
	history, err := h.admin.History(t.Context(), item, "")
	if err != nil {
		return evidence, err
	}
	for _, event := range history.Items {
		switch event.Type {
		case "workflow.transitioned":
			if evidence.LatestTransition == nil || event.AggregateSequence > evidence.LatestTransition.AggregateSequence {
				copy := event
				evidence.LatestTransition = &copy
			}
		case "scheduler.decision":
			if evidence.LatestDecision == nil || event.AggregateSequence > evidence.LatestDecision.AggregateSequence {
				copy := event
				evidence.LatestDecision = &copy
			}
		}
	}
	return evidence, nil
}
