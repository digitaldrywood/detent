package hubrecords

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AttachmentEvidencePublication struct {
	Plan    WorkItemRecordPlan              `json:"-"`
	Fields  WorkItemRecordFields            `json:"-"`
	Version eventstore.CollaborationVersion `json:"-"`
	Event   eventstore.Event                `json:"-"`
	Search  searchstore.Document            `json:"-"`
	Bytes   int64                           `json:"-"`
}

type WorkItemAttachmentReferences struct {
	Previous []string `json:"-"`
	Current  []string `json:"-"`
}

func NativeContractIssue(issue tracker.NativeIssue) connector.Issue {
	return connector.Issue{Title: issue.Title, Description: issue.Body, IssueContract: issue.IssueContract}
}

func ScheduledRepairOriginMatches(source string, evidence gate.ScheduledEvidence) bool {
	runURL := "https://github.com/" + evidence.Repository + "/actions/runs/" + evidence.RunID
	return source == runURL || source == runURL+"/attempts/"+evidence.RunAttempt
}

type LinkedIssueCommentPlan struct {
	Comment tracker.NativeComment           `json:"-"`
	Row     corestore.Comment               `json:"-"`
	Version eventstore.CollaborationVersion `json:"-"`
	Event   eventstore.Event                `json:"-"`
	Bytes   int64                           `json:"-"`
}

func NativeTriageStates(states []tracker.NativeState) []tracker.NativeState {
	return projectstore.TriageStates(states)
}

func TenantProjectStateTerminal(project projectstore.Record, name string) bool {
	for _, state := range project.States {
		if state.Name == name {
			return state.Terminal
		}
	}
	return false
}

func IssueBrowserPath(organization, project string, number int) string {
	return "/" + organization + "/" + project + "/issues/" + strconv.Itoa(number)
}

type WorkItemRecordFields struct {
	Body     string
	Contract *string
	Report   *string
}

type WorkItemRecordPlan struct {
	Project                 projectstore.Record            `json:"-"`
	Current                 corestore.WorkItem             `json:"-"`
	Related                 corestore.WorkItem             `json:"-"`
	Relationship            *corestore.Relationship        `json:"-"`
	RelationshipType        string                         `json:"-"`
	RelationshipOperate     string                         `json:"-"`
	RelationshipPresent     bool                           `json:"-"`
	Issue                   tracker.NativeIssue            `json:"-"`
	PreviousContent         string                         `json:"-"`
	PreviousFieldBytes      int64                          `json:"-"`
	LinkedSource            *eventstore.LinkedSource       `json:"-"`
	LinkedComments          []LinkedIssueCommentPlan       `json:"-"`
	Comment                 tracker.NativeComment          `json:"-"`
	VerificationComment     *AttachmentEvidencePublication `json:"-"`
	VerificationAttachments []string                       `json:"-"`
	CommentRow              corestore.Comment              `json:"-"`
	EventType               string                         `json:"-"`
	Fields                  []string                       `json:"-"`
	Create                  bool                           `json:"-"`
	Existing                bool                           `json:"-"`
}

func SameWorkItemProjectPlan(planned, current projectstore.Record) bool {
	if planned.Core.ContentSettingsRevision != current.Core.ContentSettingsRevision || planned.Settings.Revision != current.Settings.Revision || planned.Core.Profile != current.Core.Profile || planned.Core.RequireDependencies != current.Core.RequireDependencies || planned.Core.WorkflowSource != current.Core.WorkflowSource || planned.Core.WorkflowSourceRevision != current.Core.WorkflowSourceRevision || planned.Settings.DuplicateChallenge != current.Settings.DuplicateChallenge {
		return false
	}
	return slices.EqualFunc(planned.States, current.States, func(a, b tracker.NativeState) bool {
		return a.OperatorOnly == b.OperatorOnly && a.Name == b.Name && a.Terminal == b.Terminal && a.Dispatchable == b.Dispatchable && slices.Equal(a.Transitions, b.Transitions)
	})
}

func (p WorkItemRecordPlan) RequiresIssueContract() bool {
	return p.Comment.ID == "" && (p.Create || slices.Contains(p.Fields, "body"))
}

func ReleaseWorkItemAuthority(authority interface{ Rollback() error }) error {
	var releaseErr error
	if err := authority.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		releaseErr = errors.Join(releaseErr, err)
	}
	return releaseErr
}

func (p WorkItemRecordPlan) Value() any {
	if p.Comment.ID != "" {
		return p.Comment
	}
	return p.Issue
}

func RelationshipOmittedIssue(issue tracker.NativeIssue) tracker.NativeIssue {
	issue.Dependencies = []tracker.NativeWorkItemID{}
	issue.Blockers, issue.Dependents, issue.Related = []tracker.NativeDependency{}, []tracker.NativeDependency{}, nil
	issue.DependencyReady, issue.BlockedReason, issue.UnblocksReason = false, tracker.DependencyReason{}, tracker.DependencyReason{}
	issue.OmittedFields = slices.Clone(issue.OmittedFields)
	for _, field := range []string{"dependencies", "blockers", "dependents", "related", "dependency_ready", "blocked_reason", "unblocks_reason"} {
		if !slices.Contains(issue.OmittedFields, field) {
			issue.OmittedFields = append(issue.OmittedFields, field)
		}
	}
	return issue
}

