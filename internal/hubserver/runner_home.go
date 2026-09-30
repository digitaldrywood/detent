package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (d *database) runnerHomeSelection(ctx context.Context, tx *sql.Tx, query claimCandidateQuery, now time.Time) (tracker.WorkItemID, bool, error) {
	if query.NativeScope == nil || query.NativeScope.credential.Runner.RunnerID == "" || query.WorkspaceLane {
		return 0, false, nil
	}
	r, err := readRunner(ctx, tx, query.NativeScope.organization, query.NativeScope.credential.Runner.RunnerID, now)
	if err != nil {
		return 0, false, err
	}
	if len(r.HomeProjectIDs) == 0 {
		return 0, false, nil
	}
	probe := query
	scope := *query.NativeScope
	scope.project = ""
	probe.NativeScope = &scope
	probe.Scope = ""
	probe.HomeProjects = r.HomeProjectIDs
	ids, err := claimCandidateIDs(ctx, tx, probe, query.RepositoryIDs, normalizedQueryStrings(query.Repositories), []string{"todo", "rework"}, normalizedQueryStrings(query.Authors), normalizedQueryStrings(query.Assignees), normalizedQueryStrings(query.LabelInclude), normalizedQueryStrings(query.LabelExclude), nil)
	if err != nil {
		return 0, false, err
	}
	var selected tracker.WorkItemID
	for _, id := range ids {
		var project tracker.ProjectID
		if err := tx.QueryRowContext(ctx, "SELECT project_id FROM issues WHERE id = ?", id).Scan(&project); err != nil {
			return 0, false, err
		}
		if !slices.Contains(r.HomeProjectIDs, project) || !slices.Contains(r.ProjectIDs, project) {
			continue
		}
		approval, err := readProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(project))
		if err != nil {
			var failure *nativeError
			if errors.As(err, &failure) && failure.Code == "policy_mismatch" {
				continue
			}
			return 0, false, err
		}
		if err := approval.Policy.Requirements.Match(r.RunnerID, string(r.MachineID), r.Tags); err != nil {
			continue
		}
		current, found, err := readUnreleasedLease(ctx, tx, id)
		if err != nil {
			return 0, false, err
		}
		if found && current.session.ExpiresAt.After(now) {
			continue
		}
		var nativeID tracker.NativeWorkItemID
		if err := tx.QueryRowContext(ctx, "SELECT native_id FROM issues WHERE id = ?", id).Scan(&nativeID); err != nil {
			return 0, false, err
		}
		if project == query.NativeScope.project || slices.ContainsFunc(query.ProviderCandidates, func(candidate tracker.NativeCapacityCandidate) bool { return candidate.WorkItemID == nativeID }) {
			if _, _, err := selectProviderCapacity(ctx, tx, query, id, now); err != nil {
				if errors.Is(err, ErrNoClaimableWork) || isProviderWait(err) {
					continue
				}
				return 0, false, err
			}
		}
		selected = id
		break
	}
	var dry any
	if selected == 0 {
		if r.HomeDrySince == nil {
			r.HomeDrySince = &now
		}
		dry = formatHubTime(*r.HomeDrySince)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE runner_identities SET home_dry_since = ? WHERE id = ?", dry, r.RunnerID); err != nil {
		return 0, false, err
	}
	if selected != 0 {
		return selected, true, nil
	}
	return 0, !r.SpilloverEligible(now), nil
}
