package postgres

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ConversionStorage struct {
	repository   *Repository
	organization tracker.OrganizationID
	converter    *storageconversion.Converter
	content      *eventstore.Content
}

var _ storageconversion.Storage = (*ConversionStorage)(nil)

func NewConversionStorage(repository *Repository, organization tracker.OrganizationID, objects attachment.DetailStore, transform storageconversion.Transform) *ConversionStorage {
	return &ConversionStorage{repository: repository, organization: organization, converter: storageconversion.New(repository, objects, transform), content: eventstore.NewContent(objects)}
}

func (s *ConversionStorage) Start(ctx context.Context) ([]string, error) {
	err := s.repository.transaction(ctx, s.organization, corestore.Background, false, func(session *session) error {
		return session.queries.ConversionInitializeCursors(ctx, string(s.organization))
	})
	return []string{"runs", "versions", "evidence", "core-work-events"}, err
}

func (s *ConversionStorage) Next(ctx context.Context, families []string) (storageconversion.FamilyCursor, error) {
	var cursor storageconversion.FamilyCursor
	err := s.repository.transaction(ctx, s.organization, corestore.Background, true, func(session *session) error {
		row, err := session.queries.ConversionNextCursor(ctx, pgquery.ConversionNextCursorParams{OrganizationID: s.organization, Families: families})
		cursor = storageconversion.FamilyCursor{Family: row.Family, Turn: row.Turn, Positioned: row.Positioned, Keys: [4]string{row.Key1, row.Key2, row.Key3, row.Key4}, Sequence: row.Sequence}
		return err
	})
	return cursor, err
}

func (s *ConversionStorage) Update(ctx context.Context, cursor storageconversion.FamilyCursor, update storageconversion.CursorUpdate, usage storageconversion.Usage) error {
	return s.repository.transaction(ctx, s.organization, corestore.Background, false, func(session *session) error {
		switch update {
		case storageconversion.Reserve:
			return session.queries.ConversionReserveCursor(ctx, pgquery.ConversionReserveCursorParams{OrganizationID: s.organization, Family: cursor.Family})
		case storageconversion.Restore:
			return session.queries.ConversionRestoreCursor(ctx, pgquery.ConversionRestoreCursorParams{OrganizationID: s.organization, Family: cursor.Family, Turn: cursor.Turn})
		case storageconversion.Record:
			return session.queries.ConversionRecordUsage(ctx, pgquery.ConversionRecordUsageParams{OrganizationID: s.organization, Family: cursor.Family, Rows: int64(usage.Rows), Bytes: usage.Bytes, Oversized: int64(usage.Oversized)})
		default:
			return storageconversion.ErrInvalid
		}
	})
}

func (s *ConversionStorage) advance(ctx context.Context, cursor storageconversion.FamilyCursor) error {
	return s.repository.transaction(ctx, s.organization, corestore.Background, false, func(session *session) error {
		return session.queries.ConversionAdvanceCursor(ctx, pgquery.ConversionAdvanceCursorParams{OrganizationID: s.organization, Family: cursor.Family, Positioned: cursor.Positioned, Key1: cursor.Keys[0], Key2: cursor.Keys[1], Key3: cursor.Keys[2], Key4: cursor.Keys[3], Sequence: cursor.Sequence})
	})
}

func (s *ConversionStorage) Convert(ctx context.Context, cursor storageconversion.FamilyCursor, turn storageconversion.Turn) error {
	candidate, next, err := s.candidate(ctx, cursor)
	if err != nil {
		return err
	}
	if candidate.Position.ID == "" {
		if cursor.Positioned {
			cursor.Positioned = false
			return s.advance(ctx, cursor)
		}
		return nil
	}
	if turn.Visit(candidate) {
		return nil
	}
	if err := turn.Charge(int(candidate.Source.UncompressedSize)); err != nil {
		return err
	}
	turn.Advance(func(ctx context.Context) error { return s.advance(ctx, next) })
	if candidate.Scope.Family == coreWorkEventFamily {
		return s.convertCoreWorkEvent(ctx, candidate, turn)
	}
	_, _, err = s.converter.Convert(ctx, candidate, turn.Expand)
	return err
}

