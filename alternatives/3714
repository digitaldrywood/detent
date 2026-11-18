package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/readableurl"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) planProjectRecordSettings(ctx context.Context, record projectstore.Record, request updateProjectIntegrationRequest) (projectstore.Record, error) {
	current := projectIntegrationRecord(record)
	if current.Revision != request.ExpectedRevision {
		return record, hubapi.NativeConflict(current.Revision)
	}
	automatic := record.Core.Profile == "native" && (record.Settings.Repository != "" || record.Settings.CheckoutRepository != "")
	preserve := automatic && request.Intake == ""
	if (!preserve && request.Intake != "disabled" && request.Intake != "manual") || (request.Projection != "disabled" && request.Projection != "summary") {
		return record, hubapi.NativePublicInvalid("Intake must be disabled or manual; projection must be disabled or summary")
	}
	completed, err := archivePeriod(request.ArchiveCompletedAfterDays, current.ArchiveCompletedAfterDays)
	if err != nil {
		return record, err
	}
	cancelled, err := archivePeriod(request.ArchiveCancelledAfterDays, current.ArchiveCancelledAfterDays)
	if err != nil {
		return record, err
	}
	if record.Settings.Repository == "" && ((!preserve && request.Intake != "disabled" && !automatic) || request.Projection != "disabled" || request.RepositoryEnabled) {
		return record, hubapi.NativePublicInvalid("Attach a GitHub repository before enabling intake, summaries or repository integration")
	}
	if current.Profile == "github_compatible" && request.Projection != "disabled" {
		return record, hubapi.NativePublicInvalid("Summary projection requires native authority; use explicit cutover first")
	}
	if request.States != nil && request.WorkflowMarkdown != nil {
		return record, hubapi.NativeWorkflowInvalid("Supply workflow_markdown or states, not both")
	}
	if (request.States != nil || request.WorkflowMarkdown != nil) && current.WorkflowSource != "" {
		return record, hubapi.NativeWorkflowInvalid("Workflow is controlled by the repository; edit its definition and approve the new repository policy")
	}
	if (request.States != nil || request.WorkflowMarkdown != nil) && current.Profile != "native" {
		return record, hubapi.NativePublicInvalid("Compatibility project workflow is externally owned")
	}
	var states *[]tracker.NativeState
	if request.WorkflowMarkdown != nil {
		if len(*request.WorkflowMarkdown) > 128*1024 {
			return record, hubapi.NativeWorkflowInvalid("Workflow Markdown is limited to 128 KiB")
		}
		workflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(*request.WorkflowMarkdown)})
		if err != nil {
			return record, hubapi.NativeWorkflowInvalid(err.Error())
		}
		if workflow.Config.Tracker.Kind != workflowconfig.TrackerHubNative {
			return record, hubapi.NativeWorkflowInvalid("Cloud workflow must use tracker.kind hub_native")
		}
		if err := workflow.Config.Validate(); err != nil {
			return record, hubapi.NativeWorkflowInvalid(err.Error())
		}
		if err := workflow.Config.ValidateNativeWorkflow(); err != nil {
			return record, hubapi.NativeWorkflowInvalid(err.Error())
		}
		value := workflow.Config.NativeWorkflowStates()
		states = &value
		raw, err := json.Marshal(*request.WorkflowMarkdown)
		if err != nil {
			return record, err
		}
		ref, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: record.Core.OrganizationID}, raw)
		if err != nil {
			return record, err
		}
		record.Settings.WorkflowKey, record.Settings.WorkflowSHA256, record.Settings.WorkflowBytes, record.Settings.WorkflowContentBytes = ref.PayloadObjectKey, ref.PayloadSHA256, ref.PayloadBytes, ref.PayloadContentBytes
	}
	if request.States != nil {
		if record.Settings.WorkflowKey != "" {
			return record, hubapi.NativeWorkflowInvalid("Workflow is authored in Markdown; update workflow_markdown instead of competing state arrays")
		}
		states = request.States
	}
	if states != nil {
		record.States = hubrecords.NativeTriageStates(*states)
		if err := hubrecords.ValidateNativeStates(record.States); err != nil {
			return record, err
		}
	}
	record.Settings.ArchiveCompletedAfterDays = nil
	if completed != nil {
		record.Settings.ArchiveCompletedAfterDays = new(int64(*completed))
	}
	record.Settings.ArchiveCancelledAfterDays = nil
	if cancelled != nil {
		record.Settings.ArchiveCancelledAfterDays = new(int64(*cancelled))
	}
	if request.AgingEnabled != nil {
		record.Settings.AgingEnabled = *request.AgingEnabled
	}
	if request.DuplicateChallenge != nil {
		record.Settings.DuplicateChallenge = *request.DuplicateChallenge
	}
	if !preserve {
		record.Settings.Intake = request.Intake
	}
	record.Settings.Projection = request.Projection
	record.Settings.RepositoryEnabled = request.RepositoryEnabled
	return record, nil
}

