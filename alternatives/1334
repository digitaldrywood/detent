package nativechange

import (
	"context"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type versionAuthority struct{ execution *nativeExecution }

func (a versionAuthority) Retain(ctx context.Context, diff tracker.AttemptDiffRequest) error {
	return a.execution.retainChangeSource(ctx, diff)
}
func (a versionAuthority) Retained() *tracker.ChangeSourceCapture { return a.execution.retainedSource }
func (a versionAuthority) SetDiscussion(discussion []tracker.NativeComment) {
	a.execution.stepDiscussion = discussion
}
func (a versionAuthority) ApplyAuditHumanAction(audit *tracker.SecurityAuditReview) {
	a.execution.applySecurityAuditHumanAction(audit)
}
