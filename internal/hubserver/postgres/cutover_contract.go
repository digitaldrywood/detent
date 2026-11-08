package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CutoverReceipt = pgquery.GithubCutover
type LocalImportMapping = pgquery.LocalBoardImport
type NeutralImportSource = pgquery.NeutralImportSource
type NeutralImportRecord = pgquery.NeutralImportRecord
type CutoverReceiptInput = pgquery.CutoverReceiptParams
type LocalImportMappingInput = pgquery.LocalImportMappingParams
type NeutralImportSourceInput = pgquery.NeutralImportSourceParams
type NeutralImportRecordInput = pgquery.NeutralImportRecordParams
type PutLocalImportMappingInput = pgquery.PutLocalImportMappingParams
type BindNeutralImportSourceInput = pgquery.BindNeutralImportSourceParams
type PutNeutralImportRecordInput = pgquery.PutNeutralImportRecordParams
type CutoverReader interface {
	projectstore.Reader
	CutoverReceipt(context.Context, CutoverReceiptInput) (CutoverReceipt, error)
	LocalImportMapping(context.Context, LocalImportMappingInput) (LocalImportMapping, error)
	NeutralImportSource(context.Context, NeutralImportSourceInput) (NeutralImportSource, error)
	NeutralImportRecord(context.Context, NeutralImportRecordInput) (NeutralImportRecord, error)
}
type CutoverWriter interface {
	CutoverReader
	projectstore.Writer
	CommitProjectCutover(context.Context, CutoverCommit) (CutoverReceipt, error)
	PutLocalImportMapping(context.Context, PutLocalImportMappingInput) (LocalImportMapping, error)
	BindNeutralImportSource(context.Context, BindNeutralImportSourceInput) (NeutralImportSource, error)
	PutNeutralImportRecord(context.Context, PutNeutralImportRecordInput) (NeutralImportRecord, error)
}

func (r *Repository) ReadCutover(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(CutoverReader) error) error {
	return r.transaction(ctx, org, workload, true, func(s *session) error { return run(s) })
}
func (r *Repository) WriteCutover(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, run func(CutoverWriter) error) error {
	return r.transaction(ctx, org, workload, false, func(s *session) error { return run(s) })
}
func (s *session) CutoverReceipt(ctx context.Context, arg CutoverReceiptInput) (CutoverReceipt, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CutoverReceipt{}, err
	}

	row, err := s.queries.CutoverReceipt(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) LocalImportMapping(ctx context.Context, arg LocalImportMappingInput) (LocalImportMapping, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return LocalImportMapping{}, err
	}

	row, err := s.queries.LocalImportMapping(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) NeutralImportSource(ctx context.Context, arg NeutralImportSourceInput) (NeutralImportSource, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return NeutralImportSource{}, err
	}

	row, err := s.queries.NeutralImportSource(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) NeutralImportRecord(ctx context.Context, arg NeutralImportRecordInput) (NeutralImportRecord, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return NeutralImportRecord{}, err
	}

	row, err := s.queries.NeutralImportRecord(ctx, arg)

	return row, mapError(ctx, err)
}
func (s *session) PutLocalImportMapping(ctx context.Context, arg PutLocalImportMappingInput) (LocalImportMapping, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return LocalImportMapping{}, err
	}
	if arg.SourceKey == "" || arg.SourceKind == "" || !artifact.ValidHash(arg.SourceDigest, 64) || arg.ImportedAt.IsZero() {
		return LocalImportMapping{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.PutLocalImportMapping(ctx, arg)
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = s.queries.LocalImportMapping(ctx, pgquery.LocalImportMappingParams{OrganizationID: s.organization, ProjectID: arg.ProjectID, SourceKey: arg.SourceKey})
		if err == nil && (row.SourceDigest != arg.SourceDigest || row.WorkItemID != arg.WorkItemID || row.SourceKind != arg.SourceKind) {
			return LocalImportMapping{}, corestore.ErrUniqueViolation
		}
	}
	return row, mapError(ctx, err)
}
func (s *session) BindNeutralImportSource(ctx context.Context, arg BindNeutralImportSourceInput) (NeutralImportSource, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return NeutralImportSource{}, err
	}
	if arg.SourceID == "" || arg.SchemaVersion != 1 || !artifact.ValidHash(arg.Checksum, 64) || !artifact.ValidHash(arg.SchemaDigest, 64) || arg.VerifiedAt.IsZero() {
		return NeutralImportSource{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.BindNeutralImportSource(ctx, arg)
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = s.queries.NeutralImportSource(ctx, pgquery.NeutralImportSourceParams{OrganizationID: s.organization, SourceID: arg.SourceID})
		if err == nil && (row.Checksum != arg.Checksum || row.SchemaDigest != arg.SchemaDigest || row.SchemaVersion != arg.SchemaVersion) {
			return NeutralImportSource{}, corestore.ErrRevisionConflict
		}
	}
	return row, mapError(ctx, err)
}
func (s *session) PutNeutralImportRecord(ctx context.Context, arg PutNeutralImportRecordInput) (NeutralImportRecord, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return NeutralImportRecord{}, err
	}
	if arg.SourceID == "" || arg.Family == "" || arg.SourceKey == "" || arg.DestinationID == "" || !artifact.ValidHash(arg.RecordDigest, 64) || arg.ImportedAt.IsZero() || eventstore.Reference(arg.PayloadObjectKey, arg.PayloadSHA256, arg.PayloadBytes, arg.PayloadContentBytes).Validate(string(s.organization)) != nil {
		return NeutralImportRecord{}, corestore.ErrInvalidPage
	}
	row, err := s.queries.PutNeutralImportRecord(ctx, arg)
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = s.queries.NeutralImportRecord(ctx, pgquery.NeutralImportRecordParams{OrganizationID: s.organization, SourceID: arg.SourceID, Family: arg.Family, SourceKey: arg.SourceKey})
		if err == nil && (row.RecordDigest != arg.RecordDigest || row.DestinationID != arg.DestinationID || row.SourceRevision != arg.SourceRevision) {
			return NeutralImportRecord{}, corestore.ErrRevisionConflict
		}
	}
	return row, mapError(ctx, err)
}

