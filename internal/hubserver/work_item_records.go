package hubserver

import (
	"context"
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
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workduplicates"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type workItemArchive struct {
	archiveIssueRequest
	Archived bool `json:"archived"`
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

func (s *Service) loadWorkItemPlan(ctx context.Context, scope hubscope.NativeScope, item, comment string, input any) (hubrecords.WorkItemRecordPlan, error) {
	var plan hubrecords.WorkItemRecordPlan
	var version eventstore.CollaborationVersion
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		plan.Project, err = r.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if plan.Project.Core.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		if item == "" {
			request, ok := input.(tracker.CreateIssue)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			if err := validateWorkItemRecordCreateInput(ctx, scope, plan.Project, request); err != nil {
				return err
			}
			if request.GitHubIssueURL != "" {
				canonical, _, _, err := tracker.ParseGitHubIssueURL(request.GitHubIssueURL)
				if err != nil {
					return hubapi.NativeInvalid(err.Error())
				}
				source, err := r.LinkedSourceByURL(ctx, scope.Project, canonical)
				if err == nil {
					plan.Current, err = r.WorkItem(ctx, scope.Project, source.WorkItemID)
					if err != nil {
						return err
					}
					plan.Existing = true
					version, err = r.CollaborationVersion(ctx, string(plan.Current.NativeID), plan.Current.Revision)
					return err
				}
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
			}
			if request.Provenance != nil {
				plan.Current, err = r.WorkItemBySource(ctx, scope.Project, request.Provenance.Provider+":"+request.Provenance.ExternalID)
				if err == nil {
					if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(plan.Current.ActorPrincipalID)}) {
						return corestore.ErrNotFound
					}
					plan.Existing = true
					version, err = r.CollaborationVersion(ctx, string(plan.Current.NativeID), plan.Current.Revision)
					return err
				}
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
			}
			number, err := r.NextWorkItemNumber(ctx, scope.Project)
			plan.Issue.Number = int(number)
			return err
		}
		plan.Current, err = r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(plan.Current.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		if comment == "" {
			if request, ok := input.(tracker.CreateComment); ok && request.Provenance != nil {
				plan.CommentRow, err = r.CommentBySource(ctx, scope.Project, plan.Current.NativeID, request.Provenance.Provider+":"+request.Provenance.ExternalID)
				if err == nil {
					plan.Existing = true
					version, err = r.CollaborationVersion(ctx, plan.CommentRow.ID, plan.CommentRow.Revision)
					return err
				}
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
			}
			if _, ok := input.(tracker.UpdateIssue); ok {
				source, err := r.LinkedSource(ctx, scope.Project, plan.Current.NativeID)
				if err == nil {
					plan.LinkedSource = &source
				} else if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
			}
			version, err = r.CollaborationVersion(ctx, item, plan.Current.Revision)
		} else {
			plan.CommentRow, err = r.Comment(ctx, scope.Project, tracker.NativeWorkItemID(item), comment)
			if err != nil {
				return err
			}
			version, err = r.CollaborationVersion(ctx, comment, plan.CommentRow.Revision)
		}
		if request, ok := input.(tracker.DependencyMutation); ok {
			plan.Related, err = r.WorkItemByNativeID(ctx, request.RelatedWorkItemID)
			if err != nil {
				return err
			}
			if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(plan.Related.ActorPrincipalID)}) {
				return corestore.ErrNotFound
			}
			relationshipType := request.RelationshipType
			if relationshipType == "" {
				relationshipType = "dependency"
			}
			plan.RelationshipType = relationshipType
			plan.RelationshipOperate = request.Operation
			plan.Relationship = &corestore.Relationship{
				OrganizationID:    scope.Organization,
				ProjectID:         plan.Current.ProjectID,
				WorkItemID:        plan.Current.NativeID,
				RelatedProjectID:  plan.Related.ProjectID,
				RelatedWorkItemID: plan.Related.NativeID,
				Type:              relationshipType,
			}
			plan.RelationshipPresent, err = r.RelationshipExists(ctx, *plan.Relationship)
		}
		return err
	})
	if err != nil {
		return plan, hubapi.ProjectRecordError(err)
	}
	if plan.Project.Core.Profile != "native" {
		return plan, hubapi.NativePublicInvalid("Compatibility project content is externally owned")
	}
	if plan.CommentRow.ID != "" {
		plan.Comment, err = s.hydrateCommentRecord(ctx, plan.CommentRow, version)
		plan.PreviousContent = plan.Comment.Body
		plan.PreviousFieldBytes = int64(len(plan.Comment.Body))
	} else if plan.Current.ID != 0 {
		plan.Issue, err = s.hydrateWorkItemRecord(ctx, plan.Current, version)
		plan.PreviousContent = plan.Issue.Title + "\n" + plan.Issue.Body
		if err == nil {
			plan.PreviousFieldBytes, err = issueCollaborationFieldBytes(plan.Issue)
		}
	}
	return plan, err
}

func validateWorkItemRecordCreateInput(ctx context.Context, scope hubscope.NativeScope, project projectstore.Record, request tracker.CreateIssue) error {
	if request.ScheduledFailure != nil || request.ScheduledValidation != nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	if len(request.ChallengeToken) > 128 || len(request.NotDuplicateOf) > 5 || (request.ChallengeToken == "") != (len(request.NotDuplicateOf) == 0) {
		return hubapi.NativePublicInvalid("Supply a duplicate challenge token and one to five issue numbers together")
	}
	if request.ChallengeToken != "" && (request.GitHubIssueURL != "" || request.Provenance != nil) {
		return hubapi.NativePublicInvalid("Duplicate confirmation requires an unlinked native issue")
	}
	if request.GitHubIssueURL != "" {
		canonical, repository, _, err := tracker.ParseGitHubIssueURL(request.GitHubIssueURL)
		if err != nil {
			return hubapi.NativeInvalid(err.Error())
		}
		if request.Provenance != nil {
			return hubapi.NativePublicInvalid("Linked issues obtain provenance during runner intake")
		}
		if err := validateLinkedIssueRepository(projectIntegrationRecord(project), repository); err != nil {
			return err
		}
		request.GitHubIssueURL = canonical
	}
	if _, machine := issueorigin.Parse(request.Body); machine && scope.Credential.HostedRole != "reporter" && request.ChallengeToken != "" {
		return hubapi.NativePublicInvalid("Machine reports do not accept duplicate challenges")
	}
	if err := hubrecords.ValidateNativeIssueType(request.Type, scope.Credential.HostedRole == "reporter"); err != nil {
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
			return hubapi.NativeInvalid("GitHub issue URL is invalid")
		}
		title = fmt.Sprintf("GitHub issue %s#%d", repository, number)
	}
	if err := hubrecords.ValidateNativeContent(title, request.Body, request.Labels, request.Assignees, request.Priority); err != nil {
		return err
	}
	if err := requireUnreservedLabels(ctx, request.Labels); err != nil {
		return err
	}
	state := request.State
	if state == "Triage" && request.GitHubIssueURL == "" && scope.Credential.HostedRole != "reporter" {
		for _, configured := range project.States {
			if configured.Name == "Backlog" && (!configured.OperatorOnly || scope.Credential.Scope != hubapi.APIScopeWorker) {
				return hubapi.NativePublicInvalid("Triage is reserved for linked GitHub issue intake; use Backlog for drafts")
			}
		}
		return hubapi.NativePublicInvalid("Triage is reserved for linked GitHub issue intake")
	}
	if request.GitHubIssueURL != "" && (scope.IntegrationRepository == "" || request.State == "") {
		state = "Triage"
	}
	if state == "" {
		state = defaultNativeIssueState(project.States)
	}
	if scope.Credential.HostedRole == "reporter" {
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
	origin, machine := issueorigin.Parse(request.Body)
	workerReport := machine && origin.Kind == "worker" && state == "Backlog" && scope.Credential.Runner.RunnerID != "" && request.LeaseID != ""
	return validateNativeIssueState(scope, tracker.NativeProject{Profile: project.Core.Profile, States: project.States}, state, nativeProgressActor(scope) || workerReport)
}

