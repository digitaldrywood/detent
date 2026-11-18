package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type SpritePool = pgquery.OrganizationSpritePool
type SpriteAllocation = pgquery.ProjectSpriteAllocation
type SpriteMember = pgquery.RunnerSpriteMembership
type SpriteOperation = pgquery.SpriteLifecycleOperation

type SpritePoolMember struct {
	SpriteMember
	RunnerID string
}

type SpriteReader interface {
	SpritePool(context.Context) (SpritePool, error)
	SpritePoolMembers(context.Context) ([]SpritePoolMember, error)
	SpritePoolAuthority(context.Context, time.Time) (string, error)
	SpriteAllocation(context.Context, tracker.ProjectID) (SpriteAllocation, error)
	SpriteAllocationSize(context.Context, tracker.ProjectID) (int, error)
	SpriteAllocationAuthority(context.Context, tracker.ProjectID, time.Time) (bool, error)
	SpriteAllocations(context.Context, tracker.ProjectID, int) ([]SpriteAllocation, error)
	SpriteMembers(context.Context, string, int) ([]SpriteMember, error)
	SpriteMember(context.Context, string) (SpriteMember, error)
	SpriteMemberPlacementCounts(context.Context, string) (int, int, int, error)
	SpriteLeaseUsage(context.Context, time.Time, string) (int, error)
	SpriteMemberHoldsWork(context.Context, string) (bool, error)
	SpriteOperation(context.Context, string) (SpriteOperation, error)
	PendingSpriteOperations(context.Context, time.Time, int) ([]SpriteOperation, error)
}

type SpriteWriter interface {
	SpriteReader
	PutSpritePool(context.Context, SpritePool, int64) (SpritePool, error)
	PutSpriteAllocation(context.Context, SpriteAllocation, int64) (SpriteAllocation, error)
	CreateRunnerSpriteMembership(context.Context, runnerstore.SpriteMembership) error
	UpdateRunnerSpriteMembership(context.Context, runnerstore.SpriteMembership) error
	CreateSpriteOperation(context.Context, SpriteOperation) (SpriteOperation, error)
	UpdateSpriteOperation(context.Context, SpriteOperation, int64) error
	RecordSpriteProvisionFailure(context.Context, tracker.ProjectID, int64) error
}

func (r *Repository) ReadSprite(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(SpriteReader) error) error {
	return r.transaction(ctx, organization, workload, true, func(s *session) error { return read(s) })
}

func (r *Repository) WriteSprite(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(SpriteWriter) error) error {
	return r.transaction(ctx, organization, workload, false, func(s *session) error { return write(s) })
}

func (s *session) SpritePool(ctx context.Context) (SpritePool, error) {
	row, err := s.queries.SpritePool(ctx, s.organization)
	return row, mapError(ctx, err)
}

