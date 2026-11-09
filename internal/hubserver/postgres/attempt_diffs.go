package postgres

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const attemptDiffColumns = `organization_id,id,attempt_id,issue_id,project_id,work_item_id,source,source_id,sequence,base_sha,head_sha,producer_kind,producer_id,producer_runner_id,producer_lease_id,producer_fencing_token,file_count,patch_bytes,posted_bytes,truncated,manifest_expired,archive_id,created_at`

func scanAttemptDiff(ctx context.Context, row interface{ Scan(...any) error }) (leasestore.AttemptDiff, error) {
	var diff leasestore.AttemptDiff
	err := row.Scan(&diff.OrganizationID, &diff.ID, &diff.AttemptID, &diff.IssueID, &diff.ProjectID, &diff.WorkItemID,
		&diff.Generation.Source, &diff.Generation.ID, &diff.Generation.Seq, &diff.BaseSHA, &diff.HeadSHA,
		&diff.Producer.Kind, &diff.Producer.ID, &diff.Producer.RunnerID, &diff.Producer.LeaseID, &diff.Producer.FencingToken,
		&diff.FileCount, &diff.PatchBytes, &diff.PostedBytes, &diff.Truncated, &diff.ManifestExpired, &diff.ArchiveID, &diff.CreatedAt)
	return diff, mapError(ctx, err)
}

func (s *session) AttemptDiff(ctx context.Context, project tracker.ProjectID, attemptID string, generation tracker.DiffGeneration) (leasestore.AttemptDiff, error) {
	if project == "" || attemptID == "" || generation.Seq < 1 || generation.Source == "" {
		return leasestore.AttemptDiff{}, corestore.ErrInvalidPage
	}
	return scanAttemptDiff(ctx, s.db.QueryRow(ctx, `SELECT `+attemptDiffColumns+` FROM attempt_diffs WHERE organization_id=$1 AND project_id=$2 AND attempt_id=$3 AND source=$4 AND source_id=$5 AND sequence=$6`, s.organization, project, attemptID, generation.Source, generation.ID, generation.Seq))
}

func (s *session) LatestAttemptDiff(ctx context.Context, project tracker.ProjectID, attemptID, source string) (leasestore.AttemptDiff, error) {
	if project == "" || attemptID == "" || source == "" {
		return leasestore.AttemptDiff{}, corestore.ErrInvalidPage
	}
	return scanAttemptDiff(ctx, s.db.QueryRow(ctx, `SELECT `+attemptDiffColumns+` FROM attempt_diffs WHERE organization_id=$1 AND project_id=$2 AND attempt_id=$3 AND source=$4 ORDER BY sequence DESC,created_at DESC,id DESC LIMIT 1`, s.organization, project, attemptID, source))
}

func (s *session) LatestWorkItemAttemptDiff(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, source string) (leasestore.AttemptDiff, error) {
	if project == "" || item == "" || source == "" {
		return leasestore.AttemptDiff{}, corestore.ErrInvalidPage
	}
	return scanAttemptDiff(ctx, s.db.QueryRow(ctx, `SELECT `+attemptDiffColumns+` FROM attempt_diffs WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND source=$4 ORDER BY created_at DESC,id DESC LIMIT 1`, s.organization, project, item, source))
}

func (s *session) LatestAttemptDiffSequence(ctx context.Context, attemptID, source, sourceID string) (int64, error) {
	var sequence int64
	err := s.db.QueryRow(ctx, `SELECT COALESCE(max(sequence),0) FROM attempt_diffs WHERE organization_id=$1 AND attempt_id=$2 AND source=$3 AND source_id=$4`, s.organization, attemptID, source, sourceID).Scan(&sequence)
	return sequence, mapError(ctx, err)
}

func (s *session) LockAttemptDiffGeneration(ctx context.Context, attemptID, leaseID, credentialID, source, sourceID string) (int64, error) {
	if s.readOnly || attemptID == "" || leaseID == "" || credentialID == "" || source == "" {
		return 0, corestore.ErrInvalidPage
	}
	var locked string
	if err := s.db.QueryRow(ctx, `SELECT a.id FROM native_attempts a JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id JOIN runner_lease_assignments rla ON rla.organization_id=l.organization_id AND rla.lease_id=l.lease_id JOIN runner_identities ri ON ri.organization_id=rla.organization_id AND ri.runner_id=rla.runner_id JOIN api_tokens t ON t.organization_id=ri.organization_id AND t.id=ri.token_id WHERE a.organization_id=$1 AND a.id=$2 AND l.lease_id=$3 AND t.id=$4 FOR UPDATE OF a,l,rla,ri`, s.organization, attemptID, leaseID, credentialID).Scan(&locked); err != nil {
		return 0, mapError(ctx, err)
	}
	return s.LatestAttemptDiffSequence(ctx, attemptID, source, sourceID)
}

func (s *session) CreateAttemptDiff(ctx context.Context, diff leasestore.AttemptDiff) error {
	if err := s.checkOrganization(diff.OrganizationID); err != nil {
		return err
	}
	if diff.ID == "" || diff.AttemptID == "" || diff.IssueID <= 0 || diff.ProjectID == "" || diff.WorkItemID == "" || diff.Generation.Seq < 1 || diff.ArchiveID == "" || diff.CreatedAt.IsZero() {
		return corestore.ErrInvalidPage
	}
	_, err := s.db.Exec(ctx, `INSERT INTO attempt_diffs (`+attemptDiffColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`, diff.OrganizationID, diff.ID, diff.AttemptID, diff.IssueID, diff.ProjectID, diff.WorkItemID, diff.Generation.Source, diff.Generation.ID, diff.Generation.Seq, diff.BaseSHA, diff.HeadSHA, diff.Producer.Kind, diff.Producer.ID, diff.Producer.RunnerID, diff.Producer.LeaseID, diff.Producer.FencingToken, diff.FileCount, diff.PatchBytes, diff.PostedBytes, diff.Truncated, diff.ManifestExpired, diff.ArchiveID, diff.CreatedAt)
	if err != nil {
		return mapError(ctx, fmt.Errorf("insert attempt diff: %w", err))
	}
	return nil
}

var _ leasestore.Reader = (*session)(nil)
var _ leasestore.Writer = (*session)(nil)
