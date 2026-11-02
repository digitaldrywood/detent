package postgres

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

type preparedCostImportRow struct {
	key          [7]string
	sourceKey    string
	sourceVoided *bool
	record       billingstore.CostObservation
}

func (i *CostImporter) Step(ctx context.Context, limit int) (CostImportProgress, error) {
	if limit < 1 || limit > costImportPageLimit {
		return CostImportProgress{}, ErrCostImport
	}
	checkpoint, err := i.start(ctx)
	if err != nil {
		return CostImportProgress{}, err
	}
	if checkpoint.progress.Done {
		return checkpoint.progress, nil
	}
	var rows []preparedCostImportRow
	if checkpoint.phase == 0 {
		rows, err = i.prepareSourcePage(ctx, checkpoint, limit)
	} else {
		rows, err = i.prepareRetainedPage(ctx, checkpoint, limit)
	}
	if err != nil {
		return checkpoint.progress, err
	}
	next := checkpoint
	err = i.repository.transaction(ctx, i.organization, corestore.Background, false, func(s *session) error {
		current, err := readCostImportCheckpoint(ctx, s.db, i.organization, true)
		if err != nil {
			return err
		}
		if current != checkpoint {
			return corestore.ErrRevisionConflict
		}
		switch checkpoint.phase {
		case 0:
			for _, row := range rows {
				if err := i.retainRow(ctx, s, row); err != nil {
					return err
				}
			}
			next.progress.SourceRows += int64(len(rows))
		case 1:
			if len(rows) > 0 {
				records := make([]billingstore.CostObservation, len(rows))
				for n, row := range rows {
					records[n] = row.record
				}
				if _, err := s.RecordBillingCosts(ctx, records); err != nil {
					return err
				}
			}
			next.progress.CurrentSources += int64(len(rows))
		case 2:
			for _, row := range rows {
				if _, err := s.ImportBillingCostRevision(ctx, row.record); err != nil {
					return err
				}
			}
			next.progress.HistoryRows += int64(len(rows))
		default:
			return ErrCostImport
		}
		if len(rows) > 0 {
			next.after, next.hasAfter = rows[len(rows)-1].key, true
		}
		if len(rows) < limit {
			next.phase++
			next.after = [7]string{}
			next.hasAfter = false
		}
		next.progress.Done = next.phase == 3
		return advanceCostImportCheckpoint(ctx, s, checkpoint, next)
	})
	if err != nil {
		return checkpoint.progress, err
	}
	return next.progress, nil
}

