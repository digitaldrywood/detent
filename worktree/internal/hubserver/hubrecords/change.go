package hubrecords

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const ChangeLegacyReadLimit = 100

type ChangeVersionApproval struct {
	Policy       policy.Descriptor
	ReviewPolicy tracker.ChangeReviewPolicy
}

type CurrentChangePolicyRows struct {
	Project   policystore.Revision
	Review    eventstore.ReviewPolicy
	HasReview bool `json:"-"`
}

func ReadChangeSummaryPolicyRows(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope) (CurrentChangePolicyRows, error) {
	var rows CurrentChangePolicyRows
	project, err := r.ProjectPolicy(ctx, scope.Project)
	if errors.Is(err, corestore.ErrNotFound) {
		return rows, ErrNoApprovedRepositoryPolicy
	}
	if err != nil {
		return rows, err
	}
	if project.OrganizationID != scope.Organization || project.ProjectID != scope.Project || project.PolicyID == "" || project.ApprovedBy == "" || project.ApprovedAt.IsZero() {
		return rows, corestore.ErrTenantMismatch
	}
	rows.Project = project
	review, err := r.ReviewPolicy(ctx, scope.Project)
	if errors.Is(err, corestore.ErrNotFound) {
		return rows, nil
	}
	if err != nil {
		return rows, err
	}
	if review.OrganizationID != scope.Organization || review.ProjectID != scope.Project || review.PolicyRevisionID == "" || review.ReviewPolicyID == "" || review.Revision < 1 || review.UpdatedBy == "" || review.UpdatedAt.IsZero() {
		return rows, corestore.ErrTenantMismatch
	}
	rows.Review, rows.HasReview = review, true
	return rows, nil
}
