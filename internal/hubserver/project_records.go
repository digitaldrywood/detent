package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func isProjectRecordCommand(input any) bool {
	switch input.(type) {
	case operatortool.HostedProjectCreateInput, bindArtifactServiceRequest, createNativeProjectRequest, renameProjectRequest, updateProjectIntegrationRequest, deleteCloudProjectRequest, cloudModelSelectionRequest, cloudContentSettingsRequest:
		return true
	}
	return false
}

func (s *Service) authorizeProjectRecord(ctx context.Context, scope hubscope.NativeScope, write bool) error {
	if s.config.AccessRepository == nil {
		return hubapi.NativeNotFound()
	}
	return s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error { return s.authorizeAccessScope(ctx, r, scope, write) })
}

type projectAccessAuthority struct{}

func (projectAccessAuthority) Rollback() error { return nil }

func (s *Service) beginProjectRecordAuthority(ctx context.Context, scope hubscope.NativeScope) (interface{ Rollback() error }, error) {
	if s.config.AccessRepository == nil {
		return nil, hubapi.NativeNotFound()
	}
	if scope.Credential.Hosted != nil {
		if _, _, err := s.hostedMutationIdentity(ctx, scope.Credential); err != nil {
			return nil, err
		}
	}
	return projectAccessAuthority{}, nil
}

func (s *Service) recheckProjectRecordMutation(ctx context.Context, scope hubscope.NativeScope) error {
	if s.config.TenantRepository != nil && s.config.AccessRepository == nil {
		return hubapi.NativeNotFound()
	}
	if s.config.AccessRepository != nil {
		if scope.Credential.Hosted != nil {
			_, membership, err := s.hostedMutationIdentity(ctx, scope.Credential)
			if err != nil {
				return err
			}
			if membership.ID != scope.Credential.HostedMembership {
				return accessstore.ErrDenied
			}
			scope.Credential.HostedRole = hubapi.LesserHostedRole(scope.Credential.HostedRole, membership.Role.Slug)
		}
		return s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error { return s.authorizeAccessScope(ctx, r, scope, true) })
	}
	tx, err := s.beginProjectRecordAuthority(ctx, scope)
	if err != nil {
		return err
	}
	return tx.Rollback()
}

type authorizedProjectRecord struct {
	projectstore.Record
	organizationModels *policystore.OrganizationModels
}

func (s *Service) readAuthorizedProjectRecord(ctx context.Context, scope hubscope.NativeScope, write bool, includeModels bool) (authorizedProjectRecord, error) {
	if s.config.AccessRepository == nil {
		return authorizedProjectRecord{}, hubapi.NativeNotFound()
	}
	var result authorizedProjectRecord
	err := s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, write); err != nil {
			return err
		}
		var err error
		if !includeModels {
			_, err = r.ProjectVersion(ctx, scope.Project)
			return err
		}
		result.Record, err = r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if owner, ok := r.(projectstore.PageReader); ok {
			page, ready, err := owner.ProjectPage(ctx, result.Record)
			if err != nil {
				return err
			}
			if ready {
				result.Page = &page
				return nil
			}
		}
		row, err := r.OrganizationModels(ctx)
		if err != nil {
			return err
		}
		result.organizationModels = &row
		return nil
	})
	return result, hubapi.ProjectRecordError(err)
}

func readProject[T any](ctx context.Context, repository tenantstore.Repository, scope hubscope.NativeScope, read func(tenantstore.Reader, context.Context, tracker.ProjectID) (T, error)) (T, error) {
	var result T
	err := repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		result, err = read(r, ctx, scope.Project)
		return err
	})
	return result, hubapi.ProjectRecordError(err)
}

func (s *Service) readProjectMetadata(ctx context.Context, scope hubscope.NativeScope) (projectstore.Metadata, error) {
	return readProject(ctx, s.config.TenantRepository, scope, tenantstore.Reader.ProjectMetadata)
}

func (s *Service) readProjectRecord(ctx context.Context, scope hubscope.NativeScope) (projectstore.Record, error) {
	return readProject(ctx, s.config.TenantRepository, scope, tenantstore.Reader.ProjectRecord)
}

func (s *Service) projectRecordWorkflow(ctx context.Context, record projectstore.Record) (string, error) {
	return s.projectWorkflowMarkdown(ctx, record.Settings)
}

