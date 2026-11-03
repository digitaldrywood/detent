package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func isProjectRecordCommand(input any) bool {
	switch input.(type) {
	case createNativeProjectRequest, renameProjectRequest, updateProjectIntegrationRequest, deleteCloudProjectRequest, cloudModelSelectionRequest, cloudContentSettingsRequest:
		return true
	}
	return false
}

func (s *Service) authorizeProjectRecord(ctx context.Context, scope nativeScope, write bool) error {
	if s.config.TenantRepository != nil && s.config.AccessRepository == nil {
		return nativeNotFound()
	}
	if s.config.AccessRepository != nil {
		return s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error { return s.authorizeAccessScope(ctx, r, scope, write) })
	}
	return s.authorizeProjectRecordWith(ctx, s.database.auth(), scope, write)
}

func (s *Service) authorizeProjectRecordWith(ctx context.Context, query nativeQueryer, scope nativeScope, write bool) error {
	if scope.credential.Runner.RunnerID != "" && scope.credential.Runner.OrganizationID != scope.organization {
		return nativeNotFound()
	}
	if s.config.Hosted != nil && string(scope.organization) != s.config.Hosted.OrganizationID {
		return nativeNotFound()
	}
	if scope.project == "" {
		if scope.credential.Scope != apiScopeAdmin && (scope.credential.Hosted == nil || scope.credential.HostedRole != "owner" && scope.credential.HostedRole != "admin") {
			return nativeNotFound()
		}
		var exists bool
		if err := query.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM organizations WHERE id=?)", scope.organization).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return nativeNotFound()
		}
		return nil
	}
	condition, args := scope.credential.projectAccessSQL("p.organization_id", "p.id", "1")
	args = append([]any{scope.organization, scope.project}, args...)
	var allowed bool
	if err := query.QueryRowContext(ctx, "WITH p AS (SELECT ? AS organization_id,? AS id) SELECT ("+condition+") FROM p", args...).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return nativeNotFound()
	}
	if scope.credential.Hosted != nil {
		if err := s.requireHostedProject(ctx, query, scope, write); err != nil {
			return err
		}
	}
	return nil
}

type projectAccessAuthority struct{}

func (projectAccessAuthority) Rollback() error { return nil }

func (s *Service) beginProjectRecordAuthority(ctx context.Context, scope nativeScope) (interface{ Rollback() error }, error) {
	if s.config.TenantRepository != nil && s.config.AccessRepository == nil {
		return nil, nativeNotFound()
	}
	if s.config.AccessRepository != nil {
		if scope.credential.Hosted != nil {
			if _, _, err := s.hostedMutationIdentity(ctx, scope.credential); err != nil {
				return nil, err
			}
		}
		return projectAccessAuthority{}, nil
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeProjectRecordWith(ctx, tx, scope, true); err != nil {
		rollbackErr := tx.Rollback()
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			rollbackErr = nil
		}
		return nil, errors.Join(err, rollbackErr)
	}
	saved := scope.project
	scope.project = ""
	err = s.recheckHostedMutation(ctx, tx, &scope)
	scope.project = saved
	if err == nil {
		err = requireRunnerAuthority(ctx, tx, scope, s.config.now())
	}
	if err != nil {
		rollbackErr := tx.Rollback()
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			rollbackErr = nil
		}
		return nil, errors.Join(err, rollbackErr)
	}
	return tx, nil
}

func (s *Service) recheckProjectRecordMutation(ctx context.Context, scope nativeScope) error {
	if s.config.TenantRepository != nil && s.config.AccessRepository == nil {
		return nativeNotFound()
	}
	if s.config.AccessRepository != nil {
		if scope.credential.Hosted != nil {
			_, membership, err := s.hostedMutationIdentity(ctx, scope.credential)
			if err != nil {
				return err
			}
			if membership.ID != scope.credential.HostedMembership {
				return accessstore.ErrDenied
			}
			scope.credential.HostedRole = lesserHostedRole(scope.credential.HostedRole, membership.Role.Slug)
		}
		return s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error { return s.authorizeAccessScope(ctx, r, scope, true) })
	}
	tx, err := s.beginProjectRecordAuthority(ctx, scope)
	if err != nil {
		return err
	}
	return tx.Rollback()
}

