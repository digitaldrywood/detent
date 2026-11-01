package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type workItemArchive struct {
	archiveIssueRequest
	Archived bool `json:"archived"`
}

func workItemRecordRoute(path string) bool {
	switch path {
	case nativeBase + "/work-items", nativeBase + "/work-items/:item", nativeBase + "/work-items/:item/comments", nativeBase + "/work-items/:item/comments/:comment", nativeBase + "/work-items/:item/archive", nativeBase + "/work-items/:item/restore":
		return true
	}
	return false
}

func workItemStorageUnavailable() error {
	return &nativeError{Code: "not_ready", Message: "This work item operation requires storage that is not available", status: http.StatusServiceUnavailable, publicMessage: true}
}

func (s *Service) readWorkItemRecord(ctx context.Context, scope nativeScope, id string) (corestore.WorkItem, eventstore.CollaborationVersion, error) {
	var row corestore.WorkItem
	var version eventstore.CollaborationVersion
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		project, err := r.Project(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		row, err = r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(id))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		version, err = r.CollaborationVersion(ctx, id, row.Revision)
		return err
	})
	return row, version, projectRecordError(err)
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (s *Service) hydrateWorkItemRecord(ctx context.Context, row corestore.WorkItem, version eventstore.CollaborationVersion) (tracker.NativeIssue, error) {
	var issue tracker.NativeIssue
	if version.OrganizationID != row.OrganizationID || version.ProjectID != row.ProjectID || version.WorkItemID != row.NativeID || version.RecordID != string(row.NativeID) || version.Revision != row.Revision {
		return issue, artifact.ErrIntegrity
	}
	raw, err := eventstore.NewContent(s.runDetailStore()).CollaborationVersion(ctx, version)
	if err != nil {
		return issue, err
	}
	if json.Unmarshal(raw, &issue) != nil || issue.OrganizationID != row.OrganizationID || issue.ProjectID != row.ProjectID || issue.WorkItemID != row.NativeID || int64(issue.Revision) != row.Revision || int64(issue.Number) != row.Number || issue.Title != row.Title || issue.Archived != row.Archived || issue.State != stringValue(row.WorkflowStateName) || issue.Actor.Kind != stringValue(row.ActorKind) || issue.Actor.PrincipalID != stringValue(row.ActorPrincipalID) {
		return issue, artifact.ErrIntegrity
	}
	issue.ChangeSequence = 0
	issue.LastActivityAt = row.LastActivityAt
	return s.nativeIssueResponse(issue), nil
}

func (s *Service) readWorkItem(ctx context.Context, scope nativeScope, id string) (tracker.NativeIssue, error) {
	row, version, err := s.readWorkItemRecord(ctx, scope, id)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	issue, err := s.hydrateWorkItemRecord(ctx, row, version)
	issue.OmittedFields = append(issue.OmittedFields, "change_sequence")
	return scope.reporterIssue(issue), err
}

func (s *Service) hydrateCommentRecord(ctx context.Context, row corestore.Comment, version eventstore.CollaborationVersion) (tracker.NativeComment, error) {
	var comment tracker.NativeComment
	if version.OrganizationID != row.OrganizationID || version.ProjectID != row.ProjectID || version.WorkItemID != row.WorkItemID || version.RecordID != row.ID || version.Revision != row.Revision {
		return comment, artifact.ErrIntegrity
	}
	raw, err := eventstore.NewContent(s.runDetailStore()).CollaborationVersion(ctx, version)
	if err != nil {
		return comment, err
	}
	if json.Unmarshal(raw, &comment) != nil || comment.OrganizationID != row.OrganizationID || comment.ProjectID != row.ProjectID || comment.WorkItemID != row.WorkItemID || comment.ID != row.ID || int64(comment.Revision) != row.Revision || comment.Sequence != row.Sequence || comment.Deleted != row.Deleted || comment.Actor.Kind != row.ActorKind || comment.Actor.PrincipalID != row.ActorPrincipalID {
		return comment, artifact.ErrIntegrity
	}
	return comment, nil
}

