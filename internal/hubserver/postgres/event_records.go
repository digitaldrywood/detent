package postgres

import (
	"cmp"
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ eventstore.Repository = (*Repository)(nil)

func (r *Repository) ReadRecords(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(eventstore.Reader) error) error {
	return r.transaction(ctx, organization, workload, true, func(s *session) error { return read(s) })
}
func (r *Repository) WriteRecords(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(eventstore.Writer) error) error {
	return r.transaction(ctx, organization, workload, false, func(s *session) error { return write(s) })
}

func validRecordPayload(organization tracker.OrganizationID, key, digest string, compressed, content int64) error {
	ref := eventstore.Reference(key, digest, compressed, content)
	if ref.Validate(string(organization)) != nil {
		return eventstore.ErrInvalidRecord
	}
	return nil
}

func recordTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC().Truncate(time.Microsecond)
	}
	return value.UTC().Truncate(time.Microsecond)
}

func recordTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := recordTime(*value)
	return &result
}
func (s *session) Events(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, after int64, limit int) ([]eventstore.Event, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.RecordListEvent(ctx, pgquery.RecordListEventParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, After: after, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	records := make([]eventstore.Event, len(rows))
	for i, row := range rows {
		records[i] = eventRecord(row)
	}
	return records, nil
}

func (s *session) ProjectEvents(ctx context.Context, project tracker.ProjectID, after int64, limit int) ([]eventstore.Event, error) {
	if project == "" || after < 0 || limit < 1 || limit > 100 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.RecordProjectEvents(ctx, pgquery.RecordProjectEventsParams{OrganizationID: s.organization, ProjectID: project, AfterProjectSequence: after, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	records := make([]eventstore.Event, len(rows))
	for i, row := range rows {
		records[i] = eventRecord(row)
	}
	return records, nil
}

func (s *session) EventsBefore(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, before int64, limit int) ([]eventstore.Event, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.RecordListEventBefore(ctx, pgquery.RecordListEventBeforeParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, Before: before, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	records := make([]eventstore.Event, len(rows))
	for i, row := range rows {
		records[i] = eventRecord(row)
	}
	return records, nil
}

func (s *session) HistoryEvents(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, after int64, limit int) ([]eventstore.Event, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.RecordListHistoryEvent(ctx, pgquery.RecordListHistoryEventParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, After: after, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	records := make([]eventstore.Event, len(rows))
	for i, row := range rows {
		records[i] = eventRecord(row)
	}
	return records, nil
}

func (s *session) HistoryEventsBefore(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, before int64, limit int) ([]eventstore.Event, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.RecordListHistoryEventBefore(ctx, pgquery.RecordListHistoryEventBeforeParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, Before: before, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	records := make([]eventstore.Event, len(rows))
	for i, row := range rows {
		records[i] = eventRecord(row)
	}
	return records, nil
}

func (s *session) ProjectEventSequence(ctx context.Context, project tracker.ProjectID) (int64, error) {
	sequence, err := s.queries.RecordProjectEventSequence(ctx, pgquery.RecordProjectEventSequenceParams{OrganizationID: s.organization, ProjectID: project})
	return sequence, mapError(ctx, err)
}

func (s *session) ProjectChanges(ctx context.Context, project tracker.ProjectID, after, through int64, since time.Time, limit int) ([]eventstore.ProjectChange, error) {
	if limit < 1 || limit > corestore.MaxPageSize+1 || project == "" || after < 0 || through < after || through-after > 200 || since.IsZero() {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.RecordProjectChanges(ctx, pgquery.RecordProjectChangesParams{OrganizationID: s.organization, ProjectID: project, After: after, Through: through, Since: since, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	changes := make([]eventstore.ProjectChange, len(rows))
	for index, row := range rows {
		changes[index] = eventstore.ProjectChange{WorkItemID: row.NativeID, Revision: row.Revision, LastActivityAt: row.LastActivityAt}
	}
	return changes, nil
}

func (s *session) EventsByKey(ctx context.Context, keys []eventstore.EventKey) (map[string]eventstore.Event, error) {
	events := make(map[string]eventstore.Event, len(keys))
	if len(keys) == 0 {
		return events, nil
	}
	requested := make(map[string]eventstore.EventKey, len(keys))
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		if key.ID == "" || key.ProjectID == "" || key.WorkItemID == "" {
			return nil, eventstore.ErrInvalidRecord
		}
		if previous, ok := requested[key.ID]; ok {
			if previous != key {
				return nil, eventstore.ErrInvalidRecord
			}
			continue
		}
		requested[key.ID] = key
		ids = append(ids, key.ID)
	}
	if len(ids) > 100000 {
		return nil, eventstore.ErrInvalidRecord
	}
	rows, err := s.queries.RecordGetEvents(ctx, pgquery.RecordGetEventsParams{OrganizationID: s.organization, Ids: ids, RowLimit: int32(len(ids))})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	for _, row := range rows {
		key := requested[row.ID]
		if row.ProjectID != key.ProjectID || row.WorkItemID != key.WorkItemID {
			continue
		}
		events[row.ID] = eventRecord(row)
	}
	return events, nil
}

func (s *session) RunEvent(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, id string) (eventstore.Event, error) {
	row, err := s.itemEvent(ctx, project, item, id)
	return eventRecord(row), mapError(ctx, err)
}

func (s *session) itemEvent(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, id string) (pgquery.CollaborationEvent, error) {
	row, err := s.queries.RecordGetEvent(ctx, pgquery.RecordGetEventParams{OrganizationID: s.organization, ID: id})
	if err == nil && (row.ProjectID != project || row.WorkItemID != item) {
		return pgquery.CollaborationEvent{}, pgx.ErrNoRows
	}
	return row, err
}

func (s *session) ProgressEvidenceSequence(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (int64, error) {
	var sequence int64
	err := s.db.QueryRow(ctx, `SELECT COALESCE((SELECT sequence FROM collaboration_events
WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3
AND (type IN ('issue.edited','comment.created','comment.edited','run.checkpointed','run.finished') OR left(type,7)='change.')
AND NOT (type='comment.created' AND actor_kind='machine' AND actor_envelope_known AND actor_agent='progress-check')
ORDER BY sequence DESC LIMIT 1),0)`, s.organization, project, item).Scan(&sequence)
	return sequence, mapError(ctx, err)
}

func (s *session) LatestRunEvent(ctx context.Context, project tracker.ProjectID, attempt string) (eventstore.Event, error) {
	row, err := s.queries.RecordLatestRunEvent(ctx, pgquery.RecordLatestRunEventParams{
		OrganizationID: s.organization,
		ProjectID:      project,
		AttemptID:      attempt,
	})
	return eventRecord(row), mapError(ctx, err)
}

func (s *session) LatestCheckpointEvents(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, attempts []string) ([]eventstore.Event, error) {
	if len(attempts) == 0 || len(attempts) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.RecordLatestCheckpointEvents(ctx, pgquery.RecordLatestCheckpointEventsParams{
		OrganizationID: s.organization,
		ProjectID:      project,
		WorkItemID:     item,
		AttemptIds:     attempts,
	})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	events := make([]eventstore.Event, len(rows))
	for i, row := range rows {
		events[i] = eventRecord(row)
	}
	return events, nil
}

func (s *session) Versions(ctx context.Context, change string, after int64, limit int) ([]eventstore.Version, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.RecordListVersion(ctx, pgquery.RecordListVersionParams{OrganizationID: s.organization, ChangeID: change, After: after, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	records := make([]eventstore.Version, len(rows))
	for i, row := range rows {
		records[i] = versionRecord(row)
	}
	return records, nil
}
func (s *session) Version(ctx context.Context, change, id string) (eventstore.Version, error) {
	row, err := s.queries.RecordGetVersionByID(ctx, pgquery.RecordGetVersionByIDParams{OrganizationID: s.organization, ChangeID: change, ID: id})
	return versionRecord(row), mapError(ctx, err)
}

func (s *session) LandingReceipt(ctx context.Context, attemptID string) (eventstore.LandingReceipt, error) {
	row, err := s.queries.RecordGetLandingReceipt(ctx, pgquery.RecordGetLandingReceiptParams{OrganizationID: s.organization, AttemptID: attemptID})
	return landingReceiptRecord(row), mapError(ctx, err)
}

func (s *session) LatestLandingReceipt(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (eventstore.LandingReceipt, error) {
	row, err := s.queries.RecordGetLatestLandingReceiptForWorkItem(ctx, pgquery.RecordGetLatestLandingReceiptForWorkItemParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item})
	return landingReceiptRecord(row), mapError(ctx, err)
}

func (s *session) CreateLandingReceipt(ctx context.Context, record eventstore.LandingReceipt) (eventstore.LandingReceipt, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return eventstore.LandingReceipt{}, err
	}
	if record.AttemptID == "" || record.VersionID == "" || len(record.MergeSHA) != 40 && len(record.MergeSHA) != 64 || !artifact.ValidHash(record.MergeSHA, len(record.MergeSHA)) {
		return eventstore.LandingReceipt{}, eventstore.ErrInvalidRecord
	}
	if err := validRecordPayload(record.OrganizationID, record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes); err != nil {
		return eventstore.LandingReceipt{}, err
	}
	versionItem, err := s.queries.RecordGetVersionWorkItemID(ctx, pgquery.RecordGetVersionWorkItemIDParams{OrganizationID: s.organization, ID: record.VersionID})
	if err != nil {
		return eventstore.LandingReceipt{}, mapError(ctx, err)
	}
	landing, err := s.Attempt(ctx, record.AttemptID)
	if errors.Is(err, corestore.ErrNotFound) {
		return eventstore.LandingReceipt{}, eventstore.ErrInvalidRecord
	}
	if err != nil {
		return eventstore.LandingReceipt{}, err
	}
	if landing.IssueID != versionItem.IssueID {
		return eventstore.LandingReceipt{}, eventstore.ErrInvalidRecord
	}
	if (record.Repository == "") != (record.BaseRef == "") {
		return eventstore.LandingReceipt{}, eventstore.ErrInvalidRecord
	}
	identity := landingRepositoryIdentity(versionItem.Repository)
	if record.Repository != "" && identity != "" && landingRepositoryIdentity(record.Repository) != identity {
		return eventstore.LandingReceipt{}, eventstore.ErrInvalidRecord
	}
	if record.Repository != "" {
		identity = landingRepositoryIdentity(record.Repository)
	}
	row, err := s.queries.RecordInsertLandingReceipt(ctx, pgquery.RecordInsertLandingReceiptParams{ProjectID: versionItem.ProjectID, IssueID: landing.IssueID, ChangeID: versionItem.ChangeID, FencingToken: landing.FencingToken, RepositoryIdentity: identity, OrganizationID: s.organization, AttemptID: record.AttemptID, VersionID: record.VersionID, MergeSha: record.MergeSHA, Repository: record.Repository, BaseRef: record.BaseRef, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes})
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = s.queries.RecordGetLandingReceipt(ctx, pgquery.RecordGetLandingReceiptParams{OrganizationID: s.organization, AttemptID: record.AttemptID})
		if err == nil && landingReceiptRecord(row) != record {
			return eventstore.LandingReceipt{}, corestore.ErrRevisionConflict
		}
	}
	return landingReceiptRecord(row), mapError(ctx, err)
}

func landingReceiptRecord(row pgquery.ChangeLandingReceipt) eventstore.LandingReceipt {
	return eventstore.LandingReceipt{OrganizationID: row.OrganizationID, AttemptID: row.AttemptID, VersionID: row.VersionID, MergeSHA: row.MergeSha, Repository: row.Repository, BaseRef: row.BaseRef, PayloadObjectKey: row.PayloadObjectKey, PayloadSHA256: row.PayloadSHA256, PayloadBytes: row.PayloadBytes, PayloadContentBytes: row.PayloadContentBytes}
}

func (s *session) ProjectPolicyLanding(ctx context.Context, project tracker.ProjectID, repository, mergeSHA, baseRef string) (eventstore.PolicyLanding, error) {
	if project == "" || repository == "" || mergeSHA == "" {
		return eventstore.PolicyLanding{}, eventstore.ErrInvalidRecord
	}
	repository = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(repository)), "https://github.com/"), "git@github.com:"), ".git")
	repositories := []string{repository, "https://github.com/" + repository, "https://github.com/" + repository + ".git", "git@github.com:" + repository + ".git"}
	var version pgquery.ChangeVersion
	var item tracker.NativeWorkItemID
	if baseRef == "" {
		row, err := s.queries.RecordProjectPolicyLandingHistory(ctx, pgquery.RecordProjectPolicyLandingHistoryParams{OrganizationID: s.organization, ProjectID: project, MergeSha: mergeSHA, Repositories: repositories})
		if errors.Is(err, pgx.ErrNoRows) {
			return s.pullRequestPolicyLanding(ctx, project, repository, mergeSHA, baseRef)
		}
		if err != nil {
			return eventstore.PolicyLanding{}, mapError(ctx, err)
		}
		version, item = row.ChangeVersion, tracker.NativeWorkItemID(row.WorkItemID)
	} else {
		row, err := s.queries.RecordProjectPolicyLanding(ctx, pgquery.RecordProjectPolicyLandingParams{OrganizationID: s.organization, ProjectID: project, MergeSha: mergeSHA, Repositories: repositories, BaseRef: baseRef})
		if errors.Is(err, pgx.ErrNoRows) {
			return s.pullRequestPolicyLanding(ctx, project, repository, mergeSHA, baseRef)
		}
		if err != nil {
			return eventstore.PolicyLanding{}, mapError(ctx, err)
		}
		version, item = row.ChangeVersion, tracker.NativeWorkItemID(row.WorkItemID)
	}
	record := versionRecord(version)
	if !record.LandingHistoryKnown || record.FrozenLanding == nil {
		return eventstore.PolicyLanding{}, eventstore.ErrInvalidRecord
	}
	frozen := *record.FrozenLanding
	frozen.WorkItemID = item
	return eventstore.PolicyLanding{ChangeID: record.ChangeID, PullRequestURL: record.ExternalURL, Native: &frozen}, nil
}

func (s *session) pullRequestPolicyLanding(ctx context.Context, project tracker.ProjectID, repository, mergeSHA, baseRef string) (eventstore.PolicyLanding, error) {
	query := `SELECT pull_number,merged_by FROM github_pull_request_refresh WHERE organization_id=$1 AND project_id=$2 AND repository=$3 AND merge_sha=$4 AND merged_at IS NOT NULL AND merge_policy_bound AND state IN ('merged','closed')`
	args := []any{s.organization, project, repository, mergeSHA}
	if baseRef != "" {
		query += ` AND base_ref=$5 AND repository_default_branch=$5 AND EXISTS(SELECT 1 FROM project_settings ps WHERE ps.organization_id=$1 AND ps.project_id=$2 AND lower(ps.repository)=$3)`
		args = append(args, baseRef)
	}
	query += ` ORDER BY pull_number,change_id NULLS FIRST LIMIT 1`
	var number int64
	var actor string
	if err := s.db.QueryRow(ctx, query, args...).Scan(&number, &actor); err != nil {
		return eventstore.PolicyLanding{}, mapError(ctx, err)
	}
	return eventstore.PolicyLanding{PullRequestURL: "https://github.com/" + repository + "/pull/" + strconv.FormatInt(number, 10), Actor: actor}, nil
}

func (s *session) ViewedFiles(ctx context.Context, versionID, principalID string, limit int) ([]eventstore.ViewedFile, error) {
	if limit < 1 || limit > 4097 || versionID == "" || principalID == "" {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.RecordListViewedFiles(ctx, pgquery.RecordListViewedFilesParams{OrganizationID: s.organization, VersionID: versionID, PrincipalID: principalID, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]eventstore.ViewedFile, 0, len(rows))
	for _, row := range rows {
		result = append(result, viewedFileRecord(row))
	}
	return result, nil
}

func (s *session) SetViewedFile(ctx context.Context, record eventstore.ViewedFile) (eventstore.ViewedFile, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return eventstore.ViewedFile{}, err
	}
	if record.ProjectID == "" || record.ChangeID == "" || record.VersionID == "" || record.PrincipalID == "" || !artifact.ValidHash(record.ManifestSHA256, 64) || !artifact.ValidHash(record.FileSHA256, 64) || record.UpdatedAt.IsZero() {
		return eventstore.ViewedFile{}, eventstore.ErrInvalidRecord
	}
	row, err := s.queries.RecordSetViewedFile(ctx, pgquery.RecordSetViewedFileParams{OrganizationID: s.organization, ProjectID: record.ProjectID, ChangeID: record.ChangeID, VersionID: record.VersionID, PrincipalID: record.PrincipalID, ManifestSha256: record.ManifestSHA256, FileSha256: record.FileSHA256, Viewed: record.Viewed, UpdatedAt: recordTime(record.UpdatedAt)})
	return viewedFileRecord(row), mapError(ctx, err)
}

func viewedFileRecord(row pgquery.ChangeViewedFile) eventstore.ViewedFile {
	return eventstore.ViewedFile{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ChangeID: row.ChangeID, VersionID: row.VersionID, PrincipalID: row.PrincipalID, ManifestSHA256: row.ManifestSha256, FileSHA256: row.FileSha256, Viewed: row.Viewed, UpdatedAt: row.UpdatedAt}
}

func versionRecord(row pgquery.ChangeVersion) eventstore.Version {
	record := eventstore.Version{
		LandingHistoryKnown: row.LandingHistoryKnown, OrganizationID: row.OrganizationID,
		ID:                    row.ID,
		ChangeID:              row.ChangeID,
		Number:                row.Number,
		HeadSHA:               row.HeadSHA,
		BaseSHA:               row.BaseSHA,
		MergeBaseSHA:          row.MergeBaseSHA,
		Repository:            row.Repository,
		PolicyRevisionID:      row.PolicyRevisionID,
		AttemptID:             row.AttemptID,
		ActorKind:             row.ActorKind,
		ActorPrincipalID:      row.ActorPrincipalID,
		ExternalProvider:      row.ExternalProvider,
		ExternalID:            row.ExternalID,
		ExternalURL:           row.ExternalURL,
		CreatedAt:             row.CreatedAt,
		PayloadObjectKey:      row.PayloadObjectKey,
		PayloadSHA256:         row.PayloadSHA256,
		PayloadBytes:          row.PayloadBytes,
		PayloadContentBytes:   row.PayloadContentBytes,
		PayloadFormatVersion:  row.PayloadFormatVersion,
		ImplementingSessionID: row.ImplementingSessionID,
		ImplementingMachineID: row.ImplementingMachineID,
	}
	if row.LandedAt != nil {
		record.FrozenLanding = &eventstore.VersionLanding{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, ChangeID: row.ChangeID, VersionID: row.ID, At: row.LandedAt.UTC(), HeadSHA: row.LandedHeadSHA, MergeSHA: row.LandedMergeSHA, BaseRef: row.LandedBaseRef, PayloadObjectKey: row.LandedObjectKey, PayloadSHA256: row.LandedSha256, PayloadBytes: row.LandedBytes, PayloadContentBytes: row.LandedContentBytes, PayloadFormatVersion: row.LandedPayloadFormatVersion}
	}
	if row.SourceObjectKey != nil {
		record.SourceObjectKey = *row.SourceObjectKey
	}
	if row.SourceSha256 != nil {
		record.SourceSHA256 = *row.SourceSha256
	}
	if row.SourceBytes.Valid {
		record.SourceBytes = row.SourceBytes.Int64
	}
	if row.SourceContentBytes.Valid {
		record.SourceContentBytes = row.SourceContentBytes.Int64
	}
	return record
}
func (s *session) Evidence(ctx context.Context, change string, after int64, limit int) ([]eventstore.Evidence, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.RecordListEvidence(ctx, pgquery.RecordListEvidenceParams{OrganizationID: s.organization, ChangeID: change, After: after, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	records := make([]eventstore.Evidence, len(rows))
	for i, row := range rows {
		records[i] = evidenceRecord(row)
	}
	return records, nil
}
func (s *session) EvidenceOfKind(ctx context.Context, change, kind, version string, includeUnversioned bool, after int64, limit int) ([]eventstore.Evidence, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.RecordListEvidenceOfKind(ctx, pgquery.RecordListEvidenceOfKindParams{OrganizationID: s.organization, ChangeID: change, Kind: kind, VersionID: version, IncludeUnversioned: includeUnversioned, After: after, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	records := make([]eventstore.Evidence, len(rows))
	for i, row := range rows {
		records[i] = evidenceRecord(row)
	}
	return records, nil
}

func (s *session) CurrentVersionGateEvidence(ctx context.Context, change, version string, through int64) ([]eventstore.Evidence, error) {
	rows, err := s.db.Query(ctx, `SELECT * FROM (
(SELECT organization_id,change_id,sequence,version_id,kind,source_key,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,payload_format_version,review_kind,review_landing,review_session_known,review_session_id,review_session_machine,project_id,quality_at
FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND version_id=$3 AND sequence<=$4 AND kind='review' ORDER BY sequence DESC LIMIT 65)
UNION ALL
(SELECT organization_id,change_id,sequence,version_id,kind,source_key,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,payload_format_version,review_kind,review_landing,review_session_known,review_session_id,review_session_machine,project_id,quality_at
FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND version_id=$3 AND sequence<=$4 AND kind='check' ORDER BY sequence DESC LIMIT 65)
) evidence ORDER BY sequence`, s.organization, change, version, through)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	values, err := pgx.CollectRows(rows, pgx.RowToStructByName[pgquery.ChangeEvidence])
	if err != nil {
		return nil, mapError(ctx, err)
	}
	records := make([]eventstore.Evidence, len(values))
	for i, row := range values {
		records[i] = evidenceRecord(row)
	}
	return records, nil
}
func (s *session) EvidenceBySource(ctx context.Context, change, kind, source string) (eventstore.Evidence, error) {
	row, err := s.queries.RecordGetEvidenceBySource(ctx, pgquery.RecordGetEvidenceBySourceParams{OrganizationID: s.organization, ChangeID: change, Kind: kind, SourceKey: &source})
	return evidenceRecord(row), mapError(ctx, err)
}
func (s *session) Links(ctx context.Context, change string, after tracker.NativeWorkItemID, limit int) ([]eventstore.Link, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.RecordListLink(ctx, pgquery.RecordListLinkParams{OrganizationID: s.organization, ChangeID: change, After: after, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	records := make([]eventstore.Link, len(rows))
	for i, row := range rows {
		records[i] = eventstore.Link(row)
	}
	return records, nil
}
func (s *session) Change(ctx context.Context, id string) (eventstore.Change, error) {
	row, err := s.queries.RecordGetChange(ctx, pgquery.RecordGetChangeParams{OrganizationID: s.organization, ID: id})
	return eventstore.Change(row), mapError(ctx, err)
}

func (s *session) LockChange(ctx context.Context, id string) (eventstore.Change, error) {
	row, err := s.queries.RecordLockChange(ctx, pgquery.RecordLockChangeParams{OrganizationID: s.organization, ID: id})
	return eventstore.Change(row), mapError(ctx, err)
}

func (s *session) LatestChange(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (eventstore.Change, error) {
	row, err := s.queries.RecordLatestChange(ctx, pgquery.RecordLatestChangeParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item})
	return eventstore.Change(row), mapError(ctx, err)
}

func (s *session) WorkItemPullRequestVersion(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (eventstore.PullRequestVersion, error) {
	row, err := s.queries.RecordWorkItemPullRequestVersion(ctx, pgquery.RecordWorkItemPullRequestVersionParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item})
	if err != nil {
		return eventstore.PullRequestVersion{}, mapError(ctx, err)
	}
	workItemID := tracker.NativeWorkItemID(row.WorkItemID)
	if row.OrganizationID != s.organization || row.ProjectID != project || workItemID != item || row.ChangeID == "" || row.VersionID == "" || row.Number < 1 || row.ExternalProvider != "github" || row.ExternalURL == "" {
		return eventstore.PullRequestVersion{}, artifact.ErrIntegrity
	}
	return eventstore.PullRequestVersion{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, WorkItemID: workItemID, ChangeID: row.ChangeID, VersionID: row.VersionID, Number: row.Number, HeadSHA: row.HeadSHA, Repository: row.Repository, PolicyID: row.PolicyRevisionID, AttemptID: row.AttemptID, External: tracker.ChangeExternalReference{Provider: row.ExternalProvider, ID: row.ExternalID, URL: row.ExternalURL}}, nil
}

func (s *session) Changes(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, after string, limit int) ([]eventstore.Change, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.RecordListChange(ctx, pgquery.RecordListChangeParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, After: after, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	records := make([]eventstore.Change, len(rows))
	for i, row := range rows {
		records[i] = eventstore.Change(row)
	}
	return records, nil
}
func (s *session) CollaborationVersion(ctx context.Context, id string, revision int64) (eventstore.CollaborationVersion, error) {
	row, err := s.queries.RecordGetCollaborationVersion(ctx, pgquery.RecordGetCollaborationVersionParams{OrganizationID: s.organization, RecordID: id, Revision: revision})
	return collaborationVersionRecord(row), mapError(ctx, err)
}
func (s *session) CollaborationVersions(ctx context.Context, keys []eventstore.CollaborationVersionKey) ([]eventstore.CollaborationVersion, error) {
	if len(keys) == 0 {
		return []eventstore.CollaborationVersion{}, nil
	}
	if err := checkPage(len(keys)); err != nil {
		return nil, err
	}
	ids := make([]string, len(keys))
	revisions := make([]int64, len(keys))
	seen := map[eventstore.CollaborationVersionKey]bool{}
	for i, key := range keys {
		if key.RecordID == "" || key.Revision < 1 || seen[key] {
			return nil, corestore.ErrInvalidPage
		}
		ids[i], revisions[i], seen[key] = key.RecordID, key.Revision, true
	}
	rows, err := s.queries.RecordGetCollaborationVersions(ctx, pgquery.RecordGetCollaborationVersionsParams{OrganizationID: s.organization, RecordIds: ids, Revisions: revisions})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]eventstore.CollaborationVersion, len(rows))
	for i, row := range rows {
		result[i] = collaborationVersionRecord(row)
	}
	return result, nil
}

func (s *session) insertRecordEvent(ctx context.Context, record eventstore.Event) (eventstore.Event, error) {
	row, err := s.queries.RecordInsertEvent(ctx, pgquery.RecordInsertEventParams{HistoryHidden: record.HistoryHidden, SchedulerOutcome: record.SchedulerOutcome, PushKnown: record.PushKnown, PushChangeID: record.PushChangeID, PushVersionID: record.PushVersionID, PushHeadSha: record.PushHeadSHA, PushExternalEffect: record.PushExternalEffect, PushEffectState: record.PushEffectState, OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, ID: record.ID, Sequence: record.Sequence, ProjectSequence: record.ProjectSequence, Type: record.Type, SchemaVersion: record.SchemaVersion, ActorKind: record.ActorKind, ActorPrincipalID: record.ActorPrincipalID, ActorEnvelopeKnown: record.ActorEnvelopeKnown, ActorViaKey: record.ActorViaKey, ActorDisplayName: record.ActorDisplayName, ActorAgent: record.ActorAgent, EventRevision: record.EventRevision, ChangeID: record.ChangeID, VersionID: record.VersionID, HeadSHA: record.HeadSHA, LandingWaiting: record.LandingWaiting, LandingChangeID: record.LandingChangeID, LandingVersionID: record.LandingVersionID, LandingHeadSHA: record.LandingHeadSHA, RunID: record.RunID, AttemptID: record.AttemptID, FromState: record.FromState, ToState: record.ToState, RecordedAt: record.RecordedAt, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, PayloadFormatVersion: record.PayloadFormatVersion})
	if err != nil {
		return eventstore.Event{}, mapError(ctx, err)
	}
	if err := s.putReportEventProjection(ctx, record); err != nil {
		return eventstore.Event{}, err
	}
	if err := s.recordActivityNotice(record); err != nil {
		return eventstore.Event{}, err
	}
	return eventRecord(row), nil
}
func (s *session) recordActivityNotice(record eventstore.Event) error {
	if record.HistoryHidden {
		return nil
	}
	return s.recordBoardDelta(record.ProjectID, viewstore.BoardDelta{Topic: "activity", EntityID: record.ID, Kind: "upsert", RelatedID: string(record.WorkItemID)})
}

func (s *session) insertRecordCollaborationVersion(ctx context.Context, record eventstore.CollaborationVersion) (eventstore.CollaborationVersion, error) {
	row, err := s.queries.RecordInsertCollaborationVersion(ctx, pgquery.RecordInsertCollaborationVersionParams{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, RecordID: record.RecordID, Revision: record.Revision, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, SourceKnown: record.SourceKnown, SourceScheduled: record.SourceScheduled, SourceObservedAt: record.SourceObservedAt})
	if err != nil {
		return eventstore.CollaborationVersion{}, mapError(ctx, err)
	}
	if _, err := s.classifyScheduledSource(ctx, record, nil, false); err != nil {
		return eventstore.CollaborationVersion{}, err
	}
	return collaborationVersionRecord(row), nil
}
func (s *session) insertRecordChange(ctx context.Context, record eventstore.Change) (eventstore.Change, error) {
	row, err := s.queries.RecordInsertChange(ctx, pgquery.RecordInsertChangeParams{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, ID: record.ID, Revision: record.Revision, Title: record.Title, CurrentVersionID: record.CurrentVersionID, EvidenceSequence: record.EvidenceSequence, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, LandedVersionID: record.LandedVersionID, LandedAt: recordTimePointer(record.LandedAt), LandedHeadSHA: record.LandedHeadSHA, LandedMergeSHA: record.LandedMergeSHA, LandedBaseRef: record.LandedBaseRef})
	return eventstore.Change(row), mapError(ctx, err)
}
func (s *session) insertRecordVersion(ctx context.Context, record eventstore.Version) (eventstore.Version, error) {
	var sourceKey, sourceDigest *string
	var sourceBytes, sourceContentBytes pgtype.Int8
	if record.SourceObjectKey != "" {
		sourceKey = &record.SourceObjectKey
		sourceDigest = &record.SourceSHA256
		sourceBytes = pgtype.Int8{Int64: record.SourceBytes, Valid: true}
		sourceContentBytes = pgtype.Int8{Int64: record.SourceContentBytes, Valid: true}
	}
	row, err := s.queries.RecordInsertVersion(ctx, pgquery.RecordInsertVersionParams{LandingHistoryKnown: record.LandingHistoryKnown, OrganizationID: record.OrganizationID, ID: record.ID, ChangeID: record.ChangeID, Number: record.Number, HeadSHA: record.HeadSHA, BaseSHA: record.BaseSHA, MergeBaseSHA: record.MergeBaseSHA, Repository: record.Repository, PolicyRevisionID: record.PolicyRevisionID, AttemptID: record.AttemptID, ActorKind: record.ActorKind, ActorPrincipalID: record.ActorPrincipalID, ExternalProvider: record.ExternalProvider, ExternalID: record.ExternalID, ExternalURL: record.ExternalURL, CreatedAt: record.CreatedAt, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, PayloadFormatVersion: record.PayloadFormatVersion, SourceObjectKey: sourceKey, SourceSha256: sourceDigest, SourceBytes: sourceBytes, SourceContentBytes: sourceContentBytes, ImplementingSessionID: record.ImplementingSessionID, ImplementingMachineID: record.ImplementingMachineID})
	return versionRecord(row), mapError(ctx, err)
}
func (s *session) insertRecordEvidence(ctx context.Context, record eventstore.Evidence, previousThrough int64) (eventstore.Evidence, error) {
	var escapeObservedAt *time.Time
	if record.ReportEscape != nil {
		at := record.ReportEscape.ObservedAt.UTC().Truncate(time.Microsecond)
		escapeObservedAt = &at
	}
	row, err := s.queries.RecordInsertEvidence(ctx, pgquery.RecordInsertEvidenceParams{EscapeObservedAt: escapeObservedAt, ReviewKind: record.ReviewKind, ReviewLanding: record.ReviewLanding, ReviewSessionKnown: record.ReviewSessionKnown, ReviewSessionID: record.ReviewSessionID, ReviewSessionMachine: string(record.ReviewSessionMachine), OrganizationID: record.OrganizationID, ChangeID: record.ChangeID, Sequence: record.Sequence, VersionID: record.VersionID, Kind: record.Kind, SourceKey: record.SourceKey, ActorKind: record.ActorKind, ActorPrincipalID: record.ActorPrincipalID, Decision: record.Decision, CreatedAt: record.CreatedAt, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, PayloadFormatVersion: record.PayloadFormatVersion})
	if err != nil {
		return eventstore.Evidence{}, mapError(ctx, err)
	}
	record.Sequence = row.Sequence
	if err := s.invalidateCurrentChangeReviews(ctx, record, previousThrough); err != nil {
		return eventstore.Evidence{}, err
	}
	if err := s.putReportEscapeProjection(ctx, record); err != nil {
		return eventstore.Evidence{}, err
	}
	return evidenceRecord(row), nil
}

func (s *session) AppendEvent(ctx context.Context, record eventstore.Event) (eventstore.Event, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return eventstore.Event{}, err
	}
	if record.ID == "" || record.Type == "" || record.SchemaVersion != 1 || record.Sequence < 0 || record.ActorKind == "" || record.ActorPrincipalID == "" {
		return eventstore.Event{}, eventstore.ErrInvalidRecord
	}
	if record.PayloadFormatVersion == 0 {
		record.PayloadFormatVersion = 2
	}
	if record.PayloadFormatVersion < 1 || record.PayloadFormatVersion > 2 {
		return eventstore.Event{}, eventstore.ErrInvalidRecord
	}
	if err := validRecordPayload(record.OrganizationID, record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes); err != nil {
		return eventstore.Event{}, err
	}
	var issueID int64
	if err := s.db.QueryRow(ctx, `SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3 FOR NO KEY UPDATE`, s.organization, record.ProjectID, record.WorkItemID).Scan(&issueID); err != nil {
		return eventstore.Event{}, mapError(ctx, err)
	}
	previous, err := s.itemEvent(ctx, record.ProjectID, record.WorkItemID, record.ID)
	if err == nil {
		requested := record
		requested.Report = nil
		if requested.Sequence == 0 {
			requested.Sequence = previous.Sequence
		}
		if requested.ProjectSequence == 0 {
			requested.ProjectSequence = previous.ProjectSequence
		}
		if requested.RecordedAt.IsZero() || recordTime(requested.RecordedAt).Equal(previous.RecordedAt) {
			requested.RecordedAt = previous.RecordedAt
		}
		requested = replayEventActor(requested, eventRecord(previous))
		if previous.Type == "scheduler.decision" && previous.SchedulerOutcome == "" {
			requested.SchedulerOutcome = ""
		}
		if previous.Type == "scheduler.decision" && !previous.PushKnown {
			requested.PushKnown = false
			requested.PushChangeID, requested.PushVersionID, requested.PushHeadSHA = previous.PushChangeID, previous.PushVersionID, previous.PushHeadSha
			requested.PushExternalEffect, requested.PushEffectState = previous.PushExternalEffect, previous.PushEffectState
		}
		if !reflect.DeepEqual(eventRecord(previous), requested) {
			return eventstore.Event{}, corestore.ErrRevisionConflict
		}
		if err := s.putReportEventProjection(ctx, record); err != nil {
			return eventstore.Event{}, err
		}
		return eventRecord(previous), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return eventstore.Event{}, mapError(ctx, err)
	}
	if record.Type == "scheduler.decision" && record.SchedulerOutcome == "" {
		return eventstore.Event{}, eventstore.ErrInvalidRecord
	}
	if err := s.expireProgressDependencies(ctx, record, issueID); err != nil {
		return eventstore.Event{}, err
	}
	sequence, err := s.queries.RecordAdvanceIssue(ctx, pgquery.RecordAdvanceIssueParams{OrganizationID: s.organization, ProjectID: record.ProjectID, NativeID: record.WorkItemID, ImportedSequence: record.Sequence, ActivityAt: recordTime(record.RecordedAt)})
	if err != nil {
		return eventstore.Event{}, mapError(ctx, err)
	}
	if record.Sequence == 0 {
		record.Sequence = sequence
	}
	record.ProjectSequence, err = s.queries.RecordAdvanceProjectEventSequence(ctx, pgquery.RecordAdvanceProjectEventSequenceParams{OrganizationID: s.organization, ProjectID: record.ProjectID})
	if err != nil {
		return eventstore.Event{}, mapError(ctx, err)
	}
	record.RecordedAt = recordTime(record.RecordedAt)
	return s.insertRecordEvent(ctx, record)
}

func (s *session) CreateCollaborationVersion(ctx context.Context, record eventstore.CollaborationVersion) (eventstore.CollaborationVersion, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return eventstore.CollaborationVersion{}, err
	}
	if record.RecordID == "" || record.Revision < 1 {
		return eventstore.CollaborationVersion{}, eventstore.ErrInvalidRecord
	}
	if err := validRecordPayload(record.OrganizationID, record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes); err != nil {
		return eventstore.CollaborationVersion{}, err
	}
	if _, err := s.queries.RecordLockIssue(ctx, pgquery.RecordLockIssueParams{OrganizationID: s.organization, ProjectID: record.ProjectID, NativeID: record.WorkItemID}); err != nil {
		return eventstore.CollaborationVersion{}, mapError(ctx, err)
	}
	previous, err := s.queries.RecordGetCollaborationVersion(ctx, pgquery.RecordGetCollaborationVersionParams{OrganizationID: s.organization, RecordID: record.RecordID, Revision: record.Revision})
	if err == nil {
		if !previous.SourceKnown && record.SourceKnown {
			comparable := collaborationVersionRecord(previous)
			comparable.SourceKnown, comparable.SourceScheduled, comparable.SourceObservedAt = record.SourceKnown, record.SourceScheduled, record.SourceObservedAt
			if !reflect.DeepEqual(comparable, record) {
				return eventstore.CollaborationVersion{}, corestore.ErrRevisionConflict
			}
			if _, err := s.classifyScheduledSource(ctx, record, nil, false); err != nil {
				return eventstore.CollaborationVersion{}, err
			}
			return collaborationVersionRecord(previous), nil
		}
		if !reflect.DeepEqual(collaborationVersionRecord(previous), record) {
			return eventstore.CollaborationVersion{}, corestore.ErrRevisionConflict
		}
		if _, err := s.classifyScheduledSource(ctx, record, nil, false); err != nil {
			return eventstore.CollaborationVersion{}, err
		}
		return collaborationVersionRecord(previous), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return eventstore.CollaborationVersion{}, mapError(ctx, err)
	}
	return s.insertRecordCollaborationVersion(ctx, record)
}

func (s *session) CreateChange(ctx context.Context, record eventstore.Change) (eventstore.Change, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return eventstore.Change{}, err
	}
	if record.ID == "" || record.Revision < 0 || record.EvidenceSequence < 0 {
		return eventstore.Change{}, eventstore.ErrInvalidRecord
	}
	if err := validRecordPayload(record.OrganizationID, record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes); err != nil {
		return eventstore.Change{}, err
	}
	if _, err := s.queries.RecordLockIssue(ctx, pgquery.RecordLockIssueParams{OrganizationID: s.organization, ProjectID: record.ProjectID, NativeID: record.WorkItemID}); err != nil {
		return eventstore.Change{}, mapError(ctx, err)
	}
	if record.Revision == 0 {
		record.Revision = 1
	}
	previous, err := s.queries.RecordGetChange(ctx, pgquery.RecordGetChangeParams{OrganizationID: s.organization, ID: record.ID})
	if err == nil {
		requested := record
		requested.EvidenceSequence = previous.EvidenceSequence
		requested.ReviewSummarySequence = previous.ReviewSummarySequence
		requested.ReviewEvidenceRevision = previous.ReviewEvidenceRevision
		if requested.CreatedAt.IsZero() || recordTime(requested.CreatedAt).Equal(previous.CreatedAt) {
			requested.CreatedAt = previous.CreatedAt
		}
		if requested.UpdatedAt.IsZero() || recordTime(requested.UpdatedAt).Equal(previous.UpdatedAt) {
			requested.UpdatedAt = previous.UpdatedAt
		}
		if !reflect.DeepEqual(eventstore.Change(previous), requested) {
			return eventstore.Change{}, corestore.ErrRevisionConflict
		}
		return eventstore.Change(previous), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return eventstore.Change{}, mapError(ctx, err)
	}
	record.CreatedAt = recordTime(record.CreatedAt)
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = record.CreatedAt
	} else {
		record.UpdatedAt = recordTime(record.UpdatedAt)
	}
	return s.insertRecordChange(ctx, record)
}

func (s *session) UpdateChange(ctx context.Context, record eventstore.Change) (eventstore.Change, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return eventstore.Change{}, err
	}
	if err := validRecordPayload(record.OrganizationID, record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes); err != nil {
		return eventstore.Change{}, err
	}
	row, err := s.queries.RecordUpdateChange(ctx, pgquery.RecordUpdateChangeParams{OrganizationID: s.organization, ID: record.ID, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, ExpectedRevision: record.Revision, Title: record.Title, CurrentVersionID: record.CurrentVersionID, UpdatedAt: recordTime(record.UpdatedAt), PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, LandedVersionID: record.LandedVersionID, LandedAt: recordTimePointer(record.LandedAt), LandedHeadSHA: record.LandedHeadSHA, LandedMergeSHA: record.LandedMergeSHA, LandedBaseRef: record.LandedBaseRef})
	if err := updated(ctx, err); err != nil {
		return eventstore.Change{}, err
	}
	if err := s.advanceLinkedIssueActivity(ctx, row.ProjectID, row.ID, row.UpdatedAt); err != nil {
		return eventstore.Change{}, err
	}
	return eventstore.Change(row), nil
}

func (s *session) advanceLinkedIssueActivity(ctx context.Context, project tracker.ProjectID, change string, at time.Time) error {
	_, err := s.db.Exec(ctx, `WITH linked AS MATERIALIZED (
 SELECT i.id FROM change_issue_links l JOIN issues i ON i.organization_id=l.organization_id AND i.project_id=l.project_id AND i.native_id=l.work_item_id
 WHERE l.organization_id=$1 AND l.project_id=$2 AND l.change_id=$3 AND i.last_activity_at<$4
 ORDER BY i.native_id FOR NO KEY UPDATE OF i
)
UPDATE issues SET last_activity_at=$4 FROM linked WHERE issues.organization_id=$1 AND issues.id=linked.id`, s.organization, project, change, at)
	return mapError(ctx, err)
}

func (s *session) CreateVersion(ctx context.Context, record eventstore.Version) (eventstore.Version, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return eventstore.Version{}, err
	}
	if record.ID == "" || record.Number < 1 || record.HeadSHA == "" || record.PolicyRevisionID == "" {
		return eventstore.Version{}, eventstore.ErrInvalidRecord
	}
	if record.PayloadFormatVersion == 0 {
		record.PayloadFormatVersion = 2
	}
	if record.PayloadFormatVersion < 1 || record.PayloadFormatVersion > 2 {
		return eventstore.Version{}, eventstore.ErrInvalidRecord
	}
	if err := validRecordPayload(record.OrganizationID, record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes); err != nil {
		return eventstore.Version{}, err
	}
	if record.SourceObjectKey != "" {
		if err := validRecordPayload(record.OrganizationID, record.SourceObjectKey, record.SourceSHA256, record.SourceBytes, record.SourceContentBytes); err != nil {
			return eventstore.Version{}, eventstore.ErrInvalidRecord
		}
	} else if record.SourceSHA256 != "" || record.SourceBytes != 0 || record.SourceContentBytes != 0 {
		return eventstore.Version{}, eventstore.ErrInvalidRecord
	}
	if _, err := s.queries.RecordLockChange(ctx, pgquery.RecordLockChangeParams{OrganizationID: s.organization, ID: record.ChangeID}); err != nil {
		return eventstore.Version{}, mapError(ctx, err)
	}
	previous, err := s.queries.RecordGetVersion(ctx, pgquery.RecordGetVersionParams{OrganizationID: s.organization, ChangeID: record.ChangeID, ID: record.ID})
	if err == nil {
		requested := record
		if requested.CreatedAt.IsZero() || recordTime(requested.CreatedAt).Equal(previous.CreatedAt) {
			requested.CreatedAt = previous.CreatedAt
		}
		previousRecord := versionRecord(previous)
		requested.FrozenLanding = previousRecord.FrozenLanding
		requested.LandingHistoryKnown = previousRecord.LandingHistoryKnown
		if !reflect.DeepEqual(versionRecord(previous), requested) {
			return eventstore.Version{}, corestore.ErrRevisionConflict
		}
		return versionRecord(previous), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return eventstore.Version{}, mapError(ctx, err)
	}
	record.CreatedAt = recordTime(record.CreatedAt)
	stored, err := s.insertRecordVersion(ctx, record)
	if err != nil {
		return eventstore.Version{}, err
	}
	if stored.ExternalProvider == "github" && stored.ExternalURL != "" {
		if _, err := s.queries.RecordRetainPullRequestVersion(ctx, pgquery.RecordRetainPullRequestVersionParams{OrganizationID: s.organization, ChangeID: stored.ChangeID, VersionID: stored.ID}); err != nil {
			return eventstore.Version{}, mapError(ctx, err)
		}
	}
	return stored, nil
}

func (s *session) AppendEvidence(ctx context.Context, record eventstore.Evidence) (eventstore.Evidence, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return eventstore.Evidence{}, err
	}
	if record.Sequence < 0 || record.SourceKey != nil && *record.SourceKey == "" {
		return eventstore.Evidence{}, eventstore.ErrInvalidRecord
	}
	if record.PayloadFormatVersion == 0 {
		record.PayloadFormatVersion = 2
	}
	if record.PayloadFormatVersion < 1 || record.PayloadFormatVersion > 2 {
		return eventstore.Evidence{}, eventstore.ErrInvalidRecord
	}
	if err := validRecordPayload(record.OrganizationID, record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes); err != nil {
		return eventstore.Evidence{}, err
	}
	lockedChange, err := s.queries.RecordLockChange(ctx, pgquery.RecordLockChangeParams{OrganizationID: s.organization, ID: record.ChangeID})
	if err != nil {
		return eventstore.Evidence{}, mapError(ctx, err)
	}
	if record.SourceKey != nil || record.Sequence > 0 {
		var previous pgquery.ChangeEvidence
		var err error
		if record.SourceKey != nil {
			previous, err = s.queries.RecordGetEvidence(ctx, pgquery.RecordGetEvidenceParams{OrganizationID: s.organization, ChangeID: record.ChangeID, Kind: record.Kind, SourceKey: record.SourceKey})
		} else {
			previous, err = s.queries.RecordGetEvidenceSequence(ctx, pgquery.RecordGetEvidenceSequenceParams{OrganizationID: s.organization, ChangeID: record.ChangeID, Sequence: record.Sequence})
		}
		if err == nil {
			requested := record
			requested.ReportEscape = nil
			if requested.Sequence == 0 {
				requested.Sequence = previous.Sequence
			}
			if requested.CreatedAt.IsZero() || recordTime(requested.CreatedAt).Equal(previous.CreatedAt) {
				requested.CreatedAt = previous.CreatedAt
			}
			if !reflect.DeepEqual(evidenceRecord(previous), requested) {
				return eventstore.Evidence{}, corestore.ErrRevisionConflict
			}
			record.Sequence = previous.Sequence
			if err := s.putReportEscapeProjection(ctx, record); err != nil {
				return eventstore.Evidence{}, err
			}
			return evidenceRecord(previous), nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return eventstore.Evidence{}, mapError(ctx, err)
		}
	}
	sequence, err := s.queries.RecordAdvanceEvidence(ctx, pgquery.RecordAdvanceEvidenceParams{OrganizationID: s.organization, ID: record.ChangeID, ImportedSequence: record.Sequence})
	if err != nil {
		return eventstore.Evidence{}, mapError(ctx, err)
	}
	if record.Sequence == 0 {
		record.Sequence = sequence
	}
	record.CreatedAt = recordTime(record.CreatedAt)
	stored, err := s.insertRecordEvidence(ctx, record, lockedChange.EvidenceSequence)
	if err != nil {
		return eventstore.Evidence{}, err
	}
	if err := s.advanceLinkedIssueActivity(ctx, lockedChange.ProjectID, lockedChange.ID, stored.CreatedAt); err != nil {
		return eventstore.Evidence{}, err
	}
	return stored, nil
}

func (s *session) LinkIssue(ctx context.Context, record eventstore.Link) error {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return err
	}
	if _, err := s.queries.RecordLockChange(ctx, pgquery.RecordLockChangeParams{OrganizationID: s.organization, ID: record.ChangeID}); err != nil {
		return mapError(ctx, err)
	}
	previous, err := s.queries.RecordGetLink(ctx, pgquery.RecordGetLinkParams{OrganizationID: s.organization, ChangeID: record.ChangeID, WorkItemID: record.WorkItemID})
	if err == nil {
		stored := eventstore.Link(previous)
		if stored.OrganizationID != record.OrganizationID || stored.ProjectID != record.ProjectID || stored.WorkItemID != record.WorkItemID || stored.ChangeID != record.ChangeID {
			return corestore.ErrRevisionConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return mapError(ctx, err)
	}
	_, err = s.queries.RecordInsertLink(ctx, pgquery.RecordInsertLinkParams{OrganizationID: s.organization, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, ChangeID: record.ChangeID})
	return mapError(ctx, err)
}

func (s *session) Receipt(ctx context.Context, actor, operation, key string) (eventstore.Receipt, error) {
	return s.receiptRecord(ctx, actor, operation, key)
}

func (s *session) PutReceipt(ctx context.Context, record eventstore.Receipt) (eventstore.Receipt, error) {
	row, inserted, err := s.insertReceipt(ctx, record)
	if err == nil && !inserted && (row.ResponseClaimed || row.ResponseExpired || row.PayloadSHA256 != record.PayloadSHA256) {
		return eventstore.Receipt{}, corestore.ErrRevisionConflict
	}
	return row, err
}

func (s *session) ChangeSummaryEvidence(ctx context.Context, change, version string, through int64, checks []string) ([]eventstore.Evidence, bool, error) {
	records, prior, err := s.changeSummaryEvidence(ctx, change, version, through, checks)
	if err != nil {
		return nil, false, err
	}
	if prior.Unprojected {
		return nil, false, corestore.ErrInvalidPage
	}
	return records, prior.Approved, nil
}

func (s *session) RecoveryChangeEvidence(ctx context.Context, change, version string, through int64, checks []string) ([]eventstore.Evidence, bool, error) {
	records, prior, err := s.changeSummaryEvidence(ctx, change, version, through, checks)
	if err != nil {
		return nil, false, err
	}
	recent, err := s.queries.RecordRecoveryChangeEvidence(ctx, pgquery.RecordRecoveryChangeEvidenceParams{
		OrganizationID: s.organization, ChangeID: change, VersionID: new(version), Through: through,
		IncludePriorUnprojected: prior.Unprojected && !prior.Approved,
	})
	if err != nil {
		return nil, false, mapError(ctx, err)
	}
	for _, row := range recent {
		records = append(records, evidenceRecord(pgquery.ChangeEvidence(row)))
	}
	slices.SortFunc(records, func(a, b eventstore.Evidence) int { return cmp.Compare(a.Sequence, b.Sequence) })
	records = slices.CompactFunc(records, func(a, b eventstore.Evidence) bool { return a.Sequence == b.Sequence })
	var unknownCurrent, unknownPrior int
	for _, row := range records {
		if row.Kind != "review" || row.ReviewKind != "" || row.VersionID == nil {
			continue
		}
		if *row.VersionID == version {
			unknownCurrent++
		} else {
			unknownPrior++
		}
	}
	if unknownCurrent > corestore.MaxPageSize || unknownPrior > corestore.MaxPageSize {
		return nil, false, corestore.ErrInvalidPage
	}
	return records, prior.Approved, nil
}

func (s *session) changeSummaryEvidence(ctx context.Context, change, version string, through int64, checks []string) ([]eventstore.Evidence, pgquery.RecordChangePriorApprovalRow, error) {
	records, err := s.CurrentChangeEvidence(ctx, change, version, through, checks)
	if err != nil {
		return nil, pgquery.RecordChangePriorApprovalRow{}, err
	}
	prior, err := s.queries.RecordChangePriorApproval(ctx, pgquery.RecordChangePriorApprovalParams{OrganizationID: s.organization, ChangeID: change, VersionID: new(version), Through: through})
	if err != nil {
		return nil, pgquery.RecordChangePriorApprovalRow{}, mapError(ctx, err)
	}
	return records, prior, nil
}

func (s *session) CurrentChangeEvidence(ctx context.Context, change, version string, through int64, checks []string) ([]eventstore.Evidence, error) {
	if len(checks) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.RecordChangeSummaryEvidence(ctx, pgquery.RecordChangeSummaryEvidenceParams{OrganizationID: s.organization, ChangeID: change, VersionID: new(version), Through: through, CheckIds: checks})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	unknown := 0
	for _, row := range rows {
		if row.Kind == "review" && row.ReviewKind == "" {
			unknown++
		}
	}
	if unknown > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	records := make([]eventstore.Evidence, len(rows))
	for i, row := range rows {
		records[i] = evidenceRecord(pgquery.ChangeEvidence(row))
	}
	return records, nil
}

const priorApprovalColumns = `organization_id,change_id,sequence,version_id,kind,source_key,actor_kind,actor_principal_id,decision,created_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,payload_format_version,review_kind,review_landing,review_session_known,review_session_id,review_session_machine,project_id,quality_at`

const priorApprovalCandidate = `SELECT ` + priorApprovalColumns + ` FROM (
(SELECT 1 AS probe,` + priorApprovalColumns + ` FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND kind='review' AND decision='approved' AND review_kind IN ('human','')
 AND version_id<$3 AND (version_id,sequence)>($5::text,$6::bigint) AND sequence<=$4 AND NOT $7::boolean ORDER BY version_id,sequence LIMIT 1)
UNION ALL
(SELECT 2,` + priorApprovalColumns + ` FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND kind='review' AND decision='approved' AND review_kind IN ('human','')
 AND version_id>$3 AND (version_id,sequence)>($5::text,$6::bigint) AND sequence<=$4 AND NOT $7::boolean ORDER BY version_id,sequence LIMIT 1)
UNION ALL
(SELECT 3,` + priorApprovalColumns + ` FROM change_evidence WHERE organization_id=$1 AND change_id=$2 AND kind='review' AND decision='approved' AND review_kind IN ('human','')
 AND version_id IS NULL AND sequence>$8::bigint AND sequence<=$4 ORDER BY sequence LIMIT 1)
) candidates ORDER BY probe LIMIT 1`

func (s *session) PriorApprovalCandidate(ctx context.Context, change, version string, through int64, after eventstore.PriorApprovalCursor) (eventstore.Evidence, error) {
	unversionedAfter := int64(0)
	if after.Unversioned {
		unversionedAfter = after.Sequence
	}
	rows, err := s.db.Query(ctx, priorApprovalCandidate, s.organization, change, version, through, after.VersionID, after.Sequence, after.Unversioned, unversionedAfter)
	if err != nil {
		return eventstore.Evidence{}, mapError(ctx, err)
	}
	row, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[pgquery.ChangeEvidence])
	if err != nil {
		return eventstore.Evidence{}, mapError(ctx, err)
	}
	return evidenceRecord(row), nil
}

func (s *session) ChangePublicationCheckpoints(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, reference tracker.NativeChangeReference) ([]eventstore.Event, error) {
	rows, err := s.queries.RecordChangePublicationCheckpoints(ctx, pgquery.RecordChangePublicationCheckpointsParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, ChangeID: reference.ChangeID, VersionID: reference.VersionID, HeadSHA: reference.HeadSHA})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	unknown := 0
	for _, row := range rows {
		if !row.PushKnown {
			unknown++
		}
	}
	if unknown > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	records := make([]eventstore.Event, len(rows))
	for i, row := range rows {
		records[i] = eventRecord(row)
	}
	return records, nil
}

func (s *session) ChangePublicationProvenance(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, run tracker.NativeRunData) (bool, error) {
	valid, err := s.queries.RecordChangePublicationProvenance(ctx, pgquery.RecordChangePublicationProvenanceParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, PolicyID: run.PolicyID, RunSequence: run.Sequence, AttemptID: run.AttemptID, RunID: run.RunID, LeaseID: string(run.LeaseID), FencingToken: int64(run.FencingToken)})
	return valid, mapError(ctx, err)
}

