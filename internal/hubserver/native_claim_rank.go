package hubserver

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	nativeClaimRankProjects = 16
	nativeClaimRankBudget   = claimPreparationLimit
)

type nativeClaimRankPosition struct {
	rank      int
	candidate claimstore.Candidate
}

func nativeClaimOutranks(left, right nativeClaimRankPosition) bool {
	l, r := left.candidate, right.candidate
	return cmp.Or(
		cmp.Compare(left.rank, right.rank),
		cmp.Compare(l.LaneRank, r.LaneRank),
		cmp.Compare(l.Priority, r.Priority),
		cmp.Compare(l.AgingBoost, r.AgingBoost),
		l.CreatedAt.Compare(r.CreatedAt),
		cmp.Compare(l.StateRank, r.StateRank),
		cmp.Compare(l.LabelRank, r.LabelRank),
		cmp.Compare(l.ProjectID, r.ProjectID),
		cmp.Compare(l.WorkItemID, r.WorkItemID),
	) < 0
}

type nativeClaimRankCompetitor struct {
	scope      hubscope.NativeScope
	request    tracker.NativeClaim
	plans      []hubrecords.NativeClaimPlan
	cursor     claimstore.SelectionCursor
	incomplete bool
}

func readNativeClaimRanking(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope) (*hubrecords.ClaimRanking, error) {
	if scope.Credential.Runner.RunnerID == "" {
		return &hubrecords.ClaimRanking{}, nil
	}
	projects, own, err := r.RunnerOutrankingProjects(ctx, scope.Credential.Runner.RunnerID, scope.Project, nativeClaimRankProjects)
	if errors.Is(err, corestore.ErrNotFound) {
		return &hubrecords.ClaimRanking{}, nil
	}
	if err != nil {
		return nil, err
	}
	return &hubrecords.ClaimRanking{Own: own, Projects: projects}, nil
}

