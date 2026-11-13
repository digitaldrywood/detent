package hubrecords

import (
	"context"
	"database/sql"
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

type ChangeLegacyRows struct {
	QueryPage     func(int) (*sql.Rows, error) `json:"-"`
	rows          *sql.Rows
	offset, count int
	err           error
	closed        bool
	Cursor        []any `json:"-"`
}

func (r *ChangeLegacyRows) NextPage() error {
	rows, err := r.QueryPage(r.offset)
	if err != nil {
		return err
	}
	r.rows, r.count = rows, 0
	return nil
}

func (r *ChangeLegacyRows) Next() bool {
	if r.closed || r.err != nil {
		return false
	}
	if r.rows.Next() {
		r.count++
		return true
	}
	r.err = errors.Join(r.rows.Err(), r.rows.Close())
	if r.err != nil || r.count < ChangeLegacyReadLimit {
		return false
	}
	r.offset += r.count
	r.err = r.NextPage()
	if r.err != nil {
		return false
	}
	return r.Next()
}

func (r *ChangeLegacyRows) Scan(dest ...any) error {
	columns := make([]any, 0, len(r.Cursor)+len(dest))
	for index := range r.Cursor {
		columns = append(columns, &r.Cursor[index])
	}
	return r.rows.Scan(append(columns, dest...)...)
}

func (r *ChangeLegacyRows) Err() error { return r.err }

func (r *ChangeLegacyRows) Close() error { r.closed = true; return r.rows.Close() }

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
