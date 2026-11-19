package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/changerules"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type validatorReviewEvidence struct {
	issue                tracker.NativeIssue
	sourceSession        int64
	sourceMachine        string
	current              tracker.NativeRunData
	reviews              []tracker.ChangeReview
	auditDispositions    []securityaudit.Disposition
	auditDispositionRows []eventstore.AuditDisposition
}

func validateValidatorReviewEvidence(scope hubscope.NativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, request tracker.ReviewChange, evidence validatorReviewEvidence) (tracker.MachineID, error) {
	return changerules.ValidateValidatorReviewEvidence(scope, change, version, request, changerules.ValidatorReviewEvidence{
		Issue:             evidence.issue,
		SourceSession:     evidence.sourceSession,
		SourceMachine:     evidence.sourceMachine,
		Current:           evidence.current,
		Reviews:           evidence.reviews,
		AuditDispositions: evidence.auditDispositions,
	}, nativeRejection)
}