func projectIntegrationChanges(current projectstore.Record, request updateProjectIntegrationRequest) bool {
	preserve := current.Core.Profile == "native" && (current.Settings.Repository != "" || current.Settings.CheckoutRepository != "") && request.Intake == ""
	return request.States != nil || request.WorkflowMarkdown != nil || (!preserve && request.Intake != current.Settings.Intake) || request.Projection != current.Settings.Projection || request.RepositoryEnabled != current.Settings.RepositoryEnabled
}

func (s *Service) projectRecordModel(ctx context.Context, record projectstore.Record, models ...policystore.OrganizationModels) (cloudModelSelection, error) {
	var inherited cloudModelSelection
	var err error
	if len(models) > 0 {
		inherited, err = s.organizationModelsFromRecord(ctx, record.Core.OrganizationID, models[0])
	} else {
		inherited, err = s.readOrganizationModels(ctx, record.Core.OrganizationID)
	}
	if err != nil {
		return cloudModelSelection{}, err
	}
	result := cloudModelSelection{Revision: tracker.Revision(record.Settings.ModelRevision)}
	var override workflowconfig.ModelSelection
	if record.Settings.ModelKey != "" {
		v := record.Settings
		raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(ctx, eventstore.Receipt{OrganizationID: record.Core.OrganizationID, PayloadObjectKey: v.ModelKey, PayloadSHA256: v.ModelSHA256, PayloadBytes: v.ModelBytes, PayloadContentBytes: v.ModelContentBytes})
		if err != nil {
			return result, err
		}
		if err := json.Unmarshal(raw, &override); err != nil {
			return result, err
		}
		result.Selection = &override
	}
	result.Effective = workflowconfig.ResolveCloudModelSelection(inherited.Effective, override)
	return result, nil
}

func (s *Service) planProjectRecordModel(ctx context.Context, record projectstore.Record, request cloudModelSelectionRequest) (projectstore.Record, any, error) {
	if request.ExpectedRevision != tracker.Revision(record.Settings.ModelRevision) {
		return record, nil, hubapi.NativeConflict(tracker.Revision(record.Settings.ModelRevision))
	}
	inherited, err := s.readOrganizationModels(ctx, record.Core.OrganizationID)
	if err != nil {
		return record, nil, err
	}
	var override workflowconfig.ModelSelection
	if request.Selection != nil {
		override = *request.Selection
	}
	effective := workflowconfig.ResolveCloudModelSelection(inherited.Effective, override)
	if problems := effective.Validate(); len(problems) > 0 {
		return record, nil, hubapi.NativePublicInvalid(strings.Join(problems, "; "))
	}
	record.Settings.ModelKey, record.Settings.ModelSHA256, record.Settings.ModelBytes, record.Settings.ModelContentBytes = "", "", 0, 0
	if request.Selection != nil {
		raw, err := json.Marshal(request.Selection)
		if err != nil {
			return record, nil, err
		}
		ref, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: record.Core.OrganizationID}, raw)
		if err != nil {
			return record, nil, err
		}
		record.Settings.ModelKey, record.Settings.ModelSHA256, record.Settings.ModelBytes, record.Settings.ModelContentBytes = ref.PayloadObjectKey, ref.PayloadSHA256, ref.PayloadBytes, ref.PayloadContentBytes
	}
	return record, cloudModelSelection{Revision: request.ExpectedRevision + 1, Selection: request.Selection, Effective: effective}, nil
}

func (s *Service) projectRecordSlugSettings(c echo.Context, scope hubscope.NativeScope) error {
	ctx := c.Request().Context()
	if err := s.authorizeProjectRecord(ctx, scope, false); err != nil {
		return s.nativeAPIError(c, err)
	}
	current, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if c.Request().Method == http.MethodGet {
		return c.JSON(http.StatusOK, map[string]string{"slug": current.Core.Slug})
	}
	if scope.Credential.HostedRole != "owner" && scope.Credential.HostedRole != "admin" {
		return s.nativeAPIError(c, hubapi.NativeNotFound())
	}
	var input struct {
		Slug         string `json:"slug"`
		ExpectedSlug string `json:"expected_slug"`
	}
	if err := hubapi.DecodeAPIJSON(c, &input); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := readableurl.Validate(input.Slug); err != nil {
		return s.nativeAPIError(c, hubapi.NativeInvalid(err.Error()))
	}
	scope.RequireHostedAdmin = true
	authority, err := s.beginProjectRecordAuthority(ctx, scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer s.releaseProjectRecordAuthority(ctx, authority)
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if s.config.AccessRepository != nil {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
		}
		return w.UpdateProjectSlug(ctx, scope.Project, input.ExpectedSlug, input.Slug)
	})
	err = errors.Join(err, authority.Rollback())
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("The slug changed; reload and try again"))
	}
	if errors.Is(err, corestore.ErrUniqueViolation) {
		return s.nativeAPIError(c, hubapi.NativePublicInvalid("This slug is already reserved"))
	}
	if err != nil {
		return s.nativeAPIError(c, hubapi.ProjectRecordError(err))
	}
	return c.JSON(http.StatusOK, map[string]string{"slug": input.Slug})
}
