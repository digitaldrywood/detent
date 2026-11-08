package postgres

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HealthRecord = pgquery.HealthFinding
type HealthProjectLink = pgquery.HealthFindingProject
type HealthIssueLink = pgquery.HealthFindingIssue
type HealthRecordInput = pgquery.HealthRecordParams
type RecentHealthFingerprintInput = pgquery.RecentHealthFingerprintParams
type HealthRecordProjectsInput = pgquery.HealthRecordProjectsParams
type HealthRecordIssuesInput = pgquery.HealthRecordIssuesParams
type CreateHealthRecordInput = pgquery.CreateHealthRecordParams
type UpdateHealthRecordInput = pgquery.UpdateHealthRecordParams
type LinkHealthProjectInput = pgquery.LinkHealthProjectParams
type LinkHealthIssueInput = pgquery.LinkHealthIssueParams
type PutHealthBaselineInput = pgquery.PutHealthBaselineParams
type ClearHealthBaselineInput = pgquery.ClearHealthBaselineParams
type PutHealthTickInput = pgquery.PutHealthTickParams
type HealthReader interface {
	healthstore.Reader
	HealthTick(context.Context) (time.Time, error)
	HealthRecord(context.Context, HealthRecordInput) (HealthRecord, error)
	RecentHealthFingerprint(context.Context, RecentHealthFingerprintInput) (HealthRecord, error)
	HealthRecordProjects(context.Context, HealthRecordProjectsInput) ([]HealthProjectLink, error)
	HealthRecordIssues(context.Context, HealthRecordIssuesInput) ([]HealthIssueLink, error)
}
type HealthWriter interface {
	HealthReader
	healthstore.PageMaintainer
	CreateHealthRecord(context.Context, CreateHealthRecordInput) (HealthRecord, error)
	UpdateHealthRecord(context.Context, UpdateHealthRecordInput) (HealthRecord, error)
	LinkHealthProject(context.Context, LinkHealthProjectInput) error
	LinkHealthIssue(context.Context, LinkHealthIssueInput) error
	PutHealthBaseline(context.Context, PutHealthBaselineInput) error
	ClearHealthBaseline(context.Context, ClearHealthBaselineInput) error
	PutHealthTick(context.Context, PutHealthTickInput) error
}

func (r *Repository) ReadHealth(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(HealthReader) error) error {
	return r.transaction(ctx, org, workload, true, func(s *session) error { return run(s) })
}
func (r *Repository) WriteHealth(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(HealthWriter) error) error {
	return r.transaction(ctx, org, workload, false, func(s *session) error { return run(s) })
}
func (s *session) HealthRecord(ctx context.Context, arg HealthRecordInput) (HealthRecord, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return HealthRecord{}, err
	}

	row, err := s.queries.HealthRecord(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) RecentHealthFingerprint(ctx context.Context, arg RecentHealthFingerprintInput) (HealthRecord, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return HealthRecord{}, err
	}
	if arg.ResolvedAt == nil || arg.ResolvedAt.IsZero() {
		return HealthRecord{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.RecentHealthFingerprint(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) HealthRecordProjects(ctx context.Context, arg HealthRecordProjectsInput) ([]HealthProjectLink, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if err := checkPage(int(arg.Limit)); err != nil {
		return nil, err
	}
	row, err := s.queries.HealthRecordProjects(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) HealthRecordIssues(ctx context.Context, arg HealthRecordIssuesInput) ([]HealthIssueLink, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return nil, err
	}
	if err := checkPage(int(arg.Limit)); err != nil {
		return nil, err
	}
	row, err := s.queries.HealthRecordIssues(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) CreateHealthRecord(ctx context.Context, arg CreateHealthRecordInput) (HealthRecord, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return HealthRecord{}, err
	}
	if arg.ID == "" || arg.Fingerprint == "" || arg.OpenedAt.IsZero() || arg.LastSeenAt.Before(arg.OpenedAt) || eventstore.Reference(arg.EvidenceObjectKey, arg.EvidenceSha256, arg.EvidenceBytes, arg.EvidenceContentBytes).Validate(string(s.organization)) != nil {
		return HealthRecord{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.CreateHealthRecord(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) UpdateHealthRecord(ctx context.Context, arg UpdateHealthRecordInput) (HealthRecord, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return HealthRecord{}, err
	}
	if arg.ExpectedRevision < 1 || arg.LastSeenAt.IsZero() || arg.ResolvedAt != nil && arg.ResolvedAt.Before(arg.LastSeenAt) || eventstore.Reference(arg.EvidenceObjectKey, arg.EvidenceSha256, arg.EvidenceBytes, arg.EvidenceContentBytes).Validate(string(s.organization)) != nil {
		return HealthRecord{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.UpdateHealthRecord(ctx, arg)

	return row, updated(ctx, err)
}
func (s *session) LinkHealthProject(ctx context.Context, arg LinkHealthProjectInput) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}

	err := s.queries.LinkHealthProject(ctx, arg)
	return mapError(ctx, err)
}
func (s *session) LinkHealthIssue(ctx context.Context, arg LinkHealthIssueInput) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}

	err := s.queries.LinkHealthIssue(ctx, arg)
	return mapError(ctx, err)
}
func (s *session) PutHealthBaseline(ctx context.Context, arg PutHealthBaselineInput) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}

	err := s.queries.PutHealthBaseline(ctx, arg)
	return mapError(ctx, err)
}
func (s *session) ClearHealthBaseline(ctx context.Context, arg ClearHealthBaselineInput) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}

	err := s.queries.ClearHealthBaseline(ctx, arg)
	return mapError(ctx, err)
}
func (s *session) PutHealthTick(ctx context.Context, arg PutHealthTickInput) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}
	if arg.LastTickAt.IsZero() {
		return corestore.ErrInvalidPage
	}
	err := s.queries.PutHealthTick(ctx, arg)
	return mapError(ctx, err)
}

func (s *session) HealthTick(ctx context.Context) (time.Time, error) {
	row, err := s.queries.HealthTick(ctx, s.organization)
	return row, mapError(ctx, err)
}
