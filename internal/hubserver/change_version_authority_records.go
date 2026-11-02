package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type tenantChangeVersionAuthority struct {
	repository tenantstore.Repository
	content    *eventstore.Content
}

func (a tenantChangeVersionAuthority) Prepare(ctx context.Context, scope nativeScope, change tracker.ChangeRequest, request tracker.PublishChangeVersion, _ time.Time) (ChangeVersionApproval, error) {
	var rows currentChangePolicyRows
	err := a.repository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = readCurrentChangePolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return ChangeVersionApproval{}, projectRecordError(err)
	}
	result, err := hydrateCurrentChangeVersionApproval(ctx, a.content, scope, rows, request.PolicyID)
	if err != nil {
		return ChangeVersionApproval{}, projectRecordError(err)
	}
	if change.OrganizationID != scope.organization || change.ProjectID != scope.project || result.Policy.ID != request.PolicyID {
		return ChangeVersionApproval{}, policyMismatch("Current approved policy changed; read the current policy and retry")
	}
	return result, nil
}

func (a tenantChangeVersionAuthority) Authorize(ctx context.Context, w tenantstore.Writer, scope nativeScope, change tracker.ChangeRequest, request tracker.PublishChangeVersion, prepared ChangeVersionApproval, _ time.Time) error {
	rows, err := readCurrentChangePolicyRows(ctx, w, scope)
	if err != nil {
		return err
	}
	if rows.Project.PolicyID != prepared.Policy.ID || rows.hasReview && rows.Review.ReviewPolicyID != prepared.ReviewPolicy.ID || !rows.hasReview && defaultChangeReviewPolicy(prepared.Policy).ID != prepared.ReviewPolicy.ID {
		return policyMismatch("Current approved Change policy changed during publication")
	}
	if change.OrganizationID != scope.organization || change.ProjectID != scope.project || change.WorkItemID == "" || request.PolicyID != prepared.Policy.ID {
		return corestore.ErrTenantMismatch
	}
	return nil
}

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
