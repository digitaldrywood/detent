package postgres

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/projecticon"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) IntegrationPage(ctx context.Context, project tracker.ProjectID) (value projectstore.Integration, resultErr error) {
	var v projectstore.Integration
	var icon projecticon.Icon
	var hasIcon bool
	var keys, values []string
	batch := &pgx.Batch{}
	batch.Queue(`SELECT name,profile,revision,intake,projection,repository_enabled,manual_import_enabled,repository,checkout_repository,aging_enabled,duplicate_challenge,archive_completed_after_days,archive_cancelled_after_days,workflow_source,workflow_source_revision,workflow_markdown,has_icon,icon_kind,icon_name,icon_color,icon_emoji,icon_image_key,icon_image_sha256,icon_url,authority_keys,authority_values FROM project_integration_pages WHERE organization_id=$1 AND project_id=$2`, s.organization, project)
	batch.Queue(`SELECT state_position,name,terminal,dispatchable,operator_only,target FROM project_integration_page_states WHERE organization_id=$1 AND project_id=$2 ORDER BY state_position,transition_position`, s.organization, project)
	timed, ok := s.db.(timedQueries)
	if !ok {
		return v, corestore.ErrInvalidPage
	}
	sender, ok := timed.DBTX.(interface {
		SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
	})
	if !ok {
		return v, corestore.ErrInvalidPage
	}
	started := time.Now()
	results := sender.SendBatch(ctx, batch)
	defer func() {
		resultErr = mapError(ctx, errors.Join(resultErr, results.Close()))
		for _, query := range batch.QueuedQueries {
			observeStatement(ctx, started, query.SQL)
		}
	}()
	err := results.QueryRow().Scan(&v.Name, &v.Profile, &v.Revision, &v.Intake, &v.Projection, &v.RepositoryEnabled, &v.ManualImportEnabled, &v.Repository, &v.CheckoutRepository, &v.AgingEnabled, &v.DuplicateChallenge, &v.ArchiveCompletedAfterDays, &v.ArchiveCancelledAfterDays, &v.WorkflowSource, &v.WorkflowSourceRevision, &v.WorkflowMarkdown, &hasIcon, &icon.Kind, &icon.Name, &icon.Color, &icon.Emoji, &icon.ImageKey, &icon.ImageSHA256, &icon.URL, &keys, &values)
	if err != nil {
		return v, mapError(ctx, err)
	}
	if hasIcon {
		v.Icon = &icon
	}
	if len(keys) != len(values) {
		return v, corestore.ErrInvalidPage
	}
	v.Authority = make(map[string]string, len(keys))
	for i, key := range keys {
		v.Authority[key] = values[i]
	}
	rows, err := results.Query()
	if err != nil {
		return v, mapError(ctx, err)
	}
	defer rows.Close()
	v.States = []tracker.NativeState{}
	for rows.Next() {
		var position int
		var state tracker.NativeState
		var target *string
		if err := rows.Scan(&position, &state.Name, &state.Terminal, &state.Dispatchable, &state.OperatorOnly, &target); err != nil {
			return v, mapError(ctx, err)
		}
		if position == len(v.States) {
			state.Transitions = []string{}
			v.States = append(v.States, state)
		}
		if position < 0 || position >= len(v.States) {
			return v, corestore.ErrInvalidPage
		}
		if target != nil {
			v.States[position].Transitions = append(v.States[position].Transitions, *target)
		}
	}
	return v, mapError(ctx, rows.Err())
}

func (s *session) ClaimIntegrationPage(ctx context.Context, now time.Time) (projectstore.IntegrationCandidate, error) {
	var candidate projectstore.IntegrationCandidate
	err := s.db.QueryRow(ctx, `UPDATE project_integration_page_dirty SET attempted_at=$2 WHERE (organization_id,project_id)=(SELECT organization_id,project_id FROM project_integration_page_dirty WHERE organization_id=$1 ORDER BY attempted_at,project_id LIMIT 1 FOR UPDATE SKIP LOCKED) RETURNING project_id,generation,source_at`, s.organization, now).Scan(&candidate.ProjectID, &candidate.Generation, &candidate.SourceAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.db.QueryRow(ctx, `INSERT INTO project_integration_page_dirty(organization_id,project_id,attempted_at)
 SELECT p.organization_id,p.id,$2 FROM projects p LEFT JOIN project_integration_pages v ON v.organization_id=p.organization_id AND v.project_id=p.id
 WHERE p.organization_id=$1 AND p.deleted_at IS NULL AND v.project_id IS NULL ORDER BY p.id LIMIT 1
 ON CONFLICT(organization_id,project_id) DO UPDATE SET attempted_at=excluded.attempted_at
 RETURNING project_id,generation,source_at`, s.organization, now).Scan(&candidate.ProjectID, &candidate.Generation, &candidate.SourceAt)
	}
	return candidate, mapError(ctx, err)
}

