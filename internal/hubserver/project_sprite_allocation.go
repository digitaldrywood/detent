package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	result, err := s.readProjectSpriteAllocation(ctx, nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, result)
}

func (s *Service) setProjectSpriteAllocation(c echo.Context) error {
	scope := nativeRequestScope(c)
	if !canManageProjectSecrets(scope.credential) {
		return s.refusedAPIError(c, http.StatusForbidden, "forbidden", "Sprite allocations require owner or admin access", hubapi.APIErrorResponse{Code: "forbidden", Message: "Sprite allocations require owner or admin access"})
	}
	var request projectSpriteAllocationRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope.requireHostedAdmin = true
	result, err := s.setProjectSpriteAllocationRecord(c.Request().Context(), scope, "PUT "+c.Request().URL.EscapedPath(), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	s.startSpritePoolForQueue(scope)
	return c.JSONBlob(http.StatusOK, result)
}

func readSpriteAllocationProjects(ctx context.Context, r tenantstore.Reader, scope nativeScope) ([]tracker.ProjectID, error) {
	owner, ok := r.(postgres.SpriteReader)
	if !ok {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	var allocations []postgres.SpriteAllocation
	after := tracker.ProjectID("")
	for {
		page, err := owner.SpriteAllocations(ctx, after, corestore.MaxPageSize)
		if err != nil {
			return nil, err
		}
		for _, allocation := range page {
			project, err := r.Project(ctx, allocation.ProjectID)
			if err != nil {
				return nil, err
			}
			if project.DeletedAt == nil {
				allocations = append(allocations, allocation)
			}
		}
		if len(page) < corestore.MaxPageSize {
			break
		}
		next := page[len(page)-1].ProjectID
		if next <= after {
			return nil, corestore.ErrInvalidCursor
		}
		after = next
	}
	slices.SortStableFunc(allocations, func(a, b postgres.SpriteAllocation) int {
		if a.RequestedAt.Before(b.RequestedAt) {
			return -1
		}
		if a.RequestedAt.After(b.RequestedAt) {
			return 1
		}
		return strings.Compare(string(a.ProjectID), string(b.ProjectID))
	})
	result := make([]tracker.ProjectID, len(allocations))
	for i, allocation := range allocations {
		result[i] = allocation.ProjectID
	}
	return result, nil
}

func (s *Service) readSpriteAllocationProjects(ctx context.Context, scope nativeScope) ([]tracker.ProjectID, error) {
	var result []tracker.ProjectID
	err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		var err error
		result, err = readSpriteAllocationProjects(ctx, r, scope)
		return err
	})
	return result, err
}

var errSpriteReservationStale = errors.New("project Sprite count changed before this member was ready")

func spriteAllocationRunnerAllowedRecord(ctx context.Context, r tenantstore.Reader, scope nativeScope, record runnerstore.RoutingRecord) (bool, error) {
	identity := record.Record.Identity
	granted := identity.Scope == "organization" || slices.Contains(identity.ProjectIDs, scope.project)
	return spriteAllocationRunnerAllowedFacts(ctx, r, scope, identity, record.Record.Machine, granted)
}

func spriteAllocationRunnerAllowedFacts(ctx context.Context, r tenantstore.Reader, scope nativeScope, identity runnerstore.Identity, machine leasestore.Machine, granted bool) (bool, error) {
	owner, ok := r.(postgres.SpriteReader)
	if !ok {
		return false, hubapi.NativeWorkItemStorageUnavailable()
	}
	target, err := owner.SpriteAllocation(ctx, scope.project)
	managed := err == nil
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return false, err
	}
	member, err := r.RunnerSpriteMembershipByEnrollment(ctx, identity.EnrollmentID)
	if errors.Is(err, corestore.ErrNotFound) {
		if !managed {
			return true, nil
		}
		if machine.SpriteName == machine.Hostname && validSpritesSlug(machine.SpriteName) {
			return true, nil
		}
		placement, _, err := readPlacementPolicy(ctx, r)
		return placement.Mode != "sprites_only", err
	}
	if err != nil {
		return false, err
	}
	if member.AllocationProjectID == "" {
		return !managed, nil
	}
	if managed && target.DesiredCount == 0 {
		return false, nil
	}
	allocation, err := owner.SpriteAllocation(ctx, member.AllocationProjectID)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	pool, err := owner.SpritePool(ctx)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if member.State != "enrolled" || identity.State != "active" || allocation.DesiredCount <= 0 || !granted || member.AllocationProjectID != scope.project && !pool.ShareIdleCapacity {
		return false, nil
	}
	checks, err := r.RunnerLocalChecks(ctx, identity.RunnerID, scope.project)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	return err == nil && checks.WorkReady(), err
}

func (s *Service) spriteAllocationRunnerAllowed(ctx context.Context, scope nativeScope, runnerID string) (bool, error) {
	var allowed bool
	err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		owner, ok := r.(runnerstore.RoutingReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		record, err := owner.RunnerRoutingRecord(ctx, runnerID, s.config.now())
		if err != nil {
			return err
		}
		allowed, err = spriteAllocationRunnerAllowedRecord(ctx, r, scope, record)
		return err
	})
	return allowed, err
}

func spritePoolProviderFree(ctx context.Context, r tenantstore.Reader, scope nativeScope, count int, now time.Time) (int, bool, error) {
	scope.project = ""
	runners, err := readPlacementRunners(ctx, r, scope, now)
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
		membership, err := r.RunnerSpriteMembershipByEnrollment(ctx, runner.Enrollment)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return 0, false, err
		}
		member := err == nil && membership.State != "deleted"
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