type CutoverCommit struct {
	Receipt                 pgquery.StoreCutoverReceiptParams
	ExpectedContentRevision int64
}

func (s *session) CommitProjectCutover(ctx context.Context, arg CutoverCommit) (CutoverReceipt, error) {
	receipt := arg.Receipt
	if err := s.checkOrganization(receipt.OrganizationID); err != nil {
		return CutoverReceipt{}, err
	}
	if receipt.SourceProfile != "github_compatible" || receipt.ExpectedRevision < 1 || arg.ExpectedContentRevision < 0 || receipt.ActorID == "" || receipt.CreatedAt.IsZero() || !artifact.ValidHash(receipt.Checkpoint, 64) || eventstore.Reference(receipt.ReceiptObjectKey, receipt.ReceiptSha256, receipt.ReceiptBytes, receipt.ReceiptContentBytes).Validate(string(s.organization)) != nil {
		return CutoverReceipt{}, corestore.ErrInvalidPage
	}
	if _, err := s.queries.LockActiveProject(ctx, pgquery.LockActiveProjectParams{OrganizationID: s.organization, ID: receipt.ProjectID}); err != nil {
		return CutoverReceipt{}, mapError(ctx, err)
	}
	existing, err := s.queries.CutoverReceipt(ctx, pgquery.CutoverReceiptParams{OrganizationID: s.organization, ProjectID: receipt.ProjectID, Checkpoint: receipt.Checkpoint})
	if err == nil {
		if existing.ReceiptSha256 != receipt.ReceiptSha256 || existing.ExpectedRevision != receipt.ExpectedRevision || existing.SourceProfile != receipt.SourceProfile {
			return CutoverReceipt{}, corestore.ErrRevisionConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CutoverReceipt{}, mapError(ctx, err)
	}
	if err := s.RequireProjectIdle(ctx, receipt.ProjectID, receipt.CreatedAt); err != nil {
		return CutoverReceipt{}, err
	}
	count, err := s.queries.ApplyCutoverProfile(ctx, pgquery.ApplyCutoverProfileParams{OrganizationID: s.organization, ID: receipt.ProjectID, Profile: receipt.SourceProfile, ContentSettingsRevision: arg.ExpectedContentRevision})
	if err != nil {
		return CutoverReceipt{}, mapError(ctx, err)
	}
	if count != 1 {
		return CutoverReceipt{}, corestore.ErrRevisionConflict
	}
	count, err = s.queries.ApplyCutoverSettings(ctx, pgquery.ApplyCutoverSettingsParams{OrganizationID: s.organization, ProjectID: receipt.ProjectID, Revision: receipt.ExpectedRevision})
	if err != nil {
		return CutoverReceipt{}, mapError(ctx, err)
	}
	if count != 1 {
		return CutoverReceipt{}, corestore.ErrRevisionConflict
	}
	row, err := s.queries.StoreCutoverReceipt(ctx, receipt)
	return row, mapError(ctx, err)
}