type workItemRecordFields struct {
	Body     string
	Contract *string
	Report   *string
}

type workItemRecordPlan struct {
	project    projectstore.Record
	current    corestore.WorkItem
	issue      tracker.NativeIssue
	comment    tracker.NativeComment
	commentRow corestore.Comment
	eventType  string
	fields     []string
	create     bool
	existing   bool
}

func sameWorkItemProjectPlan(planned, current projectstore.Record) bool {
	if planned.Core.ContentSettingsRevision != current.Core.ContentSettingsRevision || planned.Settings.Revision != current.Settings.Revision || planned.Core.Profile != current.Core.Profile || planned.Core.RequireDependencies != current.Core.RequireDependencies {
		return false
	}
	return slices.EqualFunc(planned.States, current.States, func(a, b tracker.NativeState) bool {
		return a.OperatorOnly == b.OperatorOnly && a.Name == b.Name && a.Terminal == b.Terminal && a.Dispatchable == b.Dispatchable && slices.Equal(a.Transitions, b.Transitions)
	})
}

func (s *Service) loadWorkItemPlan(ctx context.Context, scope nativeScope, item, comment string, input any) (workItemRecordPlan, error) {
	var plan workItemRecordPlan
	var version eventstore.CollaborationVersion
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		plan.project, err = r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if plan.project.Core.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		if item == "" {
			request, ok := input.(tracker.CreateIssue)
			if !ok {
				return workItemStorageUnavailable()
			}
			if err := validateWorkItemRecordCreateInput(ctx, scope, plan.project, request); err != nil {
				return err
			}
			if request.Provenance != nil {
				plan.current, err = r.WorkItemBySource(ctx, scope.project, request.Provenance.Provider+":"+request.Provenance.ExternalID)
				if err == nil {
					if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(plan.current.ActorPrincipalID)}) {
						return corestore.ErrNotFound
					}
					plan.existing = true
					version, err = r.CollaborationVersion(ctx, string(plan.current.NativeID), plan.current.Revision)
					return err
				}
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
			}
			number, err := r.NextWorkItemNumber(ctx, scope.project)
			plan.issue.Number = int(number)
			return err
		}
		plan.current, err = r.WorkItem(ctx, scope.project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(plan.current.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		if comment == "" {
			if request, ok := input.(tracker.CreateComment); ok && request.Provenance != nil {
				plan.commentRow, err = r.CommentBySource(ctx, scope.project, plan.current.NativeID, request.Provenance.Provider+":"+request.Provenance.ExternalID)
				if err == nil {
					plan.existing = true
					version, err = r.CollaborationVersion(ctx, plan.commentRow.ID, plan.commentRow.Revision)
					return err
				}
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
			}
			version, err = r.CollaborationVersion(ctx, item, plan.current.Revision)
		} else {
			plan.commentRow, err = r.Comment(ctx, scope.project, tracker.NativeWorkItemID(item), comment)
			if err != nil {
				return err
			}
			version, err = r.CollaborationVersion(ctx, comment, plan.commentRow.Revision)
		}
		return err
	})
	if err != nil {
		return plan, projectRecordError(err)
	}
	if plan.project.Core.Profile != "native" {
		return plan, nativePublicInvalid("Compatibility project content is externally owned")
	}
	if plan.commentRow.ID != "" {
		plan.comment, err = s.hydrateCommentRecord(ctx, plan.commentRow, version)
	} else if plan.current.ID != 0 {
		plan.issue, err = s.hydrateWorkItemRecord(ctx, plan.current, version)
	}
	return plan, err
}