func (s *session) ValidatorReviewEvidence(ctx context.Context, change, version string, through int64, machine tracker.MachineID, sessionID int64) ([]eventstore.Evidence, bool, error) {
	if through < 0 || machine == "" || sessionID < 1 {
		return nil, false, corestore.ErrInvalidPage
	}
	rows, err := s.queries.RecordValidatorReviewEvidence(ctx, pgquery.RecordValidatorReviewEvidenceParams{OrganizationID: s.organization, ChangeID: change, VersionID: new(version), Through: through, MachineID: string(machine), SessionID: sessionID})
	if err != nil {
		return nil, false, mapError(ctx, err)
	}
	records := make([]eventstore.Evidence, len(rows))
	for i, row := range rows {
		if !row.ReviewSessionKnown || row.ReviewKind == "" {
			return nil, false, nil
		}
		records[i] = evidenceRecord(pgquery.ChangeEvidence(row))
	}
	return records, true, nil
}

func (s *session) LatestVersionLandingEvent(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, ref tracker.NativeChangeReference) (eventstore.LandingObservation, error) {
	row, err := s.queries.RecordLatestVersionLandingEvent(ctx, pgquery.RecordLatestVersionLandingEventParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, LandingChangeID: ref.ChangeID, LandingVersionID: ref.VersionID, LandingHeadSHA: ref.HeadSHA})
	return eventstore.LandingObservation{Event: eventRecord(row.CollaborationEvent), FromState: row.LandingFromState, TargetState: row.LandingTargetState}, mapError(ctx, err)
}