func projectRecordError(err error) error {
	if errors.Is(err, corestore.ErrInvalidRelationship) {
		return nativePublicInvalid("Relationship type, operation, or endpoints are invalid")
	}
	if errors.Is(err, corestore.ErrDependencyCycle) {
		return nativePublicInvalid("Dependencies cannot form a cycle")
	}
	if errors.Is(err, corestore.ErrNotFound) {
		return nativeNotFound()
	}
	if errors.Is(err, ErrProjectParticipants) {
		return &nativeError{Code: "not_ready", Message: "Project lifecycle storage is unavailable", status: http.StatusServiceUnavailable, publicMessage: true}
	}
	if errors.Is(err, projectstore.ErrStateInUse) {
		return nativeWorkflowInvalid("Workflow states used by work items cannot be removed")
	}
	if errors.Is(err, projectstore.ErrActiveWork) {
		return nativePublicInvalid("Finish or cancel active work before deleting this project")
	}
	return err
}

func (s *Service) readAuthorizedProjectRecord(ctx context.Context, scope nativeScope, write bool) (projectstore.Record, error) {
	if s.config.AccessRepository == nil {
		return projectstore.Record{}, nativeNotFound()
	}
	var result projectstore.Record
	err := s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, write); err != nil {
			return err
		}
		var err error
		result, err = r.ProjectRecord(ctx, scope.project)
		return err
	})
	return result, projectRecordError(err)
}

func (s *Service) readProjectRecord(ctx context.Context, scope nativeScope) (projectstore.Record, error) {
	var result projectstore.Record
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		result, err = r.ProjectRecord(ctx, scope.project)
		return err
	})
	return result, projectRecordError(err)
}

func projectIntegrationRecord(record projectstore.Record) ProjectIntegration {
	v := record.Settings
	owner := "detent"
	if record.Core.Profile == "github_compatible" {
		owner = "github"
	}
	result := ProjectIntegration{Name: record.Core.Name, Profile: record.Core.Profile, Revision: tracker.Revision(v.Revision), Intake: v.Intake, ManualImportEnabled: v.Intake == "manual", Projection: v.Projection, RepositoryEnabled: v.RepositoryEnabled, Repository: v.Repository, CheckoutRepository: v.CheckoutRepository, States: record.States, WorkflowSource: record.Core.WorkflowSource, WorkflowSourceRevision: record.Core.WorkflowSourceRevision, AgingEnabled: v.AgingEnabled, DuplicateChallenge: v.DuplicateChallenge, Authority: map[string]string{"title": owner, "body": owner, "discussion": owner, "dependencies": owner, "authors": owner, "source_timestamps": "source", "workflow": owner, "labels": owner, "assignees": owner, "priority": owner, "scheduling": "detent", "progress": "detent", "repository_policy": "trusted_repository_revision", "github_merge": "github_branch_protections_and_fresh_checks", "native_approval": "detent_only"}}
	if record.Core.WorkflowSource != "" {
		result.Authority["workflow"] = "repository"
	}
	if v.ArchiveCompletedAfterDays != nil {
		result.ArchiveCompletedAfterDays = new(int(*v.ArchiveCompletedAfterDays))
	}
	if v.ArchiveCancelledAfterDays != nil {
		result.ArchiveCancelledAfterDays = new(int(*v.ArchiveCancelledAfterDays))
	}
	if result.Profile == "native" && (v.Repository != "" || v.CheckoutRepository != "") {
		result.Intake = "automatic"
	}
	return result
}