func (i *CostImporter) prepareSourcePage(ctx context.Context, checkpoint costImportCheckpoint, limit int) ([]preparedCostImportRow, error) {
	tx, err := i.sourceTransaction(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	page, err := readCostImportPage(ctx, tx, i.source.Source.Schema, i.source.Catalog, i.binding, checkpoint.after, checkpoint.hasAfter, limit)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	prepared := make([]preparedCostImportRow, 0, len(page))
	for _, row := range page {
		record, err := decodeCostImportRecord(i.organization, row.record)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(record.Observation)
		if err != nil {
			return nil, err
		}
		ref, err := i.source.Objects.StoreDetail(ctx, string(i.organization), raw)
		if err != nil {
			return nil, err
		}
		record.Reference = billingstore.AuditReferenceFromArchive(ref)
		o := record.Observation
		key := costIdentity(o.Provider, o.ProviderAccount, o.SourceID, o.From.UTC().Format(costPeriodLayout), o.To.UTC().Format(costPeriodLayout))
		prepared = append(prepared, preparedCostImportRow{key: row.key, sourceKey: key, sourceVoided: row.record.Voided, record: record})
	}
	return prepared, nil
}

func (i *CostImporter) retainRow(ctx context.Context, s *session, row preparedCostImportRow) error {
	if err := s.validateBillingCost(row.record); err != nil {
		return err
	}
	if row.record.ProjectID != "" {
		if _, err := s.Project(ctx, row.record.ProjectID); err != nil {
			return err
		}
	}
	r := row.record.Reference
	_, err := s.db.ExecContext(ctx, `INSERT INTO cost_import_rows (organization_id,source_key,revision,project_id,received_at,source_voided,binding_id,customer_owned,object_key,sha256,compressed_bytes,content_bytes,codec,format_version,kind,retention) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, i.organization, row.sourceKey, row.record.Observation.Revision, row.record.ProjectID, row.record.ReceivedAt.UTC().Format(costPeriodLayout), row.sourceVoided, r.BindingID, r.CustomerOwned, r.Key, r.SHA256, r.CompressedBytes, r.ContentBytes, r.Codec, r.FormatVersion, r.Kind, r.Retention)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO cost_import_heads (organization_id,source_key,revision) VALUES ($1,$2,$3) ON CONFLICT (organization_id,source_key) DO UPDATE SET revision=GREATEST(cost_import_heads.revision,EXCLUDED.revision)`, i.organization, row.sourceKey, row.record.Observation.Revision)
	return err
}

func (i *CostImporter) prepareRetainedPage(ctx context.Context, checkpoint costImportCheckpoint, limit int) ([]preparedCostImportRow, error) {
	var prepared []preparedCostImportRow
	err := i.repository.transaction(ctx, i.organization, corestore.Background, true, func(s *session) error {
		projection := `SELECT r.source_key,r.revision,r.project_id,r.received_at,r.binding_id,r.customer_owned,r.object_key,r.sha256,r.compressed_bytes,r.content_bytes,r.codec,r.format_version,r.kind,r.retention FROM cost_import_rows r`
		statement := projection + ` WHERE r.organization_id=$1`
		args := []any{i.organization}
		if checkpoint.phase == 1 {
			statement = `WITH page AS MATERIALIZED (SELECT source_key,revision FROM cost_import_heads WHERE organization_id=$1 AND source_key>$2 ORDER BY source_key LIMIT $3) ` + projection + ` JOIN page h ON h.source_key=r.source_key AND h.revision=r.revision WHERE r.organization_id=$1 ORDER BY r.source_key`
			args = append(args, checkpoint.after[0], limit)
		} else {
			if checkpoint.hasAfter {
				revision, err := strconv.ParseInt(checkpoint.after[1], 10, 64)
				if err != nil {
					return ErrCostImport
				}
				statement += ` AND (r.source_key,r.revision)>($2,$3)`
				args = append(args, checkpoint.after[0], revision)
			}
			statement += ` ORDER BY r.source_key,r.revision LIMIT $` + strconv.Itoa(len(args)+1)
			args = append(args, limit)
		}
		rows, err := s.db.QueryContext(ctx, statement, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row preparedCostImportRow
			var revision int64
			var received string
			r := &row.record.Reference
			if err := rows.Scan(&row.sourceKey, &revision, &row.record.ProjectID, &received, &r.BindingID, &r.CustomerOwned, &r.Key, &r.SHA256, &r.CompressedBytes, &r.ContentBytes, &r.Codec, &r.FormatVersion, &r.Kind, &r.Retention); err != nil {
				return err
			}
			row.record.OrganizationID = i.organization
			row.record.ReceivedAt, err = time.Parse(costPeriodLayout, received)
			if err != nil {
				return err
			}
			row.key[0], row.key[1] = row.sourceKey, strconv.FormatInt(revision, 10)
			prepared = append(prepared, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	for n := range prepared {
		row := &prepared[n]
		raw, err := i.source.Objects.ReadDetail(ctx, string(i.organization), row.record.Reference.ArchiveReference())
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &row.record.Observation); err != nil {
			return nil, err
		}
		o := row.record.Observation
		if strconv.FormatInt(o.Revision, 10) != row.key[1] || costIdentity(o.Provider, o.ProviderAccount, o.SourceID, o.From.UTC().Format(costPeriodLayout), o.To.UTC().Format(costPeriodLayout)) != row.sourceKey {
			return nil, ErrCostImport
		}
	}
	return prepared, nil
}
