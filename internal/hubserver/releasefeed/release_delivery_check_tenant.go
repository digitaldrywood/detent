package releasefeed

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/changerules"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Delivery) readReleaseDeliveryRows(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, item string, request tracker.AssociateReleasePick, publishing bool) (releaseDeliveryRows, error) {
	var rows releaseDeliveryRows
	var err error
	rows.project, err = r.ProjectRecord(ctx, scope.Project)
	if err != nil {
		return rows, err
	}
	if rows.project.Core.DeletedAt != nil {
		return rows, corestore.ErrNotFound
	}
	rows.checkoutVerified, err = r.RunnerCheckoutVerified(ctx, scope.Project, rows.project.Settings.CheckoutRepository, s.deps.Clock())
	if err != nil {
		return rows, err
	}
	rows.current, err = r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
	if err != nil {
		return rows, err
	}
	rows.version, err = r.CollaborationVersion(ctx, item, rows.current.Revision)
	if err != nil {
		return rows, err
	}
	deliveryReader, ok := r.(eventstore.ReleaseDeliveryReader)
	if !ok {
		return rows, hubapi.NativeWorkItemStorageUnavailable()
	}
	rows.deliveryRow, err = deliveryReader.ReleaseDelivery(ctx, scope.Project, tracker.NativeWorkItemID(item))
	rows.found = err == nil
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return rows, err
	}
	facts, ok := r.(eventstore.ReleaseDeliveryFactsReader)
	if !ok {
		return rows, hubapi.NativeWorkItemStorageUnavailable()
	}
	rows.facts, err = facts.ReleaseDeliveryFacts(ctx, scope.Project, tracker.NativeWorkItemID(item), request.ReleasePickInput)
	if err != nil {
		return rows, err
	}
	rows.lease, err = r.ActiveLeaseForIssue(ctx, rows.current.ID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return rows, err
	}
	rows.source, err = r.WorkItem(ctx, scope.Project, request.SourceIssueID)
	if err != nil {
		return rows, err
	}
	rows.change, err = r.Change(ctx, request.SourceChangeID)
	if err != nil {
		return rows, err
	}
	if rows.change.ProjectID != scope.Project || rows.change.WorkItemID != request.SourceIssueID {
		return rows, corestore.ErrNotFound
	}
	if rows.change.CurrentVersionID == nil || rows.change.LandedVersionID == nil || *rows.change.CurrentVersionID != *rows.change.LandedVersionID || rows.change.LandedMergeSHA == nil || rows.change.LandedBaseRef == nil {
		return rows, hubapi.NativePublicInvalid("Source dependency must have a recorded merged develop Change")
	}
	rows.sourceVersion, err = r.Version(ctx, request.SourceChangeID, *rows.change.LandedVersionID)
	if err != nil {
		return rows, err
	}
	rows.prior, err = r.LatestChange(ctx, scope.Project, tracker.NativeWorkItemID(item))
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return rows, err
	}
	rows.policy, err = changerules.ReadCurrentChangePolicyRows(ctx, r, scope)
	if err != nil {
		return rows, err
	}
	if publishing {
		rows.reviews, err = facts.ReleaseSourceReviews(ctx, rows.change.ID, rows.sourceVersion.ID)
		if err != nil {
			return rows, err
		}
		rows.attempt.Attempt, err = r.LatestAttempt(ctx, rows.current.ID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return rows, err
		}
		attempt := rows.attempt.Attempt
		if attempt.ID != "" && attempt.Status == "succeeded" && attempt.WorkItemRevision >= rows.current.Revision && attempt.DispatchGeneration >= rows.current.DispatchGeneration {
			rows.attempt.Lease, err = r.Lease(ctx, attempt.LeaseID)
			if err != nil {
				return rows, err
			}
			if err = hubrecords.ReadTenantAttemptPayload(ctx, r, &rows.attempt); err != nil {
				return rows, err
			}
			if rows.attempt.Payload == nil {
				rows.attempt.Event, err = r.LatestRunEvent(ctx, scope.Project, attempt.ID)
				if err != nil {
					return rows, err
				}
			}
		}
	}
	return rows, nil
}