func (s *Service) nativeClaimOutranked(ctx context.Context, scope hubscope.NativeScope, request tracker.NativeClaim, plan hubrecords.NativeClaimPlan, now time.Time) (bool, error) {
	if plan.Replay != nil || slices.Contains(request.Capabilities, tracker.NativeWorkspaceCapability) || plan.Ranking != nil && len(plan.Ranking.Projects) == 0 {
		return false, nil
	}
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return false, hubapi.NativeRunEventStorageUnavailable()
	}
	var identity runnerstore.Identity
	var reports []providercapacity.Report
	var competitors []nativeClaimRankCompetitor
	err := repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		batch, ok := r.(tenantstore.ClaimPreviewPlanRowsReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		identity, err = r.RunnerIdentityByCredential(ctx, scope.Credential.ID)
		if err != nil {
			return err
		}
		ranking := plan.Ranking
		if ranking == nil {
			if ranking, err = readNativeClaimRanking(ctx, r, scope); err != nil {
				return err
			}
		}
		projects, own := ranking.Projects, ranking.Own
		if len(projects) == 0 {
			return nil
		}
		reports, err = r.RunnerProviderReports(ctx, identity.RunnerID)
		if err != nil {
			return err
		}
		machine, err := r.Machine(ctx, string(identity.MachineID))
		if err != nil {
			return err
		}
		self := nativeClaimRankPosition{rank: own, candidate: plan.Candidate}
		quota := max(1, nativeClaimRankBudget/(len(projects)+1))
		for _, ranked := range projects {
			projectScope := scope
			projectScope.Project = ranked.ProjectID
			project, err := r.ProjectRecord(ctx, ranked.ProjectID)
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if !nativeClaimProjectEnabled(project) {
				continue
			}
			current, err := r.ProjectPolicy(ctx, ranked.ProjectID)
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if limit, limited := identity.ProjectCapacityLimits[ranked.ProjectID]; limited {
				_, used, err := r.RunnerProjectUsage(ctx, identity.RunnerID, ranked.ProjectID, now)
				if err != nil {
					return err
				}
				if used >= int64(limit) {
					continue
				}
			}
			allowed, err := spriteAllocationRunnerAllowedFacts(ctx, r, projectScope, identity, machine, true)
			if err != nil {
				return err
			}
			if !allowed {
				continue
			}
			projectRequest := request
			projectRequest.PolicyID, projectRequest.WorkItemID, projectRequest.SessionID, projectRequest.ProviderCandidates = current.PolicyID, "", "", nil
			query := nativeTenantCandidateQuery(projectScope, projectRequest, "", now)
			sourceRevision, err := nativeClaimSelectionSourceRevision(project, current, query)
			if err != nil {
				return err
			}
			cursor, err := r.SelectionCursor(ctx, identity.RunnerID, ranked.ProjectID)
			if errors.Is(err, corestore.ErrNotFound) {
				cursor = claimstore.SelectionCursor{OrganizationID: scope.Organization, RunnerID: identity.RunnerID, ProjectID: ranked.ProjectID, SourceRevision: sourceRevision}
			} else if err != nil {
				return err
			} else if cursor.SourceRevision != sourceRevision {
				cursor.SourceRevision, cursor.Position = sourceRevision, claimstore.CandidateCursor{}
			}
			query.After = cursor.Position
			query.Limit = quota + 1
			page, err := r.CandidatePage(ctx, query)
			if err != nil {
				return err
			}
			var outranking []claimstore.Candidate
			for _, candidate := range page.Items {
				if len(outranking) == quota || !nativeClaimOutranks(nativeClaimRankPosition{rank: ranked.Rank, candidate: candidate}, self) {
					break
				}
				outranking = append(outranking, candidate)
			}
			if len(outranking) == 0 {
				continue
			}
			incomplete := len(page.Items) > quota && nativeClaimOutranks(nativeClaimRankPosition{rank: ranked.Rank, candidate: page.Items[quota]}, self)
			cursor.Position = claimCandidateCursor(outranking[len(outranking)-1])
			rows, err := batch.ClaimPreviewPlanRows(ctx, ranked.ProjectID, outranking)
			if err != nil {
				return err
			}
			if len(rows) != len(outranking) {
				return corestore.ErrRevisionConflict
			}
			shared := hubrecords.NativeClaimPlan{Project: project, Policy: current}
			competitor := nativeClaimRankCompetitor{scope: projectScope, request: projectRequest, cursor: cursor, incomplete: incomplete}
			for index, row := range rows {
				if errors.Is(row.Err, corestore.ErrNotFound) {
					continue
				}
				if row.Err != nil {
					return row.Err
				}
				candidate := nativeClaimPlanFromBatch(shared, outranking[index], row)
				if candidate.Item.Revision != candidate.Candidate.Revision || candidate.Item.Archived {
					continue
				}
				if err := validateClaimSnapshotPolicy(projectScope, candidate); err != nil {
					return err
				}
				competitor.plans = append(competitor.plans, candidate)
			}
			if err := readNativeClaimReleaseDeliveryFacts(ctx, r, projectScope, competitor.plans); err != nil {
				return err
			}
			if err := readNativeClaimChangeFacts(ctx, r, projectScope, competitor.plans); err != nil {
				return err
			}
			if len(competitor.plans) > 0 || incomplete {
				competitors = append(competitors, competitor)
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	var pending []claimstore.SelectionCursor
	for _, competitor := range competitors {
		claimable, excluded, err := s.nativeClaimRankCompetitorClaimable(ctx, competitor, request, identity, reports, now)
		if err != nil || claimable {
			return claimable, err
		}
		if competitor.incomplete && !excluded {
			pending = append(pending, competitor.cursor)
		}
	}
	if len(pending) == 0 {
		return false, nil
	}
	return true, s.writeNativeClaimCommand(ctx, scope, func(ctx context.Context, w tenantstore.Writer) error {
		for _, cursor := range pending {
			if _, err := w.PutSelectionCursor(ctx, cursor, cursor.Revision); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) nativeClaimRankCompetitorClaimable(ctx context.Context, competitor nativeClaimRankCompetitor, request tracker.NativeClaim, identity runnerstore.Identity, reports []providercapacity.Report, now time.Time) (bool, bool, error) {
	plans := competitor.plans
	if err := s.prepareNativeClaimLegacyAuditFacts(ctx, competitor.scope, plans); err != nil {
		return false, false, err
	}
	if err := s.prepareNativeClaimApprovals(ctx, competitor.scope, plans); err != nil {
		return false, false, err
	}
	var descriptor *policy.Descriptor
	var resolver *runner.BoardIdentityResolver
	for _, rows := range plans {
		plan, err := s.hydrateNativeClaimPlan(ctx, competitor.scope, competitor.request, now, rows, descriptor)
		if errors.Is(err, ErrNoClaimableWork) {
			continue
		}
		if err != nil {
			return false, false, err
		}
		descriptor = &plan.Descriptor
		if plan.Descriptor.Requirements.Match(identity.RunnerID, string(identity.MachineID), identity.Tags) != nil {
			return false, true, nil
		}
		if !nativeClaimIssueContractAllowed(plan) {
			continue
		}
		if (hubapi.StringsEqualFold(plan.Candidate.State, "Merging") || plan.LandingPending) && !plan.ChangeReady {
			continue
		}
		if plan.LandingRequired {
			available, err := s.nativeClaimLandingCapacityAvailable(ctx, competitor.scope.Organization, competitor.scope.Project, request.LandingCapacity, now)
			if err != nil {
				return false, false, err
			}
			if !available {
				continue
			}
		}
		if len(reports) > 0 {
			if resolver == nil {
				resolver, err = s.nativeClaimRankResolver(ctx, competitor.scope, plan)
				if err != nil {
					return false, false, err
				}
			}
			if !nativeClaimRankProviderSupported(resolver, plan, request, reports) {
				continue
			}
		}
		return true, false, nil
	}
	return false, false, nil
}

func (s *Service) nativeClaimRankResolver(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.NativeClaimPlan) (*runner.BoardIdentityResolver, error) {
	workflow, err := hubrecords.ProjectPolicyWorkflow(ctx, string(scope.Organization)+"/"+string(scope.Project), plan.Descriptor)
	if err != nil {
		return nil, err
	}
	workflow.Config.Plan.Enabled = plan.Descriptor.Gates.PlanEnabled
	selection, err := s.projectRecordModel(ctx, plan.Project)
	if err != nil {
		return nil, err
	}
	workflow.Config.Agents.ModelSelection = selection.Effective
	return runner.NewBoardIdentityResolver(workflow.Config, selector.Context{})
}

func nativeClaimRankProviderSupported(resolver *runner.BoardIdentityResolver, plan hubrecords.NativeClaimPlan, request tracker.NativeClaim, reports []providercapacity.Report) bool {
	var requirement providercapacity.Requirement
	if candidate := nativeClaimProviderCandidate(request, plan.Item.NativeID); candidate != nil && int64(candidate.Revision) == plan.Item.Revision {
		requirement = candidate.Requirement
	} else {
		issue := plan.Issue
		identity, err := resolver.Identity(connector.Issue{Title: issue.Title, Description: issue.Body, Labels: issue.Labels, State: issue.State})
		if err != nil {
			return false
		}
		requirement = providercapacity.Requirement{Role: identity.Role, Backend: identity.BackendKind, Model: identity.RequestedModel.Value}
	}
	if requirement.Validate() != nil {
		return false
	}
	return slices.ContainsFunc(reports, func(report providercapacity.Report) bool { return report.Supports(requirement) })
}
