package coordinator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
)

const (
	coordinatorAttentionDefault = 20
	coordinatorAttentionMax     = 50
	coordinatorAttentionScan    = 500
	coordinatorIssueBodyRunes   = 4000
	coordinatorCommentRunes     = 1000
	coordinatorCommentCount     = 5
)

// readableProjects returns the projects in the conversation's organization
// the owner can read right now, by the rules the conversation API applies to
// the owner's own requests: hosted grants of an active member for a hosted
// owner; otherwise the owner token's grants, or every project for an
// instance administrator token. A revoked or expired token reads nothing.
// The conversation's own project is not assumed: a grant withdrawn after the
// conversation started is honored on the next tool call.
func (d WorkToolDeps) ReadableProjects(ctx context.Context, record conversations.ConversationRecord) ([]tracker.ProjectID, error) {
	actorCtx, err := d.ActionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	credential, err := d.Operator(actorCtx)
	if err != nil {
		return nil, err
	}
	projects := []tracker.ProjectID{}
	err = d.ReadTenant(actorCtx, record.OrganizationID, func(r tenantstore.Reader) error {
		if err := d.Authorize(actorCtx, r, hubscope.NativeScope{Organization: record.OrganizationID, Project: record.ProjectID, Credential: credential}, false); err != nil {
			return err
		}
		var after tracker.ProjectID
		for {
			var ids []tracker.ProjectID
			if credential.Hosted != nil {
				grants, err := r.AccessProjectGrants(ctx, credential.Hosted.Subject, after, 100)
				if err != nil {
					return err
				}
				for _, grant := range grants {
					ids = append(ids, grant.ProjectID)
				}
			} else if credential.Scope == hubapi.APIScopeAdmin && !credential.NativeOnly {
				rows, err := r.Projects(ctx, after, 100)
				if err != nil {
					return err
				}
				for _, project := range rows {
					if project.DeletedAt == nil {
						ids = append(ids, project.ID)
					}
				}
				if len(rows) > 0 {
					after = rows[len(rows)-1].ID
				}
				allowed, err := d.AllowedProjects(actorCtx, r, hubscope.NativeScope{Organization: record.OrganizationID, Credential: credential}, ids)
				if err != nil {
					return err
				}
				projects = append(projects, allowed...)
				if len(rows) < 100 {
					return nil
				}
				continue
			} else {
				grants, err := r.AccessTokenGrants(ctx, credential.ID, after, 100)
				if err != nil {
					return err
				}
				for _, grant := range grants {
					ids = append(ids, grant.ProjectID)
				}
			}
			allowed, err := d.AllowedProjects(actorCtx, r, hubscope.NativeScope{Organization: record.OrganizationID, Credential: credential}, ids)
			if err != nil {
				return err
			}
			projects = append(projects, allowed...)
			if len(ids) < 100 {
				return nil
			}
			after = ids[len(ids)-1]
		}
	})
	return projects, err
}

type coordinatorAttentionItem struct {
	WorkItemID string            `json:"work_item_id"`
	Number     int64             `json:"number"`
	Title      string            `json:"title"`
	State      string            `json:"state"`
	ProjectID  tracker.ProjectID `json:"project_id"`
	Reason     string            `json:"reason"`
	UpdatedAt  string            `json:"updated_at"`
	URL        string            `json:"url"`
}

type coordinatorAttention struct {
	Scope           string                     `json:"scope"`
	Projects        []tracker.ProjectID        `json:"projects"`
	Blocked         []coordinatorAttentionItem `json:"blocked"`
	WaitingForInput []coordinatorAttentionItem `json:"waiting_for_input"`
	Running         []coordinatorAttentionItem `json:"running"`
	Review          []coordinatorAttentionItem `json:"review"`
	Unavailable     []coordinatorAttentionItem `json:"unavailable"`
	Truncated       bool                       `json:"truncated"`
}

