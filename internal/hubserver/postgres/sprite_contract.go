package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
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
type SpritePoolMember = pgquery.SpritePoolMemberPageRow

type SpriteMemberCursor struct {
	CreatedAt time.Time
	Name      string
}

type SpriteAllocationCursor struct {
	RequestedAt time.Time
	ProjectID   tracker.ProjectID
}

type SpriteReader interface {
	SpritePromotionSource(context.Context) (accessstore.Secret, error)
	SpritePool(context.Context) (SpritePool, error)
	SpriteMember(context.Context, string) (SpriteMember, error)
	SpriteMemberBusy(context.Context, string) (bool, error)
	SpriteMemberWanted(context.Context, string) (bool, error)
	SpriteRunnerAllowed(context.Context, string, tracker.ProjectID, time.Time) (bool, error)
	SpriteMemberRunner(context.Context, string) (string, error)
	SpriteBudgetRetainsMember(context.Context, string) (bool, error)
	SpriteActiveMemberCount(context.Context, tracker.ProjectID) (int64, error)
	SpriteRetainedMemberCount(context.Context, tracker.ProjectID) (int64, error)
	SpritePoolMemberCount(context.Context) (int64, error)
	SpriteRetainedPoolCount(context.Context) (int64, error)
	SpritePoolMemberPage(context.Context, SpriteMemberCursor, int) ([]SpritePoolMember, error)
	SpriteAllocationQueue(context.Context, SpriteAllocationCursor, int) ([]SpriteAllocation, error)
	SpriteProvisionQueue(context.Context, SpriteAllocationCursor, int) ([]SpriteAllocation, error)
	SpriteRetirementProjects(context.Context, []string) ([]tracker.ProjectID, error)
	SpriteAllocation(context.Context, tracker.ProjectID) (SpriteAllocation, error)
	SpriteAllocationAuthority(context.Context, tracker.ProjectID, time.Time) (bool, error)
	SpriteAllocations(context.Context, tracker.ProjectID, int) ([]SpriteAllocation, error)
	SpriteMembers(context.Context, string, int) ([]SpriteMember, error)
	SpriteOperation(context.Context, string) (SpriteOperation, error)
	SpriteProvisionSourceFailure(context.Context, tracker.ProjectID, string) (SpriteOperation, error)
	SpriteEnrollmentWake(context.Context, string) (SpriteOperation, error)
	SpriteWakeTarget(context.Context, string, string, tracker.ProjectID, time.Time) (bool, error)
	SpriteMemberOperations(context.Context, string) ([]SpriteOperation, error)
	PendingSpriteOperations(context.Context, time.Time, int) ([]SpriteOperation, error)
}

