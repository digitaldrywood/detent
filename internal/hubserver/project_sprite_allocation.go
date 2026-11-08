package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type projectSpriteAllocation struct {
	ProjectID         tracker.ProjectID `json:"project_id"`
	Managed           bool              `json:"managed"`
	DesiredCount      int               `json:"desired_count"`
	AllocatedCount    int               `json:"allocated_count"`
	Shortfall         int               `json:"shortfall"`
	ProvisionFailures int               `json:"provision_failures"`
	Revision          tracker.Revision  `json:"revision,string"`
}

type projectSpriteAllocationRequest struct {
	tracker.Mutation
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
	DesiredCount     int              `json:"desired_count"`
}

func readProjectSpriteAllocation(ctx context.Context, q nativeQueryer, scope nativeScope) (projectSpriteAllocation, error) {
	result := projectSpriteAllocation{ProjectID: scope.project}
	err := q.QueryRowContext(ctx, `SELECT desired_count,revision,provision_failures,(SELECT count(*) FROM organization_sprite_members m WHERE m.organization_id=a.organization_id AND m.allocation_project_id=a.project_id AND m.state<>'deleted') FROM project_sprite_allocations a WHERE organization_id=? AND project_id=?`, scope.organization, scope.project).Scan(&result.DesiredCount, &result.Revision, &result.ProvisionFailures, &result.AllocatedCount)
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
	result, err := s.projectSpriteAllocation(ctx, nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, result)
}

func (s *Service) setProjectSpriteAllocation(c echo.Context) error {
	scope := nativeRequestScope(c)
	if !canManageProjectSecrets(scope.credential) {
		return s.refusedAPIError(c, http.StatusForbidden, "forbidden", "Sprite allocations require owner or admin access", apiErrorResponse{Code: "forbidden", Message: "Sprite allocations require owner or admin access"})
	}
	var request projectSpriteAllocationRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope.requireHostedAdmin = true
	if s.config.TenantRepository != nil {
		result, err := s.setSpriteAllocationRecord(c.Request().Context(), scope, "PUT "+c.Request().URL.EscapedPath(), request)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		s.startSpritePoolForQueue(scope)
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSONBlob(http.StatusOK, result)
	}
	result, err := s.executeNativeMutation(c.Request().Context(), scope, nativeCommandOptions{OperationID: "PUT " + c.Request().URL.EscapedPath(), Feature: "collaboration"}, request.Mutation, request, updateProjectSpriteAllocation(request))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	s.startSpritePoolForQueue(scope)
	return c.JSONBlob(http.StatusOK, result)
}

func (s *Service) projectSpriteAllocation(ctx context.Context, scope nativeScope) (projectSpriteAllocation, error) {
	if s.config.TenantRepository == nil {
		return readProjectSpriteAllocation(ctx, s.database.reader, scope)
	}
	var result projectSpriteAllocation
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		var err error
		result, err = readSpriteAllocationRecord(ctx, owner, scope.project)
		return err
	})
	return result, err
}

func readSpriteAllocationRecord(ctx context.Context, r postgres.SpriteReader, project tracker.ProjectID) (projectSpriteAllocation, error) {
	result := projectSpriteAllocation{ProjectID: project}
	row, err := r.SpriteAllocation(ctx, project)
	if errors.Is(err, corestore.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	count, err := r.SpriteActiveMemberCount(ctx, project)
	result.Managed, result.DesiredCount, result.Revision = true, int(row.DesiredCount), tracker.Revision(row.Revision)
	result.AllocatedCount, result.ProvisionFailures = int(count), int(row.ProvisionFailures)
	result.Shortfall = max(result.DesiredCount-result.AllocatedCount, 0)
	return result, err
}

func (s *Service) setSpriteAllocationRecord(ctx context.Context, scope nativeScope, operation string, request projectSpriteAllocationRequest) (json.RawMessage, error) {
	scope.requireHostedAdmin = true
	if !canManageProjectSecrets(scope.credential) || scope.project == "" {
		return nil, accessstore.ErrDenied
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 || request.DesiredCount < 0 || request.DesiredCount > 100 || request.ExpectedRevision < 0 {
		return nil, spritePoolInvalid("A command key, nonnegative revision and desired count between 0 and 100 are required")
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash); found || err != nil {
		return raw, err
	}
	current, err := s.projectSpriteAllocation(ctx, scope)
	if err != nil {
		return nil, err
	}
	if current.Revision != request.ExpectedRevision {
		return nil, nativeConflict(current.Revision)
	}
	value := projectSpriteAllocation{ProjectID: scope.project, Managed: true, DesiredCount: request.DesiredCount, AllocatedCount: current.AllocatedCount, Shortfall: max(request.DesiredCount-current.AllocatedCount, 0), Revision: current.Revision + 1}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: s.config.now()}, raw)
	if err != nil {
		return nil, err
	}
	var previous *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if existing, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey); err == nil {
			if existing.RequestHash != hash {
				return nativeConflict(0)
			}
			previous = &existing
			return nil
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		project, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil {
			return nativeNotFound()
		}
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return accessstore.ErrTransactionRequired
		}
		if _, err := owner.LockSpritePool(ctx); errors.Is(err, corestore.ErrNotFound) {
			return spritePoolInvalid("Configure the organization Sprite pool before allocating project members")
		} else if err != nil {
			return err
		}
		count, err := owner.SpriteActiveMemberCount(ctx, scope.project)
		if err != nil {
			return err
		}
		if count != int64(current.AllocatedCount) {
			return nativeConflict(current.Revision)
		}
		_, err = owner.PutSpriteAllocation(ctx, postgres.SpriteAllocation{OrganizationID: scope.organization, ProjectID: scope.project, DesiredCount: int32(request.DesiredCount), RequestedAt: s.config.now(), ConfiguredBy: secretActor(scope)}, int64(request.ExpectedRevision))
		if err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		if errors.Is(err, corestore.ErrRevisionConflict) || errors.Is(err, corestore.ErrUniqueViolation) {
			if raw, found, readErr := s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash); found || readErr != nil {
				return raw, readErr
			}
		}
		return nil, projectRecordError(err)
	}
	if previous != nil {
		return content.Receipt(ctx, *previous)
	}
	return raw, nil
}

