package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ConfigurationHeader = pgquery.RunnerConfigurationSnapshot
type ConfigurationProject = pgquery.RunnerProjectConfiguration
type ConfigurationObservation = pgquery.ObserveConfigurationProjectParams
type ConfigurationAuthority struct {
	RunnerID     string
	EnrollmentID string
	Revision     int64
	At           time.Time
}
type ConfigurationChange struct {
	OrganizationID         tracker.OrganizationID
	Authority              ConfigurationAuthority
	ExpectedRevision       int64
	ExpectedSourceRevision string
	SourceRevision         string
	Defaults               *attachment.ArchiveReference
	ClearDefaults          bool
	Projects               []ConfigurationProject
}
type ConfigurationReader interface {
	ConfigurationHeader(context.Context, string) (ConfigurationHeader, error)
	ConfigurationProjects(context.Context, string, time.Time, tracker.ProjectID, int) ([]ConfigurationProject, error)
	OrganizationSchedulingRevision(context.Context) (int64, error)
	OrganizationRankProjects(context.Context, int) ([]corestore.Project, error)
}
type ConfigurationWriter interface {
	ConfigurationReader
	InitializeConfiguration(context.Context, ConfigurationHeader, ConfigurationAuthority) (bool, error)
	ApplyConfiguration(context.Context, ConfigurationChange) (ConfigurationHeader, error)
	ObserveConfiguration(context.Context, ConfigurationObservation, ConfigurationAuthority) (ConfigurationProject, error)
	SetOrganizationProjectRanks(context.Context, int64, []tracker.ProjectID) error
}

