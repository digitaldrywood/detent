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
type ConfigurationCommand struct {
	OrganizationID tracker.OrganizationID
	RunnerID       string
	ProjectID      tracker.ProjectID
	RequestID      string
	Operation      string
	ActorID        string
	RequestHash    string
	RunnerRevision int64
	Status         string
	Intent         attachment.ArchiveReference
	Observation    attachment.ArchiveReference
}

type ConfigurationReader interface {
	ConfigurationProject(context.Context, string, tracker.ProjectID) (ConfigurationProject, error)
	ConfigurationCommand(context.Context, string, tracker.ProjectID) (ConfigurationCommand, error)
	ConfigurationUnsettled(context.Context, string, tracker.ProjectID, time.Time) (bool, error)
	ConfigurationHeader(context.Context, string) (ConfigurationHeader, error)
	ConfigurationProjects(context.Context, string, time.Time, tracker.ProjectID, int) ([]ConfigurationProject, error)
	OrganizationSchedulingRevision(context.Context) (int64, error)
	OrganizationRankProjects(context.Context, int) ([]corestore.Project, error)
}
type ConfigurationWriter interface {
	RestoreConfiguration(context.Context, ConfigurationProject, ConfigurationCommand, ConfigurationAuthority, int64) error
	SetConfigurationCommand(context.Context, ConfigurationCommand, int64) error
	StoreConfigurationObservationPayload(context.Context, string, tracker.ProjectID, attachment.ArchiveReference, int64, time.Time) error
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

func (s *session) ConfigurationProject(ctx context.Context, runner string, project tracker.ProjectID) (ConfigurationProject, error) {
	row, err := s.queries.ConfigurationProject(ctx, pgquery.ConfigurationProjectParams{OrganizationID: s.organization, RunnerID: runner, ProjectID: project})
	return row, mapError(ctx, err)
}
func (s *session) ConfigurationCommand(ctx context.Context, runner string, project tracker.ProjectID) (ConfigurationCommand, error) {
	command := ConfigurationCommand{OrganizationID: s.organization, RunnerID: runner, ProjectID: project}
	var intent, sha, observation, observationSHA string
	var bytes, content, observationBytes, observationContent int64
	err := s.db.QueryRow(ctx, `SELECT request_id,request_operation,request_actor_id,request_hash,request_runner_revision,request_status,request_payload_object_key,request_payload_sha256,request_payload_bytes,request_payload_content_bytes,observation_payload_object_key,observation_payload_sha256,observation_payload_bytes,observation_payload_content_bytes FROM runner_project_configurations WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, s.organization, runner, project).Scan(&command.RequestID, &command.Operation, &command.ActorID, &command.RequestHash, &command.RunnerRevision, &command.Status, &intent, &sha, &bytes, &content, &observation, &observationSHA, &observationBytes, &observationContent)
	if intent != "" {
		command.Intent = eventstore.Reference(intent, sha, bytes, content)
	}
	if observation != "" {
		command.Observation = eventstore.Reference(observation, observationSHA, observationBytes, observationContent)
	}
	return command, mapError(ctx, err)
}
func (s *session) SetConfigurationCommand(ctx context.Context, arg ConfigurationCommand, expected int64) error {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return err
	}
	if arg.RunnerID == "" || arg.ProjectID == "" || arg.RequestID == "" || arg.Operation == "" || arg.ActorID == "" || !artifact.ValidHash(arg.RequestHash, 64) || arg.RunnerRevision < 1 || expected < 1 || arg.Status != "pending" && arg.Status != "completed" || arg.Intent.Validate(string(s.organization)) != nil {
		return corestore.ErrInvalidPage
	}
	result, err := s.db.Exec(ctx, `UPDATE runner_project_configurations SET request_id=$4,request_operation=$5,request_actor_id=$6,request_hash=$7,request_runner_revision=$8,request_status=$9,request_payload_object_key=$10,request_payload_sha256=$11,request_payload_bytes=$12,request_payload_content_bytes=$13 WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3 AND revision=$14`, s.organization, arg.RunnerID, arg.ProjectID, arg.RequestID, arg.Operation, arg.ActorID, arg.RequestHash, arg.RunnerRevision, arg.Status, arg.Intent.Key, arg.Intent.SHA256, arg.Intent.CompressedSize, arg.Intent.UncompressedSize, expected)
	if err != nil {
		return mapError(ctx, err)
	}
	if result.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}
func (s *session) StoreConfigurationObservationPayload(ctx context.Context, runner string, project tracker.ProjectID, ref attachment.ArchiveReference, expected int64, observedAt time.Time) error {
	if runner == "" || project == "" || expected < 1 || observedAt.IsZero() || ref.Validate(string(s.organization)) != nil {
		return corestore.ErrInvalidPage
	}
	result, err := s.db.Exec(ctx, `UPDATE runner_project_configurations SET observation_payload_object_key=$4,observation_payload_sha256=$5,observation_payload_bytes=$6,observation_payload_content_bytes=$7,observation_payload_observed_at=$9 WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3 AND revision=$8 AND (observation_payload_observed_at IS NULL OR observation_payload_observed_at<=$9)`, s.organization, runner, project, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize, expected, observedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	if result.RowsAffected() != 1 {
		var current int64
		if err := s.db.QueryRow(ctx, `SELECT revision FROM runner_project_configurations WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3`, s.organization, runner, project).Scan(&current); err != nil {
			return mapError(ctx, err)
		}
		if current != expected {
			return corestore.ErrRevisionConflict
		}
		return nil
	}
	return nil
}
func (s *session) ConfigurationUnsettled(ctx context.Context, runner string, project tracker.ProjectID, now time.Time) (bool, error) {
	var unsettled bool
	err := s.db.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM leases l JOIN runner_lease_assignments a ON a.organization_id=l.organization_id AND a.lease_id=l.lease_id JOIN issues i ON i.organization_id=l.organization_id AND i.id=l.issue_id WHERE l.organization_id=$1 AND l.released_at IS NULL AND l.expires_at>$4 AND a.runner_id=$2 AND i.project_id=$3)
 OR EXISTS(SELECT 1 FROM native_attempts n JOIN runner_lease_assignments a ON a.organization_id=n.organization_id AND a.lease_id=n.lease_id JOIN leases l ON l.organization_id=n.organization_id AND l.lease_id=n.lease_id JOIN issues i ON i.organization_id=n.organization_id AND i.id=n.issue_id WHERE n.organization_id=$1 AND n.status='running' AND l.released_at IS NULL AND l.expires_at>$4 AND a.runner_id=$2 AND i.project_id=$3)
 OR EXISTS(SELECT 1 FROM native_attempts n JOIN runner_lease_assignments a ON a.organization_id=n.organization_id AND a.lease_id=n.lease_id JOIN issues i ON i.organization_id=n.organization_id AND i.id=n.issue_id WHERE n.organization_id=$1 AND n.status IN ('running','interrupted') AND n.checkpoint_object_key IS NOT NULL AND n.cleanup_acknowledged_at IS NULL AND a.runner_id=$2 AND i.project_id=$3)`, s.organization, runner, project, now).Scan(&unsettled)
	return unsettled, mapError(ctx, err)
}

func (s *session) RestoreConfiguration(ctx context.Context, row ConfigurationProject, command ConfigurationCommand, authority ConfigurationAuthority, expected int64) error {
	if row.OrganizationID != s.organization || row.RunnerID != authority.RunnerID || row.RunnerID != command.RunnerID || row.ProjectID != command.ProjectID || command.Status != "pending" || !artifact.ValidHash(row.ConfigRevision, 64) || eventstore.Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes).Validate(string(s.organization)) != nil {
		return corestore.ErrInvalidPage
	}
	if err := s.lockConfigurationAuthority(ctx, authority); err != nil {
		return err
	}
	result, err := s.db.Exec(ctx, `UPDATE runner_project_configurations SET revision=revision+1,config_revision=$7,policy_id=$8,state=$9,paused=$10,allow_local_binding=$11,local_intake_enabled=$12,payload_object_key=$13,payload_sha256=$14,payload_bytes=$15,payload_content_bytes=$16,updated_at=$17 WHERE organization_id=$1 AND runner_id=$2 AND project_id=$3 AND revision=$4 AND request_status='pending' AND request_id=$5 AND request_hash=$6`, s.organization, row.RunnerID, row.ProjectID, expected, command.RequestID, command.RequestHash, row.ConfigRevision, row.PolicyID, row.State, row.Paused, row.AllowLocalBinding, row.LocalIntakeEnabled, row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes, authority.At)
	if err != nil {
		return mapError(ctx, err)
	}
	if result.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	header, err := s.ConfigurationHeader(ctx, row.RunnerID)
	if err != nil {
		return err
	}
	_, err = s.queries.AdvanceConfigurationHeader(ctx, pgquery.AdvanceConfigurationHeaderParams{OrganizationID: s.organization, RunnerID: row.RunnerID, ExpectedRevision: header.Revision, ExpectedSourceRevision: header.SourceRevision, SourceRevision: row.ConfigRevision, DefaultsObjectKey: header.DefaultsObjectKey, DefaultsSha256: header.DefaultsSha256, DefaultsBytes: header.DefaultsBytes, DefaultsContentBytes: header.DefaultsContentBytes, UpdatedAt: authority.At})
	if err != nil {
		return updated(ctx, err)
	}

	return nil
}