func (s *Service) planWorkItemRecord(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.WorkItemRecordPlan, input any) (hubrecords.WorkItemRecordPlan, error) {
	now := s.config.Clock().UTC().Truncate(time.Microsecond)
	if plan.Existing {
		return plan, nil
	}
	switch request := input.(type) {
	case tracker.CreateIssue:
		state := request.State
		if request.GitHubIssueURL != "" && (scope.IntegrationRepository == "" || request.State == "") {
			state = "Triage"
		}
		if state == "" {
			state = defaultNativeIssueState(plan.Project.States)
		}
		if scope.Credential.HostedRole == "reporter" {
			state = "Triage"
		}
		title := request.Title
		if request.GitHubIssueURL != "" {
			canonical, repository, number, err := tracker.ParseGitHubIssueURL(request.GitHubIssueURL)
			if err != nil {
				return plan, hubapi.NativeInvalid(err.Error())
			}
			if title == "" {
				title = fmt.Sprintf("GitHub issue %s#%d", repository, number)
			}
			plan.LinkedSource = &eventstore.LinkedSource{OrganizationID: scope.Organization, ProjectID: scope.Project, URL: canonical, TitleSupplied: request.Title != "", BodySupplied: request.Body != ""}
		}
		plan.Issue = tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: tracker.NativeWorkItemID(hubapi.NewID("wi")), Number: plan.Issue.Number, Revision: 1, Profile: "native"}, Type: request.Type, ReportContext: request.ReportContext, Title: title, Body: request.Body, State: state, Priority: request.Priority, Labels: request.Labels, Assignees: request.Assignees, Actor: scope.Actor(), Provenance: request.Provenance, CreatedAt: now, UpdatedAt: now, LastActivityAt: now, Dependencies: []tracker.NativeWorkItemID{}, Blockers: []tracker.NativeDependency{}, Dependents: []tracker.NativeDependency{}, IgnoreDependencies: !plan.Project.Core.RequireDependencies, DependencyReady: true, IssueContract: &issuecontract.State{Exempt: workspaceLabelAllowed(ctx)}}
		if err := s.applyScheduledRepairRecord(ctx, scope, &plan.Issue); err != nil {
			return plan, err
		}
		if nativeHumanFiled(plan.Issue) && scope.Credential.HostedRole != "reporter" {
			plan.Issue.IssueContract.ConfirmedSections = config.IssueContractSectionDigests(request.Body)
		}
		if plan.Issue.Labels == nil {
			plan.Issue.Labels = []string{}
		}
		if plan.Issue.Assignees == nil {
			plan.Issue.Assignees = []string{}
		}
		if plan.Issue.ReportContext != nil {
			plan.Issue.ReportContext = plan.Issue.ReportContext.Normalized()
		}
		importedURL := ""
		if plan.Issue.Provenance != nil && plan.Issue.Provenance.Provider == "github" {
			importedURL = githubcontract.MigrationSourceURL(plan.Issue.Body)
		}
		plan.Issue.ExternalReferences = nativeSourceReferences("", "", "", 0, "", importedURL, plan.Issue.Provenance)
		if plan.LinkedSource != nil {
			plan.Issue.LinkedSource = &tracker.LinkedIssueSource{URL: plan.LinkedSource.URL, Status: "pending"}
			plan.Issue.ExternalReferences = append(plan.Issue.ExternalReferences, tracker.GitHubIssueSourceReference(plan.LinkedSource.URL, plan.LinkedSource.URL))
		}
		for _, state := range plan.Project.States {
			if state.Name == plan.Issue.State {
				plan.Issue.Terminal = state.Terminal
			}
		}
		if plan.Issue.Terminal {
			plan.Issue.ClosedAt = &now
		}
		plan.Create = true
		plan.EventType = "issue.created"
	case workItemArchive:
		if err := hubrecords.RequireNativeEdit(plan.Issue, request.ExpectedRevision); err != nil {
			return plan, err
		}
		if plan.Issue.Archived == request.Archived {
			plan.Existing = true
			return plan, nil
		}
		plan.Issue.Archived = request.Archived
		plan.Issue.Revision++
		plan.Issue.UpdatedAt = now
		plan.Issue.LastActivityAt = now
		plan.EventType = "issue.edited"
		plan.Fields = []string{"archived"}
	case tracker.VerifyFix:
		return s.planVerificationRecord(ctx, scope, plan, request)
	case tracker.UpdateIssue:
		if err := requireReporterTriage(scope, plan.Issue); err != nil {
			return plan, err
		}
		if err := hubrecords.RequireNativeEdit(plan.Issue, request.ExpectedRevision); err != nil {
			return plan, err
		}
		if scope.Credential.HostedRole == "reporter" && (request.Type != nil || request.Priority.Present() || request.Labels != nil || request.Assignees != nil) {
			return plan, operatortool.ErrAccessDenied
		}
		if request.Type == nil && request.Title == nil && request.Body == nil && !request.Priority.Present() && request.Labels == nil && request.Assignees == nil {
			return plan, hubapi.NativePublicInvalid("At least one field must be supplied")
		}
		if request.Type != nil {
			if err := hubrecords.ValidateNativeIssueType(*request.Type, true); err != nil {
				return plan, err
			}
			if plan.Issue.Type != *request.Type {
				plan.Issue.Type = *request.Type
				plan.Fields = append(plan.Fields, "type")
			}
		}
		if request.Title != nil && (plan.Issue.Title != *request.Title || plan.LinkedSource != nil && !plan.LinkedSource.TitleOwned) {
			plan.Issue.Title = *request.Title
			plan.Fields = append(plan.Fields, "title")
		}
		var confirmedSections map[string]string
		if plan.Issue.IssueContract != nil {
			confirmedSections = plan.Issue.IssueContract.ConfirmedSections
		}
		humanConfirmation := request.Body != nil && scope.Actor().Kind == "human" && scope.Credential.HostedRole != "reporter" && !maps.Equal(confirmedSections, config.IssueContractSectionDigests(*request.Body))
		if request.Body != nil && (plan.Issue.Body != *request.Body || plan.LinkedSource != nil && !plan.LinkedSource.BodyOwned || humanConfirmation) {
			bodyChanged := plan.Issue.Body != *request.Body
			plan.Issue.Body = *request.Body
			contractChanged := false
			plan.Fields = append(plan.Fields, "body")
			if scope.Actor().Kind == "human" && scope.Credential.HostedRole != "reporter" {
				if plan.Issue.IssueContract == nil {
					plan.Issue.IssueContract = &issuecontract.State{}
				}
				confirmed := config.IssueContractSectionDigests(plan.Issue.Body)
				if bodyChanged || !maps.Equal(plan.Issue.IssueContract.ConfirmedSections, confirmed) {
					plan.Issue.IssueContract.ConfirmedSections = confirmed
					plan.Issue.IssueContract.RecordedAt = now
					plan.Issue.IssueContract.ReleaseDelivery = nil
					contractChanged = true
				}
			}
			if (bodyChanged || contractChanged) && plan.Issue.IssueContract != nil && plan.Issue.IssueContract.ReturnState != "" {
				approval, err := s.readTenantProjectPolicy(ctx, scope.Organization, scope.Project)
				if err != nil {
					return plan, err
				}
				contract, err := config.ResolvePolicyIssueContract(approval.Policy)
				if err != nil {
					return plan, err
				}
				evaluation := contract.Evaluate(hubrecords.NativeContractIssue(plan.Issue))
				if evaluation.Satisfied() {
					plan.Issue.IssueContract.HumanAction = ""
				} else {
					plan.Issue.IssueContract.HumanAction = contract.HumanAction(hubrecords.NativeContractIssue(plan.Issue), evaluation)
				}
			}
		}
		if request.Priority.Present() {
			priority := request.Priority.Level()
			if (plan.Issue.Priority == nil) != (priority == nil) || plan.Issue.Priority != nil && priority != nil && *plan.Issue.Priority != *priority {
				plan.Issue.Priority = priority
				plan.Fields = append(plan.Fields, "priority")
			}
		}
		previousLabels, previousAssignees := plan.Issue.Labels, plan.Issue.Assignees
		if request.Labels != nil {
			if err := requireUnreservedLabels(ctx, *request.Labels); err != nil {
				return plan, err
			}
			if !slices.Equal(plan.Issue.Labels, *request.Labels) {
				plan.Issue.Labels = *request.Labels
				plan.Fields = append(plan.Fields, "labels")
			}
		}
		if request.Assignees != nil && !slices.Equal(plan.Issue.Assignees, *request.Assignees) {
			plan.Issue.Assignees = *request.Assignees
			plan.Fields = append(plan.Fields, "assignees")
		}
		if err := hubrecords.ValidateNativeEditContent(plan.Issue.Title, plan.Issue.Body, plan.Issue.Labels, previousLabels, plan.Issue.Assignees, previousAssignees, plan.Issue.Priority); err != nil {
			return plan, err
		}
		if len(plan.Fields) == 0 {
			plan.Existing = true
			return plan, nil
		}
		plan.Issue.Revision++
		plan.Issue.UpdatedAt = now
		plan.Issue.LastActivityAt = now
		plan.EventType = "issue.edited"
	case tracker.DependencyMutation:
		if err := hubrecords.RequireNativeEdit(plan.Issue, request.ExpectedRevision); err != nil {
			return plan, err
		}
		relationshipType := request.RelationshipType
		if relationshipType == "" {
			relationshipType = "dependency"
		}
		if relationshipType != "dependency" && relationshipType != "related" {
			return plan, hubapi.NativePublicInvalid("Relationship type must be dependency or related")
		}
		if request.Operation != "add" && request.Operation != "remove" {
			return plan, hubapi.NativePublicInvalid("Relationship operation must be add or remove")
		}
		if plan.Current.NativeID == plan.Related.NativeID && plan.Current.ProjectID == plan.Related.ProjectID {
			return plan, hubapi.NativePublicInvalid("An issue cannot be related to itself")
		}
		if plan.RelationshipPresent == (request.Operation == "add") {
			plan.Existing = true
			return plan, nil
		}
		if relationshipType == "dependency" {
			if request.Operation == "add" {
				if !slices.Contains(plan.Issue.Dependencies, plan.Related.NativeID) {
					plan.Issue.Dependencies = append(plan.Issue.Dependencies, plan.Related.NativeID)
					slices.Sort(plan.Issue.Dependencies)
				}
			} else {
				plan.Issue.Dependencies = slices.DeleteFunc(plan.Issue.Dependencies, func(id tracker.NativeWorkItemID) bool { return id == plan.Related.NativeID })
			}
			plan.EventType = "dependency.changed"
		} else {
			plan.Fields = []string{"related"}
			plan.EventType = "issue.edited"
		}
		plan.Issue.Revision++
		plan.Issue.UpdatedAt = now
		plan.Issue.LastActivityAt = now
	case tracker.CreateComment:
		if strings.TrimSpace(request.Body) == "" || len(request.Body) > 64<<10 {
			return plan, hubapi.NativePublicInvalid("Comment body must contain 1 byte to 64 KiB")
		}
		if err := validateNativeProvenance(scope, request.Provenance); err != nil {
			return plan, err
		}
		plan.Comment = tracker.NativeComment{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.Current.NativeID, ID: hubapi.NewID("cmt"), Revision: 1, Sequence: plan.Current.EventSequence + 1, Body: request.Body, Actor: scope.Actor(), Provenance: request.Provenance, CreatedAt: now, UpdatedAt: now}
		plan.PreviousContent = ""
		plan.Create = true
		plan.EventType = "comment.created"
	case tracker.UpdateComment:
		if scope.Credential.Scope == hubapi.APIScopeWorker && plan.Comment.Actor.PrincipalID != scope.Actor().PrincipalID {
			return plan, hubapi.NativeNotFound()
		}
		if scope.Credential.Hosted != nil && plan.Comment.Actor.PrincipalID != scope.Actor().PrincipalID {
			return plan, operatortool.ErrAccessDenied
		}
		if request.ExpectedRevision <= 0 {
			return plan, hubapi.NativePublicInvalid("Expected revision must be positive")
		}
		if plan.Comment.Revision != request.ExpectedRevision {
			return plan, hubapi.NativeConflict(plan.Comment.Revision)
		}
		if plan.Comment.Deleted {
			return plan, hubapi.NativePublicInvalid("Deleted comments cannot be changed")
		}
		if !request.Deleted && (strings.TrimSpace(request.Body) == "" || len(request.Body) > 64<<10) {
			return plan, hubapi.NativePublicInvalid("Comment body must contain 1 byte to 64 KiB")
		}
		if plan.Comment.Body == request.Body && plan.Comment.Deleted == request.Deleted {
			plan.Existing = true
			return plan, nil
		}
		plan.Comment.Body = request.Body
		plan.Comment.Deleted = request.Deleted
		if request.Deleted {
			plan.Comment.Body = ""
		}
		plan.Comment.Revision++
		plan.Comment.UpdatedAt = now
		actor := scope.Actor()
		plan.Comment.EditedBy = &actor
		plan.EventType = "comment.edited"
	default:
		return plan, hubapi.NativeWorkItemStorageUnavailable()
	}
	return plan, nil
}

