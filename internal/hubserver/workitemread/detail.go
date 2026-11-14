package workitemread

import (
	"context"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var detailFields = []string{"change_sequence", "dependencies", "blockers", "dependents", "related", "blocked_reason", "unblocks_reason"}

func (rd Reader) LinkSource(ctx context.Context, organization tracker.OrganizationID, issue tracker.NativeIssue, source eventstore.LinkedSource) (tracker.NativeIssue, error) {
	issue.LinkedSource = nil
	if source.OrganizationID != "" {
		linked, err := rd.HydrateLinkedSource(ctx, organization, source)
		if err != nil {
			return tracker.NativeIssue{}, err
		}
		issue.LinkedSource = &linked
		if reference := tracker.GitHubIssueSourceReference(source.URL, source.URL); !slices.Contains(issue.ExternalReferences, reference) {
			issue.ExternalReferences = append(issue.ExternalReferences, reference)
		}
	}
	issue.OmittedFields = slices.DeleteFunc(slices.Clone(issue.OmittedFields), func(field string) bool { return field == "linked_source.snapshot" })
	return issue, nil
}

func (rd Reader) Complete(ctx context.Context, scope hubscope.NativeScope, issue *tracker.NativeIssue) error {
	if err := rd.HydrateRelationshipFacts(ctx, scope, issue); err != nil {
		return err
	}
	if err := rd.HydrateActorNames(ctx, scope.Organization, []*tracker.NativeIssue{issue}); err != nil {
		return err
	}
	if err := rd.deps.Tenants.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		head, err := r.BoardSequence(ctx, scope.Project)
		issue.ChangeSequence = head.Sequence
		return err
	}); err != nil {
		return err
	}
	issue.OmittedFields = slices.DeleteFunc(slices.Clone(issue.OmittedFields), func(field string) bool { return slices.Contains(detailFields, field) })
	if issue.ExternalReferences == nil {
		issue.ExternalReferences = []tracker.ExternalReference{}
	}
	*issue = scope.ReporterIssue(*issue)
	return nil
}
