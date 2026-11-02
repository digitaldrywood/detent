package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/readableurl"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func projectSettings(row pgquery.ProjectSetting) projectstore.Settings {
	result := projectstore.Settings{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, Revision: row.Revision, Intake: row.Intake, Projection: row.Projection, RepositoryEnabled: row.RepositoryEnabled, Repository: row.Repository, CheckoutRepository: row.CheckoutRepository, AgingEnabled: row.AgingEnabled, DuplicateChallenge: row.DuplicateChallenge, WorkflowKey: row.WorkflowKey, WorkflowSHA256: row.WorkflowSha256, WorkflowBytes: row.WorkflowBytes, WorkflowContentBytes: row.WorkflowContentBytes, RequireVisualAttachments: row.RequireVisualAttachments, ModelRevision: row.ModelRevision, ModelKey: row.ModelKey, ModelSHA256: row.ModelSha256, ModelBytes: row.ModelBytes, ModelContentBytes: row.ModelContentBytes}
	if row.ArchiveCompletedAfterDays.Valid {
		result.ArchiveCompletedAfterDays = new(row.ArchiveCompletedAfterDays.Int64)
	}
	if row.ArchiveCancelledAfterDays.Valid {
		result.ArchiveCancelledAfterDays = new(row.ArchiveCancelledAfterDays.Int64)
	}
	return result
}

func (s *session) ProjectRecord(ctx context.Context, id tracker.ProjectID) (projectstore.Record, error) {
	rows, err := s.queries.BoundProjectRecord(ctx, pgquery.BoundProjectRecordParams{OrganizationID: s.organization, ID: id})
	if err != nil {
		return projectstore.Record{}, mapError(ctx, err)
	}
	if len(rows) == 0 {
		return projectstore.Record{}, corestore.ErrNotFound
	}
	result := projectstore.Record{Core: corestore.Project(rows[0].Project), Settings: projectSettings(rows[0].ProjectSetting), States: []tracker.NativeState{}}
	positions := make(map[string]int)
	for _, row := range rows {
		if row.StateName == nil {
			continue
		}
		i, found := positions[*row.StateName]
		if !found {
			i = len(result.States)
			positions[*row.StateName] = i
			result.States = append(result.States, tracker.NativeState{Name: *row.StateName, Terminal: row.StateTerminal, Dispatchable: row.StateDispatchable, OperatorOnly: row.StateOperatorOnly, Transitions: []string{}})
		}
		if row.TargetName != nil {
			result.States[i].Transitions = append(result.States[i].Transitions, *row.TargetName)
		}
	}
	return result, nil
}

func (s *session) ProjectSlugOwner(ctx context.Context, slug string) (tracker.ProjectID, error) {
	id, err := s.queries.ResolveProjectSlug(ctx, pgquery.ResolveProjectSlugParams{OrganizationID: s.organization, Slug: slug})
	return id, mapError(ctx, err)
}

func (s *session) ProjectBySlug(ctx context.Context, slug string) (projectstore.Record, error) {
	id, err := s.queries.ResolveProjectSlug(ctx, pgquery.ResolveProjectSlugParams{OrganizationID: s.organization, Slug: slug})
	if err != nil {
		return projectstore.Record{}, mapError(ctx, err)
	}
	return s.ProjectRecord(ctx, id)
}

func (s *session) reserveProjectSlug(ctx context.Context, id tracker.ProjectID, slug string) error {
	rows, err := s.queries.ReserveProjectSlug(ctx, pgquery.ReserveProjectSlugParams{OrganizationID: s.organization, Slug: slug, ProjectID: id})
	if err != nil {
		return mapError(ctx, err)
	}
	if rows != 1 {
		return corestore.ErrUniqueViolation
	}
	return nil
}

