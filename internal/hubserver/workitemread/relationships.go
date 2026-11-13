package workitemread

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (rd Reader) HydrateRelationshipFacts(ctx context.Context, scope hubscope.NativeScope, issue *tracker.NativeIssue) error {
	type dependencyEdges struct {
		blocked, unblocks               []string
		privateBlocked, privateUnblocks bool
	}

	issue.Dependencies = []tracker.NativeWorkItemID{}
	issue.Blockers = []tracker.NativeDependency{}
	issue.Dependents = []tracker.NativeDependency{}
	issue.Related = []tracker.NativeDependency{}
	issue.DependencyReady = true
	edges := dependencyEdges{}
	projectVisible := map[tracker.ProjectID]bool{scope.Project: true}

	var relations []corestore.Relationship
	err := rd.deps.Tenants.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := LoadDependencyReasons(ctx, r, scope.Project, []*tracker.NativeIssue{issue}); err != nil {
			return err
		}
		batch, ok := r.(tenantstore.WorkItemRelationshipBatchReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		facts, err := batch.WorkItemRelationshipFacts(ctx, scope.Project, []tracker.NativeWorkItemID{issue.WorkItemID})
		if err != nil {
			return err
		}
		for _, fact := range facts {
			relations = append(relations, fact.Relationship)
			if fact.Relationship.RelatedProjectID != scope.Project {
				projectVisible[fact.Relationship.RelatedProjectID] = fact.RelatedProjectVisible
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	authorizedProjects, err := rd.ProjectAccess(ctx, scope, projectVisible)
	if err != nil {
		return err
	}

	for _, relation := range relations {
		if relation.Type == "dependency" {
			switch relation.Direction {
			case "blocked_by":
				edges.blocked = append(edges.blocked, string(relation.RelatedWorkItemID))
				edges.privateBlocked = edges.privateBlocked || relation.RelatedProjectID != scope.Project
			case "blocks":
				edges.unblocks = append(edges.unblocks, string(relation.RelatedWorkItemID))
				edges.privateUnblocks = edges.privateUnblocks || relation.RelatedProjectID != scope.Project
			}
		}
		visible := authorizedProjects[relation.RelatedProjectID]
		visible = visible && scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(relation.ActorPrincipalID)})
		if !visible {
			if relation.Type == "dependency" {
				switch relation.Direction {
				case "blocked_by":
					edges.privateBlocked = true
				case "blocks":
					edges.privateUnblocks = true
				}
			}
			continue
		}
		link := tracker.NativeDependency{
			Dispatchable: relation.Dispatchable,
			Number:       relation.Number,
			Title:        relation.Title,
			Identifier:   string(relation.RelatedProjectID) + "#" + strconv.Itoa(relation.Number),
			ID:           relation.RelatedWorkItemID,
			ProjectID:    relation.RelatedProjectID,
			State:        relation.State,
			Terminal:     relation.Terminal,
			ClosedAt:     relation.ClosedAt,
			StartedAt:    relation.StartedAt,
		}
		if relation.OrganizationSlug != "" && relation.ProjectSlug != "" && rd.deps.Hosted {
			link.WebURL = rd.IssueResponse(tracker.NativeIssue{WebURL: hubrecords.IssueBrowserPath(relation.OrganizationSlug, relation.ProjectSlug, relation.Number)}).WebURL
		}
		switch relation.Type {
		case "dependency":
			switch relation.Direction {
			case "blocked_by":
				issue.Dependencies = append(issue.Dependencies, relation.RelatedWorkItemID)
				issue.Blockers = append(issue.Blockers, link)
				issue.DependencyReady = issue.DependencyReady && relation.Terminal
			case "blocks":
				issue.Dependents = append(issue.Dependents, link)
			}
		case "related":
			issue.Related = append(issue.Related, link)
		}
	}
	slices.Sort(issue.Dependencies)
	slices.SortFunc(issue.Blockers, func(a, b tracker.NativeDependency) int {
		return strings.Compare(string(a.ID), string(b.ID))
	})
	slices.SortFunc(issue.Dependents, func(a, b tracker.NativeDependency) int {
		return strings.Compare(string(a.ID), string(b.ID))
	})
	slices.SortFunc(issue.Related, func(a, b tracker.NativeDependency) int {
		return strings.Compare(string(a.ID), string(b.ID))
	})
	issue.BlockedReason.CurrentBasis = DependencyBasis(edges.blocked)
	issue.UnblocksReason.CurrentBasis = DependencyBasis(edges.unblocks)
	if issue.BlockedReason.Basis != issue.BlockedReason.CurrentBasis || edges.privateBlocked {
		issue.BlockedReason.Text = ""
	}
	if issue.UnblocksReason.Basis != issue.UnblocksReason.CurrentBasis || edges.privateUnblocks {
		issue.UnblocksReason.Text = ""
	}
	if edges.privateBlocked {
		issue.BlockedReason.CurrentBasis = ""
	}
	if edges.privateUnblocks {
		issue.UnblocksReason.CurrentBasis = ""
	}
	return nil
}

type ProjectReader interface {
	RelationshipProjects(context.Context, accessstore.Authority, []tracker.ProjectID, time.Time) (map[tracker.ProjectID]bool, error)
}

func (rd Reader) ProjectAccess(ctx context.Context, scope hubscope.NativeScope, visible map[tracker.ProjectID]bool) (map[tracker.ProjectID]bool, error) {
	allowed := map[tracker.ProjectID]bool{scope.Project: true}
	projects := []tracker.ProjectID{}
	for project, active := range visible {
		if project != scope.Project && active && (scope.Credential.EntryKey == nil || scope.Credential.EntryKey.Allows(string(project))) {
			projects = append(projects, project)
		}
	}
	if len(projects) == 0 {
		return allowed, nil
	}
	err := rd.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
		if batch, ok := r.(ProjectReader); ok {
			if rd.deps.Hosted && scope.Organization != rd.deps.Organization {
				return auth.ErrHostedIdentity
			}
			required := apikey.ScopeRead
			if scope.RequireHostedAdmin {
				required = apikey.ScopeAdmin
			}
			if scope.Credential.EntryKey != nil && (!hubscope.HostedKeyAllows(scope.Credential.HostedKeyScope, required) || !hubscope.HostedRoleAllows(scope.Credential.HostedRole, required, scope.Action)) {
				return auth.ErrHostedIdentity
			}
			found, err := batch.RelationshipProjects(ctx, rd.deps.Authority(scope, false), projects, rd.deps.Now())
			if err != nil {
				return err
			}
			for project, visible := range found {
				allowed[project] = visible
			}
			return nil
		}
		for _, project := range projects {
			target := scope
			target.Project = project
			err := rd.deps.Authorize(ctx, r, target, false)
			if errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) || errors.Is(err, operatortool.ErrAccessDenied) || hubapi.IsNativeNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			allowed[project] = true
		}
		return nil
	})
	return allowed, err
}