// listAttention groups open issues by what they wait for. Classification
// order: a pending question, a live lease, no workflow state (unavailable),
// a non-terminal dependency or blocked state, a review state.
func (d WorkToolDeps) ListAttention(ctx context.Context, record conversations.ConversationRecord, readable []tracker.ProjectID, scope string, limit int) (any, error) {
	if scope == "" {
		scope = "project"
	}
	if scope != "project" && scope != "all_projects" {
		return nil, fmt.Errorf("%w: scope must be project or all_projects", ErrToolArguments)
	}
	if limit <= 0 {
		limit = coordinatorAttentionDefault
	}
	if limit > coordinatorAttentionMax {
		limit = coordinatorAttentionMax
	}
	projects := []tracker.ProjectID{record.ProjectID}
	if scope == "all_projects" {
		projects = readable
	}
	result := coordinatorAttention{Scope: scope, Projects: projects, Blocked: []coordinatorAttentionItem{}, WaitingForInput: []coordinatorAttentionItem{}, Running: []coordinatorAttentionItem{}, Review: []coordinatorAttentionItem{}, Unavailable: []coordinatorAttentionItem{}}
	_, viewer, err := d.WorkReadContext(ctx, record)
	if err != nil {
		return nil, err
	}
	visibility := corestore.RelationshipVisibility{Projects: slices.Sorted(slices.Values(readable)), Reporter: viewer.ReporterRestriction()}
	err = d.ReadTenant(ctx, record.OrganizationID, func(r tenantstore.Reader) error {
		owner, ok := r.(conversationstore.AttentionReader)
		if !ok {
			return hubapi.FromFeatureError(conversations.ConversationContentUnavailable())
		}
		rows, err := owner.CoordinatorAttentionItems(ctx, projects, coordinatorAttentionScan+1)
		if err != nil {
			return err
		}
		if len(rows) > coordinatorAttentionScan {
			result.Truncated = true
			rows = rows[:coordinatorAttentionScan]
		}
		org, err := r.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		for _, candidate := range rows {
			row, err := r.WorkItem(ctx, candidate.ProjectID, candidate.WorkItemID)
			if err != nil {
				return err
			}
			project, err := r.Project(ctx, row.ProjectID)
			if err != nil {
				return err
			}
			item := coordinatorAttentionItem{WorkItemID: string(row.NativeID), ProjectID: row.ProjectID, Number: row.Number, Title: conversations.BoundRunes(row.Title, ToolSummaryRunes), State: hubapi.StringValue(row.WorkflowStateName), UpdatedAt: hubapi.FormatHubTime(row.NativeUpdatedAt), URL: hubrecords.IssueBrowserPath(org.Slug, project.Slug, int(row.Number))}
			waiting := false
			linked, err := r.ConversationByWorkItem(ctx, row.ProjectID, string(row.NativeID))
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err == nil {
				questions, err := r.OutstandingQuestions(ctx, linked.ID, "", "", "", 1)
				if err != nil {
					return err
				}
				waiting = len(questions.Items) > 0
			}
			lease, err := r.ActiveLeaseForIssue(ctx, row.ID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			running := err == nil && lease.ReleasedAt == nil && d.Now().Before(lease.ExpiresAt)
			var blockers []string
			relationships, err := r.VisibleRelationships(ctx, row.ProjectID, row.NativeID, corestore.RelationshipCursor{}, corestore.MaxPageSize, visibility)
			if err != nil {
				return err
			}
			if relationships.Next != (corestore.RelationshipCursor{}) {
				result.Truncated = true
			}
			for _, relationship := range relationships.Items {
				if relationship.Type == "dependency" && relationship.Direction == "blocked_by" && !relationship.Terminal {
					blockers = append(blockers, string(relationship.RelatedWorkItemID))
				}
			}
			var group *[]coordinatorAttentionItem
			switch {
			case waiting:
				item.Reason = "a runner question is waiting for an answer"
				group = &result.WaitingForInput
			case running:
				attempt, err := r.LatestAttempt(ctx, row.ID)
				if err != nil {
					return err
				}
				item.Reason = "attempt " + attempt.ID + " holds a live lease"
				group = &result.Running
			case row.WorkflowStateName == nil:
				item.Reason = "no workflow state mapping"
				group = &result.Unavailable
			case len(blockers) > 0:
				item.Reason = "blocked by " + strings.Join(blockers, ", ")
				group = &result.Blocked
			case strings.Contains(strings.ToLower(item.State), "block"):
				item.Reason = "workflow state " + item.State
				group = &result.Blocked
			case strings.Contains(strings.ToLower(item.State), "review"):
				item.Reason = "workflow state " + item.State
				group = &result.Review
			default:
				continue
			}
			if len(*group) >= limit {
				result.Truncated = true
				continue
			}
			*group = append(*group, item)
		}
		return nil
	})
	return result, err
}

type coordinatorAttempt struct {
	AttemptID string `json:"attempt_id"`
	Status    string `json:"status"`
	Outcome   string `json:"outcome"`
	StartedAt string `json:"started_at"`
	UpdatedAt string `json:"updated_at"`
}

type coordinatorComment struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

type IssueExplanation struct {
	WorkItemID     string               `json:"work_item_id"`
	Number         int64                `json:"number"`
	ProjectID      tracker.ProjectID    `json:"project_id"`
	Title          string               `json:"title"`
	Body           string               `json:"body"`
	State          string               `json:"state"`
	Terminal       bool                 `json:"terminal"`
	UpdatedAt      string               `json:"updated_at"`
	LatestAttempt  *coordinatorAttempt  `json:"latest_attempt"`
	Comments       []coordinatorComment `json:"comments"`
	ConversationID *string              `json:"conversation_id"`
	URL            string               `json:"url"`
}

func (d WorkToolDeps) ResolveIssueReference(ctx context.Context, record conversations.ConversationRecord, reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" || strings.HasPrefix(reference, "wi_") {
		return reference, nil
	}
	ctx, scope, err := d.WorkReadContext(ctx, record)
	if err != nil {
		return "", err
	}
	scope.Project = record.ProjectID
	issue, err := d.ResolveIssue(ctx, scope, reference)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, corestore.ErrNotFound) || errors.Is(err, explain.ErrNotFound) || hubapi.IsNativeNotFound(err) {
		return "", fmt.Errorf("no issue %s in this project", "#"+strings.TrimPrefix(reference, "#"))
	}
	if err != nil {
		return "", err
	}
	return string(issue.WorkItemID), nil
}