func WriteWorkItemRecord(ctx context.Context, w tenantstore.Writer, plan WorkItemRecordPlan, fields WorkItemRecordFields) error {
	if plan.Comment.ID != "" {
		current, err := w.LockWorkItem(ctx, plan.Current.ProjectID, plan.Current.NativeID)
		if err != nil {
			return err
		}
		if current.EventSequence != plan.Current.EventSequence {
			return corestore.ErrSerializationFailure
		}
		if !plan.Create {
			currentComment, err := w.Comment(ctx, plan.Current.ProjectID, plan.Current.NativeID, plan.Comment.ID)
			if err != nil {
				return err
			}
			if currentComment.Revision != int64(plan.Comment.Revision)-1 {
				return hubapi.NativeConflict(tracker.Revision(currentComment.Revision))
			}
		}
		row := plan.CommentRow
		c := plan.Comment
		if !c.UpdatedAt.IsZero() {
			row.SourceRevision = int64(c.Revision)
			row.SourceScheduled = strings.Contains(c.Body, "detent-checks")
			row.SourceObservedAt = new(c.UpdatedAt.UTC().Truncate(time.Microsecond))
		}
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
		if plan.Create {
			_, err = w.CreateComment(ctx, row)
		} else {
			_, err = w.UpdateComment(ctx, row)
		}
		return err
	}
	issue := plan.Issue
	row := plan.Current
	if !issue.UpdatedAt.IsZero() {
		row.SourceRevision = int64(issue.Revision)
		row.SourceScheduled = strings.Contains(issue.Body, "detent-checks")
		row.SourceObservedAt = new(issue.UpdatedAt.UTC().Truncate(time.Microsecond))
	}
	var previousFacts claimstore.WorkItemFacts
	previousWasDispatchable := !plan.Create && DispatchableProjectState(plan.Project, hubapi.StringValue(plan.Current.WorkflowStateName))
	if previousWasDispatchable {
		var err error
		previousFacts, err = w.WorkItemFacts(ctx, plan.Current.ProjectID, plan.Current.NativeID)
		if err != nil {
			return err
		}
	}
	if plan.Create {
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
	if plan.Create {
		if _, err := w.CreateWorkItem(ctx, row); err != nil {
			return err
		}
		if err := w.SetWorkItemLabels(ctx, row.ProjectID, row.NativeID, issue.Labels); err != nil {
			return err
		}
		if err := WriteClaimFactsAndQueue(ctx, w, plan, nil); err != nil {
			return err
		}
		if plan.LinkedSource != nil {
			source := *plan.LinkedSource
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
		return hubapi.NativeConflict(tracker.Revision(current.Revision))
	}
	row.EventSequence = current.EventSequence
	if _, err := w.UpdateWorkItem(ctx, row); err != nil {
		return err
	}
	if slices.Contains(plan.Fields, "labels") {
		if err := w.SetWorkItemLabels(ctx, row.ProjectID, row.NativeID, issue.Labels); err != nil {
			return err
		}
	}
	var prior *claimstore.WorkItemFacts
	if previousWasDispatchable {
		prior = &previousFacts
	}
	return WriteClaimFactsAndQueue(ctx, w, plan, prior)
}

func DispatchableProjectState(project projectstore.Record, state string) bool {
	for _, candidate := range project.States {
		if candidate.Name == state {
			return candidate.Dispatchable && !candidate.Terminal && !candidate.OperatorOnly
		}
	}
	return false
}

func WriteClaimFactsAndQueue(ctx context.Context, w tenantstore.Writer, plan WorkItemRecordPlan, previous *claimstore.WorkItemFacts) error {
	issue := plan.Issue
	author := issue.Actor.PrincipalID
	if issue.Provenance != nil {
		author = issue.Provenance.AuthorID
	}
	var dispatchWaitSince *time.Time
	if DispatchableProjectState(plan.Project, issue.State) {
		if previous != nil && DispatchableProjectState(plan.Project, hubapi.StringValue(plan.Current.WorkflowStateName)) {
			dispatchWaitSince = previous.DispatchWaitSince
		} else {
			entered := issue.UpdatedAt
			if plan.Create {
				entered = issue.CreatedAt
			}
			dispatchWaitSince = &entered
		}
	}
	facts := claimstore.WorkItemFacts{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, Author: author, Repository: plan.Project.Settings.Repository, Assignees: issue.Assignees, DispatchWaitSince: dispatchWaitSince}
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
	if issue.Profile != "github_compatible" {
		entry.Priority = nil
		if issue.Priority != nil {
			priority := int64(*issue.Priority)
			entry.Priority = &priority
		}
	}
	return w.PutQueueEntry(ctx, entry)
}