func validateWorkItemRecordCreateInput(ctx context.Context, scope nativeScope, project projectstore.Record, request tracker.CreateIssue) error {
	if request.GitHubIssueURL != "" || request.ChallengeToken != "" || request.ScheduledFailure != nil || request.ScheduledValidation != nil || len(request.NotDuplicateOf) > 0 {
		return workItemStorageUnavailable()
	}
	if _, machine := issueorigin.Parse(request.Body); machine && scope.credential.HostedRole != "reporter" {
		return workItemStorageUnavailable()
	}
	if err := validateNativeIssueType(request.Type, scope.credential.HostedRole == "reporter"); err != nil {
		return err
	}
	if err := validateNativeReportContext(request.ReportContext); err != nil {
		return err
	}
	if err := validateNativeProvenance(scope, request.Provenance); err != nil {
		return err
	}
	if err := validateNativeContent(request.Title, request.Body, request.Labels, request.Assignees, request.Priority); err != nil {
		return err
	}
	if err := requireUnreservedLabels(ctx, request.Labels); err != nil {
		return err
	}
	state := request.State
	if state == "" {
		state = defaultNativeIssueState(project.States)
	}
	if scope.credential.HostedRole == "reporter" {
		state = "Triage"
	}
	return validateNativeIssueState(scope, tracker.NativeProject{Profile: project.Core.Profile, States: project.States}, state, false)
}