type SpriteWriter interface {
	PromoteSpriteSecret(context.Context, accessstore.Secret, accessstore.Secret) error
	SpriteReader
	LockSpritePool(context.Context) (SpritePool, error)
	SetSpriteProvisionCursor(context.Context, SpriteAllocationCursor) error
	PutSpritePool(context.Context, SpritePool, int64) (SpritePool, error)
	PutSpriteAllocation(context.Context, SpriteAllocation, int64) (SpriteAllocation, error)
	CreateRunnerSpriteMembership(context.Context, runnerstore.SpriteMembership) error
	UpdateRunnerSpriteMembership(context.Context, runnerstore.SpriteMembership) error
	CreateSpriteOperation(context.Context, SpriteOperation) (SpriteOperation, error)
	UpdateSpriteOperation(context.Context, SpriteOperation, int64) error
	SetSpriteExecSession(context.Context, SpriteOperation, string) error
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

func (s *session) PutSpritePool(ctx context.Context, row SpritePool, expected int64) (SpritePool, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return SpritePool{}, err
	}
	placement := policy.Placement{Mode: row.PlacementMode, TodoThreshold: int(row.TodoThreshold), OverflowSlots: int(row.OverflowSlots), MaxSpriteSlots: int(row.MaxSpriteSlots)}
	if expected < 0 || placement.Validate() != nil || row.MinRunners < 0 || row.MaxRunners < row.MinRunners || row.IdleSeconds < 30 || row.ConfiguredBy == "" || (row.IsolationTier != "sandbox" && row.IsolationTier != "native-trusted") || row.BootstrapObjectKey != nil && *row.BootstrapObjectKey == "" {
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
	if row.IdempotencyKey == "" || row.Name == "" || row.Attempts < 0 || len(row.SourceRevision) > 128 || row.CreatedAt.IsZero() || row.NextAttemptAt.IsZero() || row.RequestObjectKey == nil || *row.RequestObjectKey == "" || row.Operation == "wake" && row.TargetEnrollmentID == "" {
		return SpriteOperation{}, corestore.ErrInvalidPage
	}
	if row.Operation == "wake" && row.ProjectID == "" {
		return SpriteOperation{}, corestore.ErrInvalidPage
	}
	if row.TargetEnrollmentID == "" {
		member, err := s.SpriteMember(ctx, row.Name)
		if err != nil {
			return SpriteOperation{}, err
		}
		row.TargetEnrollmentID = member.EnrollmentID
		row.ProjectID = tracker.ProjectID(member.AllocationProjectID)
	} else {
		if _, err := s.RunnerEnrollment(ctx, row.TargetEnrollmentID); err != nil {
			return SpriteOperation{}, err
		}
		if row.Operation != "wake" {
			member, err := s.SpriteMember(ctx, row.Name)
			if err != nil {
				return SpriteOperation{}, err
			}
			if member.EnrollmentID != row.TargetEnrollmentID || tracker.ProjectID(member.AllocationProjectID) != row.ProjectID {
				return SpriteOperation{}, corestore.ErrNotFound
			}
		}
		if row.Operation == "wake" {
			allowed, err := s.SpriteWakeTarget(ctx, row.TargetEnrollmentID, row.Name, row.ProjectID, row.CreatedAt)
			if err != nil {
				return SpriteOperation{}, mapError(ctx, err)
			}
			if !allowed {
				return SpriteOperation{}, corestore.ErrNotFound
			}
		}
	}
	if row.ProjectID != "" && row.Operation != "delete" {
		if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: row.ProjectID}); err != nil {
			return SpriteOperation{}, mapError(ctx, err)
		}
	}
	result, err := s.queries.CreateSpriteOperation(ctx, pgquery.CreateSpriteOperationParams{OrganizationID: s.organization, IdempotencyKey: row.IdempotencyKey, Name: row.Name, Operation: row.Operation, NextAttemptAt: row.NextAttemptAt, CreatedAt: row.CreatedAt, RequestObjectKey: row.RequestObjectKey, TargetEnrollmentID: row.TargetEnrollmentID, ProjectID: row.ProjectID, ProviderResourceID: row.ProviderResourceID, InitialAttempts: row.Attempts, SourceRevision: row.SourceRevision})
	if !errors.Is(err, pgx.ErrNoRows) {
		return result, mapError(ctx, err)
	}
	result, err = s.SpriteOperation(ctx, row.IdempotencyKey)
	if err == nil && (result.Name != row.Name || result.Operation != row.Operation || result.TargetEnrollmentID != row.TargetEnrollmentID || result.ProjectID != row.ProjectID || result.SourceRevision != row.SourceRevision || row.ProviderResourceID != "" && result.ProviderResourceID != row.ProviderResourceID) {
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
	count, err := s.queries.UpdateSpriteOperation(ctx, pgquery.UpdateSpriteOperationParams{OrganizationID: s.organization, IdempotencyKey: row.IdempotencyKey, Outcome: row.Outcome, ReceiptObjectKey: row.ReceiptObjectKey, NextAttemptAt: row.NextAttemptAt, UpdatedAt: row.UpdatedAt, ExpectedAttempts: expected, ProviderResourceID: row.ProviderResourceID})
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

func (s *session) LockSpritePool(ctx context.Context) (SpritePool, error) {
	row, err := s.queries.LockSpritePool(ctx, s.organization)
	return row, mapError(ctx, err)
}

func (s *session) SpriteMember(ctx context.Context, name string) (SpriteMember, error) {
	row, err := s.queries.SpriteMember(ctx, pgquery.SpriteMemberParams{OrganizationID: s.organization, Name: name})
	return row, mapError(ctx, err)
}

func (s *session) SpriteActiveMemberCount(ctx context.Context, project tracker.ProjectID) (int64, error) {
	count, err := s.queries.SpriteActiveMemberCount(ctx, pgquery.SpriteActiveMemberCountParams{OrganizationID: s.organization, AllocationProjectID: string(project)})
	return count, mapError(ctx, err)
}

func (s *session) SpritePoolMemberCount(ctx context.Context) (int64, error) {
	count, err := s.queries.SpritePoolMemberCount(ctx, s.organization)
	return count, mapError(ctx, err)
}

func (s *session) SpritePoolMemberPage(ctx context.Context, after SpriteMemberCursor, limit int) ([]SpritePoolMember, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	if after.CreatedAt.IsZero() {
		after.CreatedAt = time.Unix(0, 0).UTC()
	}
	rows, err := s.queries.SpritePoolMemberPage(ctx, pgquery.SpritePoolMemberPageParams{OrganizationID: s.organization, AfterCreatedAt: after.CreatedAt, AfterName: after.Name, PageLimit: int32(limit)})
	return rows, mapError(ctx, err)
}

func (s *session) SpriteAllocationQueue(ctx context.Context, after SpriteAllocationCursor, limit int) ([]SpriteAllocation, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	if after.RequestedAt.IsZero() {
		after.RequestedAt = time.Unix(0, 0).UTC()
	}
	rows, err := s.queries.SpriteAllocationQueue(ctx, pgquery.SpriteAllocationQueueParams{OrganizationID: s.organization, AfterRequestedAt: after.RequestedAt, AfterProjectID: string(after.ProjectID), PageLimit: int32(limit)})
	return rows, mapError(ctx, err)
}

func (s *session) SpriteProvisionQueue(ctx context.Context, after SpriteAllocationCursor, limit int) ([]SpriteAllocation, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	if after.RequestedAt.IsZero() {
		after.RequestedAt = time.Unix(0, 0).UTC()
	}
	rows, err := s.queries.SpriteProvisionQueue(ctx, pgquery.SpriteProvisionQueueParams{OrganizationID: s.organization, AfterRequestedAt: after.RequestedAt, AfterProjectID: string(after.ProjectID), PageLimit: int32(limit)})
	return rows, mapError(ctx, err)
}

func (s *session) SetSpriteProvisionCursor(ctx context.Context, after SpriteAllocationCursor) error {
	if after.RequestedAt.IsZero() {
		after.RequestedAt = time.Unix(0, 0).UTC()
	}
	count, err := s.queries.SetSpriteProvisionCursor(ctx, pgquery.SetSpriteProvisionCursorParams{OrganizationID: s.organization, AfterRequestedAt: after.RequestedAt, AfterProjectID: string(after.ProjectID)})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrNotFound
	}
	return nil
}