func (s *ConversionStorage) candidate(ctx context.Context, cursor storageconversion.FamilyCursor) (storageconversion.Candidate, storageconversion.FamilyCursor, error) {
	var candidate storageconversion.Candidate
	next := cursor
	err := s.repository.transaction(ctx, s.organization, corestore.Background, true, func(session *session) error {
		for positioned := cursor.Positioned; ; positioned = false {
			keys, sequence := cursor.Keys, cursor.Sequence
			if !positioned {
				keys, sequence = [4]string{}, 0
			}
			var err error
			candidate.Scope = storageconversion.Scope{Organization: s.organization, Family: storageconversion.Family(cursor.Family)}
			switch candidate.Scope.Family {
			case coreWorkEventFamily:
				row, readErr := session.queries.ConversionNextCoreWorkEvent(ctx, pgquery.ConversionNextCoreWorkEventParams{OrganizationID: s.organization, AfterSourceID: sequence})
				err = readErr
				candidate.Scope.Project, candidate.Scope.AggregateID = row.ProjectID, strconv.FormatInt(row.IssueID, 10)
				candidate.Position = storageconversion.Position{ID: strconv.FormatInt(row.SourceID, 10), Sequence: row.SourceID}
				candidate.FormatVersion = 1
				candidate.Source.UncompressedSize = row.PayloadContentBytes
				next.Keys = [4]string{}
			case storageconversion.Runs:
				row, readErr := session.queries.ConversionNextRun(ctx, pgquery.ConversionNextRunParams{OrganizationID: s.organization, ProjectID: keys[0], AggregateID: keys[1], RecordID: keys[2], Sequence: sequence})
				err = readErr
				candidate.Scope.Project, candidate.Scope.AggregateID = row.ProjectID, string(row.AggregateID)
				candidate.Position = storageconversion.Position{ID: row.RecordID, Sequence: row.RecordSequence}
				candidate.FormatVersion = row.PayloadFormatVersion
				candidate.Source = eventstore.Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes)
				next.Keys = [4]string{string(row.ProjectID), string(row.AggregateID), row.RecordID}
			case storageconversion.Versions:
				row, readErr := session.queries.ConversionNextVersion(ctx, pgquery.ConversionNextVersionParams{OrganizationID: s.organization, AggregateID: keys[0], RecordID: keys[1], Sequence: sequence})
				err = readErr
				candidate.Scope.Project, candidate.Scope.AggregateID = row.ProjectID, row.AggregateID
				candidate.Position = storageconversion.Position{ID: row.RecordID, Sequence: row.RecordSequence}
				candidate.FormatVersion = row.PayloadFormatVersion
				candidate.Source = eventstore.Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes)
				next.Keys = [4]string{row.AggregateID, row.RecordID}
			case storageconversion.Evidence:
				row, readErr := session.queries.ConversionNextEvidence(ctx, pgquery.ConversionNextEvidenceParams{OrganizationID: s.organization, AggregateID: keys[0], Sequence: sequence})
				err = readErr
				candidate.Scope.Project, candidate.Scope.AggregateID = row.ProjectID, row.AggregateID
				candidate.Position = storageconversion.Position{ID: row.RecordID, Sequence: row.RecordSequence}
				candidate.FormatVersion = row.PayloadFormatVersion
				candidate.Source = eventstore.Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes)
				next.Keys = [4]string{row.AggregateID}
			default:
				return storageconversion.ErrInvalid
			}
			if positioned && errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if errors.Is(err, pgx.ErrNoRows) {
				candidate = storageconversion.Candidate{}
				return nil
			}
			if err != nil {
				return err
			}
			next.Positioned, next.Sequence = true, candidate.Position.Sequence
			return nil
		}
	})
	return candidate, next, err
}
