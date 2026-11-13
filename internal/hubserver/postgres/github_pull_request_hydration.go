package postgres

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const pullRequestRefreshColumns = `project_id,change_id,repository,pull_number,work_item_id,requested_version,requested_head_sha,request_count,observed_request_count,requested_at,next_attempt_at,pending,attempts,observed_head_sha,source_updated_at,observed_at,state,draft,mergeable,checks_status,checks_conclusion,checks_total,checks_pending,checks_passed,checks_failed,reviews_state,reviews_decision,reviews_approvals,reviews_changes_requested,reviews_comments,merge_sha,base_ref,merged_at,merged_by,repository_default_branch`

func (s *session) readGitHubPullRequestRefresh(ctx context.Context, predicate string, args ...any) (projectstore.GitHubPullRequestRefresh, error) {
	var row projectstore.GitHubPullRequestRefresh
	var observedRequestCount *int64
	var head, state, checkStatus, checkConclusion, reviewState, reviewDecision *string
	var sourceAt, observedAt, mergedAt *time.Time
	var mergeSHA, baseRef, mergedBy, defaultBranch *string
	var draft, mergeable *bool
	var total, pending, passed, failed, approvals, changesRequested, comments *int64
	err := s.db.QueryRow(ctx, `SELECT `+pullRequestRefreshColumns+` FROM github_pull_request_refresh WHERE organization_id=$1 AND source_kind='change' AND `+predicate, args...).Scan(
		&row.ProjectID, &row.ChangeID, &row.Repository, &row.Number, &row.WorkItemID, &row.RequestedVersion, &row.RequestedHeadSHA, &row.RequestCount, &observedRequestCount, &row.RequestedAt, &row.NextAttemptAt, &row.Pending, &row.Attempts,
		&head, &sourceAt, &observedAt, &state, &draft, &mergeable, &checkStatus, &checkConclusion, &total, &pending, &passed, &failed, &reviewState, &reviewDecision, &approvals, &changesRequested, &comments, &mergeSHA, &baseRef, &mergedAt, &mergedBy, &defaultBranch)
	if err != nil {
		return row, mapError(ctx, err)
	}
	if observedRequestCount != nil {
		row.ObservedRequestCount = *observedRequestCount
	}
	if observedAt != nil {
		if head == nil || sourceAt == nil || state == nil || draft == nil || checkStatus == nil || checkConclusion == nil || total == nil || pending == nil || passed == nil || failed == nil || reviewState == nil || reviewDecision == nil || approvals == nil || changesRequested == nil || comments == nil {
			return row, artifact.ErrIntegrity
		}
		row.Observation = &projectstore.GitHubPullRequestObservation{RepositoryDefaultBranch: defaultBranch, MergeSHA: mergeSHA, BaseRef: baseRef, MergedAt: mergedAt, MergedBy: mergedBy, Repository: row.Repository, Number: row.Number, HeadSHA: *head, SourceUpdatedAt: *sourceAt, ObservedAt: *observedAt, State: *state, Draft: *draft, Mergeable: mergeable,
			Checks:  tracker.CheckSummary{Status: *checkStatus, Conclusion: *checkConclusion, Total: int(*total), Pending: int(*pending), Passed: int(*passed), Failed: int(*failed)},
			Reviews: tracker.ReviewSummary{State: *reviewState, Decision: *reviewDecision, Approvals: int(*approvals), ChangesRequested: int(*changesRequested), Comments: int(*comments)}}
	}
	return row, nil
}

func (s *session) GitHubPullRequestRefresh(ctx context.Context, key projectstore.GitHubPullRequestRefreshKey) (projectstore.GitHubPullRequestRefresh, error) {
	return s.readGitHubPullRequestRefresh(ctx, `project_id=$2 AND change_id=$3 AND repository=$4 AND pull_number=$5`, s.organization, key.ProjectID, key.ChangeID, key.Repository, key.Number)
}

func (s *session) NextGitHubPullRequestRefresh(ctx context.Context, now time.Time) (projectstore.GitHubPullRequestRefresh, error) {
	return s.readGitHubPullRequestRefresh(ctx, `pending AND next_attempt_at<=$2 ORDER BY next_attempt_at,project_id,change_id,repository,pull_number LIMIT 1`, s.organization, now)
}