func DependencyBasis(ids []string) string {
	slices.Sort(ids)
	digest := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return hex.EncodeToString(digest[:])
}

func LoadDependencyReasons(ctx context.Context, r tenantstore.Reader, project tracker.ProjectID, issues []*tracker.NativeIssue) error {
	reader, ok := r.(corestore.DependencyReasonReader)
	if !ok {
		return nil
	}
	byID := map[tracker.NativeWorkItemID][]*tracker.NativeIssue{}
	ids := []tracker.NativeWorkItemID{}
	for _, issue := range issues {
		if _, ok := byID[issue.WorkItemID]; !ok {
			ids = append(ids, issue.WorkItemID)
		}
		byID[issue.WorkItemID] = append(byID[issue.WorkItemID], issue)
	}
	for start := 0; start < len(ids); start += corestore.MaxRelationshipBatch {
		records, err := reader.DependencyReasonRecords(ctx, project, ids[start:min(start+corestore.MaxRelationshipBatch, len(ids))])
		if err != nil {
			return err
		}
		for _, record := range records {
			for _, issue := range byID[record.WorkItemID] {
				switch record.Kind {
				case "blocked":
					issue.BlockedReason = record.Reason
				case "unblocks":
					issue.UnblocksReason = record.Reason
				}
			}
		}
	}
	return nil
}