func (s *session) LatestWorkflowTransition(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (eventstore.Event, error) {
	row, err := s.queries.RecordLatestWorkflowTransition(ctx, pgquery.RecordLatestWorkflowTransitionParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item})
	return eventRecord(row), mapError(ctx, err)
}

func (s *session) PRPublication(ctx context.Context, version string) (eventstore.NativePRPublication, error) {
	row, err := s.queries.RecordGetPRPublication(ctx, pgquery.RecordGetPRPublicationParams{OrganizationID: s.organization, VersionID: version})
	if err != nil {
		return eventstore.NativePRPublication{}, mapError(ctx, err)
	}
	return eventstore.NativePRPublication{OrganizationID: row.OrganizationID, VersionID: row.VersionID, ChangeID: row.ChangeID, Repository: row.Repository, BaseRef: row.BaseRef, Branch: row.Branch, HeadSHA: row.HeadSHA, PolicyID: row.PolicyID, SourceAttemptID: stringValue(row.SourceAttemptID), RecorderAttemptID: row.RecorderAttemptID, External: tracker.ChangeExternalReference{Provider: row.Provider, ID: row.ExternalID, URL: row.ExternalURL}}, nil
}
func (s *session) PutPRPublication(ctx context.Context, record eventstore.NativePRPublication) error {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return err
	}
	var source *string
	if record.SourceAttemptID != "" {
		source = &record.SourceAttemptID
	}
	err := s.queries.RecordPutPRPublication(ctx, pgquery.RecordPutPRPublicationParams{OrganizationID: record.OrganizationID, VersionID: record.VersionID, ChangeID: record.ChangeID, Repository: record.Repository, BaseRef: record.BaseRef, Branch: record.Branch, HeadSHA: record.HeadSHA, PolicyID: record.PolicyID, SourceAttemptID: source, RecorderAttemptID: record.RecorderAttemptID, Provider: record.External.Provider, ExternalID: record.External.ID, ExternalURL: record.External.URL})
	if err != nil {
		return mapError(ctx, err)
	}
	existing, err := s.PRPublication(ctx, record.VersionID)
	if err != nil {
		return err
	}
	existing.RecorderAttemptID = record.RecorderAttemptID
	if existing != record {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) LatestSchedulerDecision(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) (eventstore.Event, error) {
	row, err := s.queries.RecordLatestSchedulerDecision(ctx, pgquery.RecordLatestSchedulerDecisionParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item})
	return eventRecord(row), mapError(ctx, err)
}

