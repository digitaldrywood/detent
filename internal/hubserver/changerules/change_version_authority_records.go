package changerules

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type VersionAuthority struct {
	Repository          tenantstore.Repository
	Content             *eventstore.Content
	DefaultReviewPolicy func(policy.Descriptor) tracker.ChangeReviewPolicy
	Reject              func(error, string, string) error
}

func (a VersionAuthority) Prepare(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, request tracker.PublishChangeVersion, _ time.Time) (hubrecords.ChangeVersionApproval, error) {
	var rows hubrecords.CurrentChangePolicyRows
	unreported := false
	err := a.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = ReadCurrentChangePolicyRows(ctx, r, scope)
		if err != nil || request.PolicyID == "" || request.PolicyID == rows.Project.PolicyID {
			return err
		}
		_, err = r.ProjectPolicyRevision(ctx, scope.Project, request.PolicyID)
		if errors.Is(err, corestore.ErrNotFound) {
			unreported = true
			return nil
		}
		return err
	})
	if err != nil {
		return hubrecords.ChangeVersionApproval{}, hubapi.ProjectRecordError(err)
	}
	if unreported {
		return hubrecords.ChangeVersionApproval{}, a.Reject(hubapi.NativePublicInvalid("Change configuration has not been reported for this project"), "version_validation", "policy_id")
	}
	result, err := HydrateCurrentChangeVersionApproval(ctx, a.Content, scope, rows, request.PolicyID, a.DefaultReviewPolicy)
	if err != nil {
		return hubrecords.ChangeVersionApproval{}, hubapi.ProjectRecordError(err)
	}
	if change.OrganizationID != scope.Organization || change.ProjectID != scope.Project || result.Policy.ID != request.PolicyID {
		return hubrecords.ChangeVersionApproval{}, hubapi.PolicyMismatch("Current approved policy changed; read the current policy and retry")
	}
	return result, nil
}

func (a VersionAuthority) Authorize(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, change tracker.ChangeRequest, request tracker.PublishChangeVersion, prepared hubrecords.ChangeVersionApproval, _ time.Time) error {
	rows, err := ReadCurrentChangePolicyRows(ctx, w, scope)
	if err != nil {
		return err
	}
	if rows.Project.PolicyID != prepared.Policy.ID || rows.HasReview && rows.Review.ReviewPolicyID != prepared.ReviewPolicy.ID || !rows.HasReview && a.DefaultReviewPolicy(prepared.Policy).ID != prepared.ReviewPolicy.ID {
		return hubapi.PolicyMismatch("Current approved Change policy changed during publication")
	}
	if change.OrganizationID != scope.Organization || change.ProjectID != scope.Project || change.WorkItemID == "" || request.PolicyID != prepared.Policy.ID {
		return corestore.ErrTenantMismatch
	}
	return nil
}

func ReadCurrentChangePolicyRows(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope) (hubrecords.CurrentChangePolicyRows, error) {
	rows, err := hubrecords.ReadChangeSummaryPolicyRows(ctx, r, scope)
	if err == nil && rows.HasReview && rows.Review.PolicyRevisionID != rows.Project.PolicyID {
		return rows, &hubapi.NativeError{Code: "policy_mismatch", Message: "Change review policy does not match the approved repository policy; read the current policy and retry", Status: http.StatusConflict, PublicMessage: true}
	}
	return rows, err
}

func HydrateCurrentChangeVersionApproval(ctx context.Context, content *eventstore.Content, scope hubscope.NativeScope, rows hubrecords.CurrentChangePolicyRows, policyID string, defaultReviewPolicy func(policy.Descriptor) tracker.ChangeReviewPolicy) (hubrecords.ChangeVersionApproval, error) {
	descriptor, err := content.ProjectPolicy(ctx, rows.Project)
	if err != nil {
		return hubrecords.ChangeVersionApproval{}, err
	}
	if err := descriptor.Validate(); err != nil || descriptor.ID != rows.Project.PolicyID {
		return hubrecords.ChangeVersionApproval{}, eventstore.ErrInvalidRecord
	}
	rules := defaultReviewPolicy(descriptor)
	if rows.HasReview {
		rules, err = content.ReviewPolicy(ctx, rows.Review)
		if err != nil {
			return hubrecords.ChangeVersionApproval{}, err
		}
	}
	if err := changerequest.ValidatePolicy(rules, descriptor); err != nil {
		return hubrecords.ChangeVersionApproval{}, hubapi.NativeConflict(0)
	}
	if policyID != "" && policyID != descriptor.ID {
		return hubrecords.ChangeVersionApproval{}, hubapi.PolicyMismatch("Published version does not match the current approved policy")
	}
	return hubrecords.ChangeVersionApproval{Policy: descriptor, ReviewPolicy: rules}, nil
}

func HydrateChangeSummaryPolicy(ctx context.Context, content *eventstore.Content, scope hubscope.NativeScope, rows hubrecords.CurrentChangePolicyRows, defaultReviewPolicy func(policy.Descriptor) tracker.ChangeReviewPolicy) (hubrecords.ChangeVersionApproval, error) {
	if rows.HasReview && rows.Review.PolicyRevisionID == rows.Project.PolicyID {
		return HydrateCurrentChangeVersionApproval(ctx, content, scope, rows, "", defaultReviewPolicy)
	}
	if rows.Project.OrganizationID != scope.Organization || rows.Project.ProjectID != scope.Project || rows.HasReview && (rows.Review.OrganizationID != scope.Organization || rows.Review.ProjectID != scope.Project) {
		return hubrecords.ChangeVersionApproval{}, corestore.ErrTenantMismatch
	}
	descriptor, err := content.ProjectPolicy(ctx, rows.Project)
	if err != nil {
		return hubrecords.ChangeVersionApproval{}, err
	}
	if err := descriptor.Validate(); err != nil || descriptor.ID != rows.Project.PolicyID {
		return hubrecords.ChangeVersionApproval{}, eventstore.ErrInvalidRecord
	}
	rules := defaultReviewPolicy(descriptor)
	if rows.HasReview {
		rules, err = content.ReviewPolicy(ctx, rows.Review)
		if err != nil {
			return hubrecords.ChangeVersionApproval{}, err
		}
	}
	return hubrecords.ChangeVersionApproval{Policy: descriptor, ReviewPolicy: rules}, nil
}
