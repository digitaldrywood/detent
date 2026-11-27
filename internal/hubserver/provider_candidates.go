package hubserver

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/claimapi"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) previewProviderCandidates(c echo.Context) error {
	var request tracker.NativeCapacityPreview
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if request.Limit < 0 || request.Limit > 100 {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Candidate limit must be between 1 and 100"))
	}
	if request.LandingCapacity < 0 {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("Landing capacity must be non-negative"))
	}
	ctx := c.Request().Context()
	if request.CheckCapacity {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, hubapi.ReadTimeout)
		defer cancel()
	}
	page, err := s.previewProviderCandidatesTenant(ctx, hubscope.NativeRequestScope(c), request, s.config.Clock().UTC())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSONBlob(http.StatusOK, page)
}

func (s *Service) previewProviderCandidatesTenant(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeCapacityPreview, now time.Time) (nativeClaimPreviewPage, error) {
	ctx = s.withRequestDetails(ctx)
	limit := request.Limit
	if limit == 0 {
		limit = 100
	}
	var candidates []claimstore.Candidate
	var plans []hubrecords.NativeClaimPlan
	var openBlockers map[tracker.NativeWorkItemID]int
	var projectPolicy policystore.Revision
	var identity runnerstore.Identity
	var capacityFull bool
	snapshotRepository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return nil, hubapi.NativeRunEventStorageUnavailable()
	}
	err := snapshotRepository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		var err error
		identity, err = r.RunnerIdentityByCredential(ctx, scope.Credential.ID)
		if err != nil {
			return err
		}
		if !runnerIdentityCanClaim(identity, scope, request.MachineID, now) {
			return tracker.ErrMachineNotFound
		}
		allowed, err := r.RunnerProjectAuthorized(ctx, identity.RunnerID, scope.Project, now)
		if err != nil {
			return err
		}
		if !allowed {
			return tracker.ErrMachineNotFound
		}
		if request.CheckCapacity {
			capacityFull, err = nativeClaimPreviewCapacityFull(ctx, r, identity, scope.Project, now)
			if err != nil || capacityFull {
				return err
			}
		}
		project, err := r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if !nativeClaimProjectEnabled(project) {
			return ErrNoClaimableWork
		}
		projectPolicy, err = r.ProjectPolicy(ctx, scope.Project)
		if err != nil {
			return err
		}
		if projectPolicy.PolicyID != request.PolicyID {
			return hubapi.PolicyMismatch("Claim configuration changed; load the current project policy before previewing candidates")
		}
		query := nativeTenantCandidateQuery(scope, request.NativeClaim, "", now)
		query.IncludeRecordedRecovery = true
		query.Limit = limit
		if request.After > 0 {
			anchor, err := r.WorkItemByID(ctx, scope.Project, int64(request.After))
			if err != nil {
				return corestore.ErrInvalidCursor
			}
			query.AfterWorkItemID = anchor.NativeID
		}
		page, err := r.CandidatePage(ctx, query)
		if errors.Is(err, corestore.ErrInvalidCursor) && query.AfterWorkItemID != "" {
			return providerWait("provider_candidate_changed", "Work item changed after local model selection; refresh before claiming")
		}
		if err != nil {
			return err
		}
		candidates = page.Items
		shared := hubrecords.NativeClaimPlan{Project: project, Policy: projectPolicy}
		batchReader, canBatch := r.(tenantstore.ClaimPreviewPlanRowsReader)
		if canBatch && request.SessionID == "" && len(candidates) > 0 {
			rows, err := batchReader.ClaimPreviewPlanRows(ctx, scope.Project, candidates)
			if err != nil {
				return err
			}
			if len(rows) != len(candidates) {
				return corestore.ErrRevisionConflict
			}
			for index, row := range rows {
				candidate := candidates[index]
				if errors.Is(row.Err, corestore.ErrNotFound) {
					s.reportIncompletePreviewCandidate(ctx, scope, candidate, row.Err)
					continue
				}
				if row.Err != nil {
					return row.Err
				}
				if row.Item.Revision != candidate.Revision || row.Item.Archived {
					continue
				}
				plan := nativeClaimPlanFromBatch(shared, candidate, row)
				if err := validateClaimSnapshotPolicy(scope, plan); err != nil {
					return err
				}
				plans = append(plans, plan)
			}
			if err := readNativeClaimChangeFacts(ctx, r, scope, plans); err != nil {
				return err
			}
			if err := readNativeClaimReleaseDeliveryFacts(ctx, r, scope, plans); err != nil {
				return err
			}
		} else {
			for _, candidate := range candidates {
				claim := request.NativeClaim
				claim.WorkItemID = candidate.WorkItemID
				plan, err := s.readNativeClaimPlanRows(ctx, scope, claim, now, r, &candidate, shared)
				if errors.Is(err, corestore.ErrNotFound) {
					s.reportIncompletePreviewCandidate(ctx, scope, candidate, err)
					continue
				}
				if errors.Is(err, ErrNoClaimableWork) {
					continue
				}
				if err != nil {
					return err
				}
				plans = append(plans, plan)
			}
		}
		ids := make([]tracker.NativeWorkItemID, 0, len(plans))
		for _, plan := range plans {
			ids = append(ids, plan.Candidate.WorkItemID)
		}
		if len(ids) > 0 {
			openBlockers, err = r.ClaimOpenBlockerCounts(ctx, tenantstore.ClaimBlockerQuery{Project: scope.Project, WorkItems: ids, CredentialID: scope.Credential.ID, Now: now, SampleLimit: hubrecords.NativeClaimBlockerLimit + 1})
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if capacityFull {
		return nativeClaimPreviewPage(`{"items":[],"capacity_full":true}`), nil
	}
	if err := s.prepareNativeClaimLegacyAuditFacts(ctx, scope, plans); err != nil {
		return nil, err
	}
	if err := s.prepareNativeClaimApprovals(ctx, scope, plans); err != nil {
		return nil, err
	}
	refs := []attachment.ArchiveReference{}
	if len(plans) > 0 {
		refs = append(refs, eventstore.Reference(projectPolicy.PayloadObjectKey, projectPolicy.PayloadSHA256, projectPolicy.PayloadBytes, projectPolicy.PayloadContentBytes))
	}
	for _, plan := range plans {
		v := plan.Version
		refs = append(refs, eventstore.Reference(v.PayloadObjectKey, v.PayloadSHA256, v.PayloadBytes, v.PayloadContentBytes))
		if hubapi.StringsEqualFold(plan.Candidate.State, "Merging") || plan.LandingPending {
			c := plan.ChangeRecord
			v := plan.ChangeVersion
			refs = append(refs, eventstore.Reference(c.PayloadObjectKey, c.PayloadSHA256, c.PayloadBytes, c.PayloadContentBytes), eventstore.Reference(v.PayloadObjectKey, v.PayloadSHA256, v.PayloadBytes, v.PayloadContentBytes))
			p := plan.ChangePolicy.Project
			refs = append(refs, eventstore.Reference(p.PayloadObjectKey, p.PayloadSHA256, p.PayloadBytes, p.PayloadContentBytes))
			if plan.ChangePolicy.HasReview {
				r := plan.ChangePolicy.Review
				refs = append(refs, eventstore.Reference(r.PayloadObjectKey, r.PayloadSHA256, r.PayloadBytes, r.PayloadContentBytes))
			}
		}
	}
	s.prefetchDetails(ctx, string(scope.Organization), refs)
	var descriptor *policy.Descriptor
	if len(plans) > 0 {
		loaded, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).ProjectPolicy(ctx, projectPolicy)
		if err != nil {
			return nil, err
		}
		descriptor = &loaded
		if err := loaded.Requirements.Match(identity.RunnerID, string(identity.MachineID), identity.Tags); err != nil {
			return nil, &hubapi.NativeError{Code: "selector_no_match", Message: err.Error(), Status: http.StatusConflict}
		}
	}
	for index := range plans {
		claim := request.NativeClaim
		claim.WorkItemID = plans[index].Candidate.WorkItemID
		rows := plans[index]
		if rows.Replay == nil && rows.Project.Core.Profile == "native" && strings.EqualFold(rows.Candidate.State, "Blocked") {
			recovered, err := s.runnerClaimDeps().PlanNativeClaimRecovery(ctx, scope, claim, rows)
			if errors.Is(err, ErrNoClaimableWork) {
				plans[index] = hubrecords.NativeClaimPlan{}
				continue
			}
			if err != nil {
				return nil, err
			}
			rows = recovered
		}
		plan, err := s.hydrateNativeClaimPlan(ctx, scope, claim, now, rows, descriptor)
		if errors.Is(err, ErrNoClaimableWork) {
			plans[index] = hubrecords.NativeClaimPlan{}
			continue
		}
		if err != nil {
			return nil, err
		}
		plans[index] = plan
	}
	landingCapacityChecked := false
	landingCapacityAvailable := false
	page := nativeClaimPreviewPage(nativePreviewPagePrefix)
	var last tracker.WorkItemID
	for _, plan := range plans {
		if plan.Candidate.WorkItemID == "" {
			continue
		}
		if !nativeClaimIssueContractAllowed(plan) || hubapi.StringsEqualFold(plan.Candidate.State, "Merging") && !plan.ChangeReady {
			continue
		}
		if plan.LandingPending && !plan.ChangeReady {
			continue
		}
		if plan.LandingRequired || request.LandingCapacity > 0 {
			if !landingCapacityChecked {
				landingCapacityAvailable, err = s.nativeClaimLandingCapacityAvailable(ctx, scope.Organization, scope.Project, request.LandingCapacity, now)
				if err != nil {
					return nil, err
				}
				landingCapacityChecked = true
			}
			if !landingCapacityChecked || !landingCapacityAvailable {
				continue
			}
		}
		issue := plan.Issue
		summary := hubrecords.SummarizeNativeClaimBlockers(openBlockers[issue.WorkItemID])
		issue.Blockers = nil
		issue.Dependencies = []tracker.NativeWorkItemID{}
		issue.DependencyReady = summary.DependencyReady
		issue.IgnoreDependencies = !plan.Project.Core.RequireDependencies
		issue.BlockersTruncated = summary.BlockersTruncated
		issue.Dependents = []tracker.NativeDependency{}
		issue.Related = nil
		issue.BlockedReason = tracker.DependencyReason{}
		issue.UnblocksReason = tracker.DependencyReason{}
		issue.OmittedFields = slices.Clone(issue.OmittedFields)
		for _, field := range []string{"dependents", "related", "blocked_reason", "unblocks_reason"} {
			if !slices.Contains(issue.OmittedFields, field) {
				issue.OmittedFields = append(issue.OmittedFields, field)
			}
		}
		cursor := tracker.WorkItemID(plan.Candidate.ID)
		var added bool
		page, added, err = page.add(nativeClaimPreviewIssue{NativeIssue: s.nativeIssueResponse(issue), Blockers: summary.Blockers})
		if err != nil {
			return nil, err
		}
		if !added {
			return page.finish(last)
		}
		last = cursor
	}
	if len(candidates) == limit {
		return page.finish(tracker.WorkItemID(candidates[len(candidates)-1].ID))
	}
	return page.finish(0)
}