func (s *Service) projectRecordWorkflow(ctx context.Context, record projectstore.Record) (string, error) {
	if record.Settings.WorkflowKey == "" {
		return "", nil
	}
	v := record.Settings
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, eventstore.Receipt{OrganizationID: record.Core.OrganizationID, PayloadObjectKey: v.WorkflowKey, PayloadSHA256: v.WorkflowSHA256, PayloadBytes: v.WorkflowBytes, PayloadContentBytes: v.WorkflowContentBytes})
	if err != nil {
		return "", err
	}
	var markdown string
	err = json.Unmarshal(raw, &markdown)
	return markdown, err
}

func (s *Service) projectRecordNative(ctx context.Context, record projectstore.Record) (tracker.NativeProject, error) {
	markdown, err := s.projectRecordWorkflow(ctx, record)
	if err != nil {
		return tracker.NativeProject{}, err
	}
	repository := record.Settings.Repository
	if repository == "" {
		repository = record.Settings.CheckoutRepository
	}
	project := tracker.NativeProject{OrganizationID: record.Core.OrganizationID, ID: record.Core.ID, Name: record.Core.Name, Slug: record.Core.Slug, Profile: record.Core.Profile, RequireDependencies: record.Core.RequireDependencies, States: record.States, WorkflowMarkdown: markdown, Repository: repository}
	if repository != "" {
		project.CloneURL = "https://github.com/" + repository + ".git"
	}
	selection, err := s.projectRecordModel(ctx, record)
	if err != nil {
		return project, err
	}
	project.ModelSelection, err = json.Marshal(selection.Effective)
	return project, err
}

