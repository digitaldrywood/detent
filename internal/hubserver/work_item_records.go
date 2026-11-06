package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
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
	case nativeBase + "/labels", nativeBase + "/work-items", nativeBase + "/work-items/:item", nativeBase + "/work-items/:item/comments", nativeBase + "/work-items/:item/comments/:comment", nativeBase + "/work-items/:item/archive", nativeBase + "/work-items/:item/restore", nativeBase + "/work-items/:item/dependencies", nativeBase + "/work-items/:item/source-intake", nativeBase + "/work-items/:item/events", nativeBase + "/work-items/:item/workflow", nativeBase + "/work-items/:item/attempts", nativeBase + "/work-items/:item/attempts/:attempt":
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
	if source, found, err := s.readLinkedIssueSourceRecord(ctx, row.OrganizationID, row.ProjectID, row.NativeID); err != nil {
		return issue, err
	} else if found {
		issue.LinkedSource = &source
		issue.ExternalReferences = append(issue.ExternalReferences, tracker.GitHubIssueSourceReference(source.URL, source.URL))
	}
	return s.nativeIssueResponse(issue), nil
}

func (s *Service) readWorkItem(ctx context.Context, scope nativeScope, id string) (tracker.NativeIssue, error) {
	row, version, err := s.readWorkItemRecord(ctx, scope, id)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	issue, err := s.hydrateWorkItemRecord(ctx, row, version)
	if err == nil {
		err = s.hydrateWorkItemRelationshipFacts(ctx, scope, &issue)
	}
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
	project             projectstore.Record
	current             corestore.WorkItem
	related             corestore.WorkItem
	relationship        *corestore.Relationship
	relationshipType    string
	relationshipOperate string
	issue               tracker.NativeIssue
	linkedSource        *eventstore.LinkedSource
	comment             tracker.NativeComment
	commentRow          corestore.Comment
	eventType           string
	fields              []string
	create              bool
	existing            bool
}

