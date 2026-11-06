package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) ProjectOnboardingRecords(ctx context.Context, projects []tracker.ProjectID) ([]projectstore.OnboardingRecord, error) {
	if len(projects) < 1 || len(projects) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	ids := make([]string, len(projects))
	for i, id := range projects {
		if id == "" {
			return nil, corestore.ErrInvalidPage
		}
		ids[i] = string(id)
	}
	rows, err := s.db.Query(ctx, `SELECT p.id,coalesce(nullif(ps.repository,''),ps.checkout_repository,''),coalesce(o.revision,0),coalesce(o.repository,''),coalesce(o.doctor,false),coalesce(o.provider,false),coalesce(o.artifacts,''),coalesce(o.updated_at,''),
 coalesce(r.policy_id,''),coalesce(a.approved_by,r.approved_by,''),coalesce(a.approved_at,r.approved_at),coalesce(r.payload_object_key,''),coalesce(r.payload_sha256,''),coalesce(r.payload_bytes,0),coalesce(r.payload_content_bytes,0),r.gate_github_pull_request
 FROM projects p JOIN project_settings ps ON ps.organization_id=p.organization_id AND ps.project_id=p.id
 LEFT JOIN project_onboarding o ON o.organization_id=p.organization_id AND o.project_id=p.id
 LEFT JOIN project_policies current ON current.organization_id=p.organization_id AND current.project_id=p.id
 LEFT JOIN policy_revisions r ON r.organization_id=current.organization_id AND r.project_id=current.project_id AND r.policy_id=current.policy_id
 LEFT JOIN policy_revision_approvals a ON a.organization_id=r.organization_id AND a.project_id=r.project_id AND a.policy_id=r.policy_id
 WHERE p.organization_id=$1 AND p.id=ANY($2) AND p.deleted_at IS NULL ORDER BY p.id`, s.organization, ids)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []projectstore.OnboardingRecord{}
	for rows.Next() {
		var record projectstore.OnboardingRecord
		revision := policystore.Revision{OrganizationID: s.organization}
		var approved pgtype.Timestamptz
		var gate pgtype.Bool
		if err := rows.Scan(&record.ProjectID, &record.Repository, &record.Progress.Revision, &record.Progress.Repository, &record.Progress.Doctor, &record.Progress.Provider, &record.Progress.Artifacts, &record.Progress.UpdatedAt, &revision.PolicyID, &revision.ApprovedBy, &approved, &revision.PayloadObjectKey, &revision.PayloadSHA256, &revision.PayloadBytes, &revision.PayloadContentBytes, &gate); err != nil {
			return nil, mapError(ctx, err)
		}
		if record.Progress.Validate() != nil {
			return nil, projectstore.ErrInvalidOnboardingProgress
		}
		if revision.PolicyID != "" {
			revision.ProjectID = record.ProjectID
			revision.ApprovedAt = approved.Time
			if gate.Valid {
				revision.GitHubPullRequest = &gate.Bool
			}
			record.Policy = &revision
		}
		result = append(result, record)
	}
	return result, mapError(ctx, rows.Err())
}