func (s *Service) projectWorkflowMarkdown(ctx context.Context, v projectstore.Settings) (string, error) {
	if v.WorkflowKey == "" {
		return "", nil
	}
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, eventstore.Receipt{OrganizationID: v.OrganizationID, PayloadObjectKey: v.WorkflowKey, PayloadSHA256: v.WorkflowSHA256, PayloadBytes: v.WorkflowBytes, PayloadContentBytes: v.WorkflowContentBytes})
	if err != nil {
		return "", err
	}
	var markdown string
	err = json.Unmarshal(raw, &markdown)
	return markdown, err
}

func (s *Service) projectRecordNative(ctx context.Context, record projectstore.Record, models ...policystore.OrganizationModels) (tracker.NativeProject, error) {
	var markdown string
	var err error
	if record.Page != nil {
		markdown = record.Page.Body
	} else {
		markdown, err = s.projectRecordWorkflow(ctx, record)
	}
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
	var effective config.ModelSelection
	if record.Page != nil {
		effective = record.Page.Model
	} else {
		selection, readErr := s.projectRecordModel(ctx, record.Settings, models...)
		if readErr != nil {
			return project, readErr
		}
		effective = selection.Effective
	}
	project.ModelSelection, err = json.Marshal(effective)
	return project, err
}

