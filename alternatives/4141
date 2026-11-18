package workitemread

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (rd Reader) Record(ctx context.Context, scope hubscope.NativeScope, id string) (corestore.WorkItem, eventstore.CollaborationVersion, error) {
	var row corestore.WorkItem
	var version eventstore.CollaborationVersion
	err := rd.deps.Tenants.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		project, err := r.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		row, err = r.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(id))
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
			return corestore.ErrNotFound
		}
		version, err = r.CollaborationVersion(ctx, id, row.Revision)
		return err
	})
	return row, version, hubapi.ProjectRecordError(err)
}

func (rd Reader) Hydrate(ctx context.Context, row corestore.WorkItem, version eventstore.CollaborationVersion) (tracker.NativeIssue, error) {
	issue, err := rd.HydrateCanonical(ctx, row, version)
	if err != nil {
		return issue, err
	}
	if source, found, err := rd.LinkedSource(ctx, row.OrganizationID, row.ProjectID, row.NativeID); err != nil {
		return issue, err
	} else if found {
		issue.LinkedSource = &source
		if reference := tracker.GitHubIssueSourceReference(source.URL, source.URL); !slices.Contains(issue.ExternalReferences, reference) {
			issue.ExternalReferences = append(issue.ExternalReferences, reference)
		}
	}
	return rd.IssueResponse(issue), nil
}

func (rd Reader) HydrateCanonical(ctx context.Context, row corestore.WorkItem, version eventstore.CollaborationVersion) (tracker.NativeIssue, error) {
	var issue tracker.NativeIssue
	if version.OrganizationID != row.OrganizationID || version.ProjectID != row.ProjectID || version.WorkItemID != row.NativeID || version.RecordID != string(row.NativeID) || version.Revision != row.Revision {
		return issue, artifact.ErrIntegrity
	}
	raw, err := eventstore.NewContent(rd.deps.Details).CollaborationVersion(ctx, version)
	if err != nil {
		return issue, err
	}
	issue, err = eventstore.DecodeCollaborationIssue(version, raw)
	if err != nil || issue.OrganizationID != row.OrganizationID || issue.ProjectID != row.ProjectID || issue.WorkItemID != row.NativeID || int64(issue.Revision) != row.Revision || int64(issue.Number) != row.Number || issue.Title != row.Title || issue.Archived != row.Archived || issue.State != hubapi.StringValue(row.WorkflowStateName) || issue.Actor.Kind != hubapi.StringValue(row.ActorKind) || issue.Actor.PrincipalID != hubapi.StringValue(row.ActorPrincipalID) {
		return issue, artifact.ErrIntegrity
	}
	issue.ChangeSequence = 0
	issue.LastActivityAt = row.LastActivityAt
	return issue, nil
}

func (rd Reader) Read(ctx context.Context, scope hubscope.NativeScope, id string) (tracker.NativeIssue, error) {
	row, version, err := rd.Record(ctx, scope, id)
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	issue, err := rd.Hydrate(ctx, row, version)
	if err == nil {
		issue.WebURL, err = rd.BrowserURL(ctx, row.OrganizationID, string(row.NativeID))
	}
	if err == nil {
		err = rd.HydrateRelationshipFacts(ctx, scope, &issue)
	}
	if err == nil {
		err = rd.HydrateActorNames(ctx, scope.Organization, []*tracker.NativeIssue{&issue})
	}
	issue.OmittedFields = append(issue.OmittedFields, "change_sequence")
	return scope.ReporterIssue(issue), err
}

func (rd Reader) HydrateActorNames(ctx context.Context, organization tracker.OrganizationID, issues []*tracker.NativeIssue) error {
	principals := make([]string, 0, len(issues))
	seen := make(map[string]bool, len(issues))
	for _, issue := range issues {
		principal := issue.Actor.PrincipalID
		if principal != "" && !seen[principal] {
			principals = append(principals, principal)
			seen[principal] = true
		}
	}
	if len(principals) == 0 {
		return nil
	}
	var names map[string]string
	if err := rd.deps.Tenants.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		names, err = reader.AccessPublicNames(ctx, principals)
		return err
	}); err != nil {
		return fmt.Errorf("read filer identities: %w", err)
	}
	for _, issue := range issues {
		if name := names[issue.Actor.PrincipalID]; name != "" {
			issue.Actor.DisplayName = name
		}
	}
	return nil
}

func (rd Reader) IssueResponse(issue tracker.NativeIssue) tracker.NativeIssue {
	if rd.deps.Hosted && strings.HasPrefix(issue.WebURL, "/") {
		issue.WebURL = strings.TrimRight(rd.deps.PublicURL, "/") + issue.WebURL
	}
	if !rd.deps.Hosted {
		issue.WebURL = ""
	}
	return issue
}

func (rd Reader) BrowserURL(ctx context.Context, organization tracker.OrganizationID, id string) (string, error) {
	path, err := rd.BrowserPath(ctx, organization, id)
	if err != nil {
		return "", err
	}
	if rd.deps.Hosted {
		path = strings.TrimRight(rd.deps.PublicURL, "/") + path
	}
	return path, nil
}

func (rd Reader) BrowserPath(ctx context.Context, organization tracker.OrganizationID, id string) (string, error) {
	if rd.deps.Tenants == nil {
		return "", hubapi.NativeWorkItemStorageUnavailable()
	}
	var organizationSlug, projectSlug string
	var number int64
	err := rd.deps.Tenants.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItemByNativeID(ctx, tracker.NativeWorkItemID(id))
		if err != nil {
			return err
		}
		project, err := r.Project(ctx, row.ProjectID)
		if err != nil {
			return err
		}
		if project.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		tenant, err := r.AccessOrganization(ctx)
		if err != nil {
			return err
		}
		organizationSlug = tenant.Slug
		projectSlug, number = project.Slug, row.Number
		return nil
	})
	if err != nil {
		return "", hubapi.ProjectRecordError(err)
	}
	return "/" + organizationSlug + "/" + projectSlug + "/issues/" + strconv.FormatInt(number, 10), nil
}