func (s *Service) prepareWorkItemField(ctx context.Context, organization tracker.OrganizationID, raw []byte) (string, error) {
	ref, err := eventstore.NewContent(s.runDetailStore()).PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: organization}, raw)
	return ref.PayloadObjectKey, err
}

func (s *Service) prepareWorkItemFields(ctx context.Context, organization tracker.OrganizationID, plan hubrecords.WorkItemRecordPlan) (hubrecords.WorkItemRecordFields, error) {
	var fields hubrecords.WorkItemRecordFields
	body := plan.Issue.Body
	if plan.Comment.ID != "" {
		body = plan.Comment.Body
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
	if plan.Comment.ID == "" {
		if plan.Issue.IssueContract != nil {
			raw, err := json.Marshal(plan.Issue.IssueContract)
			if err != nil {
				return fields, err
			}
			key, err := s.prepareWorkItemField(ctx, organization, raw)
			if err != nil {
				return fields, err
			}
			fields.Contract = &key
		}
		if plan.Issue.ReportContext != nil {
			raw, err := json.Marshal(plan.Issue.ReportContext)
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

func (s *Service) executeWorkItemRecordMutation(ctx context.Context, scope hubscope.NativeScope, options hubapi.NativeCommandOptions, command tracker.Mutation, input any, commentID string) (json.RawMessage, error) {
	raw, err := s.executeWorkItemRecordMutationWithConversationLink(ctx, scope, options, command, input, commentID, nil)
	if err != nil || commentID != "" {
		return raw, err
	}
	switch input.(type) {
	case tracker.CreateComment, tracker.UpdateComment:
		return raw, nil
	}
	var challenge duplicateChallenge
	if json.Unmarshal(raw, &challenge) == nil && challenge.ChallengeToken != "" {
		return s.nativeIssueJSON(ctx, scope, raw)
	}
	return raw, nil
}

type preparedWorkItemRecordMutation struct {
	plan        hubrecords.WorkItemRecordPlan
	browserPath string
	write       func(tenantstore.Writer) error
}

func (s *Service) prepareWorkItemRecordMutation(ctx context.Context, scope hubscope.NativeScope, options hubapi.NativeCommandOptions, command tracker.Mutation, input any, commentID string) (preparedWorkItemRecordMutation, json.RawMessage, error) {
	var prepared preparedWorkItemRecordMutation
	raw, err := s.runWorkItemRecordMutation(ctx, scope, options, command, input, commentID, nil, &prepared)
	return prepared, raw, err
}

func (s *Service) executeWorkItemRecordMutationWithConversationLink(ctx context.Context, scope hubscope.NativeScope, options hubapi.NativeCommandOptions, command tracker.Mutation, input any, commentID string, link *conversationLinkMutation) (json.RawMessage, error) {
	return s.runWorkItemRecordMutation(ctx, scope, options, command, input, commentID, link, nil)
}

func (s *Service) runWorkItemRecordMutation(ctx context.Context, scope hubscope.NativeScope, options hubapi.NativeCommandOptions, command tracker.Mutation, input any, commentID string, link *conversationLinkMutation, prepared *preparedWorkItemRecordMutation) (json.RawMessage, error) {
	if s.config.AccessRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if scope.Credential.Scope == hubapi.APIScopeWorker {
		_, comment := input.(tracker.CreateComment)
		_, editComment := input.(tracker.UpdateComment)
		_, edit := input.(tracker.UpdateIssue)
		request, defect := input.(tracker.CreateIssue)
		origin, machine := issueorigin.Parse(request.Body)
		workerReport := defect && machine && origin.Kind == "worker" && request.State == "Backlog"
		if (!comment && !editComment && !edit && !workerReport && (!defect || !nativeProgressActor(scope) || scope.ProgressOrigin == nil)) || link != nil {
			if defect && link == nil && s.config.TenantRepository != nil {
				if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
						return err
					}
					project, err := r.ProjectRecord(ctx, scope.Project)
					if err != nil {
						return err
					}
					for _, state := range project.States {
						if state.Name == request.State && state.OperatorOnly {
							return validateNativeIssueState(scope, tracker.NativeProject{Profile: project.Core.Profile, States: project.States}, request.State, false)
						}
					}
					return nil
				}); err != nil {
					return nil, err
				}
			}
			return nil, hubapi.NativePublicInvalid("Runner credentials cannot perform this work item mutation")
		}
		if !options.RequireLease || command.LeaseID != "" || command.FencingToken != 0 {
			if command.LeaseID == "" || command.FencingToken <= 0 {
				return nil, tracker.ErrStaleFencingToken
			}
			if _, err := s.validateRunnerLeaseTenant(ctx, scope, command.LeaseID, command.FencingToken); err != nil {
				return nil, err
			}
		}
	}
	hash, err := hubapi.CommandHash(input)
	if err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	mutationAudit, err := s.prepareWorkItemMutationAudit(ctx, scope, options)
	if err != nil {
		return nil, err
	}
	sourceMoved := false
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
			return s.readWorkItemRecordReceipt(ctx, scope, receipt, input, options.Item)
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
					return s.readWorkItemRecordReceipt(ctx, scope, replayed, input, options.Item)
				}
				return nil, err
			}
		}
		plan, err := s.loadWorkItemPlan(ctx, scope, options.Item, commentID, input)
		if err != nil {
			return nil, err
		}
		plannedInput := input
		if request, ok := input.(tracker.CreateIssue); ok && scope.Credential.HostedRole != "reporter" {
			origin, machine := issueorigin.Parse(request.Body)
			workerReport := machine && origin.Kind == "worker" && request.State == "Backlog" && scope.Credential.Runner.RunnerID != "" && request.LeaseID != ""
			scheduledReport := machine && origin.Kind == "doctor" && origin.Instance == "github-actions" && scope.Credential.Scope != hubapi.APIScopeWorker
			if workerReport || scheduledReport {
				request, err = normalizeNativeMachineIntake(scope, plan.Project.States, request)
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
		var machineSearch *workduplicates.Search
		var machineReused bool
		var machinePriority *workItemMachinePriority
		if request, ok := plannedInput.(tracker.CreateIssue); ok && !plan.Existing && request.GitHubIssueURL == "" && scope.Credential.HostedRole != "reporter" {
			if origin, machine := issueorigin.Parse(request.Body); machine {
				machineSearch, err = s.prepareWorkItemDuplicates(ctx, scope, request, true)
				if err != nil {
					return nil, err
				}
				if len(machineSearch.Matches) > 0 {
					candidate := machineSearch.Matches[0]
					matched, err := s.loadWorkItemPlan(ctx, scope, string(candidate.WorkItemID), "", tracker.CreateComment{})
					if err != nil {
						return nil, err
					}
					if matched.Issue.Revision != candidate.Revision || matched.Issue.Archived {
						return nil, hubapi.NativeConflict(matched.Issue.Revision)
					}
					terminalRepair := matched.Issue.Terminal && origin.Kind == "doctor" && origin.Instance == "github-actions" && scope.Credential.Scope != hubapi.APIScopeWorker && matched.Issue.State != "Cancelled" && !strings.Contains(strings.ToLower(matched.Issue.Body), "detent:no-dispatch")
					if !terminalRepair {
						plan = matched
						if plan.Issue.Terminal {
							plan.Existing = true
						} else {
							plan, err = s.planWorkItemRecord(ctx, scope, plan, tracker.CreateComment{Body: issueorigin.Occurrence(request.Body)})
							if err != nil {
								return nil, err
							}
						}
						if !plan.Issue.Terminal && request.Priority != nil && (plan.Issue.Priority == nil || *request.Priority < *plan.Issue.Priority) {
							machinePriority, err = s.prepareWorkItemMachinePriority(ctx, scope, plan, request.Priority)
							if err != nil {
								return nil, err
							}
							plan.Issue = machinePriority.plan.Issue
						}
						machineReused = true
					}
				}
			}
		}
		var scheduledOccurrence bool
		if request, ok := plannedInput.(tracker.CreateIssue); ok && !plan.Existing && request.GitHubIssueURL != "" {
			plan, machineSearch, scheduledOccurrence, err = s.prepareLinkedScheduledDuplicate(ctx, scope, plan, request)
			if err != nil {
				return nil, err
			}
			machineReused = machineReused || scheduledOccurrence
		}
		plan, err = s.prepareLinkedIssueCreation(ctx, scope, plan)
		if err != nil {
			return nil, err
		}
		var policyRevision policystore.Revision
		var policyFound bool
		if plan.RequiresIssueContract() && !plan.Existing {
			policyRevision, policyFound, err = s.validateWorkItemRecordFilingTenant(ctx, scope, plan)
			if err != nil {
				return nil, err
			}
		}
		duplicateSearch := machineSearch
		var challenge *corestore.DuplicateChallenge
		var challengeResponse *duplicateChallenge
		if request, ok := input.(tracker.CreateIssue); ok && !plan.Existing {
			policyHash, err := workItemDuplicatePolicyHash(policyRevision, policyFound)
			if err != nil {
				return nil, err
			}
			if confirmation != nil {
				if confirmation.challenge.PolicyHash != policyHash {
					return nil, hubapi.PolicyMismatch("The approved policy changed after the duplicate challenge")
				}
			} else if machineSearch == nil && request.GitHubIssueURL == "" && request.Provenance == nil && scope.Credential.Scope != hubapi.APIScopeWorker && plan.Project.Settings.DuplicateChallenge {
				duplicateSearch, err = s.prepareWorkItemDuplicates(ctx, scope, request, false)
				if err != nil {
					return nil, err
				}
				if sourceMoved && !duplicateSearch.Available {
					return nil, hubapi.NativeConflict(0)
				}
				if len(duplicateSearch.Matches) > 0 {
					now := s.config.Clock().UTC().Truncate(time.Microsecond)
					challenge = &corestore.DuplicateChallenge{OrganizationID: scope.Organization, ProjectID: scope.Project, Token: hubapi.NewID("dup"), PrincipalID: scope.Actor().PrincipalID, RequestID: request.IdempotencyKey, ContentHash: hash, PolicyHash: policyHash, SourceSequence: duplicateSearch.Sequence, CreatedAt: now, ExpiresAt: now.Add(duplicateChallengeLifetime)}
					for _, candidate := range duplicateSearch.Matches {
						challenge.Candidates = append(challenge.Candidates, corestore.DuplicateCandidate{WorkItemID: candidate.WorkItemID, Number: int64(candidate.Number), Revision: int64(candidate.Revision)})
					}
					challengeResponse = &duplicateChallenge{ChallengeToken: challenge.Token, PossibleDuplicates: duplicateSearch.Matches}
				}
			}
		}
		attachmentReferences, err := s.prepareWorkItemAttachmentReferences(ctx, scope, plan)
		if err != nil {
			return nil, err
		}
		if link != nil {
			if err := link.Prepare(ctx, plan); err != nil {
				return nil, err
			}
		}
		duplicateLinks, err := s.prepareWorkItemDuplicateLinks(ctx, scope, plan.Project, plan.Issue, confirmation)
		if err != nil {
			return nil, err
		}
		valueRaw, err := json.Marshal(plan.Value())
		if err != nil {
			return nil, err
		}
		value := plan.Value()
		if plan.Comment.ID == "" {
			issue := plan.Issue
			if len(duplicateLinks) > 0 {
				issue = duplicateLinks[len(duplicateLinks)-1].issue
			}
			value = scope.ReporterIssue(issue)
		}
		if machineReused {
			issue := plan.Issue
			issue.PublicationReused = true
			value = issue
		}
		if challengeResponse != nil {
			value = challengeResponse
		}
		var browserPath string
		if issue, ok := value.(tracker.NativeIssue); ok {
			if !scope.ReporterOwns(issue.Actor) {
				return nil, hubapi.NativeNotFound()
			}
			issue = hubrecords.RelationshipOmittedIssue(scope.ReporterIssue(issue))
			err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				organization, err := r.AccessOrganization(ctx)
				if err == nil {
					issue.WebURL = hubrecords.IssueBrowserPath(organization.Slug, plan.Project.Core.Slug, issue.Number)
				}
				return err
			})
			if err != nil {
				issue.WebURL = ""
				issue.OmittedFields = append(issue.OmittedFields, "web_url")
			}
			browserPath = issue.WebURL
			value = s.nativeIssueResponse(issue)
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		now := s.config.Clock().UTC().Truncate(time.Microsecond)
		receipt, err = content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: options.OperationID, CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
		if err != nil {
			return nil, err
		}
		var version eventstore.CollaborationVersion
		var event eventstore.Event
		var growth workItemGrowth
		var fields hubrecords.WorkItemRecordFields
		var searchDocument searchstore.Document
		var searchRevision int64
		if !plan.Existing && challenge == nil {
			fields, err = s.prepareWorkItemFields(ctx, scope.Organization, plan)
			if err != nil {
				return nil, err
			}
			id, revision := string(plan.Issue.WorkItemID), int64(plan.Issue.Revision)
			item := plan.Issue.WorkItemID
			at := plan.Issue.UpdatedAt
			if plan.Comment.ID != "" {
				id, revision, item, at = plan.Comment.ID, int64(plan.Comment.Revision), plan.Comment.WorkItemID, plan.Comment.UpdatedAt
			}
			version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item, RecordID: id, Revision: revision}, valueRaw)
			if err != nil {
				return nil, err
			}
			searchDocument = searchstore.Document{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item, ID: "issue:" + id, SourceID: id, Kind: "issue", Scope: "issue", Revision: revision, UpdatedAt: at}
			if len(duplicateLinks) > 0 {
				searchDocument.Revision = int64(duplicateLinks[len(duplicateLinks)-1].issue.Revision)
			}
			searchText := searchstore.Text{Title: plan.Issue.Title, Body: plan.Issue.Body, Labels: plan.Issue.Labels}
			searchRevision = plan.Current.Revision
			if plan.Comment.ID != "" {
				searchDocument.ID, searchDocument.Kind = "comment:"+id, "comment"
				searchText = searchstore.Text{Body: plan.Comment.Body}
				searchRevision = plan.CommentRow.Revision
			}
			searchDocument, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchDocument, searchText)
			if err != nil {
				return nil, err
			}
			data := tracker.CollaborationData{Revision: tracker.Revision(revision), Fields: plan.Fields}
			if request, ok := input.(workItemArchive); ok {
				data.Operation = "restore"
				if request.Archived {
					data.Operation = "archive"
				}
			}
			if request, ok := input.(tracker.VerifyFix); ok {
				if plan.Issue.Verification != nil {
					data.Verification = plan.Issue.Verification.State
				}
				if request.Result == tracker.VerifyResultNotFixed {
					data.FromState = hubapi.StringValue(plan.Current.WorkflowStateName)
					data.ToState = plan.Issue.State
					data.Reason = "user_requested"
					data.ReasonDetail = "Filer verification: not fixed"
				}
			}
			if plan.Comment.ID != "" {
				data.CommentID = plan.Comment.ID
			}
			if plan.Relationship != nil {
				data.RelatedWorkItemID = plan.Related.NativeID
				data.Operation = plan.RelationshipOperate
			}
			eventRaw, err := json.Marshal(tracker.CollaborationEvent{OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: item, ID: hubapi.NewID("evt"), Type: plan.EventType, SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: at, Data: data})
			if err != nil {
				return nil, err
			}
			event, err = content.PrepareEvent(ctx, scope.Organization, eventRaw)
			if err != nil {
				return nil, err
			}
			growth, err = workItemMutationGrowth(plan, valueRaw, eventRaw, duplicateLinks)
			if err != nil {
				return nil, err
			}
		}
		write := func(w tenantstore.Writer) error {
			if scope.IntegrationRepository != "" || scope.ScheduledRepair != nil {
				if err := w.LockAccessOrganization(ctx); err != nil {
					return err
				}
				if err := s.requireGitHubPolicyRecord(ctx, w, scope); err != nil {
					return err
				}
			}
			if plan.Relationship != nil {
				if err := w.LockRelationshipGraph(ctx); err != nil {
					return err
				}
				projects := []tracker.ProjectID{plan.Current.ProjectID, plan.Related.ProjectID}
				slices.Sort(projects)
				projects = slices.Compact(projects)
				for _, projectID := range projects {
					if err := w.LockWorkItemProject(ctx, projectID); err != nil {
						return err
					}
				}
				targetProject, err := w.ProjectRecord(ctx, plan.Related.ProjectID)
				if err != nil {
					return err
				}
				if targetProject.Core.DeletedAt != nil || targetProject.Core.Profile != "native" {
					return corestore.ErrNotFound
				}
				targetScope := scope
				targetScope.Project = plan.Related.ProjectID
				if s.config.AccessRepository != nil {
					if err := s.authorizeAccessScope(ctx, w, targetScope, false); err != nil {
						return err
					}
				}
				if targetScope.Credential.EntryKey != nil && !targetScope.Credential.EntryKey.Allows(string(targetScope.Project)) {
					return auth.ErrHostedIdentity
				}
				endpoints := []corestore.WorkItem{plan.Current, plan.Related}
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
				if currentRelated.ProjectID == plan.Current.ProjectID && currentRelated.NativeID == plan.Current.NativeID {
					currentRelated = locked[1]
				}
				if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(currentRelated.ActorPrincipalID)}) {
					return corestore.ErrNotFound
				}
			}
			if s.config.AccessRepository != nil {
				if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
					return err
				}
			}
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			if plan.Comment.ID != "" && scope.Action == operatortool.AddComment && scope.Credential.HostedRole == "reporter" {
				current, err := w.LockWorkItem(ctx, scope.Project, plan.Current.NativeID)
				if err != nil {
					return err
				}
				if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(current.ActorPrincipalID)}) {
					return corestore.ErrNotFound
				}
			}
			project, err := w.ProjectRecord(ctx, scope.Project)
			if err != nil {
				return err
			}
			if project.Core.DeletedAt != nil {
				return corestore.ErrNotFound
			}
			if !hubrecords.SameWorkItemProjectPlan(plan.Project, project) {
				return corestore.ErrSerializationFailure
			}
			if duplicateSearch != nil && duplicateSearch.Available {
				if err := lockWorkItemDuplicateSource(ctx, w, scope.Project, duplicateSearch.Sequence); err != nil {
					if errors.Is(err, corestore.ErrRevisionConflict) {
						if machineSearch != nil && len(machineSearch.Matches) > 0 {
							return hubapi.NativeConflict(0)
						}
						sourceMoved = true
						return corestore.ErrSerializationFailure
					}
					return err
				}
			} else if confirmation != nil {
				if err := lockWorkItemDuplicateSource(ctx, w, scope.Project, confirmation.challenge.SourceSequence); err != nil {
					if errors.Is(err, searchstore.ErrIndexPending) || errors.Is(err, corestore.ErrRevisionConflict) {
						return hubapi.NativeConflict(0)
					}
					return err
				}
			}
			if machineSearch != nil && len(machineSearch.Matches) > 0 {
				candidate := machineSearch.Matches[0]
				current, err := w.LockWorkItem(ctx, scope.Project, candidate.WorkItemID)
				if err != nil {
					return err
				}
				if current.Revision != int64(candidate.Revision) || current.Archived {
					return hubapi.NativeConflict(tracker.Revision(current.Revision))
				}
				if scheduledOccurrence && plan.Comment.ID != "" {
					_, err := w.CommentBySource(ctx, scope.Project, current.NativeID, "github:"+plan.Comment.Provenance.ExternalID)
					if err == nil {
						return corestore.ErrSerializationFailure
					}
					if !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
				}
			}
			if options.Feature != "" && !options.Completion {
				now, err := s.database.currentTime()
				if err != nil {
					return err
				}
				if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, options.Feature, now)); err != nil {
					return err
				}
			}
			if plan.RequiresIssueContract() && !plan.Existing {
				currentPolicy, err := w.ProjectPolicy(ctx, scope.Project)
				if errors.Is(err, corestore.ErrNotFound) {
					if policyFound {
						return corestore.ErrSerializationFailure
					}
				} else if err != nil {
					return err
				} else if !policyFound || !hubrecords.SameNativeClaimProjectPolicy(policyRevision, currentPolicy) {
					return corestore.ErrSerializationFailure
				}
			}
			if confirmation != nil {
				if err := consumeWorkItemDuplicateConfirmation(ctx, w, scope, confirmation, s.config.Clock()); err != nil {
					return err
				}
			}
			if plan.LinkedSource != nil && plan.Current.ID == 0 {
				hasTriage := slices.ContainsFunc(project.States, func(state tracker.NativeState) bool { return state.Name == "Triage" })
				if !hasTriage && project.Core.WorkflowSource != "" {
					return hubapi.PolicyMismatch("The configured workflow must include Triage for linked issues")
				}
				if !hasTriage || project.Settings.Intake != "manual" {
					if project.Core.WorkflowSource == "" {
						project.States = hubrecords.NativeTriageStates(project.States)
					}
					project.Settings.Intake = "manual"
					if _, err := w.UpdateProjectRecord(ctx, project); err != nil {
						return err
					}
				}
			}
			previous, err := w.Receipt(ctx, scope.Credential.ID, options.OperationID, command.IdempotencyKey)
			if err == nil {
				if previous.ResponseExpired || previous.ResponseReplayUntilNs > 0 && s.config.Clock().UnixNano() >= previous.ResponseReplayUntilNs {
					return receiptstore.ErrExpired
				}
				if previous.RequestHash != hash {
					return hubapi.NativeConflict(0)
				}
				return corestore.ErrSerializationFailure
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if challenge != nil {
				writer, ok := w.(corestore.DuplicateChallengeWriter)
				if !ok {
					return hubapi.NativeWorkItemStorageUnavailable()
				}
				if err := writer.CreateDuplicateChallenge(ctx, *challenge); err != nil {
					return err
				}
				_, err := w.PutReceipt(ctx, receipt)
				return err
			}
			if plan.Existing {
				switch request := input.(type) {
				case tracker.UpdateIssue:
					current, err := w.WorkItem(ctx, scope.Project, plan.Issue.WorkItemID)
					if err != nil {
						return err
					}
					if current.Revision != int64(request.ExpectedRevision) {
						return hubapi.NativeConflict(tracker.Revision(current.Revision))
					}
				case tracker.UpdateComment:
					current, err := w.Comment(ctx, scope.Project, plan.Comment.WorkItemID, plan.Comment.ID)
					if err != nil {
						return err
					}
					if current.Revision != int64(request.ExpectedRevision) {
						return hubapi.NativeConflict(tracker.Revision(current.Revision))
					}
				case tracker.DependencyMutation:
					current, err := w.WorkItem(ctx, scope.Project, plan.Issue.WorkItemID)
					if err != nil {
						return err
					}
					if current.Revision != int64(request.ExpectedRevision) {
						return hubapi.NativeConflict(tracker.Revision(current.Revision))
					}
				case workItemArchive:
					current, err := w.WorkItem(ctx, scope.Project, plan.Issue.WorkItemID)
					if err != nil {
						return err
					}
					if current.Revision != int64(request.ExpectedRevision) {
						return hubapi.NativeConflict(tracker.Revision(current.Revision))
					}
				}
			}
			if request, defect := input.(tracker.CreateIssue); defect && scope.Credential.Scope == hubapi.APIScopeWorker {
				if nativeProgressActor(scope) && scope.ProgressOrigin != nil {
					if err := scope.ProgressOrigin.AuthorizeProgressDefect(ctx, w, s, scope, command.LeaseID, command.FencingToken); err != nil {
						return err
					}
				} else {
					origin, machine := issueorigin.Parse(request.Body)
					if !machine || origin.Kind != "worker" || request.State != "Backlog" {
						return hubscope.RunnerUnauthorized()
					}
					lease, err := w.Lease(ctx, string(command.LeaseID))
					if err != nil {
						return err
					}
					item, err := w.WorkItemByID(ctx, scope.Project, lease.IssueID)
					if err != nil {
						return err
					}
					if err := s.authorizeAttachmentCommentLease(ctx, w, scope, item, command); err != nil {
						return err
					}
				}
			}
			if _, edit := input.(tracker.UpdateIssue); edit && scope.Credential.Scope == hubapi.APIScopeWorker {
				authorityItem := plan.Current
				origin, machine := issueorigin.Parse(plan.Issue.Body)
				if command.LeaseID != "" && machine && origin.Kind == "worker" && plan.Issue.State == "Backlog" && hubapi.StringValue(plan.Current.ActorPrincipalID) == scope.Actor().PrincipalID {
					lease, err := w.Lease(ctx, string(command.LeaseID))
					if err != nil {
						return err
					}
					authorityItem, err = w.WorkItemByID(ctx, scope.Project, lease.IssueID)
					if err != nil {
						return err
					}
				}
				if err := s.authorizeWorkerItemFence(ctx, w, scope, authorityItem, command); err != nil {
					return err
				}
			}
			if _, editComment := input.(tracker.UpdateComment); editComment && options.RequireLease && scope.Credential.Scope == hubapi.APIScopeWorker {
				if err := s.authorizeWorkerItemFence(ctx, w, scope, plan.Current, command); err != nil {
					return err
				}
			}
			if !plan.Existing {
				if _, comment := input.(tracker.CreateComment); comment && scope.Credential.Scope == hubapi.APIScopeWorker {
					if err := s.authorizeWorkerItemFence(ctx, w, scope, plan.Current, command); err != nil {
						return err
					}
				}
				if plan.Comment.ID != "" && (scope.Credential.Scope == hubapi.APIScopeWorker || len(attachmentReferences.Current) > 0) {
					if _, err := s.attachmentQuotaLimit(ctx, w); err != nil {
						return err
					}
				}
				if err := s.applyWorkItemGrowth(ctx, w, scope.Organization, "work-item/"+scope.Credential.ID+"/"+options.OperationID+"/"+command.IdempotencyKey, growth, options.Completion); err != nil {
					return err
				}
				if plan.Relationship != nil {
					if err := w.SetRelationship(ctx, *plan.Relationship, plan.RelationshipOperate, plan.Issue.UpdatedAt); err != nil {
						return err
					}
				}
				if plan.VerificationComment != nil {
					if err := s.writeVerificationComment(ctx, w, scope, plan.VerificationComment, plan.VerificationAttachments); err != nil {
						return err
					}
				}
				if err := hubrecords.WriteWorkItemRecord(ctx, w, plan, fields); err != nil {
					return err
				}
				if request, ok := input.(tracker.VerifyFix); ok && request.Result == tracker.VerifyResultNotFixed {
					if s.config.ConversationDispatch == nil {
						return conversationDispatchUnavailable()
					}
					if err := s.config.ConversationDispatch.RequestDispatch(ctx, w, scope.Project, string(plan.Issue.WorkItemID), leasestore.Fence{}, s.config.Clock()); err != nil {
						return err
					}
				}
				if plan.Comment.ID == "" && plan.Current.ID != 0 && (slices.Contains(plan.Fields, "title") || slices.Contains(plan.Fields, "body")) {
					source, err := w.LinkedSource(ctx, scope.Project, plan.Issue.WorkItemID)
					if err == nil {
						if source.SnapshotSHA256 != "" {
							source.ExpectedSnapshotSHA256 = new(source.SnapshotSHA256)
						}
						source.TitleOwned = source.TitleOwned || slices.Contains(plan.Fields, "title")
						source.BodyOwned = source.BodyOwned || slices.Contains(plan.Fields, "body")
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
				if plan.Comment.ID == "" {
					if err := w.BindDoctorProposalIdentities(ctx, postgres.DoctorProposalIdentityBinding{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: version.WorkItemID, Revision: version.Revision, VersionSHA256: version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(plan.Issue.Body)}); err != nil {
						return err
					}
				}
				if _, err := w.AppendEvent(ctx, event); err != nil {
					return err
				}
				for _, imported := range plan.LinkedComments {
					if _, err := w.CreateComment(ctx, imported.Row); err != nil {
						return err
					}
					if _, err := w.CreateCollaborationVersion(ctx, imported.Version); err != nil {
						return err
					}
					if _, err := w.AppendEvent(ctx, imported.Event); err != nil {
						return err
					}
				}
				if machinePriority != nil {
					if err := machinePriority.write(ctx, w); err != nil {
						return err
					}
				}
				for _, duplicate := range duplicateLinks {
					relation := corestore.Relationship{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.Issue.WorkItemID, RelatedProjectID: scope.Project, RelatedWorkItemID: duplicate.candidate.WorkItemID, Type: "related"}
					if err := w.SetRelationship(ctx, relation, "add", duplicate.issue.UpdatedAt); err != nil {
						return err
					}
					current, err := w.WorkItem(ctx, scope.Project, plan.Issue.WorkItemID)
					if err != nil {
						return err
					}
					update := hubrecords.WorkItemRecordPlan{Project: plan.Project, Current: current, Issue: duplicate.issue, Fields: []string{"related"}}
					if err := hubrecords.WriteWorkItemRecord(ctx, w, update, fields); err != nil {
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
				attachmentItem := plan.Comment.WorkItemID
				if plan.Comment.ID == "" {
					attachmentItem = plan.Issue.WorkItemID
				}
				if err := w.ReplaceAttachmentReferences(ctx, scope.Project, attachmentItem, plan.Comment.ID, attachmentReferences.Previous, attachmentReferences.Current); err != nil {
					return err
				}
			}
			if link != nil {
				if err := link.Write(ctx, w, plan); err != nil {
					return err
				}
			}
			if err := s.recordEntryAccessKeyAuditWith(ctx, w, hubscope.EntryKeyAuditFrom(ctx), http.StatusOK); err != nil {
				return err
			}
			if mutationAudit != nil {
				if _, err := w.RecordAccessAudit(ctx, *mutationAudit); err != nil {
					return err
				}
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		}
		if prepared != nil {
			*prepared = preparedWorkItemRecordMutation{plan: plan, browserPath: browserPath, write: write}
			return raw, nil
		}
		authority, err := s.beginProjectRecordAuthority(ctx, scope)
		if err != nil {
			return nil, err
		}
		err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, write)
		err = errors.Join(err, hubrecords.ReleaseWorkItemAuthority(authority))
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) || duplicateSearch != nil && duplicateSearch.Available && errors.Is(err, searchstore.ErrIndexPending) {
			continue
		}
		if err != nil {
			if confirmation != nil {
				replayed, found, replayErr := s.workItemRecordReceipt(ctx, scope, options.OperationID, command.IdempotencyKey, hash)
				if replayErr != nil {
					return nil, replayErr
				}
				if found {
					return s.readWorkItemRecordReceipt(ctx, scope, replayed, input, options.Item)
				}
			}
			return nil, hubapi.ProjectRecordError(err)
		}
		s.signalNativeDispatch(scope)
		return raw, nil
	}
}

func (s *Service) prepareLinkedScheduledDuplicate(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.WorkItemRecordPlan, request tracker.CreateIssue) (hubrecords.WorkItemRecordPlan, *workduplicates.Search, bool, error) {
	verified := scope.ScheduledRepair
	if scope.IntegrationRepository == "" || verified == nil || verified.ImportURL != request.GitHubIssueURL || verified.ImportReason != "" || verified.PolicyID == "" || request.Title != "" || request.Body != "" || request.Priority != nil {
		return plan, nil, false, nil
	}
	if !slices.ContainsFunc(plan.Project.States, func(state tracker.NativeState) bool {
		return state.Name == "Todo" && state.Dispatchable && !state.Terminal && !state.OperatorOnly
	}) {
		return plan, nil, false, nil
	}
	preview := tracker.NativeIssue{Body: verified.ImportBody, IssueContract: &issuecontract.State{}}
	if err := s.applyScheduledRepairRecord(ctx, scope, &preview); err != nil {
		return plan, nil, false, err
	}
	if len(preview.IssueContract.ConfirmedSections) == 0 {
		return plan, nil, false, nil
	}
	search, err := s.prepareWorkItemDuplicates(ctx, scope, tracker.CreateIssue{Body: verified.ImportBody}, true)
	if err != nil || len(search.Matches) == 0 {
		return plan, search, false, err
	}
	candidate := search.Matches[0]
	matched, err := s.loadWorkItemPlan(ctx, scope, string(candidate.WorkItemID), "", tracker.CreateComment{})
	if err != nil {
		return plan, search, false, err
	}
	if !hubrecords.SameWorkItemProjectPlan(plan.Project, matched.Project) {
		return plan, search, false, hubapi.NativeConflict(0)
	}
	if matched.Issue.Revision != candidate.Revision || matched.Issue.Archived {
		return plan, search, false, hubapi.NativeConflict(matched.Issue.Revision)
	}
	if matched.Issue.Terminal {
		matched.Existing = true
		return matched, search, true, nil
	}
	externalID := "scheduled:" + verified.Evidence.OccurrenceKey
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.CommentBySource(ctx, scope.Project, matched.Current.NativeID, "github:"+externalID)
		return err
	})
	if err == nil {
		matched.Existing = true
		return matched, search, true, nil
	}
	if !errors.Is(err, corestore.ErrNotFound) {
		return plan, search, false, hubapi.ProjectRecordError(err)
	}
	now := s.config.Clock().UTC().Truncate(time.Microsecond)
	body := fmt.Sprintf("Verified CI report: %s\nRun: %s\nJob: %s\nTested commit: %s\n", verified.ImportURL, verified.Evidence.RunURL, verified.Evidence.JobURL, verified.Evidence.HeadSHA) + verified.Evidence.Stamp()
	comment := tracker.CreateComment{Body: body, Provenance: &tracker.Provenance{Provider: "github", ExternalID: externalID, AuthorID: "github-actions[bot]", CreatedAt: now, UpdatedAt: now, ObservedAt: now}}
	matched, err = s.planWorkItemRecord(ctx, scope, matched, comment)
	return matched, search, true, err
}