func (s *session) PublishIntegrationPage(ctx context.Context, c projectstore.IntegrationCandidate, value *projectstore.Integration, now time.Time) error {
	var project tracker.ProjectID
	err := s.db.QueryRow(ctx, `SELECT id FROM projects WHERE organization_id=$1 AND id=$2 FOR KEY SHARE`, s.organization, c.ProjectID).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapError(ctx, err)
	}
	var generation int64
	err = s.db.QueryRow(ctx, `SELECT generation FROM project_integration_page_dirty WHERE organization_id=$1 AND project_id=$2 FOR UPDATE`, s.organization, c.ProjectID).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return mapError(ctx, err)
	}
	if generation != c.Generation {
		return corestore.ErrRevisionConflict
	}
	if value == nil {
		if _, err := s.db.Exec(ctx, `DELETE FROM project_integration_pages WHERE organization_id=$1 AND project_id=$2`, s.organization, c.ProjectID); err != nil {
			return mapError(ctx, err)
		}
		_, err := s.db.Exec(ctx, `DELETE FROM project_integration_page_dirty WHERE organization_id=$1 AND project_id=$2 AND generation=$3`, s.organization, c.ProjectID, c.Generation)
		return mapError(ctx, err)
	}
	v := *value
	keys := make([]string, 0, len(v.Authority))
	for key := range v.Authority {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	values := make([]string, len(keys))
	for i, key := range keys {
		values[i] = v.Authority[key]
	}
	var icon projecticon.Icon
	if v.Icon != nil {
		icon = *v.Icon
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO project_integration_pages(organization_id,project_id,name,profile,revision,intake,projection,repository_enabled,manual_import_enabled,repository,checkout_repository,aging_enabled,duplicate_challenge,archive_completed_after_days,archive_cancelled_after_days,workflow_source,workflow_source_revision,workflow_markdown,has_icon,icon_kind,icon_name,icon_color,icon_emoji,icon_image_key,icon_image_sha256,icon_url,authority_keys,authority_values,source_generation,source_at,projected_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31) ON CONFLICT(organization_id,project_id) DO UPDATE SET name=excluded.name,profile=excluded.profile,revision=excluded.revision,intake=excluded.intake,projection=excluded.projection,repository_enabled=excluded.repository_enabled,manual_import_enabled=excluded.manual_import_enabled,repository=excluded.repository,checkout_repository=excluded.checkout_repository,aging_enabled=excluded.aging_enabled,duplicate_challenge=excluded.duplicate_challenge,archive_completed_after_days=excluded.archive_completed_after_days,archive_cancelled_after_days=excluded.archive_cancelled_after_days,workflow_source=excluded.workflow_source,workflow_source_revision=excluded.workflow_source_revision,workflow_markdown=excluded.workflow_markdown,has_icon=excluded.has_icon,icon_kind=excluded.icon_kind,icon_name=excluded.icon_name,icon_color=excluded.icon_color,icon_emoji=excluded.icon_emoji,icon_image_key=excluded.icon_image_key,icon_image_sha256=excluded.icon_image_sha256,icon_url=excluded.icon_url,authority_keys=excluded.authority_keys,authority_values=excluded.authority_values,source_generation=excluded.source_generation,source_at=excluded.source_at,projected_at=excluded.projected_at`, s.organization, c.ProjectID, v.Name, v.Profile, v.Revision, v.Intake, v.Projection, v.RepositoryEnabled, v.ManualImportEnabled, v.Repository, v.CheckoutRepository, v.AgingEnabled, v.DuplicateChallenge, v.ArchiveCompletedAfterDays, v.ArchiveCancelledAfterDays, v.WorkflowSource, v.WorkflowSourceRevision, v.WorkflowMarkdown, v.Icon != nil, icon.Kind, icon.Name, icon.Color, icon.Emoji, icon.ImageKey, icon.ImageSHA256, icon.URL, keys, values, c.Generation, c.SourceAt, now); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM project_integration_page_states WHERE organization_id=$1 AND project_id=$2`, s.organization, c.ProjectID); err != nil {
		return mapError(ctx, err)
	}
	positions, transitions := []int32{}, []int32{}
	names, targets := []string{}, []*string{}
	terminals, dispatchable, operators := []bool{}, []bool{}, []bool{}
	for position, state := range v.States {
		for transition := range max(1, len(state.Transitions)) {
			positions = append(positions, int32(position))
			transitions = append(transitions, int32(transition))
			names = append(names, state.Name)
			terminals = append(terminals, state.Terminal)
			dispatchable = append(dispatchable, state.Dispatchable)
			operators = append(operators, state.OperatorOnly)
			var target *string
			if transition < len(state.Transitions) {
				target = new(state.Transitions[transition])
			}
			targets = append(targets, target)
		}
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO project_integration_page_states SELECT $1,$2,u.*,$10,$11,$12,$13 FROM unnest($3::int[],$4::int[],$5::text[],$6::bool[],$7::bool[],$8::bool[],$9::text[]) AS u`, s.organization, c.ProjectID, positions, transitions, names, terminals, dispatchable, operators, targets, c.Generation, c.SourceAt, now, v.Revision); err != nil {
		return mapError(ctx, err)
	}
	_, err = s.db.Exec(ctx, `DELETE FROM project_integration_page_dirty WHERE organization_id=$1 AND project_id=$2 AND generation=$3`, s.organization, c.ProjectID, c.Generation)
	return mapError(ctx, err)
}