func (s *Service) planWorkItemRecord(ctx context.Context, scope nativeScope, plan workItemRecordPlan, input any) (workItemRecordPlan, error) {
	now := s.config.now().UTC().Truncate(time.Microsecond)
	if plan.existing {
		return plan, nil
	}
	switch request := input.(type) {
	case tracker.CreateIssue:
		state := request.State
		if state == "" {
			state = defaultNativeIssueState(plan.project.States)
		}
		if scope.credential.HostedRole == "reporter" {
			state = "Triage"
		}
		plan.issue = tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: tracker.NativeWorkItemID(newNativeID("wi")), Number: plan.issue.Number, Revision: 1, Profile: "native"}, Type: request.Type, ReportContext: request.ReportContext, Title: request.Title, Body: request.Body, State: state, Priority: request.Priority, Labels: request.Labels, Assignees: request.Assignees, Actor: scope.actor(), Provenance: request.Provenance, CreatedAt: now, UpdatedAt: now, LastActivityAt: now, Dependencies: []tracker.NativeWorkItemID{}, Blockers: []tracker.NativeDependency{}, Dependents: []tracker.NativeDependency{}, IgnoreDependencies: !plan.project.Core.RequireDependencies, DependencyReady: true, IssueContract: &issuecontract.State{}}
		if plan.issue.Actor.Kind == "human" && scope.credential.HostedRole != "reporter" && request.Provenance == nil {
			plan.issue.IssueContract.ConfirmedSections = config.IssueContractSectionDigests(request.Body)
		}
		if plan.issue.Labels == nil {
			plan.issue.Labels = []string{}
		}
		if plan.issue.Assignees == nil {
			plan.issue.Assignees = []string{}
		}
		if plan.issue.ReportContext != nil {
			plan.issue.ReportContext = plan.issue.ReportContext.Normalized()
		}
		plan.issue.ExternalReferences = nativeSourceReferences("", "", "", 0, "", "", plan.issue.Provenance)
		for _, state := range plan.project.States {
			if state.Name == plan.issue.State {
				plan.issue.Terminal = state.Terminal
			}
		}
		plan.create = true
		plan.eventType = "issue.created"
	case workItemArchive:
		if err := requireNativeEdit(plan.issue, request.ExpectedRevision); err != nil {
			return plan, err
		}
		if plan.issue.Archived == request.Archived {
			plan.existing = true
			return plan, nil
		}
		plan.issue.Archived = request.Archived
		plan.issue.Revision++
		plan.issue.UpdatedAt = now
		plan.issue.LastActivityAt = now
		plan.eventType = "issue.edited"
		plan.fields = []string{"archived"}
	case tracker.UpdateIssue:
		if err := requireReporterTriage(scope, plan.issue); err != nil {
			return plan, err
		}
		if err := requireNativeEdit(plan.issue, request.ExpectedRevision); err != nil {
			return plan, err
		}
		if scope.credential.HostedRole == "reporter" && (request.Type != nil || request.Priority.Present() || request.Labels != nil || request.Assignees != nil) {
			return plan, operatortool.ErrAccessDenied
		}
		if request.Type != nil {
			if err := validateNativeIssueType(*request.Type, true); err != nil {
				return plan, err
			}
			plan.issue.Type = *request.Type
			plan.fields = append(plan.fields, "type")
		}
		if request.Title != nil {
			plan.issue.Title = *request.Title
			plan.fields = append(plan.fields, "title")
		}
		if request.Body != nil {
			bodyChanged := plan.issue.Body != *request.Body
			plan.issue.Body = *request.Body
			contractChanged := false
			plan.fields = append(plan.fields, "body")
			if scope.actor().Kind == "human" && scope.credential.HostedRole != "reporter" {
				if plan.issue.IssueContract == nil {
					plan.issue.IssueContract = &issuecontract.State{}
				}
				confirmed := config.IssueContractSectionDigests(plan.issue.Body)
				if bodyChanged || !maps.Equal(plan.issue.IssueContract.ConfirmedSections, confirmed) {
					plan.issue.IssueContract.ConfirmedSections = confirmed
					plan.issue.IssueContract.RecordedAt = now
					plan.issue.IssueContract.ReleaseDelivery = nil
					contractChanged = true
				}
			}
			if (bodyChanged || contractChanged) && plan.issue.IssueContract != nil && plan.issue.IssueContract.ReturnState != "" {
				contract, err := nativeIssueContract(ctx, s.database.reader, scope)
				if err != nil {
					return plan, err
				}
				evaluation := contract.Evaluate(nativeContractIssue(plan.issue))
				if evaluation.Satisfied() {
					plan.issue.IssueContract.HumanAction = ""
				} else {
					plan.issue.IssueContract.HumanAction = contract.HumanAction(nativeContractIssue(plan.issue), evaluation)
				}
			}
		}
		if request.Priority.Present() {
			plan.issue.Priority = request.Priority.Level()
			plan.fields = append(plan.fields, "priority")
		}
		if request.Labels != nil {
			if err := requireUnreservedLabels(ctx, *request.Labels); err != nil {
				return plan, err
			}
			plan.issue.Labels = *request.Labels
			plan.fields = append(plan.fields, "labels")
		}
		if request.Assignees != nil {
			plan.issue.Assignees = *request.Assignees
			plan.fields = append(plan.fields, "assignees")
		}
		if len(plan.fields) == 0 {
			return plan, nativePublicInvalid("At least one field must be supplied")
		}
		if err := validateNativeContent(plan.issue.Title, plan.issue.Body, plan.issue.Labels, plan.issue.Assignees, plan.issue.Priority); err != nil {
			return plan, err
		}
		plan.issue.Revision++
		plan.issue.UpdatedAt = now
		plan.issue.LastActivityAt = now
		plan.eventType = "issue.edited"
	case tracker.CreateComment:
		if strings.TrimSpace(request.Body) == "" || len(request.Body) > 64<<10 {
			return plan, nativePublicInvalid("Comment body must contain 1 byte to 64 KiB")
		}
		if err := validateNativeProvenance(scope, request.Provenance); err != nil {
			return plan, err
		}
		plan.comment = tracker.NativeComment{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.current.NativeID, ID: newNativeID("cmt"), Revision: 1, Sequence: plan.current.EventSequence + 1, Body: request.Body, Actor: scope.actor(), Provenance: request.Provenance, CreatedAt: now, UpdatedAt: now}
		plan.create = true
		plan.eventType = "comment.created"
	case tracker.UpdateComment:
		if scope.credential.Scope == apiScopeWorker && plan.comment.Actor.PrincipalID != scope.credential.ID {
			return plan, nativeNotFound()
		}
		if scope.credential.Hosted != nil && plan.comment.Actor.PrincipalID != scope.actor().PrincipalID {
			return plan, operatortool.ErrAccessDenied
		}
		if request.ExpectedRevision <= 0 {
			return plan, nativePublicInvalid("Expected revision must be positive")
		}
		if plan.comment.Revision != request.ExpectedRevision {
			return plan, nativeConflict(plan.comment.Revision)
		}
		if plan.comment.Deleted {
			return plan, nativePublicInvalid("Deleted comments cannot be changed")
		}
		if !request.Deleted && (strings.TrimSpace(request.Body) == "" || len(request.Body) > 64<<10) {
			return plan, nativePublicInvalid("Comment body must contain 1 byte to 64 KiB")
		}
		if plan.comment.Body == request.Body && plan.comment.Deleted == request.Deleted {
			plan.existing = true
			return plan, nil
		}
		plan.comment.Body = request.Body
		plan.comment.Deleted = request.Deleted
		if request.Deleted {
			plan.comment.Body = ""
		}
		plan.comment.Revision++
		plan.comment.UpdatedAt = now
		actor := scope.actor()
		plan.comment.EditedBy = &actor
		plan.eventType = "comment.edited"
	default:
		return plan, workItemStorageUnavailable()
	}
	for _, body := range []string{plan.issue.Title, plan.issue.Body, plan.comment.Body} {
		references, err := cloudContentAttachmentReferences(body, scope)
		if err != nil {
			return plan, err
		}
		if len(references) > 0 {
			return plan, workItemStorageUnavailable()
		}
	}
	if plan.project.Core.WorkflowSource != "" {
		return plan, workItemStorageUnavailable()
	}
	if plan.requiresIssueContract() {
		if err := validateWorkItemRecordFiling(ctx, s.database.reader, scope, plan); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

func (p workItemRecordPlan) requiresIssueContract() bool {
	return p.comment.ID == "" && (p.create || slices.Contains(p.fields, "body"))
}

func validateWorkItemRecordFiling(ctx context.Context, query nativeQueryer, scope nativeScope, plan workItemRecordPlan) error {
	if scope.credential.HostedRole == "reporter" && plan.create {
		contracts, err := nativeReporterContracts(ctx, query, scope)
		if err != nil {
			return err
		}
		contract := config.IssueContract{Sections: contracts[plan.issue.Type]}
		if missing := reporterSectionsMissing(contract.Evaluate(nativeContractIssue(plan.issue)).Missing, plan.issue.ReportContext); len(missing) > 0 {
			return nativePublicInvalid("Add or complete these required Reporter sections: " + strings.Join(missing, ", "))
		}
	}
	return requireNativeIssueContractAtFiling(ctx, query, scope, plan.issue)
}

func releaseWorkItemAuthority(authority interface{ Rollback() error }, policyTx *sql.Tx) error {
	var releaseErr error
	if policyTx != nil {
		if err := policyTx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			releaseErr = errors.Join(releaseErr, err)
		}
	}
	if err := authority.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		releaseErr = errors.Join(releaseErr, err)
	}
	return releaseErr
}