func (s *Service) mutateCompatibilityDependency(ctx context.Context, scope hubscope.NativeScope, source corestore.WorkItem, request operatortool.WorkArguments, command tracker.Mutation) (json.RawMessage, error) {
	if strings.TrimSpace(command.IdempotencyKey) == "" || len(command.IdempotencyKey) > 128 {
		return nil, hubapi.NativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	scope.Action = operatortool.SetDependency
	operation := operatortool.SetDependency + " " + string(scope.Organization) + "/" + string(scope.Project) + "/" + request.Identifier
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	previous, found, err := s.workItemRecordReceipt(ctx, scope, operation, command.IdempotencyKey, hash)
	if err != nil {
		return nil, err
	}
	if found {
		return content.Receipt(ctx, previous)
	}
	if request.RelationshipType != "" && request.RelationshipType != "dependency" {
		return nil, hubapi.NativePublicInvalid("Related links require native issue authority")
	}
	if request.Operation != "add" && request.Operation != "remove" {
		return nil, hubapi.NativePublicInvalid("Dependency operation must be add or remove")
	}
	related := tracker.NativeWorkItemID(request.Related)
	if related == source.NativeID {
		return nil, hubapi.NativePublicInvalid("dependency must name another item")
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItem(ctx, scope.Project, related)
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		return nil
	}); err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}

	raw, err := json.Marshal(mutationResponse{WorkItemID: tracker.WorkItemID(source.ID), Kind: operatortool.SetDependency, Status: "applied"})
	if err != nil {
		return nil, err
	}
	now := s.config.Clock().UTC()
	eventRaw, err := json.Marshal(tracker.CollaborationEvent{
		OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "work_item", AggregateID: source.NativeID,
		ID: hubapi.NewID("evt"), Type: "dependency.changed", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: now,
		Data: tracker.CollaborationData{RelatedWorkItemID: related, Operation: request.Operation},
	})
	if err != nil {
		return nil, err
	}
	event, err := content.PrepareEvent(ctx, scope.Organization, eventRaw)
	if err != nil {
		return nil, err
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{
		OrganizationID: scope.Organization, ActorID: scope.Credential.ID, Operation: operation,
		CommandKey: command.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano(),
	}, raw)
	if err != nil {
		return nil, err
	}
	replay := false
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockRelationshipGraph(ctx); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
			return err
		}
		project, err := w.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil || project.Profile != "github_compatible" {
			return corestore.ErrNotFound
		}
		current, err := w.WorkItem(ctx, scope.Project, source.NativeID)
		if err != nil {
			return err
		}
		if current.ID != source.ID || !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(current.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		previous, err = w.Receipt(ctx, scope.Credential.ID, operation, command.IdempotencyKey)
		if err == nil {
			if previous.ResponseExpired || previous.ResponseReplayUntilNs > 0 && s.config.Clock().UnixNano() >= previous.ResponseReplayUntilNs {
				return receiptstore.ErrExpired
			}
			if previous.RequestHash != hash {
				return hubapi.NativeConflict(0)
			}
			replay = true
			return s.recordEntryAccessKeyAuditWith(ctx, w, hubscope.EntryKeyAuditFrom(ctx), http.StatusOK)
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		target, err := w.WorkItem(ctx, scope.Project, related)
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(target.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, "collaboration", now)); err != nil {
			return err
		}
		relationship := corestore.Relationship{
			OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: source.NativeID,
			RelatedProjectID: scope.Project, RelatedWorkItemID: target.NativeID, Type: "dependency", CompatibilityEdge: true,
		}
		if err := w.SetRelationship(ctx, relationship, request.Operation, now); err != nil {
			return err
		}
		if _, err := w.AppendEvent(ctx, event); err != nil {
			return err
		}
		if err := s.recordEntryAccessKeyAuditWith(ctx, w, hubscope.EntryKeyAuditFrom(ctx), http.StatusOK); err != nil {
			return err
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	if replay {
		return content.Receipt(ctx, previous)
	}
	s.signalNativeDispatch(scope)
	return raw, nil
}

func (s *Service) workItemRecordReceipt(ctx context.Context, scope hubscope.NativeScope, operation, key, hash string) (eventstore.Receipt, bool, error) {
	var receipt eventstore.Receipt
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		receipt, err = r.ReplayReceipt(ctx, scope.Credential.ID, operation, key)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return receipt, false, nil
	}
	if err != nil {
		return receipt, false, err
	}
	if s.config.AccessRepository != nil {
		err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
				return err
			}
			if scope.Action == operatortool.AddComment && scope.ActionItem != "" && scope.Credential.HostedRole == "reporter" {
				if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
					return err
				}
				current, err := w.LockWorkItem(ctx, scope.Project, tracker.NativeWorkItemID(scope.ActionItem))
				if err != nil {
					return hubapi.ProjectRecordError(err)
				}
				if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(current.ActorPrincipalID)}) {
					return hubapi.NativeNotFound()
				}
			}
			var err error
			receipt, err = w.ReplayReceipt(ctx, scope.Credential.ID, operation, key)
			if err != nil {
				return err
			}
			if receipt.RequestHash == hash && !receipt.ResponseExpired && (receipt.ResponseReplayUntilNs == 0 || s.config.Clock().UnixNano() < receipt.ResponseReplayUntilNs) {
				return s.recordEntryAccessKeyAuditWith(ctx, w, hubscope.EntryKeyAuditFrom(ctx), http.StatusOK)
			}
			return nil
		})
		if err != nil {
			return receipt, false, err
		}
	}
	if receipt.ResponseExpired || receipt.ResponseReplayUntilNs > 0 && s.config.Clock().UnixNano() >= receipt.ResponseReplayUntilNs {
		return receipt, false, receiptstore.ErrExpired
	}
	if receipt.RequestHash != hash {
		return receipt, false, hubapi.NativeConflict(0)
	}
	return receipt, true, nil
}