func sameWorkItemProjectPlan(planned, current projectstore.Record) bool {
	if planned.Core.ContentSettingsRevision != current.Core.ContentSettingsRevision || planned.Settings.Revision != current.Settings.Revision || planned.Core.Profile != current.Core.Profile || planned.Core.RequireDependencies != current.Core.RequireDependencies || planned.Core.WorkflowSource != current.Core.WorkflowSource || planned.Core.WorkflowSourceRevision != current.Core.WorkflowSourceRevision || planned.Settings.DuplicateChallenge != current.Settings.DuplicateChallenge {
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
			if request.GitHubIssueURL != "" {
				canonical, _, _, err := tracker.ParseGitHubIssueURL(request.GitHubIssueURL)
				if err != nil {
					return nativeInvalid(err.Error())
				}
				source, err := r.LinkedSourceByURL(ctx, scope.project, canonical)
				if err == nil {
					plan.current, err = r.WorkItem(ctx, scope.project, source.WorkItemID)
					if err != nil {
						return err
					}
					plan.existing = true
					version, err = r.CollaborationVersion(ctx, string(plan.current.NativeID), plan.current.Revision)
					return err
				}
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
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
		if request, ok := input.(tracker.DependencyMutation); ok {
			plan.related, err = r.WorkItemByNativeID(ctx, request.RelatedWorkItemID)
			if err != nil {
				return err
			}
			if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(plan.related.ActorPrincipalID)}) {
				return corestore.ErrNotFound
			}
			relationshipType := request.RelationshipType
			if relationshipType == "" {
				relationshipType = "dependency"
			}
			plan.relationshipType = relationshipType
			plan.relationshipOperate = request.Operation
			plan.relationship = &corestore.Relationship{
				OrganizationID:    scope.organization,
				ProjectID:         plan.current.ProjectID,
				WorkItemID:        plan.current.NativeID,
				RelatedProjectID:  plan.related.ProjectID,
				RelatedWorkItemID: plan.related.NativeID,
				Type:              relationshipType,
			}
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
	if request.ScheduledFailure != nil || request.ScheduledValidation != nil {
		return workItemStorageUnavailable()
	}
	if len(request.ChallengeToken) > 128 || len(request.NotDuplicateOf) > 5 || (request.ChallengeToken == "") != (len(request.NotDuplicateOf) == 0) {
		return nativePublicInvalid("Supply a duplicate challenge token and one to five issue numbers together")
	}
	if request.ChallengeToken != "" && (request.GitHubIssueURL != "" || request.Provenance != nil) {
		return nativePublicInvalid("Duplicate confirmation requires an unlinked native issue")
	}
	if request.GitHubIssueURL != "" {
		canonical, repository, _, err := tracker.ParseGitHubIssueURL(request.GitHubIssueURL)
		if err != nil {
			return nativeInvalid(err.Error())
		}
		if request.Provenance != nil {
			return nativePublicInvalid("Linked issues obtain provenance during runner intake")
		}
		if err := validateLinkedIssueRepository(projectIntegrationRecord(project), repository); err != nil {
			return err
		}
		request.GitHubIssueURL = canonical
	}
	if _, machine := issueorigin.Parse(request.Body); machine && scope.credential.HostedRole != "reporter" && request.ChallengeToken != "" {
		return nativePublicInvalid("Machine reports do not accept duplicate challenges")
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
	title := request.Title
	if request.GitHubIssueURL != "" && title == "" {
		canonical, repository, number, err := tracker.ParseGitHubIssueURL(request.GitHubIssueURL)
		if err != nil || canonical == "" {
			return nativeInvalid("GitHub issue URL is invalid")
		}
		title = fmt.Sprintf("GitHub issue %s#%d", repository, number)
	}
	if err := validateNativeContent(title, request.Body, request.Labels, request.Assignees, request.Priority); err != nil {
		return err
	}
	if err := requireUnreservedLabels(ctx, request.Labels); err != nil {
		return err
	}
	state := request.State
	if request.GitHubIssueURL != "" {
		state = "Triage"
	}
	if state == "" {
		state = defaultNativeIssueState(project.States)
	}
	if scope.credential.HostedRole == "reporter" {
		state = "Triage"
	}
	if request.GitHubIssueURL != "" {
		for _, configured := range project.States {
			if configured.Name == "Triage" {
				return validateNativeIssueState(scope, tracker.NativeProject{Profile: project.Core.Profile, States: project.States}, state, false)
			}
		}
		return nil
	}
	return validateNativeIssueState(scope, tracker.NativeProject{Profile: project.Core.Profile, States: project.States}, state, nativeProgressActor(scope))
}

func (s *Service) planWorkItemRecord(ctx context.Context, scope nativeScope, plan workItemRecordPlan, input any) (workItemRecordPlan, error) {
	now := s.config.now().UTC().Truncate(time.Microsecond)
	if plan.existing {
		return plan, nil
	}
	switch request := input.(type) {
	case tracker.CreateIssue:
		state := request.State
		if request.GitHubIssueURL != "" {
			state = "Triage"
		}
		if state == "" {
			state = defaultNativeIssueState(plan.project.States)
		}
		if scope.credential.HostedRole == "reporter" {
			state = "Triage"
		}
		title := request.Title
		if request.GitHubIssueURL != "" {
			canonical, repository, number, err := tracker.ParseGitHubIssueURL(request.GitHubIssueURL)
			if err != nil {
				return plan, nativeInvalid(err.Error())
			}
			if title == "" {
				title = fmt.Sprintf("GitHub issue %s#%d", repository, number)
			}
			plan.linkedSource = &eventstore.LinkedSource{OrganizationID: scope.organization, ProjectID: scope.project, URL: canonical, TitleSupplied: request.Title != "", BodySupplied: request.Body != ""}
		}
		plan.issue = tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: tracker.NativeWorkItemID(newNativeID("wi")), Number: plan.issue.Number, Revision: 1, Profile: "native"}, Type: request.Type, ReportContext: request.ReportContext, Title: title, Body: request.Body, State: state, Priority: request.Priority, Labels: request.Labels, Assignees: request.Assignees, Actor: scope.actor(), Provenance: request.Provenance, CreatedAt: now, UpdatedAt: now, LastActivityAt: now, Dependencies: []tracker.NativeWorkItemID{}, Blockers: []tracker.NativeDependency{}, Dependents: []tracker.NativeDependency{}, IgnoreDependencies: !plan.project.Core.RequireDependencies, DependencyReady: true, IssueContract: &issuecontract.State{}}
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
		if plan.linkedSource != nil {
			plan.issue.LinkedSource = &tracker.LinkedIssueSource{URL: plan.linkedSource.URL, Status: "pending"}
			plan.issue.ExternalReferences = append(plan.issue.ExternalReferences, tracker.GitHubIssueSourceReference(plan.linkedSource.URL, plan.linkedSource.URL))
		}
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
				approval, err := s.readTenantProjectPolicy(ctx, scope.organization, scope.project)
				if err != nil {
					return plan, err
				}
				contract, err := config.ResolvePolicyIssueContract(approval.Policy)
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
	case tracker.DependencyMutation:
		if err := requireNativeEdit(plan.issue, request.ExpectedRevision); err != nil {
			return plan, err
		}
		relationshipType := request.RelationshipType
		if relationshipType == "" {
			relationshipType = "dependency"
		}
		if relationshipType != "dependency" && relationshipType != "related" {
			return plan, nativePublicInvalid("Relationship type must be dependency or related")
		}
		if request.Operation != "add" && request.Operation != "remove" {
			return plan, nativePublicInvalid("Relationship operation must be add or remove")
		}
		if plan.current.NativeID == plan.related.NativeID && plan.current.ProjectID == plan.related.ProjectID {
			return plan, nativePublicInvalid("An issue cannot be related to itself")
		}
		if relationshipType == "dependency" {
			if request.Operation == "add" {
				if !slices.Contains(plan.issue.Dependencies, plan.related.NativeID) {
					plan.issue.Dependencies = append(plan.issue.Dependencies, plan.related.NativeID)
					slices.Sort(plan.issue.Dependencies)
				}
			} else {
				plan.issue.Dependencies = slices.DeleteFunc(plan.issue.Dependencies, func(id tracker.NativeWorkItemID) bool { return id == plan.related.NativeID })
			}
			plan.eventType = "dependency.changed"
		} else {
			plan.fields = []string{"related"}
			plan.eventType = "issue.edited"
		}
		plan.issue.Revision++
		plan.issue.UpdatedAt = now
		plan.issue.LastActivityAt = now
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
		if len(references) > 0 && !(plan.comment.ID != "" && plan.create) {
			return plan, workItemStorageUnavailable()
		}
	}
	if !plan.create && plan.comment.ID == "" && plan.project.Core.WorkflowSource != "" {
		return plan, workItemStorageUnavailable()
	}
	return plan, nil
}

