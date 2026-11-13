package hubserver

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readReleaseDeliveryRows(ctx context.Context, r tenantstore.Reader, scope nativeScope, item string, request tracker.AssociateReleasePick, publishing bool) (releaseDeliveryRows, error) {
	var rows releaseDeliveryRows
	var err error
	rows.project, err = r.ProjectRecord(ctx, scope.project)
	if err != nil {
		return rows, err
	}
	if rows.project.Core.DeletedAt != nil {
		return rows, corestore.ErrNotFound
	}
	rows.checkoutVerified, err = r.RunnerCheckoutVerified(ctx, scope.project, rows.project.Settings.CheckoutRepository, s.config.now())
	if err != nil {
		return rows, err
	}
	rows.current, err = r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
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
	rows.deliveryRow, err = deliveryReader.ReleaseDelivery(ctx, scope.project, tracker.NativeWorkItemID(item))
	rows.found = err == nil
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return rows, err
	}
	facts, ok := r.(eventstore.ReleaseDeliveryFactsReader)
	if !ok {
		return rows, hubapi.NativeWorkItemStorageUnavailable()
	}
	rows.facts, err = facts.ReleaseDeliveryFacts(ctx, scope.project, tracker.NativeWorkItemID(item), request.ReleasePickInput)
	if err != nil {
		return rows, err
	}
	rows.lease, err = r.ActiveLeaseForIssue(ctx, rows.current.ID)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return rows, err
	}
	rows.source, err = r.WorkItem(ctx, scope.project, request.SourceIssueID)
	if err != nil {
		return rows, err
	}
	rows.change, err = r.Change(ctx, request.SourceChangeID)
	if err != nil {
		return rows, err
	}
	if rows.change.ProjectID != scope.project || rows.change.WorkItemID != request.SourceIssueID {
		return rows, corestore.ErrNotFound
	}
	if rows.change.CurrentVersionID == nil || rows.change.LandedVersionID == nil || *rows.change.CurrentVersionID != *rows.change.LandedVersionID || rows.change.LandedMergeSHA == nil || rows.change.LandedBaseRef == nil {
		return rows, hubapi.NativePublicInvalid("Source dependency must have a recorded merged develop Change")
	}
	rows.sourceVersion, err = r.Version(ctx, request.SourceChangeID, *rows.change.LandedVersionID)
	if err != nil {
		return rows, err
	}
	rows.prior, err = r.LatestChange(ctx, scope.project, tracker.NativeWorkItemID(item))
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return rows, err
	}
	rows.policy, err = readCurrentChangePolicyRows(ctx, r, scope)
	if err != nil {
		return rows, err
	}
	if publishing {
		rows.reviews, err = facts.ReleaseSourceReviews(ctx, rows.change.ID, rows.sourceVersion.ID)
		if err != nil {
			return rows, err
		}
		rows.attempt.attempt, err = r.LatestAttempt(ctx, rows.current.ID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return rows, err
		}
		attempt := rows.attempt.attempt
		if attempt.ID != "" && attempt.Status == "succeeded" && attempt.WorkItemRevision >= rows.current.Revision && attempt.DispatchGeneration >= rows.current.DispatchGeneration {
			rows.attempt.lease, err = r.Lease(ctx, attempt.LeaseID)
			if err != nil {
				return rows, err
			}
			if err = readTenantAttemptPayload(ctx, r, &rows.attempt); err != nil {
				return rows, err
			}
			if rows.attempt.payload == nil {
				rows.attempt.event, err = r.LatestRunEvent(ctx, scope.project, attempt.ID)
				if err != nil {
					return rows, err
				}
			}
		}
	}
	return rows, nil
}