func (s *session) SpritePoolMembers(ctx context.Context) ([]SpritePoolMember, error) {
	rows, err := s.db.Query(ctx, `WITH selected AS MATERIALIZED (
 SELECT name FROM runner_sprite_memberships WHERE organization_id=$1 AND state IN ('bootstrapping','enrolled','deleting')
 UNION ALL
 (SELECT name FROM runner_sprite_memberships WHERE organization_id=$1 AND state='deleted' ORDER BY created_at DESC,name DESC LIMIT 20)
)
SELECT m.organization_id,m.name,m.provider_organization,m.token_project_id,m.enrollment_id,m.state,m.bootstrap_log,m.idle_since,m.created_at,m.allocation_project_id,m.allocation_revision,COALESCE(r.runner_id,'')
FROM selected
JOIN runner_sprite_memberships m ON m.organization_id=$1 AND m.name=selected.name
LEFT JOIN runner_identities r ON r.organization_id=m.organization_id AND r.enrollment_id=m.enrollment_id
ORDER BY m.created_at,m.name`, s.organization)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []SpritePoolMember{}
	for rows.Next() {
		var row SpritePoolMember
		if err := rows.Scan(&row.OrganizationID, &row.Name, &row.ProviderOrganization, &row.TokenProjectID, &row.EnrollmentID, &row.State, &row.BootstrapLog, &row.IdleSince, &row.CreatedAt, &row.AllocationProjectID, &row.AllocationRevision, &row.RunnerID); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) SpritePoolAuthority(ctx context.Context, now time.Time) (string, error) {
	if now.IsZero() {
		return "", corestore.ErrInvalidPage
	}
	var actor string
	err := s.db.QueryRow(ctx, `SELECT p.configured_by FROM organization_sprite_pools p
JOIN api_tokens t ON t.organization_id=p.organization_id AND t.id=p.configured_by
WHERE p.organization_id=$1 AND t.revoked_at IS NULL AND t.created_at<=$2 AND (t.expires_at IS NULL OR t.expires_at>$2)
AND ((t.scope='admin' AND NOT t.native_only) OR EXISTS (
 SELECT 1 FROM hosted_members m WHERE m.organization_id=t.organization_id AND m.principal_id=t.id AND m.active AND m.role IN ('owner','admin')
))`, s.organization, now).Scan(&actor)
	return actor, mapError(ctx, err)
}

func (s *session) PutSpritePool(ctx context.Context, row SpritePool, expected int64) (SpritePool, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return SpritePool{}, err
	}
	placement := policy.Placement{Mode: row.PlacementMode, TodoThreshold: int(row.TodoThreshold), OverflowSlots: int(row.OverflowSlots), MaxSpriteSlots: int(row.MaxSpriteSlots)}
	if expected < 0 || placement.Validate() != nil || row.MinRunners < 0 || row.MaxRunners < row.MinRunners || row.MaxRunners > 100 || row.IdleSeconds < 30 || row.ConfiguredBy == "" || (row.IsolationTier != "sandbox" && row.IsolationTier != "native-trusted") || row.BootstrapObjectKey != nil && *row.BootstrapObjectKey == "" {
		return SpritePool{}, corestore.ErrInvalidPage
	}
	var result SpritePool
	var err error
	if expected == 0 {
		result, err = s.queries.PutSpritePool(ctx, pgquery.PutSpritePoolParams{OrganizationID: s.organization, MinRunners: row.MinRunners, MaxRunners: row.MaxRunners, IdleSeconds: row.IdleSeconds, BootstrapObjectKey: row.BootstrapObjectKey, ConfiguredBy: row.ConfiguredBy, IsolationTier: row.IsolationTier, ShareIdleCapacity: row.ShareIdleCapacity, PlacementMode: row.PlacementMode, TodoThreshold: row.TodoThreshold, OverflowSlots: row.OverflowSlots, MaxSpriteSlots: row.MaxSpriteSlots, ExpectedRevision: expected})
	} else {
		result, err = s.queries.UpdateSpritePool(ctx, pgquery.UpdateSpritePoolParams{OrganizationID: s.organization, MinRunners: row.MinRunners, MaxRunners: row.MaxRunners, IdleSeconds: row.IdleSeconds, BootstrapObjectKey: row.BootstrapObjectKey, ConfiguredBy: row.ConfiguredBy, IsolationTier: row.IsolationTier, ShareIdleCapacity: row.ShareIdleCapacity, PlacementMode: row.PlacementMode, TodoThreshold: row.TodoThreshold, OverflowSlots: row.OverflowSlots, MaxSpriteSlots: row.MaxSpriteSlots, ExpectedRevision: expected})
	}
	return result, updated(ctx, err)
}

func (s *session) SpriteAllocation(ctx context.Context, project tracker.ProjectID) (SpriteAllocation, error) {
	row, err := s.queries.SpriteAllocation(ctx, pgquery.SpriteAllocationParams{OrganizationID: s.organization, ProjectID: project})
	return row, mapError(ctx, err)
}

func (s *session) PutSpriteAllocation(ctx context.Context, row SpriteAllocation, expected int64) (SpriteAllocation, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return SpriteAllocation{}, err
	}
	if row.ProjectID == "" || row.DesiredCount < 0 || row.DesiredCount > 100 || row.RequestedAt.IsZero() || row.ConfiguredBy == "" || expected < 0 {
		return SpriteAllocation{}, corestore.ErrInvalidPage
	}
	var result SpriteAllocation
	var err error
	if expected == 0 {
		result, err = s.queries.CreateSpriteAllocation(ctx, pgquery.CreateSpriteAllocationParams{OrganizationID: s.organization, ProjectID: row.ProjectID, DesiredCount: row.DesiredCount, RequestedAt: row.RequestedAt, ConfiguredBy: row.ConfiguredBy})
	} else {
		result, err = s.queries.UpdateSpriteAllocation(ctx, pgquery.UpdateSpriteAllocationParams{OrganizationID: s.organization, ProjectID: row.ProjectID, DesiredCount: row.DesiredCount, RequestedAt: row.RequestedAt, ConfiguredBy: row.ConfiguredBy, ExpectedRevision: expected})
	}
	return result, updated(ctx, err)
}

func (s *session) SpriteAllocations(ctx context.Context, after tracker.ProjectID, limit int) ([]SpriteAllocation, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.SpriteAllocations(ctx, pgquery.SpriteAllocationsParams{OrganizationID: s.organization, ProjectID: after, Limit: int32(limit)})
	return rows, mapError(ctx, err)
}

func (s *session) SpriteMembers(ctx context.Context, after string, limit int) ([]SpriteMember, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.SpriteMembers(ctx, pgquery.SpriteMembersParams{OrganizationID: s.organization, Name: after, Limit: int32(limit)})
	return rows, mapError(ctx, err)
}

func (s *session) SpriteOperation(ctx context.Context, key string) (SpriteOperation, error) {
	row, err := s.queries.SpriteOperation(ctx, pgquery.SpriteOperationParams{OrganizationID: s.organization, IdempotencyKey: key})
	return row, mapError(ctx, err)
}