func (s *Service) executeProjectRecordMutation(ctx context.Context, scope nativeScope, options nativeCommandOptions, command tracker.Mutation, input any) (json.RawMessage, error) {
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if (scope.credential.Hosted != nil || s.config.Hosted != nil) && s.config.ProjectParticipants == nil {
		switch input.(type) {
		case createNativeProjectRequest, deleteCloudProjectRequest:
			return nil, projectRecordError(ErrProjectParticipants)
		}
	}
	if s.config.AccessRepository == nil {
		return nil, nativeNotFound()
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := nativeCommandHash(input)
	if err != nil {
		return nil, err
	}
	var current projectstore.Record
	var organizationModelRevision int64
	var receipt eventstore.Receipt
	var replay bool
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		var err error
		receipt, err = r.Receipt(ctx, scope.credential.ID, options.OperationID, command.IdempotencyKey)
		if err == nil {
			replay = true
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if scope.project != "" {
			if _, ok := input.(cloudModelSelectionRequest); ok {
				models, readErr := r.OrganizationModels(ctx)
				if readErr != nil {
					return readErr
				}
				organizationModelRevision = models.Revision
			}
			current, err = r.ProjectRecord(ctx, scope.project)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	if replay {
		if receipt.RequestHash != hash {
			return nil, nativeConflict(0)
		}
		return content.Receipt(ctx, receipt)
	}
	planned, value, err := s.planProjectRecordMutation(ctx, scope, current, input)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: options.OperationID, CommandKey: command.IdempotencyKey, RequestHash: hash}, raw)
	if err != nil {
		return nil, err
	}
	authority, err := s.beginProjectRecordAuthority(ctx, scope)
	if err != nil {
		return nil, err
	}
	defer s.releaseProjectRecordAuthority(ctx, authority)
	var existing *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		previous, err := w.Receipt(ctx, scope.credential.ID, options.OperationID, command.IdempotencyKey)
		if err == nil {
			if previous.RequestHash != hash {
				return nativeConflict(0)
			}
			existing = &previous
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		switch request := input.(type) {
		case createNativeProjectRequest:
			var created projectstore.Record
			created, err = w.CreateProjectRecord(ctx, planned)
			if err == nil && (scope.credential.Hosted != nil || s.config.Hosted != nil) {
				err = s.config.ProjectParticipants.CreateProject(ctx, w, projectRecordActor(scope), created)
			}
		case cloudContentSettingsRequest:
			err = w.UpdateProjectContent(ctx, scope.project, int64(request.ExpectedRevision), request.RequireVisualAttachments)
		case cloudModelSelectionRequest:
			err = w.RequireOrganizationModels(ctx, organizationModelRevision)
			if err == nil {
				err = w.UpdateProjectModel(ctx, planned.Settings)
			}
		case deleteCloudProjectRequest:
			if s.config.ProjectParticipants == nil {
				return ErrProjectParticipants
			}
			err = s.config.ProjectParticipants.RetireProject(ctx, w, projectRecordActor(scope), current, s.config.now())
			if err == nil {
				err = w.RetireProject(ctx, scope.project, current.Settings.Revision, s.config.now())
			}
		default:
			if request, ok := input.(updateProjectIntegrationRequest); ok && projectIntegrationChanges(current, request) {
				if err := w.RequireProjectIdle(ctx, scope.project, s.config.now()); err != nil {
					return err
				}
			}
			if _, ok := input.(updateProjectIntegrationRequest); ok {
				if s.config.ProjectParticipants != nil {
					if err := s.config.ProjectParticipants.UpdateProject(ctx, w, projectRecordActor(scope), current, planned); err != nil {
						return err
					}
				} else if current.Settings.Repository != "" || current.Settings.CheckoutRepository != "" {
					return ErrProjectParticipants
				}
			}
			_, err = w.UpdateProjectRecord(ctx, planned)
		}

		if err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	rollbackErr := authority.Rollback()
	if errors.Is(rollbackErr, sql.ErrTxDone) {
		rollbackErr = nil
	}
	err = errors.Join(err, rollbackErr)
	if err != nil {
		if errors.Is(err, corestore.ErrRevisionConflict) || errors.Is(err, corestore.ErrUniqueViolation) {
			var committed eventstore.Receipt
			replayErr := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				committed, err = r.Receipt(ctx, scope.credential.ID, options.OperationID, command.IdempotencyKey)
				return err
			})
			if replayErr == nil {
				if committed.RequestHash != hash {
					return nil, nativeConflict(0)
				}
				return content.Receipt(ctx, committed)
			}
			if !errors.Is(replayErr, corestore.ErrNotFound) {
				return nil, replayErr
			}
			if errors.Is(err, corestore.ErrRevisionConflict) {
				fresh, readErr := s.readProjectRecord(ctx, scope)
				if readErr != nil {
					return nil, readErr
				}
				revision := fresh.Settings.Revision
				switch input.(type) {
				case cloudModelSelectionRequest:
					revision = fresh.Settings.ModelRevision
				case cloudContentSettingsRequest:
					revision = fresh.Core.ContentSettingsRevision
				}
				return nil, nativeConflict(tracker.Revision(revision))
			}
		}
		return nil, projectRecordError(err)
	}
	if existing != nil {
		return content.Receipt(ctx, *existing)
	}
	return raw, nil
}