func (s *session) SpriteRetirementProjects(ctx context.Context, names []string) ([]tracker.ProjectID, error) {
	rows, err := s.queries.SpriteRetirementProjects(ctx, pgquery.SpriteRetirementProjectsParams{OrganizationID: s.organization, MemberNames: names})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	projects := make([]tracker.ProjectID, len(rows))
	for i, id := range rows {
		projects[i] = tracker.ProjectID(id)
	}
	return projects, nil
}

func (s *session) SpriteAllocationAuthority(ctx context.Context, project tracker.ProjectID, at time.Time) (bool, error) {
	allowed, err := s.queries.SpriteAllocationAuthority(ctx, pgquery.SpriteAllocationAuthorityParams{OrganizationID: s.organization, ProjectID: project, At: at})
	return allowed, mapError(ctx, err)
}

func (s *session) SetSpriteExecSession(ctx context.Context, row SpriteOperation, id string) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if row.Operation != "bootstrap" || id == "" || len(id) > 200 {
		return corestore.ErrInvalidPage
	}
	count, err := s.queries.SetSpriteExecSession(ctx, pgquery.SetSpriteExecSessionParams{OrganizationID: s.organization, IdempotencyKey: row.IdempotencyKey, ExpectedAttempts: row.Attempts, ExecSessionID: id})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) SpriteMemberBusy(ctx context.Context, name string) (bool, error) {
	value, err := s.queries.SpriteMemberBusy(ctx, pgquery.SpriteMemberBusyParams{OrganizationID: s.organization, Name: name})
	return value, mapError(ctx, err)
}
func (s *session) SpriteMemberWanted(ctx context.Context, name string) (bool, error) {
	value, err := s.queries.SpriteMemberWanted(ctx, pgquery.SpriteMemberWantedParams{OrganizationID: s.organization, Name: name})
	return value, mapError(ctx, err)
}
func (s *session) SpriteMemberRunner(ctx context.Context, name string) (string, error) {
	value, err := s.queries.SpriteMemberRunner(ctx, pgquery.SpriteMemberRunnerParams{OrganizationID: s.organization, Name: name})
	return value, mapError(ctx, err)
}
func (s *session) SpriteBudgetRetainsMember(ctx context.Context, name string) (bool, error) {
	value, err := s.queries.SpriteBudgetRetainsMember(ctx, pgquery.SpriteBudgetRetainsMemberParams{OrganizationID: s.organization, Name: name})
	return value, mapError(ctx, err)
}

func (s *session) SpriteMemberOperations(ctx context.Context, name string) ([]SpriteOperation, error) {
	rows, err := s.queries.SpriteMemberOperations(ctx, pgquery.SpriteMemberOperationsParams{OrganizationID: s.organization, Name: name})
	return rows, mapError(ctx, err)
}

func (s *session) SpritePromotionSource(ctx context.Context) (accessstore.Secret, error) {
	row, err := s.queries.SpritePromotionSource(ctx, s.organization)
	return accessstore.Secret{OrganizationID: s.organization, ProjectID: row.ProjectID, Kind: row.Kind, OrganizationSlug: row.OrganizationSlug, UpdatedAt: row.UpdatedAt, Envelope: hubsecrets.Envelope{Ciphertext: row.Ciphertext, Nonce: row.Nonce, WrappedKey: row.WrappedDataKey, Version: int(row.MasterKeyVersion)}}, mapError(ctx, err)
}

