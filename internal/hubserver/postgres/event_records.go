package postgres

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
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
	versionAttempt, err := s.queries.RecordGetVersionAttemptID(ctx, pgquery.RecordGetVersionAttemptIDParams{OrganizationID: s.organization, ID: record.VersionID})
	if err != nil {
		return eventstore.LandingReceipt{}, mapError(ctx, err)
	}
	if versionAttempt != record.AttemptID {
		return eventstore.LandingReceipt{}, eventstore.ErrInvalidRecord
	}
	if (record.Repository == "") != (record.BaseRef == "") {
		return eventstore.LandingReceipt{}, eventstore.ErrInvalidRecord
	}
	row, err := s.queries.RecordInsertLandingReceipt(ctx, pgquery.RecordInsertLandingReceiptParams{OrganizationID: s.organization, AttemptID: record.AttemptID, VersionID: record.VersionID, MergeSha: record.MergeSHA, Repository: record.Repository, BaseRef: record.BaseRef, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes})
	if errors.Is(err, sql.ErrNoRows) {
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
	if project == "" || repository == "" || mergeSHA == "" || baseRef == "" {
		return eventstore.PolicyLanding{}, eventstore.ErrInvalidRecord
	}
	row, err := s.queries.RecordProjectPolicyLanding(ctx, pgquery.RecordProjectPolicyLandingParams{OrganizationID: s.organization, ProjectID: project, Repository: repository, MergeSha: mergeSHA, BaseRef: baseRef})
	if err != nil {
		return eventstore.PolicyLanding{}, mapError(ctx, err)
	}
	return eventstore.PolicyLanding{ChangeID: row.ChangeID, PullRequestURL: row.ExternalURL, Actor: row.ActorPrincipalID}, nil
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
		OrganizationID:        row.OrganizationID,
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
	return eventstore.CollaborationVersion(row), mapError(ctx, err)
}
func (s *session) CollaborationVersions(ctx context.Context, keys []eventstore.CollaborationVersionKey) ([]eventstore.CollaborationVersion, error) {
	if len(keys) == 0 {
		return []eventstore.CollaborationVersion{}, nil
	}
	if len(keys) > 64 {
		return nil, corestore.ErrInvalidPage
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
		result[i] = eventstore.CollaborationVersion(row)
	}
	return result, nil
}