func replayEventActor(requested, previous eventstore.Event) eventstore.Event {
	if !previous.ActorEnvelopeKnown {
		requested.ActorEnvelopeKnown, requested.ActorViaKey, requested.ActorDisplayName, requested.ActorAgent = false, previous.ActorViaKey, previous.ActorDisplayName, previous.ActorAgent
	}
	return requested
}

func (s *session) LatestEventRecordedAt(ctx context.Context) (*time.Time, error) {
	var recorded time.Time
	err := s.db.QueryRow(ctx, `SELECT recorded_at FROM collaboration_events WHERE organization_id=$1 ORDER BY recorded_at DESC,id DESC LIMIT 1`, s.organization).Scan(&recorded)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	recorded = recorded.UTC()
	return &recorded, nil
}

func (s *session) LatestRunEvents(ctx context.Context, project tracker.ProjectID, attempts []string) ([]eventstore.Event, error) {
	if project == "" || len(attempts) > 1000 {
		return nil, corestore.ErrInvalidPage
	}
	if len(attempts) == 0 {
		return []eventstore.Event{}, nil
	}
	seen := make(map[string]bool, len(attempts))
	for _, id := range attempts {
		if id == "" || seen[id] {
			return nil, corestore.ErrInvalidPage
		}
		seen[id] = true
	}
	rows, err := s.queries.RecordLatestRunEventsByAttempts(ctx, pgquery.RecordLatestRunEventsByAttemptsParams{OrganizationID: s.organization, ProjectID: project, AttemptIds: attempts})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	out := make([]eventstore.Event, len(rows))
	for i, row := range rows {
		out[i] = eventRecord(row)
	}
	return out, nil
}