func (s *session) PromoteSpriteSecret(ctx context.Context, source, promoted accessstore.Secret) error {
	if s.readOnly || source.OrganizationID != s.organization || promoted.OrganizationID != s.organization || source.ProjectID == "" || promoted.ProjectID != "" || source.Kind != "fly_sprites_token" || promoted.Kind != source.Kind || promoted.Envelope.Version < 1 || promoted.UpdatedAt.IsZero() {
		return accessstore.ErrDenied
	}
	count, err := s.queries.PromoteSpriteSecret(ctx, pgquery.PromoteSpriteSecretParams{OrganizationID: s.organization, SourceProjectID: source.ProjectID, SourceCiphertext: source.Envelope.Ciphertext, SourceNonce: source.Envelope.Nonce, Ciphertext: promoted.Envelope.Ciphertext, Nonce: promoted.Envelope.Nonce, WrappedDataKey: promoted.Envelope.WrappedKey, MasterKeyVersion: int32(promoted.Envelope.Version), UpdatedAt: promoted.UpdatedAt})
	if err != nil {
		return mapError(ctx, err)
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) SpriteEnrollmentWake(ctx context.Context, enrollment string) (SpriteOperation, error) {
	row, err := s.queries.SpriteEnrollmentWake(ctx, pgquery.SpriteEnrollmentWakeParams{OrganizationID: s.organization, TargetEnrollmentID: enrollment})
	return row, mapError(ctx, err)
}

func (s *session) SpriteWakeTarget(ctx context.Context, enrollment, name string, project tracker.ProjectID, at time.Time) (bool, error) {
	value, err := s.queries.SpriteWakeTarget(ctx, pgquery.SpriteWakeTargetParams{OrganizationID: s.organization, EnrollmentID: enrollment, SpriteName: name, ProjectID: project, At: at})
	return value, mapError(ctx, err)
}

func (s *session) SpriteRetainedMemberCount(ctx context.Context, project tracker.ProjectID) (int64, error) {
	count, err := s.queries.SpriteRetainedMemberCount(ctx, pgquery.SpriteRetainedMemberCountParams{OrganizationID: s.organization, AllocationProjectID: string(project)})
	return count, mapError(ctx, err)
}

func (s *session) SpriteRetainedPoolCount(ctx context.Context) (int64, error) {
	count, err := s.queries.SpriteRetainedPoolCount(ctx, s.organization)
	return count, mapError(ctx, err)
}

func (s *session) SpriteRunnerAllowed(ctx context.Context, runnerID string, project tracker.ProjectID, now time.Time) (bool, error) {
	identity, err := s.RunnerIdentity(ctx, runnerID)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if identity.State != "active" || identity.RemovedAt != nil {
		return false, nil
	}
	granted, err := s.RunnerProjectAuthorized(ctx, runnerID, project, now)
	if err != nil || !granted {
		return false, err
	}
	allocation, err := s.SpriteAllocation(ctx, project)
	managed := err == nil
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return false, err
	}
	member, err := s.RunnerSpriteMembershipByEnrollment(ctx, identity.EnrollmentID)
	if errors.Is(err, corestore.ErrNotFound) {
		machine, err := s.Machine(ctx, string(identity.MachineID))
		if err != nil {
			return false, err
		}
		if machine.SpriteName != "" && machine.SpriteName == machine.Hostname {
			return true, nil
		}
		pool, err := s.SpritePool(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			return true, nil
		}
		return pool.PlacementMode != "sprites_only", err
	}
	if err != nil {
		return false, err
	}
	if member.State != "enrolled" {
		return false, nil
	}
	pool, err := s.SpritePool(ctx)
	if err != nil {
		return false, err
	}
	if pool.MaxRunners == 0 {
		return false, nil
	}
	if member.AllocationProjectID == "" {
		return !managed, nil
	}
	if managed && allocation.DesiredCount == 0 {
		return false, nil
	}
	owner, err := s.SpriteAllocation(ctx, member.AllocationProjectID)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil || owner.DesiredCount == 0 {
		return false, err
	}
	if member.AllocationProjectID != project && !pool.ShareIdleCapacity {
		return false, nil
	}
	checks, err := s.RunnerLocalChecks(ctx, runnerID, project)
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	return err == nil && checks.WorkReady(), err
}

func (s *session) SpriteProvisionSourceFailure(ctx context.Context, project tracker.ProjectID, source string) (SpriteOperation, error) {
	row, err := s.queries.SpriteProvisionSourceFailure(ctx, pgquery.SpriteProvisionSourceFailureParams{OrganizationID: s.organization, ProjectID: project, SourceRevision: source})
	return row, mapError(ctx, err)
}