func (s *Service) executeProjectRecordMutation(ctx context.Context, scope hubscope.NativeScope, options hubapi.NativeCommandOptions, command tracker.Mutation, input any) (json.RawMessage, error) {
	hashInput := input
	if request, ok := input.(operatortool.HostedProjectCreateInput); ok {
		if s.config.Hosted == nil || scope.Credential.Hosted == nil || !request.GrantAccess {
			return nil, hubapi.NativeNotFound()
		}
		states := HostedProjectStates()
		if request.States != nil {
			states = *request.States
		}
		input = createNativeProjectRequest{Mutation: command, Name: strings.TrimSpace(request.Name), States: states}
	}
	if _, ok := input.(bindArtifactServiceRequest); ok {
		scope.RequireHostedAdmin = true
	}

	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if (scope.Credential.Hosted != nil || s.config.Hosted != nil) && s.config.ProjectParticipants == nil {
		switch input.(type) {
		case createNativeProjectRequest, deleteCloudProjectRequest:
			return nil, hubapi.ProjectRecordError(hubapi.ErrProjectParticipants)
		}
	}
	if s.config.AccessRepository == nil {
		return nil, hubapi.NativeNotFound()
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := hubapi.CommandHash(hashInput)
	if err != nil {
		return nil, err
	}
	var current projectstore.Record
	var organizationModelRevision int64
	var receipt eventstore.Receipt
	var replay bool
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		var err error
		receipt, err = r.Receipt(ctx, scope.Credential.ID, options.OperationID, command.IdempotencyKey)
		if err == nil {
			replay = true
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if scope.Project != "" {
			if _, ok := input.(cloudModelSelectionRequest); ok {
				models, readErr := r.OrganizationModels(ctx)
				if readErr != nil {
					return readErr
				}
				organizationModelRevision = models.Revision
			}
			current, err = r.ProjectRecord(ctx, scope.Project)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	if replay {
		if receipt.RequestHash != hash {
			return nil, hubapi.NativeConflict(0)
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
	receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: options.OperationID, CommandKey: command.IdempotencyKey, RequestHash: hash}, raw)
	if err != nil {
		return nil, err
	}
	mutationAudit, err := s.prepareWorkItemMutationAudit(ctx, scope, options)
	if err != nil {
		return nil, err
	}
	authority, err := s.beginProjectRecordAuthority(ctx, scope)
	if err != nil {
		return nil, err
	}
	defer s.releaseProjectRecordAuthority(ctx, authority)
	var existing *eventstore.Receipt
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		switch input.(type) {
		case createNativeProjectRequest, deleteCloudProjectRequest, cloudModelSelectionRequest:
			if err := w.LockAccessOrganization(ctx); err != nil {
				return err
			}
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		previous, err := w.Receipt(ctx, scope.Credential.ID, options.OperationID, command.IdempotencyKey)
		if err == nil {
			if previous.RequestHash != hash {
				return hubapi.NativeConflict(0)
			}
			existing = &previous
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		switch request := input.(type) {
		case bindArtifactServiceRequest:
			err = w.BindArtifactService(ctx, scope.Project, request.Binding)
		case createNativeProjectRequest:
			var created projectstore.Record
			created, err = w.CreateProjectRecord(ctx, planned)
			if err == nil && (scope.Credential.Hosted != nil || s.config.Hosted != nil) {
				err = s.config.ProjectParticipants.CreateProject(ctx, w, projectRecordActor(scope), created)
			}
		case cloudContentSettingsRequest:
			err = w.UpdateProjectContent(ctx, scope.Project, int64(request.ExpectedRevision), request.RequireVisualAttachments)
		case cloudModelSelectionRequest:
			err = w.RequireOrganizationModels(ctx, organizationModelRevision)
			if err == nil {
				err = w.UpdateProjectModel(ctx, planned.Settings)
			}
		case deleteCloudProjectRequest:
			if s.config.ProjectParticipants == nil {
				return hubapi.ErrProjectParticipants
			}
			err = s.config.ProjectParticipants.RetireProject(ctx, w, projectRecordActor(scope), current, s.config.Clock())
			if err == nil {
				err = w.RetireProject(ctx, scope.Project, current.Settings.Revision, s.config.Clock())
			}
		default:
			if request, ok := input.(updateProjectIntegrationRequest); ok && projectIntegrationChanges(current, request) {
				if err := w.RequireProjectIdle(ctx, scope.Project, s.config.Clock()); err != nil {
					return err
				}
			}
			if _, ok := input.(updateProjectIntegrationRequest); ok {
				if s.config.ProjectParticipants != nil {
					if err := s.config.ProjectParticipants.UpdateProject(ctx, w, projectRecordActor(scope), current, planned); err != nil {
						return err
					}
				} else if current.Settings.Repository != "" || current.Settings.CheckoutRepository != "" {
					return hubapi.ErrProjectParticipants
				}
			}
			_, err = w.UpdateProjectRecord(ctx, planned)
		}

		if err != nil {
			return err
		}
		if mutationAudit != nil {
			if _, err := w.RecordAccessAudit(ctx, *mutationAudit); err != nil {
				return err
			}
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
			replayErr := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				committed, err = r.Receipt(ctx, scope.Credential.ID, options.OperationID, command.IdempotencyKey)
				return err
			})
			if replayErr == nil {
				if committed.RequestHash != hash {
					return nil, hubapi.NativeConflict(0)
				}
				return content.Receipt(ctx, committed)
			}
			if !errors.Is(replayErr, corestore.ErrNotFound) {
				return nil, replayErr
			}
			if errors.Is(err, corestore.ErrRevisionConflict) {
				fresh, readErr := readProject(ctx, s.config.TenantRepository, scope, tenantstore.Reader.ProjectVersion)
				if readErr != nil {
					return nil, readErr
				}
				revision := fresh.SettingsRevision
				switch input.(type) {
				case cloudModelSelectionRequest:
					revision = fresh.ModelRevision
				case cloudContentSettingsRequest:
					revision = fresh.ContentSettingsRevision
				}
				return nil, hubapi.NativeConflict(tracker.Revision(revision))
			}
		}
		return nil, hubapi.ProjectRecordError(err)
	}
	if existing != nil {
		return content.Receipt(ctx, *existing)
	}
	return raw, nil
}

func (s *Service) planProjectRecordMutation(ctx context.Context, scope hubscope.NativeScope, current projectstore.Record, input any) (projectstore.Record, any, error) {
	switch request := input.(type) {
	case bindArtifactServiceRequest:
		binding := request.Binding
		if !artifact.ValidID(binding.ServiceID, "service") || !artifact.ValidOrigin(binding.Origin) || !slices.Contains([]string{"customer", "hosted"}, binding.Mode) || binding.Mode == "hosted" && !binding.HostedOptIn || binding.PublisherTokenID == "" {
			return current, nil, hubapi.NativePublicInvalid("Invalid artifact service binding")
		}
		binding.PublisherTokenID = ""
		return current, binding, nil

	case cloudContentSettingsRequest:
		if int64(request.ExpectedRevision) != current.Core.ContentSettingsRevision {
			return current, nil, hubapi.NativeConflict(tracker.Revision(current.Core.ContentSettingsRevision))
		}
		current.Settings.RequireVisualAttachments = request.RequireVisualAttachments
		return current, projectsettings.Content{Revision: request.ExpectedRevision + 1, RequireVisualAttachments: request.RequireVisualAttachments}, nil
	case cloudModelSelectionRequest:
		return s.planProjectRecordModel(ctx, current, request)
	case createNativeProjectRequest:
		if strings.TrimSpace(request.Name) == "" || len(request.Name) > 200 {
			return current, nil, hubapi.NativePublicInvalid("Project name is required and limited to 200 bytes")
		}
		states := hubrecords.NativeTriageStates(request.States)
		if err := hubrecords.ValidateNativeStates(states); err != nil {
			return current, nil, err
		}
		identity, err := hubapi.CommandHash([]string{string(scope.Organization), scope.Credential.ID, "create_project", request.IdempotencyKey})
		if err != nil {
			return current, nil, err
		}
		record := projectstore.Record{Core: corestore.Project{OrganizationID: scope.Organization, ID: tracker.ProjectID("prj_" + identity[:32]), Name: request.Name, Profile: "native", RequireDependencies: true}, States: states}
		if request.RequireDependencies != nil {
			record.Core.RequireDependencies = *request.RequireDependencies
		}
		record.Core.Slug = projectstore.Slug(request.Name)
		err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
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
			return current, nil, hubapi.NativeInvalid("Project name is externally owned")
		}
		if scope.Credential.Hosted == nil && scope.Credential.Scope != hubapi.APIScopeAdmin {
			return current, nil, hubapi.NativeNotFound()
		}
		name := strings.TrimSpace(request.Name)
		if name == "" || len(name) > 200 || request.ExpectedRevision <= 0 {
			return current, nil, &hubapi.NativeError{Code: "invalid_request", Message: "A project name of at most 200 bytes and a positive expected revision are required", Status: http.StatusBadRequest, PublicMessage: true}
		}
		if current.Settings.Revision != int64(request.ExpectedRevision) {
			return current, nil, hubapi.NativeConflict(tracker.Revision(current.Settings.Revision))
		}
		current.Core.Name = name
	case updateProjectIntegrationRequest:
		var err error
		current, err = s.planProjectRecordSettings(ctx, current, request)
		if err != nil {
			return current, nil, err
		}
	case deleteCloudProjectRequest:
		if s.config.Hosted == nil || scope.Credential.Hosted == nil {
			return current, nil, hubapi.NativeNotFound()
		}
		if request.ConfirmName != current.Core.Name {
			return current, nil, hubapi.NativePublicInvalid("Confirm the current project name before deleting it")
		}
		return current, struct {
			ProjectID tracker.ProjectID `json:"project_id"`
		}{scope.Project}, nil
	default:
		return current, nil, hubapi.NativeInvalid("Unsupported project command")
	}
	result := projectIntegrationRecord(current)
	result.Revision++
	var err error
	result.WorkflowMarkdown, err = s.projectRecordWorkflow(ctx, current)
	return current, result, err
}

func (s *Service) getProjectRecord(c echo.Context) error {
	scope := hubscope.NativeRequestScope(c)
	authorized, ok := c.Get("native_project_record").(authorizedProjectRecord)
	record := authorized.Record
	var models []policystore.OrganizationModels
	if ok && authorized.organizationModels != nil {
		models = append(models, *authorized.organizationModels)
	}
	var err error
	if !ok || record.Core.OrganizationID != scope.Organization || record.Core.ID != scope.Project {
		record, err = s.readProjectRecord(c.Request().Context(), scope)
		models = nil
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	project, err := s.projectRecordNative(c.Request().Context(), record, models...)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	templates, err := s.projectReporterTemplates(c.Request().Context(), scope, record.Page)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, struct {
		tracker.NativeProject
		ReporterTemplates config.ReporterContracts `json:"reporter_templates"`
	}{project, templates})
}

func isProjectCreation(input any) bool {
	switch input.(type) {
	case createNativeProjectRequest, operatortool.HostedProjectCreateInput:
		return true
	}
	return false
}

func (s *Service) releaseProjectRecordAuthority(ctx context.Context, authority interface{ Rollback() error }) {
	if err := authority.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		s.config.Logger.ErrorContext(ctx, "release project authority", "error", err)
	}
}