func (s *session) insertRecordEvent(ctx context.Context, record eventstore.Event) (eventstore.Event, error) {
	row, err := s.queries.RecordInsertEvent(ctx, pgquery.RecordInsertEventParams{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, ID: record.ID, Sequence: record.Sequence, ProjectSequence: record.ProjectSequence, Type: record.Type, SchemaVersion: record.SchemaVersion, ActorKind: record.ActorKind, ActorPrincipalID: record.ActorPrincipalID, EventRevision: record.EventRevision, ChangeID: record.ChangeID, VersionID: record.VersionID, HeadSHA: record.HeadSHA, LandingWaiting: record.LandingWaiting, LandingChangeID: record.LandingChangeID, LandingVersionID: record.LandingVersionID, LandingHeadSHA: record.LandingHeadSHA, RunID: record.RunID, AttemptID: record.AttemptID, FromState: record.FromState, ToState: record.ToState, RecordedAt: record.RecordedAt, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, PayloadFormatVersion: record.PayloadFormatVersion})
	if err != nil {
		return eventstore.Event{}, mapError(ctx, err)
	}
	if err := s.putReportEventProjection(ctx, record); err != nil {
		return eventstore.Event{}, err
	}
	return eventRecord(row), nil
}
func (s *session) insertRecordCollaborationVersion(ctx context.Context, record eventstore.CollaborationVersion) (eventstore.CollaborationVersion, error) {
	row, err := s.queries.RecordInsertCollaborationVersion(ctx, pgquery.RecordInsertCollaborationVersionParams{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, RecordID: record.RecordID, Revision: record.Revision, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes})
	return eventstore.CollaborationVersion(row), mapError(ctx, err)
}
func (s *session) insertRecordChange(ctx context.Context, record eventstore.Change) (eventstore.Change, error) {
	row, err := s.queries.RecordInsertChange(ctx, pgquery.RecordInsertChangeParams{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, ID: record.ID, Revision: record.Revision, Title: record.Title, CurrentVersionID: record.CurrentVersionID, EvidenceSequence: record.EvidenceSequence, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, LandedVersionID: record.LandedVersionID, LandedAt: recordTimePointer(record.LandedAt), LandedHeadSHA: record.LandedHeadSHA, LandedMergeSHA: record.LandedMergeSHA, LandedBaseRef: record.LandedBaseRef})
	return eventstore.Change(row), mapError(ctx, err)
}
func (s *session) insertRecordVersion(ctx context.Context, record eventstore.Version) (eventstore.Version, error) {
	var sourceKey, sourceDigest *string
	var sourceBytes, sourceContentBytes sql.NullInt64
	if record.SourceObjectKey != "" {
		sourceKey = &record.SourceObjectKey
		sourceDigest = &record.SourceSHA256
		sourceBytes = sql.NullInt64{Int64: record.SourceBytes, Valid: true}
		sourceContentBytes = sql.NullInt64{Int64: record.SourceContentBytes, Valid: true}
	}
	row, err := s.queries.RecordInsertVersion(ctx, pgquery.RecordInsertVersionParams{OrganizationID: record.OrganizationID, ID: record.ID, ChangeID: record.ChangeID, Number: record.Number, HeadSHA: record.HeadSHA, BaseSHA: record.BaseSHA, MergeBaseSHA: record.MergeBaseSHA, Repository: record.Repository, PolicyRevisionID: record.PolicyRevisionID, AttemptID: record.AttemptID, ActorKind: record.ActorKind, ActorPrincipalID: record.ActorPrincipalID, ExternalProvider: record.ExternalProvider, ExternalID: record.ExternalID, ExternalURL: record.ExternalURL, CreatedAt: record.CreatedAt, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, PayloadFormatVersion: record.PayloadFormatVersion, SourceObjectKey: sourceKey, SourceSha256: sourceDigest, SourceBytes: sourceBytes, SourceContentBytes: sourceContentBytes, ImplementingSessionID: record.ImplementingSessionID, ImplementingMachineID: record.ImplementingMachineID})
	return versionRecord(row), mapError(ctx, err)
}
func (s *session) insertRecordEvidence(ctx context.Context, record eventstore.Evidence) (eventstore.Evidence, error) {
	row, err := s.queries.RecordInsertEvidence(ctx, pgquery.RecordInsertEvidenceParams{OrganizationID: record.OrganizationID, ChangeID: record.ChangeID, Sequence: record.Sequence, VersionID: record.VersionID, Kind: record.Kind, SourceKey: record.SourceKey, ActorKind: record.ActorKind, ActorPrincipalID: record.ActorPrincipalID, Decision: record.Decision, CreatedAt: record.CreatedAt, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, PayloadFormatVersion: record.PayloadFormatVersion})
	if err != nil {
		return eventstore.Evidence{}, mapError(ctx, err)
	}
	record.Sequence = row.Sequence
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
	if _, err := s.queries.RecordLockIssue(ctx, pgquery.RecordLockIssueParams{OrganizationID: s.organization, ProjectID: record.ProjectID, NativeID: record.WorkItemID}); err != nil {
		return eventstore.Event{}, mapError(ctx, err)
	}
	previous, err := s.queries.RecordGetEvent(ctx, pgquery.RecordGetEventParams{OrganizationID: s.organization, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, ID: record.ID})
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
		if !reflect.DeepEqual(eventRecord(previous), requested) {
			return eventstore.Event{}, corestore.ErrRevisionConflict
		}
		if err := s.putReportEventProjection(ctx, record); err != nil {
			return eventstore.Event{}, err
		}
		return eventRecord(previous), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return eventstore.Event{}, mapError(ctx, err)
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
		if !reflect.DeepEqual(eventstore.CollaborationVersion(previous), record) {
			return eventstore.CollaborationVersion{}, corestore.ErrRevisionConflict
		}
		return eventstore.CollaborationVersion(previous), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
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
	if !errors.Is(err, sql.ErrNoRows) {
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
	return eventstore.Change(row), updated(ctx, err)
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
		if !reflect.DeepEqual(versionRecord(previous), requested) {
			return eventstore.Version{}, corestore.ErrRevisionConflict
		}
		return versionRecord(previous), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return eventstore.Version{}, mapError(ctx, err)
	}
	record.CreatedAt = recordTime(record.CreatedAt)
	return s.insertRecordVersion(ctx, record)
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
	if _, err := s.queries.RecordLockChange(ctx, pgquery.RecordLockChangeParams{OrganizationID: s.organization, ID: record.ChangeID}); err != nil {
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
		if !errors.Is(err, sql.ErrNoRows) {
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
	return s.insertRecordEvidence(ctx, record)
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
	if !errors.Is(err, sql.ErrNoRows) {
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