func (p workItemRecordPlan) value() any {
	if p.comment.ID != "" {
		return p.comment
	}
	return p.issue
}

func (s *Service) prepareWorkItemField(ctx context.Context, organization tracker.OrganizationID, raw []byte) (string, error) {
	ref, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: organization}, raw)
	return ref.PayloadObjectKey, err
}

func (s *Service) prepareWorkItemFields(ctx context.Context, organization tracker.OrganizationID, plan workItemRecordPlan) (workItemRecordFields, error) {
	var fields workItemRecordFields
	body := plan.issue.Body
	if plan.comment.ID != "" {
		body = plan.comment.Body
	}
	var err error
	rawBody, err := json.Marshal(body)
	if err != nil {
		return fields, err
	}
	fields.Body, err = s.prepareWorkItemField(ctx, organization, rawBody)
	if err != nil {
		return fields, err
	}
	if plan.comment.ID == "" {
		if plan.issue.IssueContract != nil {
			raw, err := json.Marshal(plan.issue.IssueContract)
			if err != nil {
				return fields, err
			}
			key, err := s.prepareWorkItemField(ctx, organization, raw)
			if err != nil {
				return fields, err
			}
			fields.Contract = &key
		}
		if plan.issue.ReportContext != nil {
			raw, err := json.Marshal(plan.issue.ReportContext)
			if err != nil {
				return fields, err
			}
			key, err := s.prepareWorkItemField(ctx, organization, raw)
			if err != nil {
				return fields, err
			}
			fields.Report = &key
		}
	}
	return fields, nil
}

