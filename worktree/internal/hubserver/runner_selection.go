package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (d *database) runnerAllowedSelection(ctx context.Context, tx *sql.Tx, query claimCandidateQuery, now time.Time) (tracker.WorkItemID, bool, error) {
	if query.NativeScope == nil || query.NativeScope.credential.Runner.RunnerID == "" || query.WorkspaceLane {
		return 0, false, nil
	}
	r, err := readRunnerForDispatch(ctx, tx, *query.NativeScope, now, query.LandingBatchGroup)
	if err != nil {
		return 0, false, err
	}
	projects, err := readRunnerProjects(ctx, tx, query.NativeScope.credential.ID)
	if err != nil {
		return 0, false, err
	}
	if len(projects) == 0 {
		return 0, true, nil
	}
	probe := query
	scope := *query.NativeScope
	scope.project = ""
	probe.NativeScope = &scope
	probe.Scope = ""
	probe.ProviderCandidates = nil
	probe.WorkItemID = 0
	probe.LandingCapacity = 0
	if query.LandingCapacityKnown {
		probe.LandingCapacity = nativeLandingCapacityUnknown
	}
	probe.Limit = 100
	var heads []tracker.WorkItemID
	var contractRefusal error
projects:
	for _, project := range projects {
		projectScope := scope
		projectScope.project = project
		projectQuery := probe
		projectQuery.NativeScope = &projectScope
		projectQuery.Scope = string(project)
		var states []string
		if project == query.NativeScope.project {
			projectQuery = query
			projectQuery.Limit = 100
			states = normalizedQueryStrings(query.WorkflowStates)
		}
		projectQuery.AvailableAt = now
		approval, err := readProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(project))
		if err != nil {
			var failure *nativeError
			if errors.As(err, &failure) && failure.Code == "policy_mismatch" {
				continue projects
			}
			return 0, false, err
		}
		if len(r.Exclusions(project, approval.Policy.Requirements, false)) > 0 {
			continue projects
		}
		for {
			projectIDs, err := claimCandidateIDs(ctx, tx, projectQuery, query.RepositoryIDs, normalizedQueryStrings(query.Repositories), states, normalizedQueryStrings(query.Authors), normalizedQueryStrings(query.Assignees), normalizedQueryStrings(query.LabelInclude), normalizedQueryStrings(query.LabelExclude), nil)
			if err != nil {
				return 0, false, err
			}
			if len(projectIDs) == 0 && project == query.NativeScope.project && query.WorkItemID > 0 {
				if err := recordNativeSchedulingOutcome(ctx, tx, query.NativeScope, query.WorkItemID, tracker.NativeSchedulerDecision{Source: "native_claim_eligibility", Outcome: "skipped", Reason: "no_claimable_work"}, now); err != nil {
					return 0, false, err
				}
			}
			for _, id := range projectIDs {
				current, found, err := readUnreleasedLease(ctx, tx, id)
				if err != nil {
					return 0, false, err
				}
				if found && current.session.ExpiresAt.After(now) {
					continue
				}
				candidateScope := scope
				candidateScope.project = project
				contract, err := readNativeIssueContractEvaluation(ctx, tx, candidateScope, id)
				if err != nil {
					return 0, false, err
				}
				if !contract.Allowed {
					if err := recordNativeSchedulingOutcome(ctx, tx, &candidateScope, id, tracker.NativeSchedulerDecision{Source: "native_claim_eligibility", Outcome: "skipped", Reason: contract.Reason}, now); err != nil {
						return 0, false, err
					}
					if project == query.NativeScope.project && contract.ReasonCode == "issue_contract_unsatisfied" {
						contractRefusal = &nativeError{Code: contract.ReasonCode, Message: contract.Reason, status: http.StatusConflict, publicMessage: true}
					}
					continue
				}
				ready, evaluated, capacity, err := nativeLandingCandidateReady(ctx, tx, &candidateScope, id, r.MachineID, projectQuery.LandingCapacity, now)
				if err != nil {
					return 0, false, err
				}
				if !ready {
					if evaluated {
						if err := recordNativeSchedulingDecision(ctx, tx, &candidateScope, id, ready, capacity, now); err != nil {
							return 0, false, err
						}
					}
					continue
				}
				joining := ""
				if id == query.WorkItemID {
					joining = query.LandingBatchGroup
				}
				allowed, reason, err := placementClaimAllowed(ctx, tx, candidateScope, r.MachineID, id, now, query.ProviderCandidates, joining)
				if err != nil {
					return 0, false, err
				}
				if !allowed {
					if err := recordNativeSchedulingOutcome(ctx, tx, &candidateScope, id, tracker.NativeSchedulerDecision{Source: "native_runner_routing", Outcome: "skipped", Reason: reason}, now); err != nil {
						return 0, false, err
					}
					continue
				}

				decision, _, err := checkMonthlyClaim(ctx, tx, candidateScope, r.MachineID, id, false, now)
				if err != nil {
					return 0, false, err
				}
				if !decision.Allowed {
					if err := recordNativeSchedulingOutcome(ctx, tx, &candidateScope, id, tracker.NativeSchedulerDecision{Source: "native_provider_capacity", Outcome: "skipped", Reason: monthlyBudgetRefusal(decision)}, now); err != nil {
						return 0, false, err
					}
					continue
				}
				if _, _, err := selectProviderCapacity(ctx, tx, query, id, now); err != nil {
					if errors.Is(err, ErrNoClaimableWork) || isProviderWait(err) {
						continue
					}
					return 0, false, err
				}
				heads = append(heads, id)
				continue projects
			}
			if len(projectIDs) < projectQuery.Limit || projectIDs[len(projectIDs)-1] == 0 {
				break
			}
			projectQuery.After = projectIDs[len(projectIDs)-1]
		}
	}
	var selected tracker.WorkItemID
	if len(heads) > 0 {
		probe.OnlyIDs = heads
		probe.Limit = 1
		ids, err := claimCandidateIDs(ctx, tx, probe, query.RepositoryIDs, normalizedQueryStrings(query.Repositories), nil, normalizedQueryStrings(query.Authors), normalizedQueryStrings(query.Assignees), normalizedQueryStrings(query.LabelInclude), normalizedQueryStrings(query.LabelExclude), nil)
		if err != nil {
			return 0, false, err
		}
		if len(ids) > 0 {
			selected = ids[0]
		}
	}
	if selected == 0 && contractRefusal != nil {
		return 0, true, contractRefusal
	}
	return selected, true, nil
}