func updateProjectSpriteAllocation(request projectSpriteAllocationRequest) func(context.Context, *sql.Tx, nativeScope, time.Time) (any, error) {
	return func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		if request.DesiredCount < 0 || request.ExpectedRevision < 0 {
			return nil, spritePoolInvalid("Sprite desired count and expected revision must be nonnegative")
		}
		var configured int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM organization_sprite_pools WHERE organization_id=?`, scope.organization).Scan(&configured); err != nil {
			return nil, err
		}
		if configured == 0 {
			return nil, spritePoolInvalid("Configure the organization Sprite pool before allocating project members")
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
		_, err = tx.ExecContext(ctx, `INSERT INTO project_sprite_allocations(organization_id,project_id,desired_count,revision,requested_at,configured_by) VALUES(?,?,?,1,?,?) ON CONFLICT(organization_id,project_id) DO UPDATE SET desired_count=excluded.desired_count,revision=project_sprite_allocations.revision+1,provision_failures=0,requested_at=CASE WHEN ? >= project_sprite_allocations.desired_count THEN excluded.requested_at ELSE project_sprite_allocations.requested_at END,configured_by=excluded.configured_by`, scope.organization, scope.project, request.DesiredCount, formatHubTime(now), actor, current.AllocatedCount)
		if err != nil {
			return nil, err
		}
		return readProjectSpriteAllocation(ctx, tx, scope)
	}
}

func readSpriteAllocationProjects(ctx context.Context, q nativeQueryer, scope nativeScope) ([]tracker.ProjectID, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.project_id FROM project_sprite_allocations a JOIN projects p ON p.id=a.project_id AND p.organization_id=a.organization_id WHERE a.organization_id=? AND p.deleted_at IS NULL ORDER BY a.requested_at,a.project_id`, scope.organization)
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

const spriteProvisionAttempts = 3

var errSpriteReservationStale = errors.New("project Sprite count changed before this member was ready")

func spriteAllocationRunnerProject(ctx context.Context, q nativeQueryer, scope nativeScope, machine tracker.MachineID) (tracker.ProjectID, error) {
	var project tracker.ProjectID
	err := q.QueryRowContext(ctx, `SELECT m.allocation_project_id FROM organization_sprite_members m JOIN runner_identities r ON r.enrollment_id=m.enrollment_id AND r.organization_id=m.organization_id WHERE m.organization_id=? AND r.machine_id=? AND m.allocation_project_id<>''`, scope.organization, machine).Scan(&project)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return project, err
}

