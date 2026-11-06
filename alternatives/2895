package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ storageconversion.Repository = (*Repository)(nil)

func (r *Repository) ConversionPage(ctx context.Context, scope storageconversion.Scope, cursor storageconversion.Cursor, limit int) (storageconversion.Page, error) {
	if scope.Validate() != nil || limit < 1 || limit > storageconversion.MaxRows {
		return storageconversion.Page{}, storageconversion.ErrInvalid
	}
	if cursor != (storageconversion.Cursor{}) && (cursor.Scope != scope || cursor.Through.ID == "" || cursor.Through.Sequence < 1 || cursor.After.Sequence < 0) {
		return storageconversion.Page{}, storageconversion.ErrInvalid
	}
	var page storageconversion.Page
	err := r.transaction(ctx, scope.Organization, corestore.Background, true, func(s *session) error {
		if cursor == (storageconversion.Cursor{}) {
			bound, err := s.queries.ConversionBounds(ctx, pgquery.ConversionBoundsParams{Family: string(scope.Family), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: tracker.NativeWorkItemID(scope.AggregateID)})
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			cursor = storageconversion.Cursor{Scope: scope, Through: storageconversion.Position{ID: bound.RecordID, Sequence: bound.RecordSequence}}
		}
		rows, err := s.queries.ConversionPage(ctx, pgquery.ConversionPageParams{Family: string(scope.Family), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: tracker.NativeWorkItemID(scope.AggregateID), AfterID: cursor.After.ID, AfterSequence: cursor.After.Sequence, ThroughID: cursor.Through.ID, ThroughSequence: cursor.Through.Sequence, PageLimit: int32(limit)})
		if err != nil {
			return err
		}
		for _, row := range rows {
			page.Items = append(page.Items, storageconversion.Candidate{Scope: scope, Position: storageconversion.Position{ID: row.RecordID, Sequence: row.RecordSequence}, FormatVersion: row.PayloadFormatVersion, Source: eventstore.Reference(row.PayloadObjectKey, row.PayloadSHA256, row.PayloadBytes, row.PayloadContentBytes)})
		}
		if len(page.Items) > 0 {
			cursor.After = page.Items[len(page.Items)-1].Position
			page.Next = cursor
		}
		return nil
	})
	return page, err
}

func (r *Repository) CommitConversion(ctx context.Context, candidate storageconversion.Candidate, target attachment.ArchiveReference) (bool, error) {
	scope := candidate.Scope
	if scope.Validate() != nil || candidate.Position.ID == "" || candidate.Position.Sequence < 1 || candidate.FormatVersion != storageconversion.CurrentVersion-1 || candidate.Source.Validate(string(scope.Organization)) != nil || target.Validate(string(scope.Organization)) != nil || target.BindingID != "" || target.CustomerOwned || target.SchemaVersion != 1 {
		return false, storageconversion.ErrInvalid
	}
	var committed bool
	err := r.transaction(ctx, scope.Organization, corestore.Background, false, func(s *session) error {
		var err error
		committed, err = s.queries.ConversionCommit(ctx, pgquery.ConversionCommitParams{OrganizationID: string(scope.Organization), ProjectID: string(scope.Project), Family: string(scope.Family), RecordID: candidate.Position.ID, RecordSequence: candidate.Position.Sequence, AggregateID: scope.AggregateID, ExpectedVersion: candidate.FormatVersion, SourceKey: candidate.Source.Key, SourceDigest: candidate.Source.SHA256, SourceBytes: candidate.Source.CompressedSize, SourceContentBytes: candidate.Source.UncompressedSize, TargetKey: target.Key, TargetDigest: target.SHA256, TargetBytes: target.CompressedSize, TargetContentBytes: target.UncompressedSize})
		return err
	})
	return committed && err == nil, err
}

func (r *Repository) ConversionProgress(ctx context.Context, scope storageconversion.Scope) (storageconversion.Progress, error) {
	if scope.Validate() != nil {
		return storageconversion.Progress{}, storageconversion.ErrInvalid
	}
	var progress storageconversion.Progress
	err := r.transaction(ctx, scope.Organization, corestore.Background, true, func(s *session) error {
		row, err := s.queries.ConversionProgress(ctx, pgquery.ConversionProgressParams{OrganizationID: scope.Organization, ProjectID: scope.Project, Family: string(scope.Family), AggregateID: scope.AggregateID})
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		progress = storageconversion.Progress{Rows: row.ConvertedRows, Bytes: row.ConvertedBytes}
		return err
	})
	return progress, err
}