func (s *session) RequestGitHubPullRequests(ctx context.Context, rows []projectstore.GitHubPullRequestRefresh) error {
	if len(rows) == 0 {
		return nil
	}
	if err := checkPage(len(rows)); err != nil {
		return err
	}
	projects, changes, repositories, items, versions, heads := make([]string, 0, len(rows)), make([]string, 0, len(rows)), make([]string, 0, len(rows)), make([]string, 0, len(rows)), make([]string, 0, len(rows)), make([]string, 0, len(rows))
	numbers := make([]int64, 0, len(rows))
	requested := make([]time.Time, 0, len(rows))
	seen := make(map[projectstore.GitHubPullRequestRefreshKey]struct{}, len(rows))
	for _, row := range rows {
		if row.ProjectID == "" || row.ChangeID == "" || row.WorkItemID == "" || row.Repository == "" || row.Repository != strings.ToLower(row.Repository) || row.Number <= 0 || row.RequestedVersion == "" || row.RequestedHeadSHA == "" || row.RequestedAt.IsZero() {
			return artifact.ErrIntegrity
		}
		if _, ok := seen[row.GitHubPullRequestRefreshKey]; ok {
			return artifact.ErrIntegrity
		}
		seen[row.GitHubPullRequestRefreshKey] = struct{}{}
		projects = append(projects, string(row.ProjectID))
		changes = append(changes, row.ChangeID)
		repositories = append(repositories, row.Repository)
		numbers = append(numbers, int64(row.Number))
		items = append(items, string(row.WorkItemID))
		versions = append(versions, row.RequestedVersion)
		heads = append(heads, row.RequestedHeadSHA)
		requested = append(requested, row.RequestedAt)
	}
	tag, err := s.db.Exec(ctx, `WITH requested AS MATERIALIZED (
 SELECT * FROM UNNEST($2::text[],$3::text[],$4::text[],$5::bigint[],$6::text[],$7::text[],$8::text[],$9::timestamptz[])
 AS q(project_id,change_id,repository,pull_number,work_item_id,requested_version,requested_head_sha,requested_at)
), bound AS MATERIALIZED (
 SELECT p.id,ps.checkout_repository FROM projects p JOIN project_settings ps ON ps.organization_id=p.organization_id AND ps.project_id=p.id
 WHERE p.organization_id=$1 AND p.id=ANY($2::text[]) AND p.deleted_at IS NULL AND p.profile='native' AND ps.repository_enabled
 ORDER BY p.id FOR UPDATE OF p,ps
), sources AS MATERIALIZED (
 SELECT q.project_id,q.change_id,q.repository,q.pull_number,target.work_item_id,q.requested_version,q.requested_head_sha,q.requested_at FROM requested q JOIN bound p ON p.id=q.project_id AND lower(p.checkout_repository)=q.repository
 JOIN change_requests c ON c.organization_id=$1 AND c.project_id=q.project_id AND c.id=q.change_id AND c.current_version_id=q.requested_version
 JOIN change_versions v ON v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=c.current_version_id AND v.head_sha=q.requested_head_sha
 JOIN issues i ON i.organization_id=c.organization_id AND i.project_id=c.project_id AND i.native_id=q.work_item_id
 CROSS JOIN LATERAL (SELECT l.work_item_id FROM change_issue_links l WHERE l.organization_id=c.organization_id AND l.project_id=c.project_id AND l.change_id=c.id ORDER BY l.work_item_id LIMIT 1) target
 WHERE EXISTS(SELECT 1 FROM change_issue_links l WHERE l.organization_id=c.organization_id AND l.project_id=c.project_id AND l.change_id=c.id AND l.work_item_id=q.work_item_id)
 ORDER BY c.id FOR UPDATE OF c
)
INSERT INTO github_pull_request_refresh AS stored(organization_id,project_id,change_id,repository,pull_number,work_item_id,requested_version,requested_head_sha,request_count,requested_at,next_attempt_at,pending,attempts)
SELECT $1,project_id,change_id,repository,pull_number,work_item_id,requested_version,requested_head_sha,1,requested_at,requested_at,true,0 FROM sources
WHERE (SELECT count(*) FROM sources)=cardinality($2::text[])
ON CONFLICT(organization_id,project_id,change_id,repository,pull_number) DO UPDATE SET
 work_item_id=excluded.work_item_id,
 requested_version=CASE WHEN stored.requested_version<>excluded.requested_version THEN excluded.requested_version ELSE stored.requested_version END,
 requested_head_sha=CASE WHEN stored.requested_head_sha<>excluded.requested_head_sha THEN excluded.requested_head_sha ELSE stored.requested_head_sha END,
 request_count=stored.request_count+1,requested_at=GREATEST(stored.requested_at,excluded.requested_at),pending=true,
 attempts=CASE WHEN stored.requested_version<>excluded.requested_version OR stored.requested_head_sha<>excluded.requested_head_sha THEN 0 ELSE stored.attempts END,
 next_attempt_at=CASE WHEN stored.requested_version<>excluded.requested_version OR stored.requested_head_sha<>excluded.requested_head_sha OR NOT stored.pending THEN excluded.requested_at ELSE stored.next_attempt_at END`, s.organization, projects, changes, repositories, numbers, items, versions, heads, requested)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() != int64(len(rows)) {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) pullRequestRefreshArgs(row projectstore.GitHubPullRequestRefresh) []any {
	return []any{s.organization, row.ProjectID, row.ChangeID, row.Repository, row.Number, row.RequestCount, row.RequestedVersion, row.RequestedHeadSHA, row.WorkItemID}
}

const pullRequestRefreshFence = `source_kind='change' AND organization_id=$1 AND project_id=$2 AND change_id=$3 AND repository=$4 AND pull_number=$5 AND pending AND request_count=$6 AND requested_version=$7 AND requested_head_sha=$8 AND work_item_id=$9`

const pullRequestRefreshLiveOrigin = `SELECT 1 FROM projects p JOIN project_settings ps ON ps.organization_id=p.organization_id AND ps.project_id=p.id
 JOIN change_requests c ON c.organization_id=p.organization_id AND c.project_id=p.id AND c.id=$3
 JOIN change_issue_links l ON l.organization_id=c.organization_id AND l.project_id=c.project_id AND l.change_id=c.id AND l.work_item_id=$9
 JOIN issues i ON i.organization_id=p.organization_id AND i.project_id=p.id AND i.native_id=$9
 WHERE p.organization_id=$1 AND p.id=$2 AND p.deleted_at IS NULL AND p.profile='native' AND ps.repository_enabled AND lower(ps.checkout_repository)=$4`

func (s *session) CompleteGitHubPullRequestRefresh(ctx context.Context, row projectstore.GitHubPullRequestRefresh, observation projectstore.GitHubPullRequestObservation) error {
	if observation.Repository != row.Repository || observation.Number != row.Number || observation.HeadSHA == "" || observation.SourceUpdatedAt.IsZero() || observation.ObservedAt.IsZero() {
		return corestore.ErrRevisionConflict
	}
	if observation.MergeSHA != nil || observation.BaseRef != nil || observation.MergedAt != nil || observation.MergedBy != nil {
		if observation.MergeSHA == nil || observation.BaseRef == nil || *observation.BaseRef == "" || observation.MergedBy == nil || observation.State == "merged" && observation.MergedAt == nil || observation.MergedAt != nil && (observation.MergedAt.IsZero() || *observation.MergeSHA == "" || observation.State != "merged" && observation.State != "closed") {
			return artifact.ErrIntegrity
		}
	}
	args := s.pullRequestRefreshArgs(row)
	args = append(args, observation.HeadSHA, observation.SourceUpdatedAt, observation.ObservedAt, observation.State, observation.Draft, observation.Mergeable, observation.Checks.Status, observation.Checks.Conclusion, observation.Checks.Total, observation.Checks.Pending, observation.Checks.Passed, observation.Checks.Failed, observation.Reviews.State, observation.Reviews.Decision, observation.Reviews.Approvals, observation.Reviews.ChangesRequested, observation.Reviews.Comments, observation.MergeSHA, observation.BaseRef, observation.MergedAt, observation.MergedBy, observation.RepositoryDefaultBranch)
	tag, err := s.db.Exec(ctx, `WITH origin AS MATERIALIZED (
 SELECT p.id,lower(ps.repository)=$4 AS policy_repository_bound FROM projects p JOIN project_settings ps ON ps.organization_id=p.organization_id AND ps.project_id=p.id
 JOIN change_requests c ON c.organization_id=p.organization_id AND c.project_id=p.id AND c.id=$3 AND c.current_version_id=$7
 JOIN change_versions v ON v.organization_id=c.organization_id AND v.change_id=c.id AND v.id=c.current_version_id AND v.head_sha=$8
 WHERE p.organization_id=$1 AND p.id=$2 AND p.deleted_at IS NULL AND p.profile='native' AND ps.repository_enabled AND lower(ps.checkout_repository)=$4
 FOR UPDATE OF p,ps,c
), eligible AS MATERIALIZED (
 SELECT 1 FROM github_pull_request_refresh WHERE `+pullRequestRefreshFence+` AND EXISTS(SELECT 1 FROM origin) AND (source_updated_at IS NULL OR source_updated_at<=$11) FOR UPDATE
), next_target AS MATERIALIZED (
 SELECT work_item_id FROM change_issue_links WHERE organization_id=$1 AND project_id=$2 AND change_id=$3 AND work_item_id>$9 AND EXISTS(SELECT 1 FROM eligible) ORDER BY work_item_id LIMIT 1
), advanced AS (
 UPDATE issues SET last_activity_at=GREATEST(last_activity_at,$11::timestamptz,$12::timestamptz)
 WHERE organization_id=$1 AND project_id=$2 AND native_id=$9 AND EXISTS(SELECT 1 FROM eligible)
 AND EXISTS(SELECT 1 FROM change_issue_links WHERE organization_id=$1 AND project_id=$2 AND change_id=$3 AND work_item_id=$9)
 AND last_activity_at<GREATEST($11::timestamptz,$12::timestamptz)
)
UPDATE github_pull_request_refresh SET work_item_id=COALESCE((SELECT work_item_id FROM next_target),$9),pending=EXISTS(SELECT 1 FROM next_target),attempts=0,observed_request_count=$6,observed_head_sha=$10,source_updated_at=$11,observed_at=$12,state=$13,draft=$14,mergeable=$15,
 checks_status=$16,checks_conclusion=$17,checks_total=$18,checks_pending=$19,checks_passed=$20,checks_failed=$21,reviews_state=$22,reviews_decision=$23,reviews_approvals=$24,reviews_changes_requested=$25,reviews_comments=$26,merge_sha=COALESCE($27,merge_sha),base_ref=COALESCE($28,base_ref),merged_at=CASE WHEN $27::text IS NULL THEN merged_at ELSE $29::timestamptz END,merged_by=COALESCE($30,merged_by),repository_default_branch=$31,
 merge_policy_bound=CASE WHEN $27::text IS NULL THEN merge_policy_bound WHEN $29::timestamptz IS NULL THEN false ELSE COALESCE((SELECT policy_repository_bound FROM origin),false) OR (merge_policy_bound AND (merge_sha,base_ref,merged_at) IS NOT DISTINCT FROM ($27::text,$28::text,$29::timestamptz)) END
WHERE `+pullRequestRefreshFence+` AND EXISTS(SELECT 1 FROM eligible)`, args...)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) RetryGitHubPullRequestRefresh(ctx context.Context, row projectstore.GitHubPullRequestRefresh, next time.Time) error {
	args := append(s.pullRequestRefreshArgs(row), next, row.Attempts)
	tag, err := s.db.Exec(ctx, `UPDATE github_pull_request_refresh SET attempts=attempts+1,next_attempt_at=$10 WHERE `+pullRequestRefreshFence+` AND attempts=$11`, args...)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) CancelGitHubPullRequestRefresh(ctx context.Context, row projectstore.GitHubPullRequestRefresh) error {
	tag, err := s.db.Exec(ctx, `UPDATE github_pull_request_refresh SET pending=false WHERE `+pullRequestRefreshFence+` AND NOT EXISTS(`+pullRequestRefreshLiveOrigin+`)`, s.pullRequestRefreshArgs(row)...)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) SupersedeGitHubPullRequestRefresh(ctx context.Context, row projectstore.GitHubPullRequestRefresh) error {
	tag, err := s.db.Exec(ctx, `UPDATE github_pull_request_refresh SET pending=false WHERE `+pullRequestRefreshFence, s.pullRequestRefreshArgs(row)...)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) RecordGitHubPullRequestSource(ctx context.Context, project tracker.ProjectID, source projectstore.GitHubWebhook, observation projectstore.GitHubPullRequestObservation) error {
	if err := s.checkOrganization(source.Payload.OrganizationID); err != nil {
		return err
	}
	if project == "" || source.DeliveryID == "" || source.EventType != "pull_request" || source.Digest == "" || observation.Repository != source.Repository || observation.Number <= 0 || observation.HeadSHA == "" || observation.SourceUpdatedAt.IsZero() || observation.ObservedAt.IsZero() || observation.MergeSHA == nil || observation.BaseRef == nil || *observation.BaseRef == "" || observation.MergedBy == nil {
		return artifact.ErrIntegrity
	}
	if observation.State == "merged" && observation.MergedAt == nil || observation.MergedAt != nil && (observation.MergedAt.IsZero() || *observation.MergeSHA == "" || observation.State != "merged" && observation.State != "closed") {
		return artifact.ErrIntegrity
	}
	tag, err := s.db.Exec(ctx, `WITH bound AS MATERIALIZED (
 SELECT p.id FROM projects p JOIN project_settings ps ON ps.organization_id=p.organization_id AND ps.project_id=p.id
 WHERE p.organization_id=$1 AND p.id=$2 AND p.profile='native' AND p.deleted_at IS NULL AND lower(ps.repository)=$3
 FOR UPDATE OF p,ps
 ), verified AS MATERIALIZED (
 SELECT 1 FROM github_webhook_receipts WHERE organization_id=$1 AND delivery_id=$4 AND repository=$3 AND event_type='pull_request' AND payload_digest=$5 AND status='pending' AND payload_object_key=$6 AND payload_sha256=$7 AND payload_bytes=$8 AND payload_content_bytes=$9 AND after_project=$20 AND attempts=$21
 AND EXISTS(SELECT 1 FROM bound) FOR UPDATE
 )
 INSERT INTO github_pull_request_refresh AS stored(organization_id,project_id,source_kind,change_id,repository,pull_number,work_item_id,requested_version,requested_head_sha,request_count,requested_at,next_attempt_at,pending,attempts,webhook_delivery_id,observed_head_sha,source_updated_at,observed_at,state,draft,merge_sha,base_ref,merged_at,merged_by,repository_default_branch,merge_policy_bound)
 SELECT $1,$2,'webhook',NULL,$3,$10,NULL,NULL,NULL,NULL,NULL,NULL,false,0,$4,$11,$12,$13,$14,$15,$16,$17,$18,$19,$22,($18::timestamptz IS NOT NULL) FROM verified
 ON CONFLICT(organization_id,project_id,change_id,repository,pull_number) DO UPDATE SET
 webhook_delivery_id=CASE WHEN stored.source_updated_at<=excluded.source_updated_at THEN excluded.webhook_delivery_id ELSE stored.webhook_delivery_id END,
 observed_head_sha=CASE WHEN stored.source_updated_at<=excluded.source_updated_at THEN excluded.observed_head_sha ELSE stored.observed_head_sha END,
 observed_at=CASE WHEN stored.source_updated_at<=excluded.source_updated_at THEN GREATEST(stored.observed_at,excluded.observed_at) ELSE stored.observed_at END,
 state=CASE WHEN stored.source_updated_at<=excluded.source_updated_at THEN excluded.state ELSE stored.state END,
 draft=CASE WHEN stored.source_updated_at<=excluded.source_updated_at THEN excluded.draft ELSE stored.draft END,
 merge_sha=CASE WHEN stored.source_updated_at<=excluded.source_updated_at THEN excluded.merge_sha ELSE stored.merge_sha END,
 base_ref=CASE WHEN stored.source_updated_at<=excluded.source_updated_at THEN excluded.base_ref ELSE stored.base_ref END,
 merged_at=CASE WHEN stored.source_updated_at<=excluded.source_updated_at THEN excluded.merged_at ELSE stored.merged_at END,
 merged_by=CASE WHEN stored.source_updated_at<=excluded.source_updated_at THEN excluded.merged_by ELSE stored.merged_by END,
 repository_default_branch=CASE WHEN stored.source_updated_at<=excluded.source_updated_at THEN excluded.repository_default_branch ELSE stored.repository_default_branch END,
 merge_policy_bound=CASE WHEN stored.source_updated_at<=excluded.source_updated_at THEN excluded.merge_policy_bound ELSE stored.merge_policy_bound END,
 source_updated_at=GREATEST(stored.source_updated_at,excluded.source_updated_at)
 WHERE stored.source_kind='webhook'`,
		s.organization, project, source.Repository, source.DeliveryID, source.Digest, source.Payload.PayloadObjectKey, source.Payload.PayloadSHA256, source.Payload.PayloadBytes, source.Payload.PayloadContentBytes,
		observation.Number, observation.HeadSHA, observation.SourceUpdatedAt, observation.ObservedAt, observation.State, observation.Draft, observation.MergeSHA, observation.BaseRef, observation.MergedAt, observation.MergedBy, source.AfterProject, source.Attempts, observation.RepositoryDefaultBranch)
	if err != nil {
		return mapError(ctx, err)
	}
	if tag.RowsAffected() != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}
