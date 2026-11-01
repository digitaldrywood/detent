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
		records[i] = eventstore.Event(row)
	}
	return records, nil
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
		records[i] = eventstore.Version(row)
	}
	return records, nil
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
		records[i] = eventstore.Evidence(row)
	}
	return records, nil
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
func (s *session) insertRecordEvent(ctx context.Context, record eventstore.Event) (eventstore.Event, error) {
	row, err := s.queries.RecordInsertEvent(ctx, pgquery.RecordInsertEventParams{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, ID: record.ID, Sequence: record.Sequence, Type: record.Type, SchemaVersion: record.SchemaVersion, ActorKind: record.ActorKind, ActorPrincipalID: record.ActorPrincipalID, EventRevision: record.EventRevision, ChangeID: record.ChangeID, VersionID: record.VersionID, HeadSHA: record.HeadSHA, RunID: record.RunID, AttemptID: record.AttemptID, FromState: record.FromState, ToState: record.ToState, RecordedAt: record.RecordedAt, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, PayloadFormatVersion: record.PayloadFormatVersion})
	return eventstore.Event(row), mapError(ctx, err)
}
func (s *session) insertRecordCollaborationVersion(ctx context.Context, record eventstore.CollaborationVersion) (eventstore.CollaborationVersion, error) {
	row, err := s.queries.RecordInsertCollaborationVersion(ctx, pgquery.RecordInsertCollaborationVersionParams{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, RecordID: record.RecordID, Revision: record.Revision, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes})
	return eventstore.CollaborationVersion(row), mapError(ctx, err)
}
func (s *session) insertRecordChange(ctx context.Context, record eventstore.Change) (eventstore.Change, error) {
	row, err := s.queries.RecordInsertChange(ctx, pgquery.RecordInsertChangeParams{OrganizationID: record.OrganizationID, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, ID: record.ID, Revision: record.Revision, Title: record.Title, CurrentVersionID: record.CurrentVersionID, EvidenceSequence: record.EvidenceSequence, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes})
	return eventstore.Change(row), mapError(ctx, err)
}
func (s *session) insertRecordVersion(ctx context.Context, record eventstore.Version) (eventstore.Version, error) {
	row, err := s.queries.RecordInsertVersion(ctx, pgquery.RecordInsertVersionParams{OrganizationID: record.OrganizationID, ID: record.ID, ChangeID: record.ChangeID, Number: record.Number, HeadSHA: record.HeadSHA, BaseSHA: record.BaseSHA, MergeBaseSHA: record.MergeBaseSHA, Repository: record.Repository, PolicyRevisionID: record.PolicyRevisionID, AttemptID: record.AttemptID, ActorKind: record.ActorKind, ActorPrincipalID: record.ActorPrincipalID, ExternalProvider: record.ExternalProvider, ExternalID: record.ExternalID, ExternalURL: record.ExternalURL, CreatedAt: record.CreatedAt, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, PayloadFormatVersion: record.PayloadFormatVersion})
	return eventstore.Version(row), mapError(ctx, err)
}
func (s *session) insertRecordEvidence(ctx context.Context, record eventstore.Evidence) (eventstore.Evidence, error) {
	row, err := s.queries.RecordInsertEvidence(ctx, pgquery.RecordInsertEvidenceParams{OrganizationID: record.OrganizationID, ChangeID: record.ChangeID, Sequence: record.Sequence, VersionID: record.VersionID, Kind: record.Kind, SourceKey: record.SourceKey, ActorKind: record.ActorKind, ActorPrincipalID: record.ActorPrincipalID, Decision: record.Decision, CreatedAt: record.CreatedAt, PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes, PayloadFormatVersion: record.PayloadFormatVersion})
	return eventstore.Evidence(row), mapError(ctx, err)
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
		if requested.Sequence == 0 {
			requested.Sequence = previous.Sequence
		}
		if requested.RecordedAt.IsZero() || recordTime(requested.RecordedAt).Equal(previous.RecordedAt) {
			requested.RecordedAt = previous.RecordedAt
		}
		if !reflect.DeepEqual(eventstore.Event(previous), requested) {
			return eventstore.Event{}, corestore.ErrRevisionConflict
		}
		return eventstore.Event(previous), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return eventstore.Event{}, mapError(ctx, err)
	}
	sequence, err := s.queries.RecordAdvanceIssue(ctx, pgquery.RecordAdvanceIssueParams{OrganizationID: s.organization, ProjectID: record.ProjectID, NativeID: record.WorkItemID, ImportedSequence: record.Sequence})
	if err != nil {
		return eventstore.Event{}, mapError(ctx, err)
	}
	if record.Sequence == 0 {
		record.Sequence = sequence
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
	row, err := s.queries.RecordUpdateChange(ctx, pgquery.RecordUpdateChangeParams{OrganizationID: s.organization, ID: record.ID, ProjectID: record.ProjectID, WorkItemID: record.WorkItemID, ExpectedRevision: record.Revision, Title: record.Title, CurrentVersionID: record.CurrentVersionID, UpdatedAt: recordTime(record.UpdatedAt), PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes})
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
	if _, err := s.queries.RecordLockChange(ctx, pgquery.RecordLockChangeParams{OrganizationID: s.organization, ID: record.ChangeID}); err != nil {
		return eventstore.Version{}, mapError(ctx, err)
	}
	previous, err := s.queries.RecordGetVersion(ctx, pgquery.RecordGetVersionParams{OrganizationID: s.organization, ChangeID: record.ChangeID, ID: record.ID})
	if err == nil {
		requested := record
		if requested.CreatedAt.IsZero() || recordTime(requested.CreatedAt).Equal(previous.CreatedAt) {
			requested.CreatedAt = previous.CreatedAt
		}
		if !reflect.DeepEqual(eventstore.Version(previous), requested) {
			return eventstore.Version{}, corestore.ErrRevisionConflict
		}
		return eventstore.Version(previous), nil
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
			if requested.Sequence == 0 {
				requested.Sequence = previous.Sequence
			}
			if requested.CreatedAt.IsZero() || recordTime(requested.CreatedAt).Equal(previous.CreatedAt) {
				requested.CreatedAt = previous.CreatedAt
			}
			if !reflect.DeepEqual(eventstore.Evidence(previous), requested) {
				return eventstore.Evidence{}, corestore.ErrRevisionConflict
			}
			return eventstore.Evidence(previous), nil
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
		if eventstore.Link(previous) != record {
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
	row, err := s.queries.RecordGetReceipt(ctx, pgquery.RecordGetReceiptParams{OrganizationID: s.organization, ActorID: actor, Operation: operation, CommandKey: key})
	return eventstore.Receipt(row), mapError(ctx, err)
}

func (s *session) PutReceipt(ctx context.Context, record eventstore.Receipt) (eventstore.Receipt, error) {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return eventstore.Receipt{}, err
	}
	if record.ActorID == "" || record.Operation == "" || record.CommandKey == "" || !artifact.ValidHash(record.RequestHash, 64) {
		return eventstore.Receipt{}, eventstore.ErrInvalidRecord
	}
	if err := validRecordPayload(record.OrganizationID, record.PayloadObjectKey, record.PayloadSHA256, record.PayloadBytes, record.PayloadContentBytes); err != nil {
		return eventstore.Receipt{}, err
	}
	row, err := s.queries.RecordInsertReceipt(ctx, pgquery.RecordInsertReceiptParams{OrganizationID: s.organization, ActorID: record.ActorID, Operation: record.Operation, CommandKey: record.CommandKey, RequestHash: record.RequestHash, CreatedAt: recordTime(record.CreatedAt), PayloadObjectKey: record.PayloadObjectKey, PayloadSHA256: record.PayloadSHA256, PayloadBytes: record.PayloadBytes, PayloadContentBytes: record.PayloadContentBytes})
	if errors.Is(err, sql.ErrNoRows) {
		row, err = s.queries.RecordGetReceipt(ctx, pgquery.RecordGetReceiptParams{OrganizationID: s.organization, ActorID: record.ActorID, Operation: record.Operation, CommandKey: record.CommandKey})
		if err == nil && row.RequestHash != record.RequestHash {
			return eventstore.Receipt{}, corestore.ErrRevisionConflict
		}
	}
	return eventstore.Receipt(row), mapError(ctx, err)
}