func (s *session) CreateProjectRecord(ctx context.Context, record projectstore.Record) (projectstore.Record, error) {
	if err := s.checkOrganization(record.Core.OrganizationID); err != nil {
		return projectstore.Record{}, err
	}
	if err := policy.ValidateStates(record.States); err != nil {
		return projectstore.Record{}, err
	}
	if record.Settings.OrganizationID != "" && record.Settings.OrganizationID != s.organization {
		return projectstore.Record{}, corestore.ErrTenantMismatch
	}
	if record.Settings.ProjectID != "" && record.Settings.ProjectID != record.Core.ID {
		return projectstore.Record{}, corestore.ErrTenantMismatch
	}
	if err := s.lockHostedOrganization(ctx); err != nil {
		return projectstore.Record{}, err
	}
	base := record.Core.Slug
	if base == "" {
		base = projectstore.Slug(record.Core.Name)
	} else if err := readableurl.Validate(base); err != nil {
		return projectstore.Record{}, err
	}
	slug := base
	_, err := s.queries.ResolveProjectSlug(ctx, pgquery.ResolveProjectSlugParams{OrganizationID: s.organization, Slug: slug})
	if err == nil {
		if record.Core.Slug != "" {
			return projectstore.Record{}, corestore.ErrUniqueViolation
		}
		slug = projectstore.CollisionSlug(base, record.Core.ID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return projectstore.Record{}, mapError(ctx, err)
	}
	record.Core.Slug = slug
	core, err := s.CreateProject(ctx, record.Core)
	if err != nil {
		return projectstore.Record{}, err
	}
	if err := s.reserveProjectSlug(ctx, core.ID, slug); err != nil {
		return projectstore.Record{}, err
	}
	if err := s.queries.CreateProjectSettings(ctx, pgquery.CreateProjectSettingsParams{OrganizationID: s.organization, ProjectID: core.ID}); err != nil {
		return projectstore.Record{}, mapError(ctx, err)
	}
	if err := s.replaceProjectStates(ctx, core.ID, record.States); err != nil {
		return projectstore.Record{}, err
	}
	return s.ProjectRecord(ctx, core.ID)
}

func (s *session) replaceProjectStates(ctx context.Context, id tracker.ProjectID, states []tracker.NativeState) error {
	if err := s.queries.DeleteProjectTransitions(ctx, pgquery.DeleteProjectTransitionsParams{OrganizationID: s.organization, ProjectID: id}); err != nil {
		return mapError(ctx, err)
	}
	names := make([]string, len(states))
	for i, state := range states {
		names[i] = state.Name
	}
	used, err := s.queries.ProjectUsedRemovedStates(ctx, pgquery.ProjectUsedRemovedStatesParams{OrganizationID: s.organization, ProjectID: id, Names: names})
	if err != nil {
		return mapError(ctx, err)
	}
	if used {
		return projectstore.ErrStateInUse
	}
	if err := s.queries.DeleteProjectStates(ctx, pgquery.DeleteProjectStatesParams{OrganizationID: s.organization, ProjectID: id, Names: names}); err != nil {
		return mapError(ctx, err)
	}
	for i, state := range states {
		if err := s.queries.CreateProjectState(ctx, pgquery.CreateProjectStateParams{OrganizationID: s.organization, ProjectID: id, Name: state.Name, Terminal: state.Terminal, Dispatchable: state.Dispatchable, OperatorOnly: state.OperatorOnly, BoardRank: int64(i)}); err != nil {
			return mapError(ctx, err)
		}
	}
	for _, state := range states {
		for i, target := range state.Transitions {
			if err := s.queries.CreateProjectTransition(ctx, pgquery.CreateProjectTransitionParams{OrganizationID: s.organization, ProjectID: id, SourceName: state.Name, TargetName: target, TransitionRank: int64(i)}); err != nil {
				return mapError(ctx, err)
			}
		}
	}
	return nil
}

func nullableDays(value *int64) sql.NullInt64 {
	if value == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *value, Valid: true}
}

func (s *session) UpdateProjectRecord(ctx context.Context, record projectstore.Record) (projectstore.Record, error) {
	if record.Core.OrganizationID != s.organization || record.Settings.OrganizationID != s.organization || record.Core.ID != record.Settings.ProjectID {
		return projectstore.Record{}, corestore.ErrTenantMismatch
	}
	if err := policy.ValidateStates(record.States); err != nil {
		return projectstore.Record{}, err
	}
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: record.Core.ID}); err != nil {
		return projectstore.Record{}, mapError(ctx, err)
	}
	current, err := s.ProjectRecord(ctx, record.Core.ID)
	if err != nil {
		return projectstore.Record{}, err
	}
	if current.Settings.Revision != record.Settings.Revision || current.Core.ContentSettingsRevision != record.Core.ContentSettingsRevision {
		return projectstore.Record{}, corestore.ErrRevisionConflict
	}
	if err := readableurl.Validate(record.Core.Slug); err != nil {
		return projectstore.Record{}, err
	}
	if err := s.reserveProjectSlug(ctx, record.Core.ID, record.Core.Slug); err != nil {
		return projectstore.Record{}, err
	}
	rows, err := s.queries.UpdateProjectMetadata(ctx, pgquery.UpdateProjectMetadataParams{OrganizationID: s.organization, ID: record.Core.ID, Name: record.Core.Name, Slug: record.Core.Slug, RequireDependencies: record.Core.RequireDependencies, WorkflowSource: record.Core.WorkflowSource, WorkflowSourceRevision: record.Core.WorkflowSourceRevision, ExpectedRevision: record.Core.ContentSettingsRevision})
	if err := affected(ctx, rows, err); err != nil {
		return projectstore.Record{}, err
	}
	v := record.Settings
	if v.WorkflowKey != "" {
		if err := validRecordPayload(s.organization, v.WorkflowKey, v.WorkflowSHA256, v.WorkflowBytes, v.WorkflowContentBytes); err != nil {
			return projectstore.Record{}, err
		}
	}
	_, err = s.queries.UpdateProjectSettings(ctx, pgquery.UpdateProjectSettingsParams{OrganizationID: s.organization, ProjectID: record.Core.ID, ExpectedRevision: v.Revision, Intake: v.Intake, Projection: v.Projection, RepositoryEnabled: v.RepositoryEnabled, Repository: v.Repository, CheckoutRepository: v.CheckoutRepository, AgingEnabled: v.AgingEnabled, DuplicateChallenge: v.DuplicateChallenge, ArchiveCompletedAfterDays: nullableDays(v.ArchiveCompletedAfterDays), ArchiveCancelledAfterDays: nullableDays(v.ArchiveCancelledAfterDays), WorkflowKey: v.WorkflowKey, WorkflowSha256: v.WorkflowSHA256, WorkflowBytes: v.WorkflowBytes, WorkflowContentBytes: v.WorkflowContentBytes})
	if err != nil {
		return projectstore.Record{}, updated(ctx, err)
	}
	if err := s.replaceProjectStates(ctx, record.Core.ID, record.States); err != nil {
		return projectstore.Record{}, err
	}
	return s.ProjectRecord(ctx, record.Core.ID)
}