func (s *Service) planProjectRecordMutation(ctx context.Context, scope nativeScope, current projectstore.Record, input any) (projectstore.Record, any, error) {
	switch request := input.(type) {
	case cloudContentSettingsRequest:
		if int64(request.ExpectedRevision) != current.Core.ContentSettingsRevision {
			return current, nil, nativeConflict(tracker.Revision(current.Core.ContentSettingsRevision))
		}
		current.Settings.RequireVisualAttachments = request.RequireVisualAttachments
		return current, projectsettings.Content{Revision: request.ExpectedRevision + 1, RequireVisualAttachments: request.RequireVisualAttachments}, nil
	case cloudModelSelectionRequest:
		return s.planProjectRecordModel(ctx, current, request)
	case createNativeProjectRequest:
		if strings.TrimSpace(request.Name) == "" || len(request.Name) > 200 {
			return current, nil, nativePublicInvalid("Project name is required and limited to 200 bytes")
		}
		states := nativeTriageStates(request.States)
		if err := validateNativeStates(states); err != nil {
			return current, nil, err
		}
		identity, err := nativeCommandHash([]string{string(scope.organization), scope.credential.ID, "create_project", request.IdempotencyKey})
		if err != nil {
			return current, nil, err
		}
		record := projectstore.Record{Core: corestore.Project{OrganizationID: scope.organization, ID: tracker.ProjectID("prj_" + identity[:32]), Name: request.Name, Profile: "native", RequireDependencies: true}, States: states}
		if request.RequireDependencies != nil {
			record.Core.RequireDependencies = *request.RequireDependencies
		}
		record.Core.Slug = projectstore.Slug(request.Name)
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.ProjectSlugOwner(ctx, record.Core.Slug)
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			record.Core.Slug = projectstore.CollisionSlug(record.Core.Slug, record.Core.ID)
			return nil
		})
		if err != nil {
			return current, nil, err
		}
		return record, tracker.NativeProject{OrganizationID: record.Core.OrganizationID, ID: record.Core.ID, Name: record.Core.Name, Slug: record.Core.Slug, Profile: record.Core.Profile, States: record.States, RequireDependencies: record.Core.RequireDependencies}, nil
	case renameProjectRequest:
		if current.Core.Profile != "native" {
			return current, nil, nativeInvalid("Project name is externally owned")
		}
		if scope.credential.Hosted == nil && scope.credential.Scope != apiScopeAdmin {
			return current, nil, nativeNotFound()
		}
		name := strings.TrimSpace(request.Name)
		if name == "" || len(name) > 200 || request.ExpectedRevision <= 0 {
			return current, nil, nativePublicInvalid("A project name of at most 200 bytes and a positive expected revision are required")
		}
		if current.Settings.Revision != int64(request.ExpectedRevision) {
			return current, nil, nativeConflict(tracker.Revision(current.Settings.Revision))
		}
		current.Core.Name = name
	case updateProjectIntegrationRequest:
		var err error
		current, err = s.planProjectRecordSettings(ctx, current, request)
		if err != nil {
			return current, nil, err
		}
	case deleteCloudProjectRequest:
		if s.config.Hosted == nil || scope.credential.Hosted == nil {
			return current, nil, nativeNotFound()
		}
		if request.ConfirmName != current.Core.Name {
			return current, nil, nativePublicInvalid("Confirm the current project name before deleting it")
		}
		return current, struct {
			ProjectID tracker.ProjectID `json:"project_id"`
		}{scope.project}, nil
	default:
		return current, nil, nativeInvalid("Unsupported project command")
	}
	result := projectIntegrationRecord(current)
	result.Revision++
	var err error
	result.WorkflowMarkdown, err = s.projectRecordWorkflow(ctx, current)
	return current, result, err
}

func (s *Service) getProjectRecord(c echo.Context) error {
	scope := nativeRequestScope(c)
	record, ok := c.Get("native_project_record").(projectstore.Record)
	var err error
	if !ok || record.Core.OrganizationID != scope.organization || record.Core.ID != scope.project {
		record, err = s.readProjectRecord(c.Request().Context(), scope)
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	project, err := s.projectRecordNative(c.Request().Context(), record)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, struct {
		tracker.NativeProject
		ReporterTemplates config.ReporterContracts `json:"reporter_templates"`
	}{project, config.DefaultReporterContracts()})
}

func isProjectCreation(input any) bool { _, ok := input.(createNativeProjectRequest); return ok }

func projectRecordRoute(path string) bool {
	switch path {
	case nativeBase, nativeBase + "/integration", nativeBase + "/model-selection", nativeBase + "/content-settings", nativeBase + "/usage/sprites", nativeBase + "/claims", nativeBase + "/claims/preview", nativeBase + "/claims/wait", nativeBase + "/policy", nativeBase + "/policy/observed", nativeBase + "/onboarding":
		return true
	}
	return false
}

func (s *Service) releaseProjectRecordAuthority(ctx context.Context, authority interface{ Rollback() error }) {
	if err := authority.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		s.config.Logger.ErrorContext(ctx, "release project authority", "error", err)
	}
}