func (p workItemRecordPlan) requiresIssueContract() bool {
	return p.comment.ID == "" && (p.create || slices.Contains(p.fields, "body"))
}

func releaseWorkItemAuthority(authority interface{ Rollback() error }) error {
	var releaseErr error
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
	return s.executeWorkItemRecordMutationWithConversationLink(ctx, scope, options, command, input, commentID, nil)
}

func (s *Service) executeWorkItemRecordMutationWithConversationLink(ctx context.Context, scope nativeScope, options nativeCommandOptions, command tracker.Mutation, input any, commentID string, link *conversationLinkMutation) (json.RawMessage, error) {
	if s.config.AccessRepository == nil {
		return nil, workItemStorageUnavailable()
	}
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if scope.credential.Scope == apiScopeWorker {
		_, comment := input.(tracker.CreateComment)
		_, defect := input.(tracker.CreateIssue)
		if (!comment && !(defect && nativeProgressActor(scope) && scope.progressOrigin != nil)) || link != nil {
			return nil, workItemStorageUnavailable()
		}
		if _, err := s.validateRunnerLeaseTenant(ctx, scope, command.LeaseID, command.FencingToken); err != nil {
			return nil, err
		}
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
			return s.readWorkItemRecordReceipt(ctx, scope, receipt)
		}
		var confirmation *workItemDuplicateConfirmation
		if request, ok := input.(tracker.CreateIssue); ok && request.ChallengeToken != "" {
			confirmation, err = s.prepareWorkItemDuplicateConfirmation(ctx, scope, request)
			if err != nil {
				replayed, found, replayErr := s.workItemRecordReceipt(ctx, scope, options.OperationID, command.IdempotencyKey, hash)
				if replayErr != nil {
					return nil, replayErr
				}
				if found {
					return s.readWorkItemRecordReceipt(ctx, scope, replayed)
				}
				return nil, err
			}
		}
		plan, err := s.loadWorkItemPlan(ctx, scope, options.Item, commentID, input)
		if err != nil {
			return nil, err
		}
		plannedInput := input
		if request, ok := input.(tracker.CreateIssue); ok && scope.credential.HostedRole != "reporter" {
			origin, machine := issueorigin.Parse(request.Body)
			workerReport := machine && origin.Kind == "worker" && request.State == "Backlog" && scope.credential.Runner.RunnerID != "" && request.LeaseID != ""
			scheduledReport := machine && origin.Kind == "doctor" && origin.Instance == "github-actions" && scope.credential.Scope != apiScopeWorker
			if workerReport || scheduledReport {
				request, err = normalizeNativeMachineIntake(scope, plan.project.States, request)
				if err != nil {
					return nil, err
				}
				plannedInput = request
			}
		}
		plan, err = s.planWorkItemRecord(ctx, scope, plan, plannedInput)
		if err != nil {
			return nil, err
		}
		var machineSearch *workItemDuplicateSearch
		var machineReused bool
		var machinePriority *workItemMachinePriority
		if request, ok := plannedInput.(tracker.CreateIssue); ok && !plan.existing && request.GitHubIssueURL == "" && scope.credential.HostedRole != "reporter" {
			if _, machine := issueorigin.Parse(request.Body); machine {
				machineSearch, err = s.prepareWorkItemDuplicates(ctx, scope, request, true)
				if err != nil {
					return nil, err
				}
				if len(machineSearch.matches) > 0 {
					candidate := machineSearch.matches[0]
					plan, err = s.loadWorkItemPlan(ctx, scope, string(candidate.WorkItemID), "", tracker.CreateComment{})
					if err != nil {
						return nil, err
					}
					if plan.issue.Revision != candidate.revision || plan.issue.Archived {
						return nil, nativeConflict(plan.issue.Revision)
					}
					if plan.issue.Terminal {
						plan.existing = true
					} else {
						plan, err = s.planWorkItemRecord(ctx, scope, plan, tracker.CreateComment{Body: issueorigin.Occurrence(request.Body)})
						if err != nil {
							return nil, err
						}
					}
					machinePriority, err = s.prepareWorkItemMachinePriority(ctx, scope, plan, request.Priority)
					if err != nil {
						return nil, err
					}
					if machinePriority != nil {
						plan.issue = machinePriority.plan.issue
					}
					machineReused = true
				}
			}
		}
		var policyRevision policystore.Revision
		var policyFound bool
		if plan.requiresIssueContract() && !plan.existing {
			policyRevision, policyFound, err = s.validateWorkItemRecordFilingTenant(ctx, scope, plan)
			if err != nil {
				return nil, err
			}
		}
		duplicateSearch := machineSearch
		var challenge *corestore.DuplicateChallenge
		var challengeResponse *duplicateChallenge
		if request, ok := input.(tracker.CreateIssue); ok && !plan.existing {
			policyHash, err := workItemDuplicatePolicyHash(policyRevision, policyFound)
			if err != nil {
				return nil, err
			}
			if confirmation != nil {
				if confirmation.challenge.PolicyHash != policyHash {
					return nil, policyMismatch("The approved policy changed after the duplicate challenge")
				}
			} else if machineSearch == nil && request.GitHubIssueURL == "" && request.Provenance == nil && scope.credential.Scope != apiScopeWorker && plan.project.Settings.DuplicateChallenge {
				duplicateSearch, err = s.prepareWorkItemDuplicates(ctx, scope, request, false)
				if err != nil {
					return nil, err
				}
				if len(duplicateSearch.matches) > 0 {
					now := s.config.now().UTC().Truncate(time.Microsecond)
					challenge = &corestore.DuplicateChallenge{OrganizationID: scope.organization, ProjectID: scope.project, Token: newNativeID("dup"), PrincipalID: scope.actor().PrincipalID, RequestID: request.IdempotencyKey, ContentHash: hash, PolicyHash: policyHash, SourceSequence: duplicateSearch.sequence, CreatedAt: now, ExpiresAt: now.Add(duplicateChallengeLifetime)}
					for _, candidate := range duplicateSearch.matches {
						challenge.Candidates = append(challenge.Candidates, corestore.DuplicateCandidate{WorkItemID: candidate.WorkItemID, Number: int64(candidate.Number), Revision: int64(candidate.revision)})
					}
					challengeResponse = &duplicateChallenge{ChallengeToken: challenge.Token, PossibleDuplicates: duplicateSearch.matches}
				}
			}
		}
		attachmentIDs, err := s.prepareCommentAttachmentReferences(ctx, scope, plan)
		if err != nil {
			return nil, err
		}
		if link != nil {
			if err := link.prepare(ctx, plan); err != nil {
				return nil, err
			}
		}
		duplicateLinks, err := s.prepareWorkItemDuplicateLinks(ctx, scope, plan.project, plan.issue, confirmation)
		if err != nil {
			return nil, err
		}
		valueRaw, err := json.Marshal(plan.value())
		if err != nil {
			return nil, err
		}
		value := plan.value()
		if plan.comment.ID == "" {
			issue := plan.issue
			if len(duplicateLinks) > 0 {
				issue = duplicateLinks[len(duplicateLinks)-1].issue
			}
			value = scope.reporterIssue(issue)
		}
		if machineReused {
			issue := plan.issue
			issue.PublicationReused = true
			value = issue
		}
		if challengeResponse != nil {
			value = challengeResponse
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		now := s.config.now().UTC().Truncate(time.Microsecond)
		receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: options.OperationID, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
		if err != nil {
			return nil, err
		}
		var version eventstore.CollaborationVersion
		var event eventstore.Event
		var fields workItemRecordFields
		var searchDocument searchstore.Document
		var searchRevision int64
		if !plan.existing && challenge == nil {
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
			searchDocument = searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: item, ID: "issue:" + id, SourceID: id, Kind: "issue", Scope: "issue", Revision: revision, UpdatedAt: at}
			if len(duplicateLinks) > 0 {
				searchDocument.Revision = int64(duplicateLinks[len(duplicateLinks)-1].issue.Revision)
			}
			searchText := searchstore.Text{Title: plan.issue.Title, Body: plan.issue.Body, Labels: plan.issue.Labels}
			searchRevision = plan.current.Revision
			if plan.comment.ID != "" {
				searchDocument.ID, searchDocument.Kind = "comment:"+id, "comment"
				searchText = searchstore.Text{Body: plan.comment.Body}
				searchRevision = plan.commentRow.Revision
			}
			searchDocument, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchDocument, searchText)
			if err != nil {
				return nil, err
			}
			data := tracker.CollaborationData{Revision: tracker.Revision(revision), Fields: plan.fields}
			if plan.comment.ID != "" {
				data.CommentID = plan.comment.ID
			}
			if plan.relationship != nil {
				data.RelatedWorkItemID = plan.related.NativeID
				data.Operation = plan.relationshipOperate
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

		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if plan.relationship != nil {
				if err := w.LockRelationshipGraph(ctx); err != nil {
					return err
				}
				projects := []tracker.ProjectID{plan.current.ProjectID, plan.related.ProjectID}
				slices.Sort(projects)
				projects = slices.Compact(projects)
				for _, projectID := range projects {
					if err := w.LockWorkItemProject(ctx, projectID); err != nil {
						return err
					}
				}
				targetProject, err := w.ProjectRecord(ctx, plan.related.ProjectID)
				if err != nil {
					return err
				}
				if targetProject.Core.DeletedAt != nil || targetProject.Core.Profile != "native" {
					return corestore.ErrNotFound
				}
				targetScope := scope
				targetScope.project = plan.related.ProjectID
				if s.config.AccessRepository != nil {
					if err := s.authorizeAccessScope(ctx, w, targetScope, false); err != nil {
						return err
					}
				}
				if targetScope.credential.EntryKey != nil && !targetScope.credential.EntryKey.Allows(string(targetScope.project)) {
					return auth.ErrHostedIdentity
				}
				endpoints := []corestore.WorkItem{plan.current, plan.related}
				slices.SortFunc(endpoints, func(a, b corestore.WorkItem) int {
					if a.ProjectID != b.ProjectID {
						return strings.Compare(string(a.ProjectID), string(b.ProjectID))
					}
					return strings.Compare(string(a.NativeID), string(b.NativeID))
				})
				locked := make([]corestore.WorkItem, 0, len(endpoints))
				for _, endpoint := range endpoints {
					current, err := w.LockWorkItem(ctx, endpoint.ProjectID, endpoint.NativeID)
					if err != nil {
						return err
					}
					locked = append(locked, current)
				}
				currentRelated := locked[0]
				if currentRelated.ProjectID == plan.current.ProjectID && currentRelated.NativeID == plan.current.NativeID {
					currentRelated = locked[1]
				}
				if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(currentRelated.ActorPrincipalID)}) {
					return corestore.ErrNotFound
				}
			}
			if s.config.AccessRepository != nil {
				if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
					return err
				}
			}
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
			if plan.comment.ID != "" && scope.action == operatortool.AddComment && scope.credential.HostedRole == "reporter" {
				current, err := w.LockWorkItem(ctx, scope.project, plan.current.NativeID)
				if err != nil {
					return err
				}
				if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(current.ActorPrincipalID)}) {
					return corestore.ErrNotFound
				}
			}
			if duplicateSearch != nil && duplicateSearch.available {
				if err := lockWorkItemDuplicateSource(ctx, w, scope.project, duplicateSearch.sequence); err != nil {
					return err
				}
			} else if confirmation != nil {
				if err := lockWorkItemDuplicateSource(ctx, w, scope.project, confirmation.challenge.SourceSequence); err != nil {
					if errors.Is(err, searchstore.ErrIndexPending) {
						return nativeConflict(0)
					}
					return err
				}
			}
			if machineReused {
				current, err := w.LockWorkItem(ctx, scope.project, plan.issue.WorkItemID)
				if err != nil {
					return err
				}
				if current.Revision != plan.current.Revision || current.Archived {
					return nativeConflict(tracker.Revision(current.Revision))
				}
			}
			if options.Feature != "" && !options.Completion {
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
			if plan.requiresIssueContract() && !plan.existing {
				currentPolicy, err := w.ProjectPolicy(ctx, scope.project)
				if errors.Is(err, corestore.ErrNotFound) {
					if policyFound {
						return corestore.ErrSerializationFailure
					}
				} else if err != nil {
					return err
				} else if !policyFound || !sameNativeClaimProjectPolicy(policyRevision, currentPolicy) {
					return corestore.ErrSerializationFailure
				}
			}
			if confirmation != nil {
				if err := consumeWorkItemDuplicateConfirmation(ctx, w, scope, confirmation, s.config.now()); err != nil {
					return err
				}
			}
			if plan.linkedSource != nil {
				hasTriage := slices.ContainsFunc(project.States, func(state tracker.NativeState) bool { return state.Name == "Triage" })
				if !hasTriage && project.Core.WorkflowSource != "" {
					return policyMismatch("The configured workflow must include Triage for linked issues")
				}
				if !hasTriage || project.Settings.Intake != "manual" {
					if project.Core.WorkflowSource == "" {
						project.States = nativeTriageStates(project.States)
					}
					project.Settings.Intake = "manual"
					if _, err := w.UpdateProjectRecord(ctx, project); err != nil {
						return err
					}
				}
			}
			previous, err := w.Receipt(ctx, scope.credential.ID, options.OperationID, command.IdempotencyKey)
			if err == nil {
				if previous.ResponseExpired || previous.ResponseReplayUntilNs > 0 && s.config.now().UnixNano() >= previous.ResponseReplayUntilNs {
					return receiptstore.ErrExpired
				}
				if previous.RequestHash != hash {
					return nativeConflict(0)
				}
				return corestore.ErrSerializationFailure
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if challenge != nil {
				writer, ok := w.(corestore.DuplicateChallengeWriter)
				if !ok {
					return workItemStorageUnavailable()
				}
				if err := writer.CreateDuplicateChallenge(ctx, *challenge); err != nil {
					return err
				}
				_, err := w.PutReceipt(ctx, receipt)
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
			if _, defect := input.(tracker.CreateIssue); defect && scope.credential.Scope == apiScopeWorker {
				origin := scope.progressOrigin
				if !nativeProgressActor(scope) || origin == nil || origin.service != s || origin.scope.organization != scope.organization || origin.scope.project != scope.project || origin.scope.credential.ID != scope.credential.ID || origin.leaseID != command.LeaseID || origin.fencingToken != command.FencingToken {
					return runnerUnauthorized()
				}
				if err := origin.validateProgressLeaseWriter(ctx, w); err != nil {
					return err
				}
			}
			if !plan.existing {
				if _, comment := input.(tracker.CreateComment); comment && scope.credential.Scope == apiScopeWorker {
					if err := s.authorizeAttachmentCommentLease(ctx, w, scope, plan.current, command); err != nil {
						return err
					}
				}
				if plan.comment.ID != "" && (scope.credential.Scope == apiScopeWorker || len(attachmentIDs) > 0) {
					limit, err := s.attachmentQuotaLimit(ctx, w)
					if err != nil {
						return err
					}
					data, err := json.Marshal(tracker.CollaborationData{Revision: plan.comment.Revision, CommentID: plan.comment.ID})
					if err != nil {
						return err
					}
					actor, err := json.Marshal(scope.actor())
					if err != nil {
						return err
					}
					usage, err := w.AdjustCollaborationBytes(ctx, int64(len(plan.comment.Body)+len(valueRaw)+len(data)+len(actor)), limit)
					if errors.Is(err, corestore.ErrNotFound) {
						return attachmentAccountingUnavailable()
					}
					if err != nil {
						return err
					}
					if !usage.Allowed {
						return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
					}
				}
				if plan.relationship != nil {
					if err := w.SetRelationship(ctx, *plan.relationship, plan.relationshipOperate, plan.issue.UpdatedAt); err != nil {
						return err
					}
				}
				if err := writeWorkItemRecord(ctx, w, plan, fields); err != nil {
					return err
				}
				if plan.comment.ID == "" && plan.current.ID != 0 && (slices.Contains(plan.fields, "title") || slices.Contains(plan.fields, "body")) {
					source, err := w.LinkedSource(ctx, scope.project, plan.issue.WorkItemID)
					if err == nil {
						if source.SnapshotSHA256 != "" {
							source.ExpectedSnapshotSHA256 = new(source.SnapshotSHA256)
						}
						source.TitleOwned = source.TitleOwned || slices.Contains(plan.fields, "title")
						source.BodyOwned = source.BodyOwned || slices.Contains(plan.fields, "body")
						if _, err := w.UpdateLinkedSource(ctx, source); err != nil {
							return err
						}
					} else if !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
				}
				if len(duplicateLinks) == 0 {
					if err := w.PutSearch(ctx, searchDocument, searchRevision); err != nil {
						return err
					}
				}
				if _, err := w.CreateCollaborationVersion(ctx, version); err != nil {
					return err
				}
				if _, err := w.AppendEvent(ctx, event); err != nil {
					return err
				}
				if machinePriority != nil {
					if err := machinePriority.write(ctx, w); err != nil {
						return err
					}
				}
				for _, duplicate := range duplicateLinks {
					relation := corestore.Relationship{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, RelatedProjectID: scope.project, RelatedWorkItemID: duplicate.candidate.WorkItemID, Type: "related"}
					if err := w.SetRelationship(ctx, relation, "add", duplicate.issue.UpdatedAt); err != nil {
						return err
					}
					current, err := w.WorkItem(ctx, scope.project, plan.issue.WorkItemID)
					if err != nil {
						return err
					}
					update := workItemRecordPlan{project: plan.project, current: current, issue: duplicate.issue, fields: []string{"related"}}
					if err := writeWorkItemRecord(ctx, w, update, fields); err != nil {
						return err
					}
					if _, err := w.CreateCollaborationVersion(ctx, duplicate.version); err != nil {
						return err
					}
					if _, err := w.AppendEvent(ctx, duplicate.event); err != nil {
						return err
					}
				}
				if len(duplicateLinks) > 0 {
					if err := w.PutSearch(ctx, searchDocument, searchRevision); err != nil {
						return err
					}
				}
				for _, id := range attachmentIDs {
					if err := w.BindAttachment(ctx, attachmentstore.Reference{ProjectID: scope.project, AttachmentID: id, WorkItemID: plan.comment.WorkItemID, CommentID: plan.comment.ID}); err != nil {
						return err
					}
				}
			}
			if link != nil {
				if err := link.write(ctx, w, plan); err != nil {
					return err
				}
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		err = errors.Join(err, releaseWorkItemAuthority(authority))
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) || duplicateSearch != nil && duplicateSearch.available && errors.Is(err, searchstore.ErrIndexPending) {
			continue
		}
		if err != nil {
			if confirmation != nil {
				replayed, found, replayErr := s.workItemRecordReceipt(ctx, scope, options.OperationID, command.IdempotencyKey, hash)
				if replayErr != nil {
					return nil, replayErr
				}
				if found {
					return s.readWorkItemRecordReceipt(ctx, scope, replayed)
				}
			}
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
			if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
				return err
			}
			if scope.action == operatortool.AddComment && scope.actionItem != "" && scope.credential.HostedRole == "reporter" {
				if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
					return err
				}
				current, err := w.LockWorkItem(ctx, scope.project, tracker.NativeWorkItemID(scope.actionItem))
				if err != nil {
					return projectRecordError(err)
				}
				if !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(current.ActorPrincipalID)}) {
					return nativeNotFound()
				}
			}
			var err error
			receipt, err = w.Receipt(ctx, scope.credential.ID, operation, key)
			return err
		})
		if err != nil {
			return receipt, false, err
		}
	}
	if receipt.ResponseExpired || receipt.ResponseReplayUntilNs > 0 && s.config.now().UnixNano() >= receipt.ResponseReplayUntilNs {
		return receipt, false, receiptstore.ErrExpired
	}
	if receipt.RequestHash != hash {
		return receipt, false, nativeConflict(0)
	}
	return receipt, true, nil
}