func (s *Service) projectSpriteAllocationAuthority(ctx context.Context, r tenantstore.Reader, scope nativeScope) (bool, error) {
	owner, ok := r.(postgres.SpriteReader)
	if !ok {
		return false, hubapi.NativeWorkItemStorageUnavailable()
	}
	return owner.SpriteAllocationAuthority(ctx, scope.project, s.config.now())
}

func projectSpriteAllocationRecord(ctx context.Context, r tenantstore.Reader, scope nativeScope) (projectSpriteAllocation, postgres.SpriteAllocation, error) {
	result := projectSpriteAllocation{ProjectID: scope.project}
	owner, ok := r.(postgres.SpriteReader)
	if !ok {
		return result, postgres.SpriteAllocation{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	row, err := owner.SpriteAllocation(ctx, scope.project)
	if errors.Is(err, corestore.ErrNotFound) {
		return result, row, nil
	}
	if err != nil {
		return result, row, err
	}
	result.Managed, result.DesiredCount, result.ProvisionFailures, result.Revision = true, int(row.DesiredCount), int(row.ProvisionFailures), tracker.Revision(row.Revision)
	result.AllocatedCount, err = owner.SpriteAllocationSize(ctx, scope.project)
	if err != nil {
		return result, row, err
	}
	result.Shortfall = max(result.DesiredCount-result.AllocatedCount, 0)
	return result, row, nil
}

func (s *Service) readProjectSpriteAllocation(ctx context.Context, scope nativeScope) (projectSpriteAllocation, error) {
	var result projectSpriteAllocation
	err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		var err error
		result, _, err = projectSpriteAllocationRecord(ctx, r, scope)
		return err
	})
	return result, hubapi.ProjectRecordError(err)
}

func (s *Service) setProjectSpriteAllocationRecord(ctx context.Context, scope nativeScope, operation string, request projectSpriteAllocationRequest) (json.RawMessage, error) {
	scope.requireHostedAdmin = true
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	if request.DesiredCount < 0 || int64(request.DesiredCount) > 2147483647 || request.ExpectedRevision < 0 {
		return nil, spritePoolInvalid("Sprite desired count and expected revision must be nonnegative supported values")
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	if raw, found, err := s.authorizedChangeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash); found || err != nil {
		return raw, err
	}
	var current projectSpriteAllocation
	var row postgres.SpriteAllocation
	err = s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		owner, ok := r.(postgres.SpriteReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if _, err := owner.SpritePool(ctx); errors.Is(err, corestore.ErrNotFound) {
			return spritePoolInvalid("Configure the organization Sprite pool before allocating project members")
		} else if err != nil {
			return err
		}
		var err error
		current, row, err = projectSpriteAllocationRecord(ctx, r, scope)
		return err
	})
	if err != nil {
		return nil, err
	}
	if current.Revision != request.ExpectedRevision {
		return nil, hubapi.NativeConflict(current.Revision)
	}
	actor := scope.credential.ID
	if scope.credential.HostedPrincipal != "" {
		actor = scope.credential.HostedPrincipal
	}
	row.OrganizationID, row.ProjectID, row.DesiredCount, row.ConfiguredBy = scope.organization, scope.project, int32(request.DesiredCount), actor
	if row.RequestedAt.IsZero() || request.DesiredCount >= current.AllocatedCount {
		row.RequestedAt = s.config.now()
	}
	result := current
	result.Managed, result.DesiredCount, result.ProvisionFailures, result.Revision = true, request.DesiredCount, 0, current.Revision+1
	result.Shortfall = max(result.DesiredCount-result.AllocatedCount, 0)
	return s.tenantReceiptMutation(ctx, scope, operation, request.Mutation, request, result, func(w tenantstore.Writer) error {
		if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", s.config.now()); err != nil {
			return err
		}
		owner, ok := w.(postgres.SpriteWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if _, err := owner.SpritePool(ctx); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		latest, _, err := projectSpriteAllocationRecord(ctx, w, scope)
		if err != nil {
			return err
		}
		if latest.Revision != current.Revision || latest.AllocatedCount != current.AllocatedCount {
			return hubapi.NativeConflict(latest.Revision)
		}
		_, err = owner.PutSpriteAllocation(ctx, row, int64(request.ExpectedRevision))
		if errors.Is(err, corestore.ErrRevisionConflict) || errors.Is(err, corestore.ErrUniqueViolation) {
			return hubapi.NativeConflict(current.Revision)
		}
		if errors.Is(err, corestore.ErrInvalidPage) {
			return spritePoolInvalid("Sprite allocation values are invalid")
		}
		return err
	})
}

func spriteMemberWantedRecord(ctx context.Context, r tenantstore.Reader, name string) (bool, error) {
	owner, ok := r.(postgres.SpriteReader)
	if !ok {
		return false, hubapi.NativeWorkItemStorageUnavailable()
	}
	member, err := owner.SpriteMember(ctx, name)
	if err != nil {
		return false, err
	}
	if member.State != "bootstrapping" && member.State != "enrolled" {
		return false, nil
	}
	if member.AllocationProjectID == "" {
		return true, nil
	}
	projectID := tracker.ProjectID(member.AllocationProjectID)
	project, err := r.Project(ctx, projectID)
	if err != nil {
		return false, err
	}
	if project.DeletedAt != nil {
		return false, nil
	}
	allocation, err := owner.SpriteAllocation(ctx, projectID)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	pool, err := owner.SpritePool(ctx)
	if err != nil {
		return false, err
	}
	_, others, ahead, err := owner.SpriteMemberPlacementCounts(ctx, member.Name)
	if err != nil {
		return false, err
	}
	return ahead < min(int(allocation.DesiredCount), max(int(pool.MaxRunners)-others, 0)), nil
}