func (r *Repository) ReadConfiguration(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(ConfigurationReader) error) error {
	return r.transaction(ctx, org, workload, true, func(s *session) error { return run(s) })
}
func (r *Repository) WriteConfiguration(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(ConfigurationWriter) error) error {
	return r.transaction(ctx, org, workload, false, func(s *session) error { return run(s) })
}
func (s *session) ConfigurationHeader(ctx context.Context, runner string) (ConfigurationHeader, error) {
	row, err := s.queries.ConfigurationHeader(ctx, pgquery.ConfigurationHeaderParams{OrganizationID: s.organization, RunnerID: runner})
	if err == nil && (!validConfigurationDefaults(row) || !artifact.ValidHash(row.SourceRevision, 64)) {
		return ConfigurationHeader{}, corestore.ErrInvalidPage
	}
	return row, mapError(ctx, err)
}
func (s *session) ConfigurationProjects(ctx context.Context, runner string, at time.Time, after tracker.ProjectID, limit int) ([]ConfigurationProject, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	if at.IsZero() || runner == "" {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.ConfigurationProjects(ctx, pgquery.ConfigurationProjectsParams{OrganizationID: s.organization, RunnerID: runner, Now: at, AfterProject: string(after), PageLimit: int32(limit)})
	return rows, mapError(ctx, err)
}
func (s *session) lockConfigurationAuthority(ctx context.Context, authority ConfigurationAuthority) error {
	if authority.RunnerID == "" || authority.EnrollmentID == "" || authority.Revision < 1 || authority.At.IsZero() {
		return accessstore.ErrDenied
	}
	row, err := s.queries.LockConfigurationRunner(ctx, pgquery.LockConfigurationRunnerParams{OrganizationID: s.organization, RunnerID: authority.RunnerID, ExpiresAt: authority.At})
	if errors.Is(err, pgx.ErrNoRows) {
		return accessstore.ErrDenied
	}
	if err != nil {
		return mapError(ctx, err)
	}
	if row.Revision != authority.Revision || row.EnrollmentID != authority.EnrollmentID {
		return corestore.ErrRevisionConflict
	}
	return nil
}
func validConfigurationDefaults(row ConfigurationHeader) bool {
	if row.DefaultsObjectKey == nil {
		return row.DefaultsSha256 == "" && row.DefaultsBytes == 0 && row.DefaultsContentBytes == 0
	}
	return eventstore.Reference(*row.DefaultsObjectKey, row.DefaultsSha256, row.DefaultsBytes, row.DefaultsContentBytes).Validate(string(row.OrganizationID)) == nil
}
func (s *session) InitializeConfiguration(ctx context.Context, row ConfigurationHeader, authority ConfigurationAuthority) (bool, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return false, err
	}
	if row.RunnerID != authority.RunnerID || !artifact.ValidHash(row.SourceRevision, 64) || row.CreatedAt.IsZero() || !validConfigurationDefaults(row) {
		return false, corestore.ErrInvalidPage
	}
	if err := s.lockConfigurationAuthority(ctx, authority); err != nil {
		return false, err
	}
	_, err := s.queries.CreateConfigurationHeader(ctx, pgquery.CreateConfigurationHeaderParams{OrganizationID: row.OrganizationID, RunnerID: row.RunnerID, SourceRevision: row.SourceRevision, DefaultsObjectKey: row.DefaultsObjectKey, DefaultsSha256: row.DefaultsSha256, DefaultsBytes: row.DefaultsBytes, DefaultsContentBytes: row.DefaultsContentBytes, CreatedAt: row.CreatedAt})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, mapError(ctx, err)
}
func (s *session) ApplyConfiguration(ctx context.Context, arg ConfigurationChange) (ConfigurationHeader, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return ConfigurationHeader{}, err
	}
	if len(arg.Projects) > 200 || arg.ExpectedRevision < 1 || !artifact.ValidHash(arg.ExpectedSourceRevision, 64) || !artifact.ValidHash(arg.SourceRevision, 64) || arg.Defaults != nil && arg.ClearDefaults {
		return ConfigurationHeader{}, corestore.ErrInvalidPage
	}
	if err := s.lockConfigurationAuthority(ctx, arg.Authority); err != nil {
		return ConfigurationHeader{}, err
	}
	current, err := s.ConfigurationHeader(ctx, arg.Authority.RunnerID)
	if err != nil {
		return ConfigurationHeader{}, err
	}
	if current.Revision != arg.ExpectedRevision || current.SourceRevision != arg.ExpectedSourceRevision {
		return ConfigurationHeader{}, corestore.ErrRevisionConflict
	}
	if len(arg.Projects) == 0 && arg.Defaults == nil && !arg.ClearDefaults {
		if arg.SourceRevision == current.SourceRevision {
			return current, nil
		}
		return ConfigurationHeader{}, corestore.ErrInvalidPage
	}
	ids := make([]string, 0, len(arg.Projects))
	seen := make(map[tracker.ProjectID]bool, len(arg.Projects))
	for _, row := range arg.Projects {
		if row.OrganizationID != s.organization || row.RunnerID != arg.Authority.RunnerID {
			return ConfigurationHeader{}, corestore.ErrTenantMismatch
		}
		if row.ProjectID == "" || seen[row.ProjectID] || !artifact.ValidHash(row.ConfigRevision, 64) || row.State != "attached" && row.State != "draining" && row.State != "detached" || eventstore.Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes).Validate(string(s.organization)) != nil {
			return ConfigurationHeader{}, corestore.ErrInvalidPage
		}
		seen[row.ProjectID] = true
		ids = append(ids, string(row.ProjectID))
	}
	if len(ids) > 0 {
		allowed, err := s.queries.AuthorizedConfigurationProjects(ctx, pgquery.AuthorizedConfigurationProjectsParams{OrganizationID: s.organization, RunnerID: arg.Authority.RunnerID, ProjectIds: ids, Now: arg.Authority.At})
		if err != nil {
			return ConfigurationHeader{}, mapError(ctx, err)
		}
		if len(allowed) != len(ids) {
			return ConfigurationHeader{}, accessstore.ErrDenied
		}
	}
	if arg.ClearDefaults {
		current.DefaultsObjectKey = nil
		current.DefaultsSha256 = ""
		current.DefaultsBytes = 0
		current.DefaultsContentBytes = 0
	}
	if arg.Defaults != nil {
		if arg.Defaults.Validate(string(s.organization)) != nil {
			return ConfigurationHeader{}, corestore.ErrInvalidPage
		}
		current.DefaultsObjectKey = new(arg.Defaults.Key)
		current.DefaultsSha256 = arg.Defaults.SHA256
		current.DefaultsBytes = arg.Defaults.CompressedSize
		current.DefaultsContentBytes = arg.Defaults.UncompressedSize
	}
	result, err := s.queries.AdvanceConfigurationHeader(ctx, pgquery.AdvanceConfigurationHeaderParams{OrganizationID: s.organization, RunnerID: arg.Authority.RunnerID, ExpectedRevision: arg.ExpectedRevision, ExpectedSourceRevision: arg.ExpectedSourceRevision, SourceRevision: arg.SourceRevision, DefaultsObjectKey: current.DefaultsObjectKey, DefaultsSha256: current.DefaultsSha256, DefaultsBytes: current.DefaultsBytes, DefaultsContentBytes: current.DefaultsContentBytes, UpdatedAt: arg.Authority.At})
	if err != nil {
		return ConfigurationHeader{}, updated(ctx, err)
	}
	for _, row := range arg.Projects {
		row.UpdatedAt = arg.Authority.At
		if _, err := s.queries.ApplyConfigurationProject(ctx, pgquery.ApplyConfigurationProjectParams{OrganizationID: row.OrganizationID, RunnerID: row.RunnerID, ProjectID: row.ProjectID, ConfigRevision: row.ConfigRevision, PolicyID: row.PolicyID, State: row.State, Paused: row.Paused, AllowLocalBinding: row.AllowLocalBinding, LocalIntakeEnabled: row.LocalIntakeEnabled, PayloadObjectKey: row.PayloadObjectKey, PayloadSHA256: row.PayloadSHA256, PayloadBytes: row.PayloadBytes, PayloadContentBytes: row.PayloadContentBytes, UpdatedAt: row.UpdatedAt}); err != nil {
			return ConfigurationHeader{}, updated(ctx, err)
		}
	}
	return result, nil
}
func (s *session) ObserveConfiguration(ctx context.Context, arg ConfigurationObservation, authority ConfigurationAuthority) (ConfigurationProject, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return ConfigurationProject{}, err
	}
	if arg.RunnerID != authority.RunnerID || arg.ObservedAt == nil || arg.ObservedAt.IsZero() {
		return ConfigurationProject{}, corestore.ErrInvalidPage
	}
	if err := s.lockConfigurationAuthority(ctx, authority); err != nil {
		return ConfigurationProject{}, err
	}
	allowed, err := s.RunnerProjectAuthorized(ctx, arg.RunnerID, arg.ProjectID, authority.At)
	if err != nil {
		return ConfigurationProject{}, err
	}
	if !allowed {
		return ConfigurationProject{}, accessstore.ErrDenied
	}
	row, err := s.queries.ObserveConfigurationProject(ctx, arg)
	return row, updated(ctx, err)
}
