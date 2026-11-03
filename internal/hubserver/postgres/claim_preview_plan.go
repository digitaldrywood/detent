package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/lib/pq"
)

var _ tenantstore.ClaimPreviewPlanRowsReader = (*session)(nil)

const claimPreviewBatchLimit = corestore.MaxPageSize

func (s *session) ClaimPreviewPlanRows(ctx context.Context, project tracker.ProjectID, candidates []claimstore.Candidate) ([]tenantstore.ClaimPreviewPlanRow, error) {
	if len(candidates) == 0 {
		return []tenantstore.ClaimPreviewPlanRow{}, nil
	}
	if len(candidates) > claimPreviewBatchLimit {
		return nil, corestore.ErrInvalidPage
	}
	itemIDs := make([]int64, len(candidates))
	for index, candidate := range candidates {
		if candidate.OrganizationID != s.organization || candidate.ProjectID != project || candidate.ID <= 0 || candidate.WorkItemID == "" {
			return nil, corestore.ErrTenantMismatch
		}
		itemIDs[index] = candidate.ID
	}

	items, err := s.queries.GetWorkItemsByIDs(ctx, pgquery.GetWorkItemsByIDsParams{OrganizationID: s.organization, ProjectID: project, Ids: itemIDs})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]tenantstore.ClaimPreviewPlanRow, len(candidates))
	indexesByItemID := make(map[int64]int, len(candidates))
	indexesByNativeID := make(map[string]int, len(candidates))
	for index, candidate := range candidates {
		indexesByItemID[candidate.ID] = index
		indexesByNativeID[string(candidate.WorkItemID)] = index
	}
	for _, row := range items {
		index, ok := indexesByItemID[row.ID]
		if !ok {
			continue
		}
		result[index].Item = corestore.WorkItem(row)
	}
	for index := range result {
		if result[index].Item.ID == 0 {
			return nil, corestore.ErrNotFound
		}
	}
	activeItemIDs := make([]int64, 0, len(candidates))
	activeRecordIDs := make([]string, 0, len(candidates))
	activeRevisions := make([]int64, 0, len(candidates))
	activeWorkItemIDs := make([]string, 0, len(candidates))
	for index, candidate := range candidates {
		item := result[index].Item
		if item.Revision != candidate.Revision || item.Archived {
			continue
		}
		activeItemIDs = append(activeItemIDs, candidate.ID)
		activeRecordIDs = append(activeRecordIDs, string(candidate.WorkItemID))
		activeRevisions = append(activeRevisions, candidate.Revision)
		activeWorkItemIDs = append(activeWorkItemIDs, string(candidate.WorkItemID))
	}
	if len(activeItemIDs) == 0 {
		return result, nil
	}

	versions, err := s.claimPreviewCollaborationVersions(ctx, project, activeRecordIDs, activeRevisions)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	for _, row := range versions {
		index, ok := indexesByNativeID[row.RecordID]
		if ok && row.Revision == candidates[index].Revision {
			result[index].Version = eventstore.CollaborationVersion(row)
		}
	}
	for index, candidate := range candidates {
		if result[index].Item.Revision == candidate.Revision && !result[index].Item.Archived && result[index].Version.RecordID == "" {
			return nil, corestore.ErrNotFound
		}
	}

	linkedRows, err := s.queries.RecordGetLinkedIssueSourcesByWorkItems(ctx, pgquery.RecordGetLinkedIssueSourcesByWorkItemsParams{
		OrganizationID: s.organization,
		ProjectID:      project,
		WorkItemIds:    activeWorkItemIDs,
	})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	for _, row := range linkedRows {
		index, ok := indexesByNativeID[string(row.WorkItemID)]
		if ok {
			linked := linkedIssueSourceRecord(row)
			result[index].LinkedSource = &linked
		}
	}
	for index, candidate := range candidates {
		item := result[index].Item
		if item.Revision == candidate.Revision && !item.Archived && item.NativeSourceKey != nil && result[index].LinkedSource == nil {
			return nil, corestore.ErrNotFound
		}
	}

	changes, err := s.queries.RecordLatestChangesByWorkItems(ctx, pgquery.RecordLatestChangesByWorkItemsParams{
		OrganizationID: s.organization,
		ProjectID:      project,
		WorkItemIds:    activeWorkItemIDs,
	})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	changeIDs := make([]string, 0, len(changes))
	versionIDs := make([]string, 0, len(changes))
	indexesByChange := make(map[string][]int, len(changes))
	for _, row := range changes {
		index, ok := indexesByNativeID[row.LinkedWorkItemID]
		if !ok {
			continue
		}
		change := eventstore.Change{
			OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, WorkItemID: row.WorkItemID,
			ID: row.ID, Revision: row.Revision, Title: row.Title, CurrentVersionID: row.CurrentVersionID,
			EvidenceSequence: row.EvidenceSequence, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
			PayloadObjectKey: row.PayloadObjectKey, PayloadSHA256: row.PayloadSHA256,
			PayloadBytes: row.PayloadBytes, PayloadContentBytes: row.PayloadContentBytes,
		}
		result[index].Change = &change
		if row.CurrentVersionID != nil {
			changeIDs = append(changeIDs, row.ID)
			versionIDs = append(versionIDs, *row.CurrentVersionID)
			key := row.ID + "\x00" + *row.CurrentVersionID
			indexesByChange[key] = append(indexesByChange[key], index)
		}
	}
	var changeVersions []pgquery.ChangeVersion
	if len(changeIDs) > 0 {
		changeVersions, err = s.claimPreviewChangeVersions(ctx, changeIDs, versionIDs)
		if err != nil {
			return nil, mapError(ctx, err)
		}
	}
	policyIDs := make([]string, 0, len(changeVersions))
	for _, row := range changeVersions {
		indexes := indexesByChange[row.ChangeID+"\x00"+row.ID]
		if len(indexes) == 0 {
			continue
		}
		version := versionRecord(row)
		for _, index := range indexes {
			result[index].ChangeVersion = &version
		}
		policyIDs = append(policyIDs, version.PolicyRevisionID)
	}
	for index := range result {
		if result[index].Change != nil && result[index].Change.CurrentVersionID != nil && result[index].ChangeVersion == nil {
			return nil, corestore.ErrNotFound
		}
	}
	if len(policyIDs) > 0 {
		policyRows, err := s.queries.PolicyGetProjectRevisions(ctx, pgquery.PolicyGetProjectRevisionsParams{
			OrganizationID: s.organization,
			ProjectID:      project,
			PolicyIds:      policyIDs,
		})
		if err != nil {
			return nil, mapError(ctx, err)
		}
		policiesByID := make(map[string]policystore.Revision, len(policyRows))
		for _, row := range policyRows {
			policiesByID[row.PolicyID] = projectPolicyFields(row.OrganizationID, row.ProjectID, row.PolicyID, row.ApprovedBy, row.ApprovedAt, row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes, row.GateGithubPullRequest)
		}
		for index := range result {
			if result[index].ChangeVersion == nil {
				continue
			}
			policy, ok := policiesByID[result[index].ChangeVersion.PolicyRevisionID]
			if !ok {
				return nil, corestore.ErrNotFound
			}
			result[index].ChangePolicyRevision = &policy
		}
	}
	for index := range result {
		if result[index].ChangeVersion != nil && result[index].ChangePolicyRevision == nil {
			return nil, corestore.ErrNotFound
		}
	}

	attemptRows, err := s.queries.GetLatestIssueAttemptsByIssues(ctx, pgquery.GetLatestIssueAttemptsByIssuesParams{OrganizationID: s.organization, IssueIds: activeItemIDs})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	attemptIDs := make([]string, 0, len(attemptRows))
	indexesByAttempt := make(map[string]int, len(attemptRows))
	for _, row := range attemptRows {
		index, ok := indexesByItemID[row.IssueID]
		if !ok {
			continue
		}
		attempt := attemptFromPG(row)
		result[index].LatestAttempt = &attempt
		attemptIDs = append(attemptIDs, attempt.ID)
		indexesByAttempt[attempt.ID] = index
	}
	if len(attemptIDs) > 0 {
		eventRows, err := s.queries.RecordLatestRunEventsByAttempts(ctx, pgquery.RecordLatestRunEventsByAttemptsParams{
			OrganizationID: s.organization,
			ProjectID:      project,
			AttemptIds:     attemptIDs,
		})
		if err != nil {
			return nil, mapError(ctx, err)
		}
		for _, row := range eventRows {
			index, ok := indexesByAttempt[row.AttemptID]
			if ok {
				event := eventRecord(row)
				result[index].LatestRunEvent = &event
			}
		}
	}
	return result, nil
}