func (s *session) ReadAnalyticsSchedulerPage(ctx context.Context, query eventstore.AnalyticsSchedulerQuery) (eventstore.AnalyticsSchedulerPage, error) {
	out := eventstore.AnalyticsSchedulerPage{Events: []eventstore.Event{}, Prior: map[string]eventstore.Event{}, Partial: query.After.ID != ""}
	if query.ProjectID == "" || !query.From.Before(query.To) || query.Outcome != "claimed" && query.Outcome != "skipped" || query.Limit < 1 || query.Limit > 1000 || (query.After.ID == "") != query.After.At.IsZero() {
		return out, corestore.ErrInvalidPage
	}
	args := []any{s.organization, query.ProjectID, query.Outcome, query.From.UTC(), query.To.UTC(), query.Limit + 1}
	seek := ""
	if query.After.ID != "" {
		seek = " AND (e.recorded_at,e.id)>($7,$8)"
		args = append(args, query.After.At.UTC(), query.After.ID)
	}
	rows, err := s.db.Query(ctx, `WITH candidates AS MATERIALIZED (
 SELECT e.* FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.type='scheduler.decision' AND e.scheduler_outcome=$3 AND e.recorded_at>=$4 AND e.recorded_at<$5`+seek+`
 ORDER BY e.recorded_at,e.id LIMIT $6
) SELECT e.*,i.id IS NOT NULL AS item_present,coalesce(i.actor_principal_id,'') AS item_principal
FROM candidates e LEFT JOIN issues i ON i.organization_id=e.organization_id AND i.project_id=e.project_id AND i.native_id=e.work_item_id
ORDER BY e.recorded_at,e.id`, args...)
	if err != nil {
		return out, mapError(ctx, err)
	}
	type candidate struct {
		pgquery.CollaborationEvent
		ItemPresent   bool
		ItemPrincipal string
	}
	selected, err := pgx.CollectRows(rows, pgx.RowToStructByName[candidate])
	if err != nil {
		return out, mapError(ctx, err)
	}
	var last eventstore.AnalyticsSchedulerCursor
	for _, row := range selected {
		out.Examined++
		if out.Examined > query.Limit {
			out.Next = last
			out.Partial = true
			break
		}
		last = eventstore.AnalyticsSchedulerCursor{At: row.RecordedAt, ID: row.ID}
		if row.ItemPresent && (query.Principal == "" || row.ItemPrincipal == query.Principal) {
			out.Events = append(out.Events, eventRecord(row.CollaborationEvent))
		}
	}
	if query.Outcome != "claimed" || len(out.Events) == 0 {
		return out, nil
	}
	ids, items, sequences := make([]string, len(out.Events)), make([]string, len(out.Events)), make([]int64, len(out.Events))
	for i, event := range out.Events {
		ids[i], items[i], sequences[i] = event.ID, string(event.WorkItemID), event.Sequence
	}
	rows, err = s.db.Query(ctx, `SELECT requested.claim_id,prior.*
FROM ROWS FROM(unnest($3::text[]),unnest($4::text[]),unnest($5::bigint[])) AS requested(claim_id,work_item_id,before_sequence)
CROSS JOIN LATERAL (
 SELECT e.* FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.work_item_id=requested.work_item_id AND e.sequence<requested.before_sequence
 ORDER BY e.sequence DESC LIMIT 1
) prior`, s.organization, query.ProjectID, ids, items, sequences)
	if err != nil {
		return out, mapError(ctx, err)
	}
	type preceding struct {
		ClaimID string
		pgquery.CollaborationEvent
	}
	previous, err := pgx.CollectRows(rows, pgx.RowToStructByName[preceding])
	if err != nil {
		return out, mapError(ctx, err)
	}
	for _, row := range previous {
		out.Prior[row.ClaimID] = eventRecord(row.CollaborationEvent)
	}
	return out, nil
}