func (s *Service) authorizeWorkItemRecordScope(ctx context.Context, reader accessstore.Reader, scope hubscope.NativeScope) error {
	if err := s.authorizeAccessScope(ctx, reader, scope, true); err != nil {
		return err
	}
	if scope.Credential.Hosted == nil {
		return nil
	}
	member, err := reader.AuthorizeAccess(ctx, s.accessAuthority(scope, true), s.config.Clock())
	if err != nil {
		return err
	}
	if member.UserID != "" && hubapi.LesserHostedRole(member.Role, scope.Credential.HostedRole) != scope.Credential.HostedRole {
		return auth.ErrHostedIdentity
	}
	return nil
}

func workItemMutationGrowth(plan hubrecords.WorkItemRecordPlan, valueRaw, eventRaw []byte, duplicates []workItemDuplicateLink) (workItemGrowth, error) {
	growth := workItemGrowth{historyRecords: 2, events: 1}
	eventBytes, err := collaborationEventChargeBytes(eventRaw)
	if err != nil {
		return growth, err
	}
	growth.collaborationBytes = int64(len(valueRaw)) + eventBytes
	if !plan.Create {
		growth.collaborationBytes -= plan.PreviousFieldBytes
	}
	if plan.Comment.ID != "" {
		growth.collaborationBytes += int64(len(plan.Comment.Body))
	} else {
		fieldBytes, err := issueCollaborationFieldBytes(plan.Issue)
		if err != nil {
			return growth, err
		}
		growth.collaborationBytes += fieldBytes
		if openWorkItemState(plan.Project, plan.Issue.Archived, plan.Issue.State) {
			growth.openIssues++
		}
		if !plan.Create && openWorkItemState(plan.Project, plan.Current.Archived, hubapi.StringValue(plan.Current.WorkflowStateName)) {
			growth.openIssues--
		}
	}
	for _, imported := range plan.LinkedComments {
		growth.collaborationBytes += imported.Bytes
		growth.historyRecords += 2
		growth.events++
	}
	for _, duplicate := range duplicates {
		growth.collaborationBytes += duplicate.bytes
		growth.historyRecords += 2
		growth.events++
	}
	return growth, nil
}
