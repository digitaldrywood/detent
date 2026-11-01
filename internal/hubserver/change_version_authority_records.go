package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type currentChangePolicyRows struct {
	Project   policystore.Revision
	Review    eventstore.ReviewPolicy
	hasReview bool
}

func readCurrentChangePolicyRows(ctx context.Context, r tenantstore.Reader, scope nativeScope) (currentChangePolicyRows, error) {
	var rows currentChangePolicyRows
	project, err := r.ProjectPolicy(ctx, scope.project)
	if errors.Is(err, corestore.ErrNotFound) {
		return rows, errNoApprovedRepositoryPolicy
	}
	if err != nil {
		return rows, err
	}
	if project.OrganizationID != scope.organization || project.ProjectID != scope.project || project.PolicyID == "" || project.ApprovedBy == "" || project.ApprovedAt.IsZero() {
		return rows, corestore.ErrTenantMismatch
	}
	rows.Project = project
	review, err := r.ReviewPolicy(ctx, scope.project)
	if errors.Is(err, corestore.ErrNotFound) {
		return rows, nil
	}
	if err != nil {
		return rows, err
	}
	if review.OrganizationID != scope.organization || review.ProjectID != scope.project || review.PolicyRevisionID != project.PolicyID || review.ReviewPolicyID == "" || review.Revision < 1 || review.UpdatedBy == "" || review.UpdatedAt.IsZero() {
		return rows, corestore.ErrTenantMismatch
	}
	rows.Review, rows.hasReview = review, true
	return rows, nil
}

func hydrateCurrentChangeVersionApproval(ctx context.Context, content *eventstore.Content, scope nativeScope, rows currentChangePolicyRows, policyID string) (ChangeVersionApproval, error) {
	descriptor, err := content.ProjectPolicy(ctx, rows.Project)
	if err != nil {
		return ChangeVersionApproval{}, err
	}
	if err := descriptor.Validate(); err != nil || descriptor.ID != rows.Project.PolicyID {
		return ChangeVersionApproval{}, eventstore.ErrInvalidRecord
	}
	rules := defaultChangeReviewPolicy(descriptor)
	if rows.hasReview {
		rules, err = content.ReviewPolicy(ctx, rows.Review)
		if err != nil {
			return ChangeVersionApproval{}, err
		}
	}
	if err := changerequest.ValidatePolicy(rules, descriptor); err != nil {
		return ChangeVersionApproval{}, nativeConflict(0)
	}
	if policyID != "" && policyID != descriptor.ID {
		return ChangeVersionApproval{}, policyMismatch("Published version does not match the current approved policy")
	}
	return ChangeVersionApproval{Policy: descriptor, ReviewPolicy: rules}, nil
}