func (s *session) VersionLanding(ctx context.Context, change, version string) (eventstore.VersionLanding, error) {
	var row eventstore.VersionLanding
	err := s.db.QueryRow(ctx, `SELECT v.organization_id,v.project_id,c.work_item_id,v.change_id,v.id,v.landed_at,v.landed_head_sha,v.landed_merge_sha,v.landed_base_ref,v.landed_object_key,v.landed_sha256,v.landed_bytes,v.landed_content_bytes,v.landed_payload_format_version FROM change_versions v JOIN change_requests c ON c.organization_id=v.organization_id AND c.project_id=v.project_id AND c.id=v.change_id WHERE v.organization_id=$1 AND v.change_id=$2 AND v.id=$3 AND v.landed_at IS NOT NULL`, s.organization, change, version).Scan(&row.OrganizationID, &row.ProjectID, &row.WorkItemID, &row.ChangeID, &row.VersionID, &row.At, &row.HeadSHA, &row.MergeSHA, &row.BaseRef, &row.PayloadObjectKey, &row.PayloadSHA256, &row.PayloadBytes, &row.PayloadContentBytes, &row.PayloadFormatVersion)
	row.At = row.At.UTC()
	return row, mapError(ctx, err)
}

func (s *session) RecordVersionLanding(ctx context.Context, row eventstore.VersionLanding) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if _, err := s.queries.RecordLockChange(ctx, pgquery.RecordLockChangeParams{OrganizationID: s.organization, ID: row.ChangeID}); err != nil {
		return mapError(ctx, err)
	}
	return s.recordVersionLandingRows(ctx, []eventstore.VersionLanding{row})
}