func nativeClaimPreviewCapacityFull(ctx context.Context, r tenantstore.Reader, identity runnerstore.Identity, project tracker.ProjectID, now time.Time) (bool, error) {
	runnerLimit, projectLimit, projectConfigured, err := claimapi.RunnerClaimCapacity(identity, project, now)
	if err != nil {
		return false, err
	}
	if runnerLimit <= 0 || projectConfigured && projectLimit <= 0 {
		return true, nil
	}
	machine, err := r.Machine(ctx, string(identity.MachineID))
	if err != nil {
		return false, err
	}
	if machine.Capacity <= 0 {
		return true, nil
	}
	usage, err := r.ClaimCapacityUsage(ctx, machine.ID, identity.RunnerID, project, now)
	if err != nil {
		return false, err
	}
	return usage.Host >= machine.Capacity || usage.Runner >= int64(runnerLimit) || projectConfigured && usage.Project >= int64(projectLimit), nil
}

func (s *Service) nativeClaimLandingCapacityAvailable(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, requested int, now time.Time) (bool, error) {
	allowed := false
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		usage, err := r.LandingCapacityUsage(ctx, project, now)
		if err != nil {
			return err
		}
		capacity := int64(requested)
		if usage.Limit > 0 && (capacity <= 0 || usage.Limit < capacity) {
			capacity = usage.Limit
		}
		allowed = !usage.Unknown && (capacity <= 0 || usage.Active < capacity)
		return nil
	})
	return allowed, err
}

func (s *Service) reportIncompletePreviewCandidate(ctx context.Context, scope hubscope.NativeScope, candidate claimstore.Candidate, err error) {
	s.config.Logger.WarnContext(ctx, "skip incomplete preview candidate", "organization_id", scope.Organization, "project_id", scope.Project, "work_item_id", candidate.WorkItemID, "error", err)
}
