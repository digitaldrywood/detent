package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrCutoverQueuePosition = errors.New("cutover requires one queue position per work item")

type CutoverReceipt = pgquery.GithubCutover
type LocalImportMapping = pgquery.LocalBoardImport
type NeutralImportSource = pgquery.NeutralImportSource
type NeutralImportRecord = pgquery.NeutralImportRecord
type CutoverReceiptInput = pgquery.CutoverReceiptParams
type LatestProjectCutoverReceiptInput = pgquery.LatestProjectCutoverReceiptParams
type LocalImportMappingInput = pgquery.LocalImportMappingParams
type NeutralImportSourceInput = pgquery.NeutralImportSourceParams
type NeutralImportRecordInput = pgquery.NeutralImportRecordParams
type PutLocalImportMappingInput = pgquery.PutLocalImportMappingParams
type BindNeutralImportSourceInput = pgquery.BindNeutralImportSourceParams
type PutNeutralImportRecordInput = pgquery.PutNeutralImportRecordParams
type CutoverReader interface {
	projectstore.Reader
	CutoverReceipt(context.Context, CutoverReceiptInput) (CutoverReceipt, error)
	LatestProjectCutoverReceipt(context.Context, LatestProjectCutoverReceiptInput) (CutoverReceipt, error)
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
func (s *session) LatestProjectCutoverReceipt(ctx context.Context, arg LatestProjectCutoverReceiptInput) (CutoverReceipt, error) {
	if err := s.checkOrganization(arg.OrganizationID); err != nil {
		return CutoverReceipt{}, err
	}
	row, err := s.queries.LatestProjectCutoverReceipt(ctx, arg)
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

type CutoverQueueItem struct {
	WorkItemID       tracker.NativeWorkItemID
	ExpectedRevision int64
	State            string
}

type CutoverCommit struct {
	States                  []tracker.NativeState
	Queues                  []CutoverQueueItem
	Receipt                 pgquery.StoreCutoverReceiptParams
	ExpectedContentRevision int64
}

func (s *session) CommitProjectCutover(ctx context.Context, arg CutoverCommit) (CutoverReceipt, error) {
	receipt := arg.Receipt
	if arg.States != nil {
		if err := policy.ValidateStates(arg.States); err != nil {
			return CutoverReceipt{}, err
		}
	}
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
	if arg.States != nil {
		if err := s.replaceProjectStates(ctx, receipt.ProjectID, arg.States); err != nil {
			return CutoverReceipt{}, err
		}
		if err := s.recordBoardDelta(receipt.ProjectID, viewstore.BoardDelta{Topic: "lane", EntityID: string(receipt.ProjectID), EntityRevision: new(arg.ExpectedContentRevision + 1), Kind: "invalidate"}); err != nil {
			return CutoverReceipt{}, err
		}
	}
	for _, queue := range arg.Queues {
		if err := s.cutoverQueue(ctx, receipt.ProjectID, queue, receipt.CreatedAt); err != nil {
			return CutoverReceipt{}, err
		}
	}
	row, err := s.queries.StoreCutoverReceipt(ctx, receipt)
	return row, mapError(ctx, err)
}

func (s *session) cutoverQueue(ctx context.Context, project tracker.ProjectID, input CutoverQueueItem, at time.Time) error {
	item, err := s.WorkItem(ctx, project, input.WorkItemID)
	if err != nil {
		return err
	}
	if item.Revision != input.ExpectedRevision {
		return corestore.ErrRevisionConflict
	}
	rows, err := s.db.Query(ctx, `SELECT scope,state,rank,priority_override,created_at,updated_at FROM queue_entries WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 ORDER BY scope LIMIT 2 FOR UPDATE`, s.organization, project, input.WorkItemID)
	if err != nil {
		return mapError(ctx, err)
	}
	entries := []claimstore.QueueEntry{}
	for rows.Next() {
		entry := claimstore.QueueEntry{OrganizationID: s.organization, ProjectID: project, WorkItemID: input.WorkItemID}
		if err := rows.Scan(&entry.Scope, &entry.State, &entry.Rank, &entry.Priority, &entry.CreatedAt, &entry.UpdatedAt); err != nil {
			rows.Close()
			return mapError(ctx, err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return mapError(ctx, err)
	}
	rows.Close()
	if len(entries) > 1 {
		return ErrCutoverQueuePosition
	}
	entry := claimstore.QueueEntry{OrganizationID: s.organization, ProjectID: project, WorkItemID: input.WorkItemID, Rank: string(input.WorkItemID), CreatedAt: at}
	if len(entries) == 1 {
		entry = entries[0]
		if err := s.DeleteQueueEntry(ctx, project, input.WorkItemID, entry.Scope); err != nil {
			return err
		}
	}
	entry.Scope, entry.State, entry.UpdatedAt = string(project), input.State, at
	return s.PutQueueEntry(ctx, entry)
}
