package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CostImportProgress struct {
	SourceRows     int64
	CurrentSources int64
	HistoryRows    int64
	Done           bool
}

type costImportCheckpoint struct {
	sourceSchema  string
	snapshotID    string
	catalogDigest string
	phase         int
	revision      int64
	progress      CostImportProgress
	hasAfter      bool
	after         [7]string
}

func (i *CostImporter) start(ctx context.Context) (costImportCheckpoint, error) {
	var checkpoint costImportCheckpoint
	err := i.repository.transaction(ctx, i.organization, corestore.Background, false, func(s *session) error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO cost_imports (organization_id,source_schema,snapshot_id,catalog_digest) VALUES ($1,$2,$3,$4) ON CONFLICT (organization_id) DO NOTHING`, i.organization, i.source.Source.Schema, i.source.SnapshotID, i.digest)
		if err != nil {
			return err
		}
		checkpoint, err = readCostImportCheckpoint(ctx, s.db, i.organization, false)
		return err
	})
	if err != nil {
		return checkpoint, err
	}
	if checkpoint.sourceSchema != i.source.Source.Schema || checkpoint.snapshotID != i.source.SnapshotID || checkpoint.catalogDigest != i.digest {
		return checkpoint, ErrCostImport
	}
	return checkpoint, nil
}

func readCostImportCheckpoint(ctx context.Context, db pgqueryDBTX, organization tracker.OrganizationID, lock bool) (costImportCheckpoint, error) {
	statement := `SELECT source_schema,snapshot_id,catalog_digest,phase,revision,source_rows,current_sources,history_rows,has_after,after_key_1,after_key_2,after_key_3,after_key_4,after_key_5,after_key_6,after_key_7 FROM cost_imports WHERE organization_id=$1`
	if lock {
		statement += " FOR UPDATE"
	}
	var row costImportCheckpoint
	err := db.QueryRowContext(ctx, statement, organization).Scan(&row.sourceSchema, &row.snapshotID, &row.catalogDigest, &row.phase, &row.revision, &row.progress.SourceRows, &row.progress.CurrentSources, &row.progress.HistoryRows, &row.hasAfter, &row.after[0], &row.after[1], &row.after[2], &row.after[3], &row.after[4], &row.after[5], &row.after[6])
	row.progress.Done = row.phase == 3
	return row, err
}

func advanceCostImportCheckpoint(ctx context.Context, s *session, before, next costImportCheckpoint) error {
	if next.phase < before.phase || next.phase > before.phase+1 || next.phase > 3 || next.progress.SourceRows < before.progress.SourceRows || next.progress.CurrentSources < before.progress.CurrentSources || next.progress.HistoryRows < before.progress.HistoryRows || next.progress.HistoryRows > next.progress.SourceRows {
		return ErrCostImport
	}
	if next.phase == 3 && next.progress.HistoryRows != next.progress.SourceRows {
		return ErrCostImport
	}
	result, err := s.db.ExecContext(ctx, `UPDATE cost_imports SET phase=$1,revision=revision+1,source_rows=$2,current_sources=$3,history_rows=$4,has_after=$5,after_key_1=$6,after_key_2=$7,after_key_3=$8,after_key_4=$9,after_key_5=$10,after_key_6=$11,after_key_7=$12 WHERE organization_id=$13 AND revision=$14`, next.phase, next.progress.SourceRows, next.progress.CurrentSources, next.progress.HistoryRows, next.hasAfter, next.after[0], next.after[1], next.after[2], next.after[3], next.after[4], next.after[5], next.after[6], s.organization, before.revision)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return corestore.ErrRevisionConflict
	}
	return nil
}