func (s *session) recordVersionLandingRows(ctx context.Context, records []eventstore.VersionLanding) error {
	if len(records) < 1 || len(records) > 100 {
		return corestore.ErrInvalidPage
	}
	b := newReportBatch(s.organization, "project_id,work_item_id,change_id,version_id,landed_at,head_sha,merge_sha,base_ref,object_key,sha256,payload_bytes,content_bytes,payload_format_version", "text", "text", "text", "text", "timestamptz", "text", "text", "text", "text", "text", "bigint", "bigint", "smallint")
	seen := map[string]bool{}
	for _, row := range records {
		if err := s.checkOrganization(row.OrganizationID); err != nil {
			return err
		}
		if row.ProjectID == "" || row.WorkItemID == "" || row.ChangeID == "" || row.VersionID == "" || row.HeadSHA == "" || row.MergeSHA == "" || row.BaseRef == "" || row.At.IsZero() || row.PayloadFormatVersion < 1 || row.PayloadFormatVersion > 2 || seen[row.VersionID] {
			return eventstore.ErrInvalidRecord
		}
		if err := validRecordPayload(row.OrganizationID, row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes); err != nil {
			return err
		}
		seen[row.VersionID] = true
		b.add(row.ProjectID, row.WorkItemID, row.ChangeID, row.VersionID, recordTime(row.At), row.HeadSHA, row.MergeSHA, row.BaseRef, row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes, row.PayloadFormatVersion)
	}
	if _, err := s.db.Exec(ctx, b.source()+`UPDATE change_versions v SET landing_history_known=true,landed_at=source.landed_at,landed_head_sha=source.head_sha,landed_merge_sha=source.merge_sha,landed_base_ref=source.base_ref,landed_object_key=source.object_key,landed_sha256=source.sha256,landed_bytes=source.payload_bytes,landed_content_bytes=source.content_bytes,landed_payload_format_version=source.payload_format_version FROM source
 WHERE v.organization_id=$1 AND v.project_id=source.project_id AND v.change_id=source.change_id AND v.id=source.version_id AND v.head_sha=source.head_sha AND v.landed_at IS NULL AND EXISTS(SELECT 1 FROM change_requests c WHERE c.organization_id=v.organization_id AND c.project_id=v.project_id AND c.id=v.change_id AND c.work_item_id=source.work_item_id)`, b.args...); err != nil {
		return mapError(ctx, err)
	}
	return s.verifyReportBatch(ctx, b, `SELECT count(*) FROM source JOIN change_versions v ON v.organization_id=$1 AND v.project_id=source.project_id AND v.change_id=source.change_id AND v.id=source.version_id AND v.head_sha=source.head_sha
 JOIN change_requests c ON c.organization_id=v.organization_id AND c.project_id=v.project_id AND c.id=v.change_id AND c.work_item_id=source.work_item_id
 WHERE v.landing_history_known AND (v.landed_at,v.landed_head_sha,v.landed_merge_sha,v.landed_base_ref,v.landed_object_key,v.landed_sha256,v.landed_bytes,v.landed_content_bytes,v.landed_payload_format_version)=(source.landed_at,source.head_sha,source.merge_sha,source.base_ref,source.object_key,source.sha256,source.payload_bytes,source.content_bytes,source.payload_format_version)`, corestore.ErrRevisionConflict)
}

