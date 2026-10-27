package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (d *database) runnerAllowedSelection(ctx context.Context, tx *sql.Tx, query claimCandidateQuery, r runnerauth.Runner, now time.Time) (tracker.WorkItemID, bool, error) {
	if query.NativeScope == nil || query.NativeScope.credential.Runner.RunnerID == "" || query.WorkspaceLane {
		return 0, false, nil
	}
	if query.WorkItemID > 0 || len(query.ProviderCandidates) == 1 {
		approval, found, err := readProjectPolicy(ctx, tx, string(query.NativeScope.organization)+"/"+string(query.NativeScope.project))
		if err != nil {
			return 0, false, err
		}
		if !found || len(r.Exclusions(query.NativeScope.project, approval.Policy.Requirements, false)) > 0 {
			return 0, true, nil
		}
		return 0, false, nil
	}
	projects, err := readRunnerProjects(ctx, tx, query.NativeScope.credential.ID, claimPreparationLimit)
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
	probe.OnlyIDs = nil
	probe.WorkItemID = 0
	probe.LandingCapacity = 0
	if query.LandingCapacityKnown {
		probe.LandingCapacity = nativeLandingCapacityUnknown
	}
	probe.Limit = 100
	preferred, err := spriteAllocationRunnerProject(ctx, tx, scope, r.MachineID)
	if err != nil {
		return 0, false, err
	}
	var preferredHead tracker.WorkItemID
	var heads []tracker.WorkItemID
	type selectionPosition struct {
		lane, rank, priority int
		preferred            bool
	}
	headPositions := make(map[tracker.WorkItemID]selectionPosition)
	incomplete := make([]selectionPosition, 0, len(projects))
	var contractRefusal error
	quota := max(1, claimPreparationLimit/len(projects))
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
		approval, found, err := readProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(project))
		if err != nil {
			return 0, false, err
		}
		if !found {
			continue projects
		}
		if len(r.Exclusions(project, approval.Policy.Requirements, false)) > 0 {
			continue projects
		}
		var resolver *runner.BoardIdentityResolver
		if project != query.NativeScope.project && len(query.ProviderCandidates) > 0 {
			workflow, err := config.ApplyNativePolicy(config.Workflow{Config: config.Default()}, approval.Policy)
			if err != nil {
				continue projects
			}
			workflow.Config.Plan.Enabled = approval.Policy.Gates.PlanEnabled
			resolver, err = readProjectIdentityResolver(ctx, tx, projectScope, workflow.Config)
			if err != nil {
				return 0, false, err
			}
		}
		progress, err := readRunnerSelectionProgress(ctx, tx, projectScope, projectQuery)
		if err != nil {
			return 0, false, err
		}
		for !progress.done && progress.visits < quota {
			projectQuery.Limit = 1
			before := progress.cursor
			projectQuery.SelectionCursor = &progress.cursor
			projectIDs, err := claimCandidateIDs(ctx, tx, projectQuery, query.RepositoryIDs, normalizedQueryStrings(query.Repositories), states, normalizedQueryStrings(query.Authors), normalizedQueryStrings(query.Assignees), normalizedQueryStrings(query.LabelInclude), normalizedQueryStrings(query.LabelExclude), nil)
			if err != nil {
				progress.cursor = before
				return 0, false, err
			}
			if len(projectIDs) == 0 {
				if progress.visits == 0 && project == query.NativeScope.project && query.WorkItemID > 0 {
					if err := recordNativeSchedulingOutcome(ctx, tx, query.NativeScope, query.WorkItemID, tracker.NativeSchedulerDecision{Source: "native_claim_eligibility", Outcome: "skipped", Reason: "no_claimable_work"}, now); err != nil {
						progress.cursor = before
						return 0, false, err
					}
				}
				progress.visits++
				progress.dirty = true
				progress.done = progress.cursor.ID == 0
				continue
			}
			id := projectIDs[0]
			allowed, err := func() (bool, error) {
				current, found, err := readUnreleasedLease(ctx, tx, id)
				if err != nil {
					return false, err
				}
				if found && current.session.ExpiresAt.After(now) {
					return false, nil
				}
				candidateScope := scope
				candidateScope.project = project
				contract, err := readNativeIssueContractEvaluation(ctx, tx, candidateScope, id)
				if err != nil {
					return false, err
				}
				if !contract.Allowed {
					if err := recordNativeSchedulingOutcome(ctx, tx, &candidateScope, id, tracker.NativeSchedulerDecision{Source: "native_claim_eligibility", Outcome: "skipped", Reason: contract.Reason}, now); err != nil {
						return false, err
					}
					if project == query.NativeScope.project && contract.ReasonCode == "issue_contract_unsatisfied" {
						contractRefusal = &nativeError{Code: contract.ReasonCode, Message: contract.Reason, status: http.StatusConflict, publicMessage: true}
					}
					return false, nil
				}
				ready, evaluated, capacity, err := nativeLandingCandidateReady(ctx, tx, &candidateScope, id, r.MachineID, projectQuery.LandingCapacity, now)
				if err != nil {
					return false, err
				}
				if !ready {
					if evaluated {
						if err := recordNativeSchedulingDecision(ctx, tx, &candidateScope, id, ready, capacity, now); err != nil {
							return false, err
						}
					}
					return false, nil
				}
				joining := ""
				if id == query.WorkItemID {
					joining = query.LandingBatchGroup
				}
				capacityQuery := projectQuery
				if resolver != nil {
					var native tracker.NativeWorkItemID
					if err := tx.QueryRowContext(ctx, `SELECT native_id FROM issues WHERE id=?`, id).Scan(&native); err != nil {
						return false, err
					}
					issue, _, err := readNativeIssue(ctx, tx, candidateScope, string(native))
					if err != nil {
						return false, err
					}
					identity, err := resolver.Identity(connector.Issue{Title: issue.Title, Description: issue.Body, Labels: issue.Labels, State: issue.State})
					if err != nil {
						return false, nil
					}
					capacityQuery.NativeScope = &candidateScope
					capacityQuery.ProviderCandidates = []tracker.NativeCapacityCandidate{{WorkItemID: native, Revision: issue.Revision, Requirement: providercapacity.Requirement{Role: identity.Role, Backend: identity.BackendKind, Model: identity.RequestedModel.Value}}}
				}
				allowed, reason, err := placementClaimAllowed(ctx, tx, candidateScope, r.MachineID, id, now, capacityQuery.ProviderCandidates, joining)
				if errors.Is(err, errClaimHintStale) && !claimWriting(ctx) {
					return false, nil
				}
				if err != nil {
					return false, err
				}
				if !allowed {
					if err := recordNativeSchedulingOutcome(ctx, tx, &candidateScope, id, tracker.NativeSchedulerDecision{Source: "native_runner_routing", Outcome: "skipped", Reason: reason}, now); err != nil {
						return false, err
					}
					return false, nil
				}

				decision, _, err := checkMonthlyClaim(ctx, tx, candidateScope, r.MachineID, id, false, now)
				if err != nil {
					return false, err
				}
				if !decision.Allowed {
					if err := recordNativeSchedulingOutcome(ctx, tx, &candidateScope, id, tracker.NativeSchedulerDecision{Source: "native_provider_capacity", Outcome: "skipped", Reason: monthlyBudgetRefusal(decision)}, now); err != nil {
						return false, err
					}
					return false, nil
				}
				if _, _, err := selectProviderCapacity(ctx, tx, capacityQuery, id, now); err != nil {
					if errors.Is(err, ErrNoClaimableWork) || isProviderWait(err) {
						return false, nil
					}
					return false, err
				}
				return true, nil
			}()
			if err != nil {
				progress.cursor = before
				return 0, false, err
			}
			progress.visits++
			progress.dirty = true
			if allowed {
				progress.headLane, progress.headPriority = progress.cursor.Landing, progress.cursor.Priority
				progress.cursor = before
				progress.head = id
				progress.done = true
			}
		}
		if progress.head != 0 {
			if project == preferred {
				preferredHead = progress.head
			}
			heads = append(heads, progress.head)
			headPositions[progress.head] = selectionPosition{lane: progress.headLane, rank: r.ProjectRanks[project], priority: progress.headPriority, preferred: project == preferred}
		} else if !progress.done {
			incomplete = append(incomplete, selectionPosition{lane: progress.cursor.Landing, rank: r.ProjectRanks[project], priority: progress.cursor.Priority, preferred: project == preferred})
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
	if preferredHead != 0 && (selected == 0 || headPositions[preferredHead].lane <= headPositions[selected].lane) {
		selected = preferredHead
	}
	if selected != 0 {
		head := headPositions[selected]
		for _, pending := range incomplete {
			switch {
			case pending.lane != head.lane:
				if pending.lane < head.lane {
					return 0, true, nil
				}
			case head.preferred:
			case pending.preferred,
				pending.rank < head.rank,
				pending.rank == head.rank && pending.priority < head.priority:
				return 0, true, nil
			}
		}
	}
	if selected == 0 && contractRefusal != nil {
		return 0, true, contractRefusal
	}
	return selected, true, nil
}