func spriteAllocationRunnerAllowed(ctx context.Context, q nativeQueryer, scope nativeScope, machine tracker.MachineID) (bool, error) {
	var target int
	var allocation sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT desired_count FROM project_sprite_allocations WHERE organization_id=? AND project_id=?),-1), (SELECT max(m.allocation_project_id) FROM organization_sprite_members m JOIN runner_identities r ON r.enrollment_id=m.enrollment_id AND r.organization_id=m.organization_id WHERE m.organization_id=? AND r.machine_id=?)`, scope.organization, scope.project, scope.organization, machine).Scan(&target, &allocation); err != nil {
		return false, err
	}
	if !allocation.Valid || allocation.String == "" {
		if target < 0 {
			return true, nil
		}
		if allocation.Valid {
			return false, nil
		}
		facts, err := readMachineSpriteFacts(ctx, q, scope.organization, machine)
		if err != nil {
			return false, err
		}
		name := spriteFactValue(facts.Name)
		var hostname string
		if err := q.QueryRowContext(ctx, `SELECT hostname FROM machines WHERE id=? AND organization_id=?`, machine, scope.organization).Scan(&hostname); err != nil {
			return false, err
		}
		if name == hostname && validSpritesSlug(name) {
			return true, nil
		}
		placement, _, err := readPlacementPolicy(ctx, q, scope)
		return placement.Mode != "sprites_only", err
	}
	if target == 0 {
		return false, nil
	}
	var owner tracker.ProjectID
	var ready, sharing, granted bool
	var runner string
	err := q.QueryRowContext(ctx, `SELECT m.allocation_project_id,m.state='enrolled' AND r.state='active' AND COALESCE(a.desired_count,0)>0,p.share_idle_capacity,
 r.scope='organization' OR EXISTS(SELECT 1 FROM token_grants g WHERE g.token_id=r.token_id AND g.organization_id=r.organization_id AND g.project_id=?),r.id
 FROM organization_sprite_members m JOIN runner_identities r ON r.enrollment_id=m.enrollment_id AND r.organization_id=m.organization_id
 LEFT JOIN project_sprite_allocations a ON a.organization_id=m.organization_id AND a.project_id=m.allocation_project_id
 JOIN organization_sprite_pools p ON p.organization_id=m.organization_id
 WHERE m.organization_id=? AND m.allocation_project_id<>'' AND r.machine_id=?`, scope.project, scope.organization, machine).Scan(&owner, &ready, &sharing, &granted, &runner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !ready || !granted || owner != scope.project && !sharing {
		return false, nil
	}
	raw, err := readLocalCheckFacts(ctx, q, runner, scope.project)
	if err != nil {
		return false, err
	}
	checks, observed, err := decodeSpriteMemberChecks(raw)
	return observed && checks.WorkReady(), err
}

func decodeSpriteMemberChecks(raw sql.NullString) (runnerauth.LocalChecks, bool, error) {
	var checks runnerauth.LocalChecks
	if !raw.Valid || raw.String == "" {
		return checks, false, nil
	}
	err := json.Unmarshal([]byte(raw.String), &checks)
	return checks, err == nil, err
}

func spriteMemberWanted(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, name string) (bool, error) {
	var project tracker.ProjectID
	var state, created string
	if err := q.QueryRowContext(ctx, `SELECT allocation_project_id,state,created_at FROM organization_sprite_members WHERE organization_id=? AND name=?`, organization, name).Scan(&project, &state, &created); err != nil {
		return false, err
	}
	if state != "bootstrapping" && state != "enrolled" {
		return false, nil
	}
	if project == "" {
		return true, nil
	}
	var desired, ceiling, others, ahead int
	err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT a.desired_count FROM project_sprite_allocations a JOIN projects p ON p.id=a.project_id AND p.organization_id=a.organization_id AND p.deleted_at IS NULL WHERE a.organization_id=? AND a.project_id=?),0),
COALESCE((SELECT max_runners FROM organization_sprite_pools WHERE organization_id=?),0),
(SELECT count(*) FROM organization_sprite_members WHERE organization_id=? AND allocation_project_id<>? AND state NOT IN ('deleted','deleting')),
(SELECT count(*) FROM organization_sprite_members WHERE organization_id=? AND allocation_project_id=? AND name<>? AND (state='enrolled' OR state='bootstrapping' AND (created_at<? OR created_at=? AND name<?)))`,
		organization, project, organization, organization, project, organization, project, name, created, created, name).Scan(&desired, &ceiling, &others, &ahead)
	if err != nil {
		return false, err
	}
	return ahead < min(desired, max(ceiling-others, 0)), nil
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
			if view.MaxConcurrent <= 0 {
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
	if s.config.TenantRepository != nil {
		var allowed bool
		err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			owner, ok := r.(postgres.SpriteReader)
			if !ok {
				return accessstore.ErrTransactionRequired
			}
			var err error
			allowed, err = owner.SpriteAllocationAuthority(ctx, scope.project, s.config.now())
			return err
		})
		return allowed, err
	}
	var allowed bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_sprite_allocations a JOIN api_tokens t ON t.id=a.configured_by AND t.revoked_at IS NULL JOIN projects p ON p.id=a.project_id AND p.organization_id=a.organization_id AND p.deleted_at IS NULL WHERE a.organization_id=? AND a.project_id=? AND (t.expires_at IS NULL OR julianday(t.expires_at)>julianday(?)) AND ((t.scope='admin' AND t.native_only=0) OR EXISTS(SELECT 1 FROM hosted_members m JOIN hosted_project_grants g ON g.user_id=m.user_id AND g.organization_id=a.organization_id AND g.project_id=a.project_id AND g.can_write=1 WHERE m.principal_id=t.id AND m.active=1 AND m.role IN ('owner','admin'))))`, scope.organization, scope.project, formatHubTime(s.config.now())).Scan(&allowed)
	return allowed, err
}