func (s *session) CreateSpriteOperation(ctx context.Context, row SpriteOperation) (SpriteOperation, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return SpriteOperation{}, err
	}
	if row.IdempotencyKey == "" || row.Name == "" || row.CreatedAt.IsZero() || row.NextAttemptAt.IsZero() {
		return SpriteOperation{}, corestore.ErrInvalidPage
	}
	result, err := s.queries.CreateSpriteOperation(ctx, pgquery.CreateSpriteOperationParams{OrganizationID: s.organization, IdempotencyKey: row.IdempotencyKey, Name: row.Name, Operation: row.Operation, NextAttemptAt: row.NextAttemptAt, CreatedAt: row.CreatedAt})
	if !errors.Is(err, pgx.ErrNoRows) {
		return result, mapError(ctx, err)
	}
	result, err = s.SpriteOperation(ctx, row.IdempotencyKey)
	if err == nil && (result.Name != row.Name || result.Operation != row.Operation) {
		return SpriteOperation{}, corestore.ErrUniqueViolation
	}
	return result, err
}

func (s *session) UpdateSpriteOperation(ctx context.Context, row SpriteOperation, expected int64) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if row.IdempotencyKey == "" || expected < 0 || row.NextAttemptAt.IsZero() || row.UpdatedAt.IsZero() || row.Outcome == "committed" && (row.ReceiptObjectKey == nil || *row.ReceiptObjectKey == "") {
		return corestore.ErrInvalidPage
	}
	count, err := s.queries.UpdateSpriteOperation(ctx, pgquery.UpdateSpriteOperationParams{OrganizationID: s.organization, IdempotencyKey: row.IdempotencyKey, Outcome: row.Outcome, ReceiptObjectKey: row.ReceiptObjectKey, NextAttemptAt: row.NextAttemptAt, UpdatedAt: row.UpdatedAt, ExpectedAttempts: expected})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) PendingSpriteOperations(ctx context.Context, now time.Time, limit int) ([]SpriteOperation, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	if now.IsZero() {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.PendingSpriteOperations(ctx, pgquery.PendingSpriteOperationsParams{OrganizationID: s.organization, NextAttemptAt: now, Limit: int32(limit)})
	return rows, mapError(ctx, err)
}

func (s *session) RecordSpriteProvisionFailure(ctx context.Context, project tracker.ProjectID, revision int64) error {
	if project == "" || revision < 1 {
		return corestore.ErrInvalidPage
	}
	count, err := s.queries.RecordSpriteProvisionFailure(ctx, pgquery.RecordSpriteProvisionFailureParams{OrganizationID: s.organization, ProjectID: project, Revision: revision})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) SpriteMember(ctx context.Context, name string) (SpriteMember, error) {
	if name == "" {
		return SpriteMember{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.SpriteMember(ctx, pgquery.SpriteMemberParams{OrganizationID: s.organization, Name: name})
	return row, mapError(ctx, err)
}

func (s *session) SpriteLeaseUsage(ctx context.Context, now time.Time, joining string) (int, error) {
	if now.IsZero() {
		return 0, corestore.ErrInvalidPage
	}
	count, err := s.queries.SpriteLeaseUsage(ctx, pgquery.SpriteLeaseUsageParams{OrganizationID: s.organization, Now: now, Joining: joining})
	return int(count), mapError(ctx, err)
}

func (s *session) SpriteAllocationSize(ctx context.Context, project tracker.ProjectID) (int, error) {
	count, err := s.queries.SpriteAllocationSize(ctx, pgquery.SpriteAllocationSizeParams{OrganizationID: s.organization, AllocationProjectID: string(project)})
	return int(count), mapError(ctx, err)
}

func (s *session) SpriteMemberHoldsWork(ctx context.Context, enrollment string) (bool, error) {
	if enrollment == "" {
		return false, corestore.ErrInvalidPage
	}
	now, err := s.leaseTime(ctx)
	if err != nil {
		return false, mapError(ctx, err)
	}
	held, err := s.queries.SpriteMemberHoldsWork(ctx, pgquery.SpriteMemberHoldsWorkParams{OrganizationID: s.organization, EnrollmentID: enrollment, Now: now})
	if err != nil {
		return false, mapError(ctx, err)
	}
	if !held.Valid {
		return false, corestore.ErrInvalidPage
	}
	return held.Bool, nil
}

func (s *session) SpriteAllocationAuthority(ctx context.Context, project tracker.ProjectID, now time.Time) (bool, error) {
	if project == "" || now.IsZero() {
		return false, corestore.ErrInvalidPage
	}
	allowed, err := s.queries.SpriteAllocationAuthority(ctx, pgquery.SpriteAllocationAuthorityParams{OrganizationID: s.organization, ProjectID: project, Now: now})
	return allowed, mapError(ctx, err)
}

func (s *session) SpriteMemberPlacementCounts(ctx context.Context, name string) (int, int, int, error) {
	row, err := s.queries.SpriteMemberPlacementCounts(ctx, pgquery.SpriteMemberPlacementCountsParams{OrganizationID: s.organization, Name: name})
	return int(row.ActiveCount), int(row.OtherCount), int(row.AheadCount), mapError(ctx, err)
}