func (s *Service) authorizeWorkItemRecordScope(ctx context.Context, reader accessstore.Reader, scope nativeScope) error {
	if err := s.authorizeAccessScope(ctx, reader, scope, true); err != nil {
		return err
	}
	if scope.credential.Hosted == nil {
		return nil
	}
	member, err := reader.AuthorizeAccess(ctx, s.accessAuthority(scope, true), s.config.now())
	if err != nil {
		return err
	}
	if member.UserID != "" && lesserHostedRole(member.Role, scope.credential.HostedRole) != scope.credential.HostedRole {
		return auth.ErrHostedIdentity
	}
	return nil
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
	var previousFacts claimstore.WorkItemFacts
	previousWasDispatchable := !plan.create && dispatchableProjectState(plan.project, stringValue(plan.current.WorkflowStateName))
	if previousWasDispatchable {
		var err error
		previousFacts, err = w.WorkItemFacts(ctx, plan.current.ProjectID, plan.current.NativeID)
		if err != nil {
			return err
		}
	}
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
	row.IssueType, row.Priority = issue.Type, issue.Priority
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
		if _, err := w.CreateWorkItem(ctx, row); err != nil {
			return err
		}
		if err := w.SetWorkItemLabels(ctx, row.ProjectID, row.NativeID, issue.Labels); err != nil {
			return err
		}
		if err := writeClaimFactsAndQueue(ctx, w, plan, nil); err != nil {
			return err
		}
		if plan.linkedSource != nil {
			source := *plan.linkedSource
			source.WorkItemID = row.NativeID
			if _, err := w.CreateLinkedSource(ctx, source); err != nil {
				return err
			}
		}
		return nil
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
	if _, err := w.UpdateWorkItem(ctx, row); err != nil {
		return err
	}
	if slices.Contains(plan.fields, "labels") {
		if err := w.SetWorkItemLabels(ctx, row.ProjectID, row.NativeID, issue.Labels); err != nil {
			return err
		}
	}
	var prior *claimstore.WorkItemFacts
	if previousWasDispatchable {
		prior = &previousFacts
	}
	return writeClaimFactsAndQueue(ctx, w, plan, prior)
}