type runnerSelectionCursor struct {
	ID                       tracker.WorkItemID
	Priority, Aging, Landing int
	Created, Identifier      string
}

type runnerSelectionProgress struct {
	cursor               runnerSelectionCursor
	runner               string
	project              tracker.ProjectID
	revision             int64
	visits               int
	head                 tracker.WorkItemID
	headLane             int
	headPriority         int
	done, dirty, durable bool
	written              bool
}

func readRunnerSelectionProgress(ctx context.Context, q nativeQueryer, scope nativeScope, query claimCandidateQuery) (*runnerSelectionProgress, error) {
	prepared := claimPrepared(ctx)
	if prepared != nil {
		if progress := prepared.selection[scope.project]; progress != nil {
			return progress, nil
		}
	}
	progress := &runnerSelectionProgress{runner: scope.credential.Runner.RunnerID, project: scope.project, durable: prepared != nil && query.WorkItemID == 0}
	if progress.durable {
		err := q.QueryRowContext(ctx, `SELECT issue_id,priority,aging,landing,created,identifier,revision FROM runner_selection_cursors WHERE runner_id=? AND project_id=?`, progress.runner, progress.project).Scan(&progress.cursor.ID, &progress.cursor.Priority, &progress.cursor.Aging, &progress.cursor.Landing, &progress.cursor.Created, &progress.cursor.Identifier, &progress.revision)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if prepared != nil {
		if prepared.selection == nil {
			prepared.selection = make(map[tracker.ProjectID]*runnerSelectionProgress)
		}
		prepared.selection[scope.project] = progress
	}
	return progress, nil
}

func writeRunnerSelectionProgress(ctx context.Context, tx *sql.Tx, selection map[tracker.ProjectID]*runnerSelectionProgress) error {
	for _, progress := range selection {
		progress.written = false
		if !progress.durable || !progress.dirty {
			continue
		}
		cursor := progress.cursor
		result, err := tx.ExecContext(ctx, `INSERT INTO runner_selection_cursors(runner_id,project_id,issue_id,priority,aging,landing,created,identifier,revision)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(runner_id,project_id) DO UPDATE SET issue_id=excluded.issue_id,priority=excluded.priority,aging=excluded.aging,landing=excluded.landing,created=excluded.created,identifier=excluded.identifier,revision=excluded.revision WHERE runner_selection_cursors.revision=?`, progress.runner, progress.project, cursor.ID, cursor.Priority, cursor.Aging, cursor.Landing, cursor.Created, cursor.Identifier, progress.revision+1, progress.revision)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		progress.written = rows > 0
	}
	return nil
}