func (s *Service) executeWorkItemRecordMutation(ctx context.Context, scope nativeScope, options nativeCommandOptions, command tracker.Mutation, input any, commentID string) (json.RawMessage, error) {
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if scope.credential.Scope == apiScopeWorker {
		return nil, workItemStorageUnavailable()
	}
	hash, err := nativeCommandHash(input)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
			return nil, err
		}
		if options.Item != "" {
			if _, _, err := s.readWorkItemRecord(ctx, scope, options.Item); err != nil {
				return nil, err
			}
		}
		receipt, found, err := s.workItemRecordReceipt(ctx, scope, options.OperationID, command.IdempotencyKey, hash)
		if err != nil {
			return nil, err
		}
		if found {
			return content.Receipt(ctx, receipt)
		}
		if options.Feature != "" && !options.Completion {
			now, err := s.database.currentTime()
			if err != nil {
				return nil, err
			}
			if err := s.database.requireHostedFeature(ctx, s.database.reader, options.Feature, now); err != nil {
				return nil, err
			}
		}
		plan, err := s.loadWorkItemPlan(ctx, scope, options.Item, commentID, input)
		if err != nil {
			return nil, err
		}
		plan, err = s.planWorkItemRecord(ctx, scope, plan, input)
		if err != nil {
			return nil, err
		}
		valueRaw, err := json.Marshal(plan.value())
		if err != nil {
			return nil, err
		}
		value := plan.value()
		if plan.comment.ID == "" {
			value = scope.reporterIssue(plan.issue)
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: options.OperationID, CommandKey: command.IdempotencyKey, RequestHash: hash}, raw)
		if err != nil {
			return nil, err
		}
		var version eventstore.CollaborationVersion
		var event eventstore.Event
		var fields workItemRecordFields
		if !plan.existing {
			fields, err = s.prepareWorkItemFields(ctx, scope.organization, plan)
			if err != nil {
				return nil, err
			}
			id, revision := string(plan.issue.WorkItemID), int64(plan.issue.Revision)
			item := plan.issue.WorkItemID
			at := plan.issue.UpdatedAt
			if plan.comment.ID != "" {
				id, revision, item, at = plan.comment.ID, int64(plan.comment.Revision), plan.comment.WorkItemID, plan.comment.UpdatedAt
			}
			version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: item, RecordID: id, Revision: revision}, valueRaw)
			if err != nil {
				return nil, err
			}
			data := tracker.CollaborationData{Revision: tracker.Revision(revision), Fields: plan.fields}
			if plan.comment.ID != "" {
				data.CommentID = plan.comment.ID
			}
			eventRaw, err := json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: item, ID: newNativeID("evt"), Type: plan.eventType, SchemaVersion: 1, Actor: scope.actor(), RecordedAt: at, Data: data})
			if err != nil {
				return nil, err
			}
			event, err = content.PrepareEvent(ctx, scope.organization, eventRaw)
			if err != nil {
				return nil, err
			}
		}
		authority, err := s.beginProjectRecordAuthority(ctx, scope)
		if err != nil {
			return nil, err
		}
		var policyTx *sql.Tx
		authorityQuery, ok := any(authority).(nativeQueryer)
		if !ok {
			policyTx, err = s.database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				return nil, errors.Join(err, releaseWorkItemAuthority(authority, nil))
			}
			authorityQuery = policyTx
		}
		if options.Feature != "" && !options.Completion && s.database.billingRepository == nil {
			now, err := s.database.currentTime()
			if err == nil {
				err = s.database.requireHostedFeature(ctx, authorityQuery, options.Feature, now)
			}
			if err != nil {
				return nil, errors.Join(err, releaseWorkItemAuthority(authority, policyTx))
			}
		}
		if plan.requiresIssueContract() && !plan.existing {
			if err := validateWorkItemRecordFiling(ctx, authorityQuery, scope, plan); err != nil {
				return nil, errors.Join(err, releaseWorkItemAuthority(authority, policyTx))
			}
		}
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if s.config.AccessRepository != nil {
				if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
					return err
				}
			}
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
			if options.Feature != "" && !options.Completion && s.database.billingRepository != nil {
				now, err := s.database.currentTime()
				if err != nil {
					return err
				}
				if err := s.database.requireHostedFeatureRecord(ctx, w, options.Feature, now); err != nil {
					return err
				}
			}
			project, err := w.ProjectRecord(ctx, scope.project)
			if err != nil {
				return err
			}
			if project.Core.DeletedAt != nil {
				return corestore.ErrNotFound
			}
			if !sameWorkItemProjectPlan(plan.project, project) {
				return corestore.ErrSerializationFailure
			}
			previous, err := w.Receipt(ctx, scope.credential.ID, options.OperationID, command.IdempotencyKey)
			if err == nil {
				if previous.RequestHash != hash {
					return nativeConflict(0)
				}
				return corestore.ErrSerializationFailure
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if plan.existing {
				switch request := input.(type) {
				case tracker.UpdateComment:
					current, err := w.Comment(ctx, scope.project, plan.comment.WorkItemID, plan.comment.ID)
					if err != nil {
						return err
					}
					if current.Revision != int64(request.ExpectedRevision) {
						return nativeConflict(tracker.Revision(current.Revision))
					}
				case workItemArchive:
					current, err := w.WorkItem(ctx, scope.project, plan.issue.WorkItemID)
					if err != nil {
						return err
					}
					if current.Revision != int64(request.ExpectedRevision) {
						return nativeConflict(tracker.Revision(current.Revision))
					}
				}
			}
			if !plan.existing {
				if err := writeWorkItemRecord(ctx, w, plan, fields); err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(ctx, version); err != nil {
					return err
				}
				if _, err := w.AppendEvent(ctx, event); err != nil {
					return err
				}
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		err = errors.Join(err, releaseWorkItemAuthority(authority, policyTx))
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			continue
		}
		if err != nil {
			return nil, projectRecordError(err)
		}
		return raw, nil
	}
}