func (s *Delivery) prepareReleaseDeliveryCheck(ctx context.Context, scope hubscope.NativeScope, item string, request tracker.AssociateReleasePick, now time.Time, publishing bool) (releaseDeliveryCheck, error) {
	plan := releaseDeliveryCheck{request: request, publishing: publishing}
	if s.deps.Repository == nil {
		return plan, hubapi.NativeWorkItemStorageUnavailable()
	}
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeReleaseScope(ctx, r, scope); err != nil {
			return err
		}
		var err error
		plan.releaseDeliveryRows, err = s.readReleaseDeliveryRows(ctx, r, scope, item, request, publishing)
		return err
	})
	if err != nil {
		return plan, hubapi.ProjectRecordError(err)
	}
	plan.issue, err = s.deps.HydrateWorkItemRecord(ctx, plan.current, plan.version)
	if err != nil {
		return plan, err
	}
	stateFound := false
	for _, state := range plan.project.States {
		if state.Name == hubapi.StringValue(plan.current.WorkflowStateName) {
			plan.issue.Terminal, stateFound = state.Terminal, true
		}
	}
	if !stateFound {
		return plan, hubapi.NativePublicInvalid("Release delivery requires a current workflow state")
	}
	plan.existing = plan.deliveryRow.ReleaseDelivery
	content := eventstore.NewContent(s.deps.RunDetailStore())
	approved, err := s.deps.HydrateCurrentChangeVersionApproval(ctx, content, scope, plan.policy, "")
	if err != nil {
		return plan, err
	}
	plan.approval = approved
	if err = hubrecords.RequireNativeEdit(plan.issue, request.ExpectedRevision); err != nil {
		return plan, err
	}
	if publishing && request.PRNumber > 0 && !plan.found {
		return plan, hubapi.NativeInvalid("Existing PR publication requires an associated delivery")
	}
	if plan.found && (!s.deps.SameReleasePickDelivery(plan.existing.ReleasePickInput, request.ReleasePickInput) || plan.existing.ReleasePickInput != request.ReleasePickInput && (plan.existing.BaseSHA == request.BaseSHA || plan.existing.HeadSHA == request.HeadSHA)) {
		return plan, hubapi.NativePublicInvalid("Refresh must retain the release delivery identity and replace its exact base and head")
	}
	if plan.issue.Terminal || plan.issue.Archived || plan.issue.IgnoreDependencies || !plan.facts.DependsOnSource {
		return plan, hubapi.NativePublicInvalid("Delivery must be an unfinished dependent issue with dependency admission enabled")
	}
	if !plan.project.Core.RequireDependencies {
		return plan, hubapi.NativePublicInvalid("Release delivery requires project dependency admission")
	}
	project := tracker.NativeProject{States: plan.project.States}
	if plan.found && strings.EqualFold(plan.issue.State, changerules.LandingLane) {
		if target := changerules.ReworkTarget(project, plan.issue.State); target == "" || target == plan.issue.State {
			return plan, hubapi.NativePublicInvalid("Release refresh requires a workflow transition back to source capture")
		}
	}
	if plan.lease.LeaseID != "" && plan.lease.ReleasedAt == nil && plan.lease.ExpiresAt.After(now) {
		return plan, hubapi.NativeConflict(plan.issue.Revision)
	}
	terminal := false
	for _, state := range plan.project.States {
		if state.Name == hubapi.StringValue(plan.source.WorkflowStateName) {
			terminal = state.Terminal
		}
	}
	if !terminal || *plan.change.LandedBaseRef != "develop" || *plan.change.LandedMergeSHA != request.SourceMergeSHA {
		return plan, hubapi.NativePublicInvalid("Source dependency must have a recorded merged develop Change")
	}
	if !plan.checkoutVerified || !s.deps.ValidCheckoutRepository(plan.project.Settings.CheckoutRepository) || plan.project.Core.Profile != "native" || request.Repository != "https://github.com/"+plan.project.Settings.CheckoutRepository || plan.sourceVersion.Repository != request.Repository {
		return plan, hubapi.NativePublicInvalid("Source and release delivery must share the project's configured repository")
	}

	if plan.prior.ID != "" && (!plan.found || plan.prior.ID != plan.existing.ChangeID || plan.prior.LandedVersionID != nil) || plan.facts.ConflictingPR {
		return plan, hubapi.NativeConflict(plan.issue.Revision)
	}
	if err = s.checkReleasePublicationRules(ctx, scope, plan, approved); err != nil {
		return plan, err
	}
	return plan, nil
}

func (s *Delivery) recheckReleaseDelivery(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, plan releaseDeliveryCheck) error {
	if err := s.authorizeReleaseScope(ctx, w, scope); err != nil {
		return err
	}
	if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
		return err
	}
	if _, err := w.LockWorkItem(ctx, scope.Project, plan.current.NativeID); err != nil {
		return err
	}
	if _, err := w.LockChange(ctx, plan.change.ID); err != nil {
		return err
	}
	if err := s.deps.RequireCollaboration(ctx, w, s.deps.Clock()); err != nil {
		return err
	}
	current, err := s.readReleaseDeliveryRows(ctx, w, scope, string(plan.current.NativeID), plan.request, plan.publishing)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, plan.releaseDeliveryRows) {
		return hubapi.NativeConflict(tracker.Revision(current.current.Revision))
	}
	return nil
}