func (s *session) RetireProject(ctx context.Context, id tracker.ProjectID, revision int64, now time.Time) error {
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: id}); err != nil {
		return mapError(ctx, err)
	}
	record, err := s.ProjectRecord(ctx, id)
	if err != nil {
		return err
	}
	if record.Settings.Revision != revision {
		return corestore.ErrRevisionConflict
	}
	serverNow, err := s.queries.LeaseTime(ctx)
	if err != nil {
		return mapError(ctx, err)
	}
	active, err := s.queries.ProjectHasActiveWork(ctx, pgquery.ProjectHasActiveWorkParams{OrganizationID: s.organization, ProjectID: id, Now: serverNow})
	if err != nil {
		return mapError(ctx, err)
	}
	if active.Bool {
		return projectstore.ErrActiveWork
	}
	record.Settings.RepositoryEnabled = false
	record.Settings.Intake = "disabled"
	if _, err := s.UpdateProjectRecord(ctx, record); err != nil {
		return err
	}
	rows, err := s.queries.RetireProject(ctx, pgquery.RetireProjectParams{OrganizationID: s.organization, ID: id, DeletedAt: &now})
	return affected(ctx, rows, err)
}

func (s *session) UpdateProjectContent(ctx context.Context, id tracker.ProjectID, revision int64, visual bool) error {
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: id}); err != nil {
		return mapError(ctx, err)
	}
	rows, err := s.queries.UpdateProjectContent(ctx, pgquery.UpdateProjectContentParams{OrganizationID: s.organization, ID: id, ContentSettingsRevision: revision})
	if err != nil {
		return mapError(ctx, err)
	}
	if rows != 1 {
		return corestore.ErrRevisionConflict
	}
	return mapError(ctx, s.queries.SetProjectVisualAttachments(ctx, pgquery.SetProjectVisualAttachmentsParams{OrganizationID: s.organization, ProjectID: id, RequireVisualAttachments: visual}))
}

func (s *session) UpdateProjectModel(ctx context.Context, v projectstore.Settings) error {
	if v.ModelKey != "" {
		if err := validRecordPayload(s.organization, v.ModelKey, v.ModelSHA256, v.ModelBytes, v.ModelContentBytes); err != nil {
			return err
		}
	}
	if err := s.checkOrganization(v.OrganizationID); err != nil {
		return err
	}
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: v.ProjectID}); err != nil {
		return mapError(ctx, err)
	}
	rows, err := s.queries.UpdateProjectModel(ctx, pgquery.UpdateProjectModelParams{OrganizationID: s.organization, ProjectID: v.ProjectID, ExpectedRevision: v.ModelRevision, ModelKey: v.ModelKey, ModelSha256: v.ModelSHA256, ModelBytes: v.ModelBytes, ModelContentBytes: v.ModelContentBytes})
	if err != nil {
		return mapError(ctx, err)
	}
	if rows != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) RequireProjectIdle(ctx context.Context, id tracker.ProjectID, _ time.Time) error {
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: id}); err != nil {
		return mapError(ctx, err)
	}
	serverNow, err := s.queries.LeaseTime(ctx)
	if err != nil {
		return mapError(ctx, err)
	}
	active, err := s.queries.ProjectHasActiveWork(ctx, pgquery.ProjectHasActiveWorkParams{OrganizationID: s.organization, ProjectID: id, Now: serverNow})
	if err != nil {
		return mapError(ctx, err)
	}
	if active.Bool {
		return projectstore.ErrActiveWork
	}
	return nil
}

func (s *session) UpdateProjectSlug(ctx context.Context, id tracker.ProjectID, expected, slug string) error {
	if err := readableurl.Validate(slug); err != nil {
		return err
	}
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: id}); err != nil {
		return mapError(ctx, err)
	}
	record, err := s.ProjectRecord(ctx, id)
	if err != nil {
		return err
	}
	if record.Core.Slug != expected {
		return corestore.ErrRevisionConflict
	}
	if err := s.reserveProjectSlug(ctx, id, slug); err != nil {
		return err
	}
	rows, err := s.queries.SetProjectSlug(ctx, pgquery.SetProjectSlugParams{OrganizationID: s.organization, ID: id, Slug: slug})
	return affected(ctx, rows, err)
}