func (s *Service) workItemRecordReceipt(ctx context.Context, scope nativeScope, operation, key, hash string) (eventstore.Receipt, bool, error) {
	var receipt eventstore.Receipt
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		receipt, err = r.Receipt(ctx, scope.credential.ID, operation, key)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return receipt, false, nil
	}
	if err != nil {
		return receipt, false, err
	}
	if s.config.AccessRepository != nil {
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			var err error
			receipt, err = w.Receipt(ctx, scope.credential.ID, operation, key)
			return err
		})
		if err != nil {
			return receipt, false, err
		}
	}
	if receipt.RequestHash != hash {
		return receipt, false, nativeConflict(0)
	}
	return receipt, true, nil
}

func writeWorkItemRecord(ctx context.Context, w tenantstore.Writer, plan workItemRecordPlan, fields workItemRecordFields) error {
	if plan.comment.ID != "" {
		current, err := w.LockWorkItem(ctx, plan.current.ProjectID, plan.current.NativeID)
		if err != nil {
			return err
		}
		if current.EventSequence != plan.current.EventSequence {
			return corestore.ErrSerializationFailure
		}
		if !plan.create {
			currentComment, err := w.Comment(ctx, plan.current.ProjectID, plan.current.NativeID, plan.comment.ID)
			if err != nil {
				return err
			}
			if currentComment.Revision != int64(plan.comment.Revision)-1 {
				return nativeConflict(tracker.Revision(currentComment.Revision))
			}
		}
		row := plan.commentRow
		c := plan.comment
		row.OrganizationID, row.ProjectID, row.WorkItemID, row.ID = c.OrganizationID, c.ProjectID, c.WorkItemID, c.ID
		row.Sequence, row.BodyObjectKey, row.ActorKind, row.ActorPrincipalID = c.Sequence, fields.Body, c.Actor.Kind, c.Actor.PrincipalID
		row.Deleted = c.Deleted
		row.CreatedAt = c.CreatedAt
		row.UpdatedAt = c.UpdatedAt
		if c.EditedBy != nil {
			row.EditedByKind = &c.EditedBy.Kind
			row.EditedByPrincipalID = &c.EditedBy.PrincipalID
		}
		if c.Provenance != nil {
			row.ProvenanceProvider = &c.Provenance.Provider
			row.ProvenanceExternalID = &c.Provenance.ExternalID
			row.SourceKey = new(c.Provenance.Provider + ":" + c.Provenance.ExternalID)
		}
		if plan.create {
			_, err = w.CreateComment(ctx, row)
		} else {
			_, err = w.UpdateComment(ctx, row)
		}
		return err
	}
	issue := plan.issue
	row := plan.current
	if plan.create {
		number, err := w.NextWorkItemNumber(ctx, issue.ProjectID)
		if err != nil {
			return err
		}
		if number != int64(issue.Number) {
			return corestore.ErrSerializationFailure
		}
	}
	row.OrganizationID, row.ProjectID, row.NativeID = issue.OrganizationID, issue.ProjectID, issue.WorkItemID
	row.Archived = issue.Archived
	row.Number, row.Title, row.WorkflowStateName = int64(issue.Number), issue.Title, &issue.State
	row.LastActivityAt = issue.LastActivityAt
	row.BodyObjectKey = &fields.Body
	row.IssueContractObjectKey = fields.Contract
	row.ReportContextObjectKey = fields.Report
	row.ActorKind = &issue.Actor.Kind
	row.ActorPrincipalID = &issue.Actor.PrincipalID
	row.CreatedAt, row.UpdatedAt, row.NativeCreatedAt, row.NativeUpdatedAt = issue.CreatedAt, issue.UpdatedAt, issue.CreatedAt, issue.UpdatedAt
	if issue.Provenance != nil {
		row.ProvenanceProvider = &issue.Provenance.Provider
		row.ProvenanceExternalID = &issue.Provenance.ExternalID
		row.NativeSourceKey = new(issue.Provenance.Provider + ":" + issue.Provenance.ExternalID)
	}
	if plan.create {
		_, err := w.CreateWorkItem(ctx, row)
		return err
	}
	current, err := w.LockWorkItem(ctx, row.ProjectID, row.NativeID)
	if err != nil {
		return err
	}
	if issue.Archived && !current.Archived {
		active, err := w.WorkItemHasActiveLease(ctx, row.ProjectID, row.NativeID, issue.UpdatedAt)
		if err != nil {
			return err
		}
		if active {
			return tracker.ErrLeaseConflict
		}
	}
	if current.Revision != row.Revision {
		return nativeConflict(tracker.Revision(current.Revision))
	}
	row.EventSequence = current.EventSequence
	_, err = w.UpdateWorkItem(ctx, row)
	return err
}