func (s *Delivery) authorizeReleaseScope(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope) error {
	if scope.Credential.Scope != hubapi.APIScopeOperator && scope.Credential.Scope != hubapi.APIScopeAdmin || scope.Credential.Runner.RunnerID != "" {
		return hubapi.NativeNotFound()
	}
	if err := s.deps.AuthorizeWorkItemRecordScope(ctx, r, scope); err != nil {
		return err
	}
	if scope.Credential.Hosted == nil {
		current, err := r.AccessCredential(ctx, scope.Credential.ID)
		if err != nil {
			return err
		}
		if current.Scope != string(scope.Credential.Scope) || current.NativeOnly != scope.Credential.NativeOnly {
			return hubapi.NativeNotFound()
		}
	}
	return nil
}

func ReadReleaseDeliveryContractFacts(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, item tracker.NativeWorkItemID, states []tracker.NativeState) (hubrecords.ReleaseDeliveryContractFacts, error) {
	var rows hubrecords.ReleaseDeliveryContractFacts
	delivery, err := r.ReleaseDelivery(ctx, scope.Project, item)
	if errors.Is(err, corestore.ErrNotFound) {
		return rows, nil
	}
	if err != nil {
		return rows, err
	}
	if delivery.OrganizationID != scope.Organization || delivery.ProjectID != scope.Project || delivery.WorkItemID != item {
		return rows, corestore.ErrNotFound
	}
	rows.Delivery = &delivery
	reader, ok := r.(eventstore.ReleaseDeliveryFactsReader)
	if !ok {
		return rows, hubapi.NativeWorkItemStorageUnavailable()
	}
	facts, err := reader.ReleaseDeliveryFacts(ctx, scope.Project, item, delivery.ReleasePickInput)
	if err != nil {
		return rows, err
	}
	rows.DependsOnSource = facts.DependsOnSource
	source, err := r.WorkItem(ctx, scope.Project, delivery.SourceIssueID)
	if errors.Is(err, corestore.ErrNotFound) {
		return rows, nil
	}
	if err != nil {
		return rows, err
	}
	for _, state := range states {
		if state.Name == hubapi.StringValue(source.WorkflowStateName) {
			rows.SourceTerminal = state.Terminal
			break
		}
	}
	rows.Change, err = r.Change(ctx, delivery.SourceChangeID)
	if errors.Is(err, corestore.ErrNotFound) {
		return rows, nil
	}
	return rows, err
}

func ReleaseDeliveryContractFactsCurrent(plan hubrecords.NativeClaimPlan, contract config.IssueContract) bool {
	delegated := plan.Issue.IssueContract.ReleaseDelivery
	rows := plan.ReleaseDelivery
	if plan.Descriptor.Authored == nil || plan.Descriptor.ID != delegated.PolicyID || policy.Digest([]byte(plan.Issue.Body)) != delegated.BodySHA256 || !changerules.ReleaseDeliverySectionsMatch(plan.Issue.Body, contract.ReleaseDeliveryBody()) {
		return false
	}
	if rows.Delivery == nil || plan.Issue.IgnoreDependencies || !plan.Project.Core.RequireDependencies || !rows.DependsOnSource || !rows.SourceTerminal {
		return false
	}
	change := rows.Change
	return change.OrganizationID == rows.Delivery.OrganizationID && change.ProjectID == rows.Delivery.ProjectID && change.WorkItemID == rows.Delivery.SourceIssueID && change.ID == rows.Delivery.SourceChangeID && change.CurrentVersionID != nil && *change.CurrentVersionID != "" && change.LandedVersionID != nil && *change.CurrentVersionID == *change.LandedVersionID && change.LandedBaseRef != nil && *change.LandedBaseRef == "develop" && change.LandedMergeSHA != nil && *change.LandedMergeSHA == rows.Delivery.SourceMergeSHA
}

func ReadNativeClaimReleaseDeliveryFacts(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, plans []hubrecords.NativeClaimPlan) error {
	for i := range plans {
		if plans[i].Item.IssueContractObjectKey == nil {
			continue
		}
		rows, err := ReadReleaseDeliveryContractFacts(ctx, r, scope, plans[i].Item.NativeID, plans[i].Project.States)
		if err != nil {
			return err
		}
		plans[i].ReleaseDelivery = rows
	}
	return nil
}