func (s *session) ConfirmVersionUnlanded(ctx context.Context, change, version, head string) error {
	if change == "" || version == "" || head == "" {
		return eventstore.ErrInvalidRecord
	}
	if _, err := s.queries.RecordLockChange(ctx, pgquery.RecordLockChangeParams{OrganizationID: s.organization, ID: change}); err != nil {
		return mapError(ctx, err)
	}
	result, err := s.db.Exec(ctx, `UPDATE change_versions SET landing_history_known=true WHERE organization_id=$1 AND change_id=$2 AND id=$3 AND head_sha=$4 AND landed_at IS NULL AND NOT landing_history_known`, s.organization, change, version, head)
	if err != nil {
		return mapError(ctx, err)
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	var confirmed bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM change_versions WHERE organization_id=$1 AND change_id=$2 AND id=$3 AND head_sha=$4 AND landed_at IS NULL AND landing_history_known)`, s.organization, change, version, head).Scan(&confirmed); err != nil {
		return mapError(ctx, err)
	}
	if !confirmed {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func landingRepositoryIdentity(repository string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(repository), "https://github.com/"), "git@github.com:"), ".git")
}

type validationCandidateKey struct {
	attempt string
	version string
	change  string
	fence   int64
	issue   int64
}

func (s *session) ValidationCandidates(ctx context.Context, query tenantstore.ValidationCandidateQuery) (tenantstore.ValidationCandidatePage, error) {
	result := tenantstore.ValidationCandidatePage{Rows: []tenantstore.ValidationCandidateRows{}}
	query.Repository = landingRepositoryIdentity(query.Repository)
	if query.ProjectID == "" || query.Repository == "" || len(query.Repository) > 255 || strings.Count(query.Repository, "/") != 1 || len(query.HeadSHA) != 40 && len(query.HeadSHA) != 64 || !artifact.ValidHash(query.HeadSHA, len(query.HeadSHA)) {
		return result, corestore.ErrInvalidPage
	}
	var item corestore.WorkItem
	var err error
	if query.WorkItemID != "" {
		item, err = s.WorkItem(ctx, query.ProjectID, query.WorkItemID)
	} else if query.AttemptID != "" {
		attempt, readErr := s.Attempt(ctx, query.AttemptID)
		if readErr != nil {
			return result, readErr
		}
		item, err = s.WorkItemByID(ctx, query.ProjectID, attempt.IssueID)
	} else if query.ChangeID != "" {
		change, readErr := s.Change(ctx, query.ChangeID)
		if readErr != nil {
			return result, readErr
		}
		if change.ProjectID != query.ProjectID {
			return result, corestore.ErrTenantMismatch
		}
		item, err = s.WorkItem(ctx, query.ProjectID, change.WorkItemID)
	}
	if err != nil {
		return result, err
	}
	keys := []validationCandidateKey{}
	if query.AttemptID != "" {
		attempt, err := s.Attempt(ctx, query.AttemptID)
		if err != nil {
			return result, err
		}
		if attempt.IssueID != item.ID {
			return result, corestore.ErrTenantMismatch
		}
		receipt, err := s.LandingReceipt(ctx, query.AttemptID)
		if errors.Is(err, corestore.ErrNotFound) {
			return result, nil
		}
		if err != nil {
			return result, err
		}
		version, err := s.queries.RecordGetVersionWorkItemID(ctx, pgquery.RecordGetVersionWorkItemIDParams{OrganizationID: s.organization, ID: receipt.VersionID})
		if err != nil {
			return result, mapError(ctx, err)
		}
		if version.IssueID != item.ID {
			return result, corestore.ErrTenantMismatch
		}
		repository := landingRepositoryIdentity(version.Repository)
		if receipt.Repository != "" {
			repository = landingRepositoryIdentity(receipt.Repository)
		}
		if repository != query.Repository || query.ChangeID != "" && query.ChangeID != version.ChangeID {
			return result, nil
		}
		keys = append(keys, validationCandidateKey{attempt.ID, receipt.VersionID, version.ChangeID, attempt.FencingToken, attempt.IssueID})
	} else if query.ChangeID != "" {
		rows, err := s.queries.RecordValidationChangeCandidates(ctx, pgquery.RecordValidationChangeCandidatesParams{OrganizationID: s.organization, IssueID: item.ID, ChangeID: query.ChangeID, RepositoryIdentity: query.Repository, HeadSHA: query.HeadSHA})
		if err != nil {
			return result, mapError(ctx, err)
		}
		for _, row := range rows {
			keys = append(keys, validationCandidateKey{row.AttemptID, row.VersionID, row.ChangeID, row.FencingToken, row.IssueID})
		}
	} else if item.NativeID == "" {
		rows, err := s.queries.RecordValidationProjectCandidates(ctx, pgquery.RecordValidationProjectCandidatesParams{OrganizationID: s.organization, ProjectID: query.ProjectID, RepositoryIdentity: query.Repository, HeadSHA: query.HeadSHA})
		if err != nil {
			return result, mapError(ctx, err)
		}
		for _, row := range rows {
			keys = append(keys, validationCandidateKey{row.AttemptID, row.VersionID, row.ChangeID, row.FencingToken, row.IssueID})
		}
	} else {
		rows, err := s.queries.RecordValidationCandidates(ctx, pgquery.RecordValidationCandidatesParams{OrganizationID: s.organization, IssueID: item.ID, RepositoryIdentity: query.Repository, HeadSHA: query.HeadSHA})
		if err != nil {
			return result, mapError(ctx, err)
		}
		for _, row := range rows {
			keys = append(keys, validationCandidateKey{row.AttemptID, row.VersionID, row.ChangeID, row.FencingToken, row.IssueID})
		}
	}
	result.Partial = len(keys) > 20
	if result.Partial {
		keys = keys[:20]
	}
	result.Rows = make([]tenantstore.ValidationCandidateRows, len(keys))
	batch := &pgx.Batch{}
	for index, key := range keys {
		batch.Queue(`SELECT * FROM change_requests WHERE organization_id=$1 AND id=$2`, s.organization, key.change).Query(func(rows pgx.Rows) error {
			value, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[pgquery.ChangeRequest])
			if err == nil {
				result.Rows[index].Change = eventstore.Change(value)
			}
			return err
		})
		batch.Queue(`SELECT * FROM change_versions WHERE organization_id=$1 AND change_id=$2 AND id=$3`, s.organization, key.change, key.version).Query(func(rows pgx.Rows) error {
			value, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[pgquery.ChangeVersion])
			if err == nil {
				result.Rows[index].Version = versionRecord(value)
			}
			return err
		})
		batch.Queue(`SELECT * FROM change_landing_receipts WHERE organization_id=$1 AND attempt_id=$2`, s.organization, key.attempt).Query(func(rows pgx.Rows) error {
			value, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[pgquery.ChangeLandingReceipt])
			if err == nil {
				result.Rows[index].Receipt = landingReceiptRecord(value)
			}
			return err
		})
		batch.Queue(`SELECT * FROM native_attempts WHERE organization_id=$1 AND id=$2`, s.organization, key.attempt).Query(func(rows pgx.Rows) error {
			value, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[pgquery.NativeAttempt])
			if err == nil {
				result.Rows[index].Attempt = attemptFromPG(value)
			}
			return err
		})
		batch.Queue(`SELECT * FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND attempt_id=$3 ORDER BY sequence DESC LIMIT 1`, s.organization, query.ProjectID, key.attempt).Query(func(rows pgx.Rows) error {
			value, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[pgquery.CollaborationEvent])
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err == nil {
				event := eventRecord(value)
				result.Rows[index].Event = &event
			}
			return err
		})
		batch.Queue(`SELECT project_id,native_id FROM work_item_identities WHERE organization_id=$1 AND id=$2`, s.organization, key.issue).QueryRow(func(row pgx.Row) error {
			var project tracker.ProjectID
			var workItem tracker.NativeWorkItemID
			if err := row.Scan(&project, &workItem); err != nil {
				return err
			}
			if project != query.ProjectID || workItem != result.Rows[index].Change.WorkItemID {
				return corestore.ErrTenantMismatch
			}
			return nil
		})
	}
	if len(keys) == 0 {
		return result, nil
	}
	timed, ok := s.db.(timedQueries)
	if !ok {
		return result, corestore.ErrInvalidPage
	}
	executor, ok := timed.DBTX.(interface {
		SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
	})
	if !ok {
		return result, corestore.ErrInvalidPage
	}
	started := time.Now()
	err = executor.SendBatch(ctx, batch).Close()
	for _, query := range batch.QueuedQueries {
		observeStatement(ctx, started, query.SQL)
	}
	if err != nil {
		return result, mapError(ctx, errors.Join(err, ctx.Err()))
	}
	for index, row := range result.Rows {
		key := keys[index]
		if row.Change.OrganizationID != s.organization || row.Change.ProjectID != query.ProjectID || query.WorkItemID != "" && row.Change.WorkItemID != query.WorkItemID || row.Change.ID != key.change || row.Version.OrganizationID != s.organization || row.Version.ID != key.version || row.Version.ChangeID != key.change || row.Version.Repository != "" && landingRepositoryIdentity(row.Version.Repository) != query.Repository || row.Receipt.OrganizationID != s.organization || row.Receipt.AttemptID != key.attempt || row.Receipt.VersionID != key.version || row.Receipt.Repository != "" && !strings.EqualFold(landingRepositoryIdentity(row.Receipt.Repository), query.Repository) || row.Attempt.OrganizationID != s.organization || row.Attempt.ID != key.attempt || row.Attempt.IssueID != key.issue || row.Attempt.FencingToken != key.fence {
			return result, corestore.ErrTenantMismatch
		}
		if row.Event != nil && (row.Event.OrganizationID != s.organization || row.Event.ProjectID != query.ProjectID || row.Event.WorkItemID != row.Change.WorkItemID || row.Event.AttemptID != key.attempt || row.Event.RunID != row.Attempt.RunID) {
			return result, corestore.ErrTenantMismatch
		}
	}
	return result, mapError(ctx, ctx.Err())
}

func collaborationVersionRecord(row pgquery.CollaborationVersion) eventstore.CollaborationVersion {
	return eventstore.CollaborationVersion{OrganizationID: row.OrganizationID, ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, RecordID: row.RecordID, Revision: row.Revision, PayloadObjectKey: row.PayloadObjectKey, PayloadSHA256: row.PayloadSHA256, PayloadBytes: row.PayloadBytes, PayloadContentBytes: row.PayloadContentBytes, SourceKnown: row.SourceKnown, SourceScheduled: row.SourceScheduled, SourceObservedAt: row.SourceObservedAt}
}

func (s *session) ScheduledSources(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID) ([]eventstore.CollaborationVersion, bool, error) {
	if project == "" {
		return nil, false, eventstore.ErrInvalidRecord
	}
	var pending pgtype.Bool
	var err error
	keys := []scheduledSourceKey{}
	if item == "" {
		pending, err = s.queries.RecordPendingProjectScheduledSources(ctx, pgquery.RecordPendingProjectScheduledSourcesParams{OrganizationID: s.organization, ProjectID: project})
		if err == nil {
			rows, readErr := s.queries.RecordProjectScheduledSources(ctx, pgquery.RecordProjectScheduledSourcesParams{OrganizationID: s.organization, ProjectID: project})
			err = readErr
			for _, row := range rows {
				if row.SourceObservedAt == nil {
					return nil, false, eventstore.ErrInvalidRecord
				}
				keys = append(keys, scheduledSourceKey{project: project, item: tracker.NativeWorkItemID(row.WorkItemID), record: string(row.RecordID), revision: row.Revision, observed: *row.SourceObservedAt})
			}
		}
	} else {
		pending, err = s.queries.RecordPendingWorkItemScheduledSources(ctx, pgquery.RecordPendingWorkItemScheduledSourcesParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item})
		if err == nil {
			rows, readErr := s.queries.RecordWorkItemScheduledSources(ctx, pgquery.RecordWorkItemScheduledSourcesParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item})
			err = readErr
			for _, row := range rows {
				if row.SourceObservedAt == nil {
					return nil, false, eventstore.ErrInvalidRecord
				}
				keys = append(keys, scheduledSourceKey{project: project, item: tracker.NativeWorkItemID(row.WorkItemID), record: string(row.RecordID), revision: row.Revision, observed: *row.SourceObservedAt})
			}
		}
	}
	if err != nil {
		return nil, false, mapError(ctx, err)
	}
	if !pending.Valid {
		return nil, false, eventstore.ErrInvalidRecord
	}
	partial := pending.Bool || len(keys) > 40
	if len(keys) > 40 {
		keys = keys[:40]
	}
	rows, err := s.readScheduledSourceVersions(ctx, keys)
	if err != nil {
		return nil, false, err
	}
	for i := range rows {
		rows[i].SourceKnown, rows[i].SourceScheduled, rows[i].SourceObservedAt = true, true, keys[i].observed
	}
	return rows, partial, nil
}

func (s *session) AttemptEvents(ctx context.Context, project tracker.ProjectID, item tracker.NativeWorkItemID, attempt string, after int64, limit int) ([]eventstore.Event, error) {
	if project == "" || item == "" || attempt == "" || after < 0 || limit < 1 || limit > 100 {
		return nil, corestore.ErrInvalidPage
	}
	rows, err := s.queries.RecordAttemptEvents(ctx, pgquery.RecordAttemptEventsParams{OrganizationID: s.organization, ProjectID: project, WorkItemID: item, AttemptID: attempt, After: after, PageLimit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	out := make([]eventstore.Event, len(rows))
	for i, row := range rows {
		out[i] = eventRecord(row)
	}
	return out, nil
}