func (s *Service) prepareReleaseDeliveryCheck(ctx context.Context, scope nativeScope, item string, request tracker.AssociateReleasePick, now time.Time, publishing bool) (releaseDeliveryCheck, error) {
	plan := releaseDeliveryCheck{request: request, publishing: publishing}
	if s.config.TenantRepository == nil {
		return plan, hubapi.NativeWorkItemStorageUnavailable()
	}
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
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
	plan.issue, err = s.hydrateWorkItemRecord(ctx, plan.current, plan.version)
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
	content := eventstore.NewContent(s.runDetailStore())
	approved, err := hydrateCurrentChangeVersionApproval(ctx, content, scope, plan.policy, "")
	if err != nil {
		return plan, err
	}
	plan.approval = approved
	if err = requireNativeEdit(plan.issue, request.ExpectedRevision); err != nil {
		return plan, err
	}
	if publishing && request.PRNumber > 0 && !plan.found {
		return plan, hubapi.NativeInvalid("Existing PR publication requires an associated delivery")
	}
	if plan.found && (!sameReleasePickDelivery(plan.existing.ReleasePickInput, request.ReleasePickInput) || plan.existing.ReleasePickInput != request.ReleasePickInput && (plan.existing.BaseSHA == request.BaseSHA || plan.existing.HeadSHA == request.HeadSHA)) {
		return plan, hubapi.NativePublicInvalid("Refresh must retain the release delivery identity and replace its exact base and head")
	}
	if plan.issue.Terminal || plan.issue.Archived || plan.issue.IgnoreDependencies || !plan.facts.DependsOnSource {
		return plan, hubapi.NativePublicInvalid("Delivery must be an unfinished dependent issue with dependency admission enabled")
	}
	if !plan.project.Core.RequireDependencies {
		return plan, hubapi.NativePublicInvalid("Release delivery requires project dependency admission")
	}
	project := tracker.NativeProject{States: plan.project.States}
	if plan.found && strings.EqualFold(plan.issue.State, landingLane) {
		if target := reworkTarget(project, plan.issue.State); target == "" || target == plan.issue.State {
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
	if !plan.checkoutVerified || !validCheckoutRepository(plan.project.Settings.CheckoutRepository) || plan.project.Core.Profile != "native" || request.Repository != "https://github.com/"+plan.project.Settings.CheckoutRepository || plan.sourceVersion.Repository != request.Repository {
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

func (s *Service) recheckReleaseDelivery(ctx context.Context, w tenantstore.Writer, scope nativeScope, plan releaseDeliveryCheck) error {
	if err := s.authorizeReleaseScope(ctx, w, scope); err != nil {
		return err
	}
	if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
		return err
	}
	if _, err := w.LockWorkItem(ctx, scope.project, plan.current.NativeID); err != nil {
		return err
	}
	if _, err := w.LockChange(ctx, plan.change.ID); err != nil {
		return err
	}
	if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", s.config.now()); err != nil {
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

func (s *Service) authorizeReleaseScope(ctx context.Context, r tenantstore.Reader, scope nativeScope) error {
	if scope.credential.Scope != hubapi.APIScopeOperator && scope.credential.Scope != hubapi.APIScopeAdmin || scope.credential.Runner.RunnerID != "" {
		return hubapi.NativeNotFound()
	}
	if err := s.authorizeWorkItemRecordScope(ctx, r, scope); err != nil {
		return err
	}
	if scope.credential.Hosted == nil {
		current, err := r.AccessCredential(ctx, scope.credential.ID)
		if err != nil {
			return err
		}
		if current.Scope != string(scope.credential.Scope) || current.NativeOnly != scope.credential.NativeOnly {
			return hubapi.NativeNotFound()
		}
	}
	return nil
}

type releaseDeliveryContractFacts struct {
	delivery        *eventstore.ReleaseDelivery
	dependsOnSource bool
	sourceTerminal  bool
	change          eventstore.Change
}

func readReleaseDeliveryContractFacts(ctx context.Context, r tenantstore.Reader, scope nativeScope, item tracker.NativeWorkItemID, states []tracker.NativeState) (releaseDeliveryContractFacts, error) {
	var rows releaseDeliveryContractFacts
	delivery, err := r.ReleaseDelivery(ctx, scope.project, item)
	if errors.Is(err, corestore.ErrNotFound) {
		return rows, nil
	}
	if err != nil {
		return rows, err
	}
	if delivery.OrganizationID != scope.organization || delivery.ProjectID != scope.project || delivery.WorkItemID != item {
		return rows, corestore.ErrNotFound
	}
	rows.delivery = &delivery
	reader, ok := r.(eventstore.ReleaseDeliveryFactsReader)
	if !ok {
		return rows, hubapi.NativeWorkItemStorageUnavailable()
	}
	facts, err := reader.ReleaseDeliveryFacts(ctx, scope.project, item, delivery.ReleasePickInput)
	if err != nil {
		return rows, err
	}
	rows.dependsOnSource = facts.DependsOnSource
	source, err := r.WorkItem(ctx, scope.project, delivery.SourceIssueID)
	if errors.Is(err, corestore.ErrNotFound) {
		return rows, nil
	}
	if err != nil {
		return rows, err
	}
	for _, state := range states {
		if state.Name == hubapi.StringValue(source.WorkflowStateName) {
			rows.sourceTerminal = state.Terminal
			break
		}
	}
	rows.change, err = r.Change(ctx, delivery.SourceChangeID)
	if errors.Is(err, corestore.ErrNotFound) {
		return rows, nil
	}
	return rows, err
}

func releaseDeliveryContractFactsCurrent(plan nativeClaimPlan, contract config.IssueContract) bool {
	delegated := plan.issue.IssueContract.ReleaseDelivery
	rows := plan.releaseDelivery
	if plan.descriptor.Authored == nil || plan.descriptor.ID != delegated.PolicyID || policy.Digest([]byte(plan.issue.Body)) != delegated.BodySHA256 || !releaseDeliverySectionsMatch(plan.issue.Body, contract.ReleaseDeliveryBody()) {
		return false
	}
	if rows.delivery == nil || plan.issue.IgnoreDependencies || !plan.project.Core.RequireDependencies || !rows.dependsOnSource || !rows.sourceTerminal {
		return false
	}
	change := rows.change
	return change.OrganizationID == rows.delivery.OrganizationID && change.ProjectID == rows.delivery.ProjectID && change.WorkItemID == rows.delivery.SourceIssueID && change.ID == rows.delivery.SourceChangeID && change.CurrentVersionID != nil && *change.CurrentVersionID != "" && change.LandedVersionID != nil && *change.CurrentVersionID == *change.LandedVersionID && change.LandedBaseRef != nil && *change.LandedBaseRef == "develop" && change.LandedMergeSHA != nil && *change.LandedMergeSHA == rows.delivery.SourceMergeSHA
}

func readNativeClaimReleaseDeliveryFacts(ctx context.Context, r tenantstore.Reader, scope nativeScope, plans []nativeClaimPlan) error {
	for i := range plans {
		if plans[i].item.IssueContractObjectKey == nil {
			continue
		}
		rows, err := readReleaseDeliveryContractFacts(ctx, r, scope, plans[i].item.NativeID, plans[i].project.States)
		if err != nil {
			return err
		}
		plans[i].releaseDelivery = rows
	}
	return nil
}