func (s *session) claimPreviewCollaborationVersions(ctx context.Context, project tracker.ProjectID, recordIDs []string, revisions []int64) ([]pgquery.CollaborationVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT cv.organization_id,cv.project_id,cv.work_item_id,cv.record_id,cv.revision,cv.payload_object_key,cv.payload_sha256,cv.payload_bytes,cv.payload_content_bytes
FROM collaboration_versions cv
JOIN unnest($3::text[],$4::bigint[]) AS requested(record_id,revision)
  ON cv.record_id=requested.record_id AND cv.revision=requested.revision
WHERE cv.organization_id=$1 AND cv.project_id=$2`, s.organization, project, pq.Array(recordIDs), pq.Array(revisions))
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []pgquery.CollaborationVersion
	for rows.Next() {
		var row pgquery.CollaborationVersion
		if err := rows.Scan(&row.OrganizationID, &row.ProjectID, &row.WorkItemID, &row.RecordID, &row.Revision, &row.PayloadObjectKey, &row.PayloadSHA256, &row.PayloadBytes, &row.PayloadContentBytes); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	if err := rows.Close(); err != nil {
		return nil, mapError(ctx, err)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	return result, nil
}

func (s *session) claimPreviewChangeVersions(ctx context.Context, changeIDs, versionIDs []string) ([]pgquery.ChangeVersion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT cv.organization_id,cv.id,cv.change_id,cv.number,cv.head_sha,cv.base_sha,cv.merge_base_sha,cv.repository,cv.policy_revision_id,cv.attempt_id,cv.actor_kind,cv.actor_principal_id,cv.external_provider,cv.external_id,cv.external_url,cv.created_at,cv.payload_object_key,cv.payload_sha256,cv.payload_bytes,cv.payload_content_bytes,cv.payload_format_version,cv.source_object_key,cv.source_sha256,cv.source_bytes,cv.source_content_bytes,cv.implementing_session_id,cv.implementing_machine_id
FROM change_versions cv
JOIN unnest($2::text[],$3::text[]) AS requested(change_id,version_id)
  ON cv.change_id=requested.change_id AND cv.id=requested.version_id
WHERE cv.organization_id=$1`, s.organization, pq.Array(changeIDs), pq.Array(versionIDs))
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	var result []pgquery.ChangeVersion
	for rows.Next() {
		var row pgquery.ChangeVersion
		if err := rows.Scan(&row.OrganizationID, &row.ID, &row.ChangeID, &row.Number, &row.HeadSHA, &row.BaseSHA, &row.MergeBaseSHA, &row.Repository, &row.PolicyRevisionID, &row.AttemptID, &row.ActorKind, &row.ActorPrincipalID, &row.ExternalProvider, &row.ExternalID, &row.ExternalURL, &row.CreatedAt, &row.PayloadObjectKey, &row.PayloadSHA256, &row.PayloadBytes, &row.PayloadContentBytes, &row.PayloadFormatVersion, &row.SourceObjectKey, &row.SourceSha256, &row.SourceBytes, &row.SourceContentBytes, &row.ImplementingSessionID, &row.ImplementingMachineID); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(ctx, err)
	}
	if err := rows.Close(); err != nil {
		return nil, mapError(ctx, err)
	}
	return result, nil
}
