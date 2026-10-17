package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type projectSpriteAllocation struct {
	ProjectID      tracker.ProjectID `json:"project_id"`
	Managed        bool              `json:"managed"`
	DesiredCount   int               `json:"desired_count"`
	AllocatedCount int               `json:"allocated_count"`
	Shortfall      int               `json:"shortfall"`
	Revision       tracker.Revision  `json:"revision,string"`
}

type projectSpriteAllocationRequest struct {
	tracker.Mutation
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
	DesiredCount     int              `json:"desired_count"`
}

func readProjectSpriteAllocation(ctx context.Context, q nativeQueryer, scope nativeScope) (projectSpriteAllocation, error) {
	result := projectSpriteAllocation{ProjectID: scope.project}
	err := q.QueryRowContext(ctx, `SELECT desired_count,revision,(SELECT count(*) FROM organization_sprite_members m WHERE m.organization_id=a.organization_id AND m.allocation_project_id=a.project_id AND m.state<>'deleted') FROM project_sprite_allocations a WHERE organization_id=? AND project_id=?`, scope.organization, scope.project).Scan(&result.DesiredCount, &result.Revision, &result.AllocatedCount)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	result.Managed = err == nil
	result.Shortfall = max(result.DesiredCount-result.AllocatedCount, 0)
	return result, err
}

func (s *Service) getProjectSpriteAllocation(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	result, err := readProjectSpriteAllocation(ctx, s.database.reader, nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, result)
}

func (s *Service) setProjectSpriteAllocation(c echo.Context) error {
	scope := nativeRequestScope(c)
	if !canManageProjectSecrets(scope.credential) {
		return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "forbidden", Message: "Sprite allocations require owner or admin access"})
	}
	var request projectSpriteAllocationRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	scope.requireHostedAdmin = true
	result, err := s.executeNativeMutation(c.Request().Context(), scope, nativeCommandOptions{OperationID: "PUT " + c.Request().URL.EscapedPath(), Feature: "collaboration", DeferBoardProjection: true}, request.Mutation, request, updateProjectSpriteAllocation(request))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	s.startSpritePoolForQueue(scope)
	return c.JSONBlob(http.StatusOK, result)
}

func updateProjectSpriteAllocation(request projectSpriteAllocationRequest) func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error) {
	return func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		if request.DesiredCount < 0 || request.ExpectedRevision < 0 {
			return nil, nativeInvalid("Sprite desired count and expected revision must be nonnegative")
		}
		var configured int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM organization_sprite_pools WHERE organization_id=?`, scope.organization).Scan(&configured); err != nil {
			return nil, err
		}
		if configured == 0 {
			return nil, nativeInvalid("Configure the organization Sprite pool before allocating project members")
		}
		current, err := readProjectSpriteAllocation(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if current.Revision != request.ExpectedRevision {
			return nil, nativeConflict(current.Revision)
		}
		actor := scope.credential.ID
		if scope.credential.HostedPrincipal != "" {
			actor = scope.credential.HostedPrincipal
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO project_sprite_allocations(organization_id,project_id,desired_count,revision,requested_at,configured_by) VALUES(?,?,?,1,?,?) ON CONFLICT(organization_id,project_id) DO UPDATE SET desired_count=excluded.desired_count,revision=project_sprite_allocations.revision+1,requested_at=CASE WHEN ? >= project_sprite_allocations.desired_count THEN excluded.requested_at ELSE project_sprite_allocations.requested_at END,configured_by=excluded.configured_by`, scope.organization, scope.project, request.DesiredCount, formatHubTime(now), actor, current.AllocatedCount)
		if err != nil {
			return nil, err
		}
		return readProjectSpriteAllocation(ctx, tx, scope)
	}
}

