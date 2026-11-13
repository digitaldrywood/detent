package hubserver

import (
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/changerules"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const landingLane = changerules.LandingLane

type nativeReleaseDeliveryDelegation = changerules.ReleaseDeliveryDelegation

func landingTarget(project tracker.NativeProject, current string) string {
	return changerules.LandingTarget(project, current)
}
func terminalTarget(project tracker.NativeProject, current string) string {
	return changerules.TerminalTarget(project, current)
}
func reworkTarget(project tracker.NativeProject, current string) string {
	return changerules.ReworkTarget(project, current)
}
func reviewInstructions(change tracker.ChangeRequest, review tracker.ChangeReview) string {
	return changerules.ReviewInstructions(change, review)
}
func nativeChangeLandingReady(state string, detail *tracker.ChangeDetail) bool {
	return changerules.LandingReady(state, detail)
}
func nativeChangeLandingClaimable(state string, detail *tracker.ChangeDetail) bool {
	return changerules.LandingClaimable(state, detail)
}
func (s *Service) landChange(c echo.Context) error { return s.landChangeRecord(c) }

func validateChangeVersion(input tracker.ChangeVersionInput) error {
	return hubapi.FromFeatureError(changerules.ValidateVersion(input))
}
func validateChangeVersionEvidence(scope hubscope.NativeScope, item string, request tracker.PublishChangeVersion) error {
	return hubapi.FromFeatureError(changerules.ValidateVersionEvidence(scope.Organization, scope.Project, item, request))
}
func securityAuditRejection(message, field string) error {
	return hubapi.FromFeatureError(changerules.SecurityAuditRejection(message, field))
}
func securityAuditEvaluationField(evaluation securityaudit.Evaluation) string {
	return changerules.SecurityAuditEvaluationField(evaluation)
}

func validateReleasePick(r tracker.AssociateReleasePick) error {
	return hubapi.FromFeatureError(changerules.ValidateReleasePick(r))
}
func validatePreparedReleasePick(r tracker.ReleasePickInput, revision tracker.Revision) error {
	return hubapi.FromFeatureError(changerules.ValidatePreparedReleasePick(r, revision))
}
func sameReleasePickDelivery(prior, next tracker.ReleasePickInput) bool {
	return changerules.SameReleasePickDelivery(prior, next)
}
func releaseDeliverySectionsMatch(body, approved string) bool {
	return changerules.ReleaseDeliverySectionsMatch(body, approved)
}
func approveReleaseDeliveryContract(issue *tracker.NativeIssue, approved policy.Descriptor, now time.Time) error {
	return changerules.ApproveReleaseDeliveryContract(issue, approved, now)
}
