package postgres

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ tenantstore.ClaimPreviewPlanRowsReader = (*session)(nil)

const claimPreviewBatchLimit = corestore.MaxPageSize

func (s *session) ClaimPreviewPlanRows(ctx context.Context, project tracker.ProjectID, candidates []claimstore.Candidate) ([]tenantstore.ClaimPreviewPlanRow, error) {
	if len(candidates) > claimPreviewBatchLimit {
		return nil, corestore.ErrInvalidPage
	}
	result := make([]tenantstore.ClaimPreviewPlanRow, len(candidates))
	if len(candidates) == 0 {
		return result, nil
	}
	ids, revisions := make([]int64, len(candidates)), make([]int64, len(candidates))
	for index, candidate := range candidates {
		if candidate.OrganizationID != s.organization || candidate.ProjectID != project || candidate.ID <= 0 || candidate.WorkItemID == "" {
			return nil, corestore.ErrTenantMismatch
		}
		ids[index], revisions[index] = candidate.ID, candidate.Revision
	}
	rows, err := s.queries.ClaimSnapshotRows(ctx, pgquery.ClaimSnapshotRowsParams{OrganizationID: string(s.organization), ProjectID: string(project), ItemIds: ids, Revisions: revisions})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	count := 0
	for _, record := range rows {
		if record.Ordinality != int64(count+1) || count >= len(result) || record.BoundOrganization != string(s.organization) || record.BoundProject != string(project) {
			return nil, corestore.ErrRevisionConflict
		}
		item := pgquery.Issue{
			OrganizationID:         record.ItemOrganizationID,
			ID:                     record.ItemID,
			NativeID:               record.ItemNativeID,
			ProjectID:              record.ItemProjectID,
			Number:                 record.ItemNumber,
			Revision:               record.ItemRevision,
			EventSequence:          record.ItemEventSequence,
			NativeSourceKey:        record.ItemNativeSourceKey,
			Title:                  record.ItemTitle,
			BodyObjectKey:          record.ItemBodyObjectKey,
			IssueContractObjectKey: record.ItemIssueContractObjectKey,
			ReportContextObjectKey: record.ItemReportContextObjectKey,
			ActorKind:              record.ItemActorKind,
			ActorPrincipalID:       record.ItemActorPrincipalID,
			ProvenanceProvider:     record.ItemProvenanceProvider,
			ProvenanceExternalID:   record.ItemProvenanceExternalID,
			Archived:               record.ItemArchived,
			CreatedAt:              record.ItemCreatedAt,
			UpdatedAt:              record.ItemUpdatedAt,
			NativeCreatedAt:        record.ItemNativeCreatedAt,
			NativeUpdatedAt:        record.ItemNativeUpdatedAt,
			WorkflowStateName:      record.ItemWorkflowStateName,
			LastActivityAt:         record.ItemLastActivityAt,
			IssueType:              record.ItemIssueType,
			Priority:               record.ItemPriority,
			DispatchGeneration:     record.ItemDispatchGeneration,
			DispatchRequestedAt:    record.ItemDispatchRequestedAt,
			RecoveryRunnerID:       record.ItemRecoveryRunnerID,
			RecoveryVersionID:      record.ItemRecoveryVersionID,
		}
		version := pgquery.CollaborationVersion{
			OrganizationID:      record.VersionOrganizationID,
			ProjectID:           record.VersionProjectID,
			WorkItemID:          record.VersionWorkItemID,
			RecordID:            record.VersionRecordID,
			Revision:            record.VersionRevision,
			PayloadObjectKey:    record.VersionPayloadObjectKey,
			PayloadSHA256:       record.VersionPayloadSha256,
			PayloadBytes:        record.VersionPayloadBytes,
			PayloadContentBytes: record.VersionPayloadContentBytes,
		}
		linked := pgquery.LinkedIssueSource{
			OrganizationID:       record.LinkedOrganizationID,
			ProjectID:            record.LinkedProjectID,
			WorkItemID:           record.LinkedWorkItemID,
			SourceUrl:            record.LinkedSourceUrl,
			TitleSupplied:        record.LinkedTitleSupplied,
			BodySupplied:         record.LinkedBodySupplied,
			TitleOwned:           record.LinkedTitleOwned,
			BodyOwned:            record.LinkedBodyOwned,
			SnapshotObjectKey:    record.LinkedSnapshotObjectKey,
			SnapshotSha256:       record.LinkedSnapshotSha256,
			SnapshotBytes:        record.LinkedSnapshotBytes,
			SnapshotContentBytes: record.LinkedSnapshotContentBytes,
		}
		change := pgquery.ChangeRequest{
			OrganizationID:         record.ChangeOrganizationID,
			ProjectID:              record.ChangeProjectID,
			WorkItemID:             record.ChangeWorkItemID,
			ID:                     record.ChangeID,
			Revision:               record.ChangeRevision,
			Title:                  record.ChangeTitle,
			CurrentVersionID:       record.ChangeCurrentVersionID,
			EvidenceSequence:       record.ChangeEvidenceSequence,
			CreatedAt:              record.ChangeCreatedAt,
			UpdatedAt:              record.ChangeUpdatedAt,
			PayloadObjectKey:       record.ChangePayloadObjectKey,
			PayloadSHA256:          record.ChangePayloadSha256,
			PayloadBytes:           record.ChangePayloadBytes,
			PayloadContentBytes:    record.ChangePayloadContentBytes,
			LandedVersionID:        record.ChangeLandedVersionID,
			LandedAt:               record.ChangeLandedAt,
			LandedHeadSHA:          record.ChangeLandedHeadSha,
			LandedMergeSHA:         record.ChangeLandedMergeSha,
			LandedBaseRef:          record.ChangeLandedBaseRef,
			ReviewSummarySequence:  record.ChangeReviewSummarySequence,
			ReviewEvidenceRevision: record.ChangeReviewEvidenceRevision,
		}
		changeVersion := pgquery.ChangeVersion{
			OrganizationID:        record.ChangeVersionOrganizationID,
			ID:                    record.ChangeVersionID,
			ChangeID:              record.ChangeVersionChangeID,
			Number:                record.ChangeVersionNumber,
			HeadSHA:               record.ChangeVersionHeadSha,
			BaseSHA:               record.ChangeVersionBaseSha,
			MergeBaseSHA:          record.ChangeVersionMergeBaseSha,
			Repository:            record.ChangeVersionRepository,
			PolicyRevisionID:      record.ChangeVersionPolicyRevisionID,
			AttemptID:             record.ChangeVersionAttemptID,
			ActorKind:             record.ChangeVersionActorKind,
			ActorPrincipalID:      record.ChangeVersionActorPrincipalID,
			ExternalProvider:      record.ChangeVersionExternalProvider,
			ExternalID:            record.ChangeVersionExternalID,
			ExternalURL:           record.ChangeVersionExternalUrl,
			CreatedAt:             record.ChangeVersionCreatedAt,
			PayloadObjectKey:      record.ChangeVersionPayloadObjectKey,
			PayloadSHA256:         record.ChangeVersionPayloadSha256,
			PayloadBytes:          record.ChangeVersionPayloadBytes,
			PayloadContentBytes:   record.ChangeVersionPayloadContentBytes,
			PayloadFormatVersion:  record.ChangeVersionPayloadFormatVersion,
			SourceObjectKey:       record.ChangeVersionSourceObjectKey,
			SourceSha256:          record.ChangeVersionSourceSha256,
			SourceBytes:           record.ChangeVersionSourceBytes,
			SourceContentBytes:    record.ChangeVersionSourceContentBytes,
			ImplementingSessionID: record.ChangeVersionImplementingSessionID,
			ImplementingMachineID: record.ChangeVersionImplementingMachineID,
		}
		policy := pgquery.PolicyRevision{
			OrganizationID:        record.PolicyOrganizationID,
			ProjectID:             record.PolicyProjectID,
			PolicyID:              record.PolicyPolicyID,
			ApprovedBy:            record.PolicyApprovedBy,
			ApprovedAt:            record.PolicyApprovedAt,
			PayloadObjectKey:      record.PolicyPayloadObjectKey,
			PayloadSHA256:         record.PolicyPayloadSha256,
			PayloadBytes:          record.PolicyPayloadBytes,
			PayloadContentBytes:   record.PolicyPayloadContentBytes,
			GateGithubPullRequest: record.PolicyGateGithubPullRequest,
		}
		attempt := pgquery.NativeAttempt{
			ClaimDisposition:       record.AttemptClaimDisposition,
			LocalAttemptID:         record.AttemptLocalAttemptID,
			LocalAttemptIDRecorded: record.AttemptLocalAttemptIDRecorded,
			OrganizationID:         record.AttemptOrganizationID,
			ID:                     record.AttemptID,
			IssueID:                record.AttemptIssueID,
			LeaseID:                record.AttemptLeaseID,
			FencingToken:           record.AttemptFencingToken,
			RunID:                  record.AttemptRunID,
			Sequence:               record.AttemptSequence,
			Status:                 record.AttemptStatus,
			WorkItemRevision:       record.AttemptWorkItemRevision,
			DispatchGeneration:     record.AttemptDispatchGeneration,
			PayloadObjectKey:       record.AttemptPayloadObjectKey,
			CheckpointObjectKey:    record.AttemptCheckpointObjectKey,
			ArtifactsObjectKey:     record.AttemptArtifactsObjectKey,
			StartedAt:              record.AttemptStartedAt,
			UpdatedAt:              record.AttemptUpdatedAt,
			StartedAtSubmicroNs:    record.AttemptStartedAtSubmicroNs,
			RunnerTokensRecorded:   record.AttemptRunnerTokensRecorded,
			CleanupAcknowledgedAt:  record.AttemptCleanupAcknowledgedAt,
			RuntimeHeartbeatAt:     record.AttemptRuntimeHeartbeatAt,
			RuntimeSummarySequence: record.AttemptRuntimeSummarySequence,
		}
		event := pgquery.CollaborationEvent{
			HistoryHidden:        record.EventHistoryHidden,
			OrganizationID:       record.EventOrganizationID,
			ProjectID:            record.EventProjectID,
			WorkItemID:           record.EventWorkItemID,
			ID:                   record.EventID,
			Sequence:             record.EventSequence,
			Type:                 record.EventType,
			SchemaVersion:        record.EventSchemaVersion,
			ActorKind:            record.EventActorKind,
			ActorPrincipalID:     record.EventActorPrincipalID,
			EventRevision:        record.EventEventRevision,
			ChangeID:             record.EventChangeID,
			VersionID:            record.EventVersionID,
			HeadSHA:              record.EventHeadSha,
			RunID:                record.EventRunID,
			AttemptID:            record.EventAttemptID,
			FromState:            record.EventFromState,
			ToState:              record.EventToState,
			RecordedAt:           record.EventRecordedAt,
			PayloadObjectKey:     record.EventPayloadObjectKey,
			PayloadSHA256:        record.EventPayloadSha256,
			PayloadBytes:         record.EventPayloadBytes,
			PayloadContentBytes:  record.EventPayloadContentBytes,
			PayloadFormatVersion: record.EventPayloadFormatVersion,
			LandingWaiting:       record.EventLandingWaiting,
			LandingChangeID:      record.EventLandingChangeID,
			LandingVersionID:     record.EventLandingVersionID,
			LandingHeadSHA:       record.EventLandingHeadSha,
			ProjectSequence:      record.EventProjectSequence,
			PushKnown:            record.EventPushKnown,
			PushChangeID:         record.EventPushChangeID,
			PushVersionID:        record.EventPushVersionID,
			PushHeadSha:          record.EventPushHeadSha,
			PushExternalEffect:   record.EventPushExternalEffect,
			PushEffectState:      record.EventPushEffectState,
			SchedulerOutcome:     record.EventSchedulerOutcome,
			ActorEnvelopeKnown:   record.EventActorEnvelopeKnown,
			ActorViaKey:          record.EventActorViaKey,
			ActorDisplayName:     record.EventActorDisplayName,
			ActorAgent:           record.EventActorAgent,
		}
		publication := pgquery.NativePrPublication{
			OrganizationID:    record.PublicationOrganizationID,
			VersionID:         record.PublicationVersionID,
			ChangeID:          record.PublicationChangeID,
			Repository:        record.PublicationRepository,
			BaseRef:           record.PublicationBaseRef,
			Branch:            record.PublicationBranch,
			HeadSHA:           record.PublicationHeadSha,
			PolicyID:          record.PublicationPolicyID,
			SourceAttemptID:   record.PublicationSourceAttemptID,
			RecorderAttemptID: record.PublicationRecorderAttemptID,
			Provider:          record.PublicationProvider,
			ExternalID:        record.PublicationExternalID,
			ExternalURL:       record.PublicationExternalUrl,
		}
		landing := pgquery.CollaborationEvent{
			OrganizationID:       record.LandingOrganizationID,
			ProjectID:            record.LandingProjectID,
			WorkItemID:           record.LandingWorkItemID,
			ID:                   record.LandingID,
			Sequence:             record.LandingSequence,
			Type:                 record.LandingType,
			SchemaVersion:        record.LandingSchemaVersion,
			ActorKind:            record.LandingActorKind,
			ActorPrincipalID:     record.LandingActorPrincipalID,
			EventRevision:        record.LandingEventRevision,
			ChangeID:             record.LandingChangeID,
			VersionID:            record.LandingVersionID,
			HeadSHA:              record.LandingHeadSHA,
			RunID:                record.LandingRunID,
			AttemptID:            record.LandingAttemptID,
			FromState:            record.LandingFromState,
			ToState:              record.LandingToState,
			RecordedAt:           record.LandingRecordedAt,
			PayloadObjectKey:     record.LandingPayloadObjectKey,
			PayloadSHA256:        record.LandingPayloadSha256,
			PayloadBytes:         record.LandingPayloadBytes,
			PayloadContentBytes:  record.LandingPayloadContentBytes,
			PayloadFormatVersion: record.LandingPayloadFormatVersion,
			LandingWaiting:       record.LandingLandingWaiting,
			LandingChangeID:      record.LandingLandingChangeID,
			LandingVersionID:     record.LandingLandingVersionID,
			LandingHeadSHA:       record.LandingLandingHeadSha,
			ProjectSequence:      record.LandingProjectSequence,
			PushKnown:            record.LandingPushKnown,
			PushChangeID:         record.LandingPushChangeID,
			PushVersionID:        record.LandingPushVersionID,
			PushHeadSha:          record.LandingPushHeadSha,
			PushExternalEffect:   record.LandingPushExternalEffect,
			PushEffectState:      record.LandingPushEffectState,
			SchedulerOutcome:     record.LandingSchedulerOutcome,
			ActorEnvelopeKnown:   record.LandingActorEnvelopeKnown,
			ActorViaKey:          record.LandingActorViaKey,
			ActorDisplayName:     record.LandingActorDisplayName,
			ActorAgent:           record.LandingActorAgent,
		}
		review := pgquery.ChangeReviewPolicyRevision{
			OrganizationID:      record.ReviewOrganizationID,
			ProjectID:           record.ReviewProjectID,
			ReviewPolicyID:      record.ReviewReviewPolicyID,
			PolicyID:            record.ReviewPolicyID,
			Revision:            record.ReviewRevision,
			UpdatedBy:           record.ReviewUpdatedBy,
			UpdatedAt:           record.ReviewUpdatedAt,
			PayloadObjectKey:    record.ReviewPayloadObjectKey,
			PayloadSHA256:       record.ReviewPayloadSha256,
			PayloadBytes:        record.ReviewPayloadBytes,
			PayloadContentBytes: record.ReviewPayloadContentBytes,
		}
		fromState, targetState := record.ObservationFromState, record.ObservationTargetState

		candidate := candidates[count]
		row := &result[count]
		count++
		row.Item = corestore.WorkItem(item)
		switch {
		case !record.ItemPresent || item.NativeID != candidate.WorkItemID:
			row.Err = fmt.Errorf("read work item: %w", corestore.ErrNotFound)
		case item.Revision != candidate.Revision || item.Archived:
			continue
		case !record.VersionPresent:
			row.Err = fmt.Errorf("read work item version: %w", corestore.ErrNotFound)
		case item.NativeSourceKey != nil && !record.LinkedPresent:
			row.Err = fmt.Errorf("read linked source: %w", corestore.ErrNotFound)
		case change.CurrentVersionID != nil && !record.ChangeVersionPresent:
			row.Err = fmt.Errorf("read change version: %w", corestore.ErrNotFound)
		case record.ChangeVersionPresent && !record.PolicyPresent:
			row.Err = fmt.Errorf("read change policy revision: %w", corestore.ErrNotFound)
		}
		row.Version = eventstore.CollaborationVersion(version)
		if record.LinkedPresent {
			value := linkedIssueSourceRecord(linked)
			row.LinkedSource = &value
		}
		if record.ChangePresent {
			value := eventstore.Change{OrganizationID: change.OrganizationID, ProjectID: change.ProjectID, WorkItemID: change.WorkItemID, ID: change.ID, Revision: change.Revision, Title: change.Title, CurrentVersionID: change.CurrentVersionID, EvidenceSequence: change.EvidenceSequence, CreatedAt: change.CreatedAt, UpdatedAt: change.UpdatedAt, PayloadObjectKey: change.PayloadObjectKey, PayloadSHA256: change.PayloadSHA256, PayloadBytes: change.PayloadBytes, PayloadContentBytes: change.PayloadContentBytes}
			row.Change = &value
		}
		if record.ChangeVersionPresent {
			value := versionRecord(changeVersion)
			row.ChangeVersion = &value
			row.Observations = &tenantstore.ChangeVersionRows{OrganizationID: s.organization, ProjectID: project, Request: tenantstore.ChangeVersionRequest{WorkItemID: item.NativeID, Reference: tracker.NativeChangeReference{ChangeID: change.ID, VersionID: changeVersion.ID, HeadSHA: changeVersion.HeadSHA}}}
			if record.PublicationPresent {
				row.Observations.Publication = &eventstore.NativePRPublication{OrganizationID: publication.OrganizationID, VersionID: publication.VersionID, ChangeID: publication.ChangeID, Repository: publication.Repository, BaseRef: publication.BaseRef, Branch: publication.Branch, HeadSHA: publication.HeadSHA, PolicyID: publication.PolicyID, SourceAttemptID: stringValue(publication.SourceAttemptID), RecorderAttemptID: publication.RecorderAttemptID, External: tracker.ChangeExternalReference{Provider: publication.Provider, ID: publication.ExternalID, URL: publication.ExternalURL}}
			}
			if record.LandingPresent {
				row.Observations.Landing = &eventstore.LandingObservation{Event: eventRecord(landing), FromState: fromState, TargetState: targetState}
			}
		}
		if record.PolicyPresent {
			value := projectPolicyRecord(policy)
			row.ChangePolicyRevision = &value
		}
		if record.AttemptPresent {
			value := attemptFromPG(attempt)
			row.LatestAttempt = &value
		}
		if record.EventPresent {
			value := eventRecord(event)
			row.LatestRunEvent = &value
		}
		if record.ReviewPresent {
			value := reviewPolicyRecord(review)
			row.ReviewPolicy = &value
		}
	}
	if count != len(result) {
		return nil, corestore.ErrRevisionConflict
	}
	return result, nil
}

func (s *session) claimPreviewChangeVersions(ctx context.Context, changeIDs, versionIDs []string) ([]pgquery.ChangeVersion, error) {
	rows, err := s.db.Query(ctx, `SELECT cv.organization_id,cv.id,cv.change_id,cv.number,cv.head_sha,cv.base_sha,cv.merge_base_sha,cv.repository,cv.policy_revision_id,cv.attempt_id,cv.actor_kind,cv.actor_principal_id,cv.external_provider,cv.external_id,cv.external_url,cv.created_at,cv.payload_object_key,cv.payload_sha256,cv.payload_bytes,cv.payload_content_bytes,cv.payload_format_version,cv.source_object_key,cv.source_sha256,cv.source_bytes,cv.source_content_bytes,cv.implementing_session_id,cv.implementing_machine_id
FROM change_versions cv
JOIN unnest($2::text[],$3::text[]) AS requested(change_id,version_id)
  ON cv.change_id=requested.change_id AND cv.id=requested.version_id
WHERE cv.organization_id=$1`, s.organization, changeIDs, versionIDs)
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
	rows.Close()
	return result, nil
}