// explainIssue describes one issue in a project the owner can read.
func (d WorkToolDeps) ExplainIssue(ctx context.Context, record conversations.ConversationRecord, readable []tracker.ProjectID, workItemID string) (any, error) {
	workItemID, err := d.ResolveIssueReference(ctx, record, workItemID)
	if err != nil {
		return nil, err
	}
	if workItemID == "" {
		return nil, fmt.Errorf("%w: work_item_id is required", ErrToolArguments)
	}
	actorCtx, err := d.ActionContext(ctx, record)
	if err != nil {
		return nil, err
	}
	credential, err := d.Operator(actorCtx)
	if err != nil {
		return nil, err
	}
	scope := hubscope.NativeScope{Organization: record.OrganizationID, Credential: credential}
	result := IssueExplanation{Comments: []coordinatorComment{}}
	var attemptID string
	err = d.ReadTenant(actorCtx, record.OrganizationID, func(r tenantstore.Reader) error {
		row, err := r.WorkItemByNativeID(ctx, tracker.NativeWorkItemID(workItemID))
		if err != nil {
			return err
		}
		if !slices.Contains(readable, row.ProjectID) {
			return hubapi.NativeNotFound()
		}
		scope.Project = row.ProjectID
		project, err := r.ProjectRecord(ctx, row.ProjectID)
		if err != nil {
			return err
		}
		org, err := r.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		result.URL = hubrecords.IssueBrowserPath(org.Slug, project.Core.Slug, int(row.Number))
		result.WorkItemID, result.ProjectID, result.Number, result.Title, result.State = string(row.NativeID), row.ProjectID, row.Number, row.Title, hubapi.StringValue(row.WorkflowStateName)
		result.UpdatedAt = hubapi.FormatHubTime(row.NativeUpdatedAt)
		for _, state := range project.States {
			if state.Name == result.State {
				result.Terminal = state.Terminal
			}
		}
		latest, err := r.LatestAttempt(ctx, row.ID)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		attemptID = latest.ID
		linked, err := r.ConversationByWorkItem(ctx, row.ProjectID, string(row.NativeID))
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err == nil {
			result.ConversationID = &linked.ID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	row, version, err := d.ReadWorkItem(actorCtx, scope, workItemID)
	if err != nil {
		return nil, err
	}
	issue, err := d.HydrateIssue(actorCtx, row, version)
	if err != nil {
		return nil, err
	}
	result.Body = conversations.BoundRunes(issue.Body, coordinatorIssueBodyRunes)
	if attemptID != "" {
		attempt, err := d.ReadAttempt(actorCtx, scope, workItemID, attemptID)
		if err != nil {
			return nil, err
		}
		status := attempt.Status
		if status == "running" && (attempt.ClaimReleasedAt != nil || !d.Now().Before(attempt.LeaseExpiresAt)) {
			status = "interrupted"
		}
		result.LatestAttempt = &coordinatorAttempt{AttemptID: attemptID, Status: status, Outcome: attempt.Outcome, StartedAt: hubapi.FormatHubTime(attempt.StartedAt), UpdatedAt: hubapi.FormatHubTime(attempt.UpdatedAt)}
	}
	comments, err := d.ReadComments(actorCtx, scope, workItemID, url.Values{"limit": []string{strconv.Itoa(coordinatorCommentCount)}, "direction": []string{"newest"}})
	if err != nil {
		return nil, err
	}
	for _, comment := range comments.Items {
		result.Comments = append(result.Comments, coordinatorComment{Author: strings.TrimSpace(comment.Actor.Kind + " " + comment.Actor.PrincipalID), Body: conversations.BoundRunes(comment.Body, coordinatorCommentRunes), CreatedAt: hubapi.FormatHubTime(comment.CreatedAt)})
	}
	slices.Reverse(result.Comments)
	return result, nil
}

func AllowedProjects(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, ids []tracker.ProjectID, authorize func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error) ([]tracker.ProjectID, error) {
	allowed := make([]tracker.ProjectID, 0, len(ids))
	for _, id := range ids {
		scope.Project = id
		if err := authorize(ctx, r, scope, false); err != nil {
			if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) || hubapi.IsNativeNotFound(err) {
				continue
			}
			return nil, err
		}
		allowed = append(allowed, id)
	}
	return allowed, nil
}
