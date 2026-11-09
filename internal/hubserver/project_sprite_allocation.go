package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

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
	result, err := s.setSpriteAllocationRecord(c.Request().Context(), scope, "PUT "+c.Request().URL.EscapedPath(), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	s.startSpritePoolForQueue(scope)
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSONBlob(http.StatusOK, result)
}

func (s *Service) projectSpriteAllocation(ctx context.Context, scope nativeScope) (projectSpriteAllocation, error) {
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

func (s *Service) projectSpriteAllocationAuthority(ctx context.Context, q nativeQueryer, scope nativeScope) (bool, error) {
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