func readSpriteAllocationProjects(ctx context.Context, q nativeQueryer, scope nativeScope) ([]tracker.ProjectID, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.project_id FROM project_sprite_allocations a JOIN projects p ON p.id=a.project_id AND p.organization_id=a.organization_id WHERE a.organization_id=? AND p.deleted_at IS NULL ORDER BY julianday(a.requested_at),a.project_id`, scope.organization)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var projects []tracker.ProjectID
	for rows.Next() {
		var project tracker.ProjectID
		if err := rows.Scan(&project); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

func spriteAllocationRunnerAllowed(ctx context.Context, q nativeQueryer, scope nativeScope, machine tracker.MachineID) (bool, error) {
	var allowed bool
	err := q.QueryRowContext(ctx, `SELECT CASE WHEN EXISTS(SELECT 1 FROM project_sprite_allocations WHERE organization_id=? AND project_id=?) THEN EXISTS(SELECT 1 FROM organization_sprite_members m JOIN runner_identities r ON r.enrollment_id=m.enrollment_id AND r.organization_id=m.organization_id JOIN project_sprite_allocations a ON a.organization_id=m.organization_id AND a.project_id=m.allocation_project_id WHERE m.organization_id=? AND m.allocation_project_id=? AND m.state='enrolled' AND a.desired_count>0 AND r.machine_id=?) ELSE NOT EXISTS(SELECT 1 FROM organization_sprite_members m JOIN runner_identities r ON r.enrollment_id=m.enrollment_id AND r.organization_id=m.organization_id WHERE m.organization_id=? AND m.allocation_project_id<>'' AND r.machine_id=?) END`, scope.organization, scope.project, scope.organization, scope.project, machine, scope.organization, machine).Scan(&allowed)
	return allowed, err
}

func spritePoolProviderFree(ctx context.Context, q nativeQueryer, scope nativeScope, count int, now time.Time) (int, bool, error) {
	scope.project = ""
	runners, err := readPlacementRunners(ctx, q, scope, now)
	if err != nil {
		return 0, false, err
	}
	type account struct {
		scopedProviderAccount
		limit, used, poolUsed int
	}
	accounts := make([]account, 0)
	for _, runner := range runners {
		if !runner.Sprite {
			continue
		}
		var member bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM organization_sprite_members WHERE organization_id=? AND enrollment_id=? AND state<>'deleted')`, scope.organization, runner.Enrollment).Scan(&member); err != nil {
			return 0, false, err
		}
		for _, view := range runner.ProviderCapacity {
			if view.MaxConcurrent <= 0 && view.State != "exhausted" {
				continue
			}
			candidate := scopedProviderAccount{runnerID: runner.RunnerID, Report: view.Report}
			index := -1
			for i, prior := range accounts {
				if sharedProviderAccount(prior.scopedProviderAccount, candidate) {
					index = i
					break
				}
			}
			limit := view.MaxConcurrent
			if view.State == "exhausted" {
				limit = 0
			}
			if index < 0 {
				accounts = append(accounts, account{scopedProviderAccount: candidate, limit: limit})
				index = len(accounts) - 1
			}
			accounts[index].limit = min(accounts[index].limit, limit)
			accounts[index].used = max(accounts[index].used, view.Used)
			if member {
				accounts[index].poolUsed += runner.Used
			}
		}
	}
	if len(accounts) == 0 {
		return 0, false, nil
	}
	capacity := 0
	for _, account := range accounts {
		capacity += max(account.limit-max(account.used-account.poolUsed, 0), 0)
	}
	return max(capacity-count, 0), true, nil
}

func (s *Service) projectSpriteAllocationAuthority(ctx context.Context, q nativeQueryer, scope nativeScope) (bool, error) {
	var allowed bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_sprite_allocations a JOIN api_tokens t ON t.id=a.configured_by AND t.revoked_at IS NULL JOIN projects p ON p.id=a.project_id AND p.organization_id=a.organization_id AND p.deleted_at IS NULL WHERE a.organization_id=? AND a.project_id=? AND (t.expires_at IS NULL OR julianday(t.expires_at)>julianday(?)) AND ((t.scope='admin' AND t.native_only=0) OR EXISTS(SELECT 1 FROM hosted_members m JOIN hosted_project_grants g ON g.user_id=m.user_id AND g.organization_id=a.organization_id AND g.project_id=a.project_id AND g.can_write=1 WHERE m.principal_id=t.id AND m.active=1 AND m.role IN ('owner','admin'))))`, scope.organization, scope.project, formatHubTime(s.config.now())).Scan(&allowed)
	return allowed, err
}