func dispatchableProjectState(project projectstore.Record, state string) bool {
	for _, candidate := range project.States {
		if candidate.Name == state {
			return candidate.Dispatchable && !candidate.Terminal && !candidate.OperatorOnly
		}
	}
	return false
}

func writeClaimFactsAndQueue(ctx context.Context, w tenantstore.Writer, plan workItemRecordPlan, previous *claimstore.WorkItemFacts) error {
	issue := plan.issue
	author := issue.Actor.PrincipalID
	if issue.Provenance != nil {
		author = issue.Provenance.AuthorID
	}
	var dispatchWaitSince *time.Time
	if dispatchableProjectState(plan.project, issue.State) {
		if previous != nil && dispatchableProjectState(plan.project, stringValue(plan.current.WorkflowStateName)) {
			dispatchWaitSince = previous.DispatchWaitSince
		} else {
			entered := issue.UpdatedAt
			if plan.create {
				entered = issue.CreatedAt
			}
			dispatchWaitSince = &entered
		}
	}
	facts := claimstore.WorkItemFacts{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, Author: author, Repository: plan.project.Settings.Repository, Assignees: issue.Assignees, DispatchWaitSince: dispatchWaitSince}
	if err := w.PutWorkItemFacts(ctx, facts); err != nil {
		return err
	}
	scope := string(issue.ProjectID)
	entry, err := w.QueueEntry(ctx, issue.ProjectID, issue.WorkItemID, scope)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if errors.Is(err, corestore.ErrNotFound) {
		entry = claimstore.QueueEntry{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, Scope: scope, Rank: string(issue.WorkItemID), CreatedAt: issue.CreatedAt}
	}
	entry.OrganizationID, entry.ProjectID, entry.WorkItemID = issue.OrganizationID, issue.ProjectID, issue.WorkItemID
	entry.Scope, entry.State, entry.UpdatedAt = scope, issue.State, issue.UpdatedAt
	entry.Priority = nil
	if issue.Priority != nil {
		priority := int64(*issue.Priority)
		entry.Priority = &priority
	}
	return w.PutQueueEntry(ctx, entry)
}
