package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type continuationSourcePlan struct {
	head int64
}

func prepareContinuationSources(ctx context.Context, db *sql.DB, sources []ConversationHistoryImportSource) error {
	seen := make(map[string]bool)
	for _, source := range sources {
		if seen[source.Source.OrganizationID] {
			return errors.New("duplicate conversation continuation source")
		}
		seen[source.Source.OrganizationID] = true
		if source.CopyPath != "" || source.BundlePath == "" || source.Objects == nil || source.Source.Store != "hub" || source.Source.OrganizationID == "" {
			return errors.New("invalid conversation continuation source")
		}
		if source.Schema == nil {
			source.Schema = schemaFromCatalog(source.Catalog)
		}
		source.Source.Schema = "hub_copy"
		if _, err := source.Catalog.Bind(source.Source, source.Schema); err != nil {
			return err
		}
		catalog, err := conversationImportCatalogDigest(source.Catalog, source.Schema)
		if err != nil {
			return err
		}
		organization := tracker.OrganizationID(source.Source.OrganizationID)
		if _, err := db.ExecContext(ctx, `SELECT set_config('detent.organization_id',$1,false)`, organization); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM conversation_continuation_recovery WHERE organization_id=$1`, organization); err != nil {
			return err
		}
		var snapshot, sourceID, manifest string
		var plans int64
		err = ReadNeutralImportBundle(ctx, source.BundlePath, organization, func(bundle *VerifiedNeutralImport) (callbackErr error) {
			source.neutral = bundle
			if err := validateNeutralConversationHistory(ctx, source); err != nil {
				return err
			}
			index, err := buildConversationMessageIndex(ctx, bundle)
			if err != nil {
				return err
			}
			defer func() { callbackErr = errors.Join(callbackErr, index.close()) }()
			source.messages = index
			copyHash := bundle.Identity().SourceSHA256
			snapshot, sourceID, manifest = copyHash, bundle.Identity().SourceID, bundle.Digest()
			var valid bool
			if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_history_imports WHERE organization_id=$1 AND snapshot_id=$2 AND catalog_digest=$3 AND completed) AND NOT EXISTS(SELECT 1 FROM neutral_import_sources WHERE organization_id=$1 AND source_id=$4 AND checksum<>$5)`, organization, copyHash, catalog, sourceID, manifest).Scan(&valid); err != nil {
				return err
			}
			if !valid {
				return mutation.ErrConflict
			}
			if err := verifyContinuationSourceReceipts(ctx, db, source, organization, copyHash); err != nil {
				return err
			}
			table := conversationImportTable{source: "conversations", target: "conversations", keys: []string{"id"}}
			var after int64
			for {
				page, err := readConversationHistoryPage(ctx, source, table, organization, after)
				if err != nil {
					return err
				}
				if len(page) == 0 {
					break
				}
				archives := make(map[string]conversationstore.ContentObject)
				for _, row := range page {
					after = row.cursor
					prepared, _, err := prepareConversationImportRow(ctx, source, copyHash, organization, table, row, archives)
					if err != nil {
						return err
					}
					if len(prepared) != 1 {
						return errors.New("conversation source row is unavailable")
					}
					values := make(map[string]any, len(prepared[0].Columns))
					for index, column := range prepared[0].Columns {
						values[column] = prepared[0].Values[index]
					}
					if values["origin"] != "worker" {
						continue
					}
					id, ok := values["id"].(string)
					if !ok || id == "" {
						return errors.New("conversation source identity is invalid")
					}
					head, ok := values["event_seq"].(int64)
					if !ok || head < 0 {
						return errors.New("conversation source event head is invalid")
					}
					evidence, err := readNeutralContinuation(ctx, source, id, head)
					proof, count := evidence.digest, evidence.accepted
					if err != nil {
						return err
					}
					_, err = db.ExecContext(ctx, `INSERT INTO conversation_continuation_recovery(organization_id,snapshot_id,record_kind,conversation_id,catalog_digest,project_id,work_item_id,source_row_id,receipt_hash,legacy_receipt_hash,event_head,continue_count,boundary_sha256) VALUES($1,$2,'conversation',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(organization_id,snapshot_id,record_kind,conversation_id) DO UPDATE SET catalog_digest=excluded.catalog_digest,project_id=excluded.project_id,work_item_id=excluded.work_item_id,source_row_id=excluded.source_row_id,receipt_hash=excluded.receipt_hash,legacy_receipt_hash=excluded.legacy_receipt_hash,event_head=excluded.event_head,continue_count=excluded.continue_count,boundary_sha256=excluded.boundary_sha256`, organization, copyHash, id, catalog, values["project_id"], values["work_item_id"], row.rowID, prepared[0].Receipt.RequestHash, prepared[0].legacyReceiptHash, head, count, proof)
					if err != nil {
						return err
					}
					plans++
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("prepare verified conversation source: %w", err)
		}
		result, err := db.ExecContext(ctx, `INSERT INTO conversation_continuation_recovery(organization_id,snapshot_id,record_kind,conversation_id,catalog_digest,plan_count,source_id,manifest_sha256) SELECT $1,$2,'source','',$3,$4,$5,$6 FROM conversation_history_imports WHERE organization_id=$1 AND snapshot_id=$2 AND catalog_digest=$3 AND completed ON CONFLICT(organization_id,snapshot_id,record_kind,conversation_id) DO UPDATE SET catalog_digest=excluded.catalog_digest,plan_count=excluded.plan_count,source_id=excluded.source_id,manifest_sha256=excluded.manifest_sha256`, organization, snapshot, catalog, plans, sourceID, manifest)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			return errors.Join(mutation.ErrConflict, err)
		}
	}
	return nil
}

func verifyContinuationSourceReceipts(ctx context.Context, db *sql.DB, source ConversationHistoryImportSource, organization tracker.OrganizationID, snapshot string) error {
	for _, name := range []string{"conversations", "conversation_messages", "conversation_commands", "conversation_events"} {
		table := conversationImportTable{source: name, target: name}
		var after, expected int64
		for {
			page, err := readConversationHistoryPage(ctx, source, table, organization, after)
			if err != nil {
				return err
			}
			if len(page) == 0 {
				break
			}
			archives := make(map[string]conversationstore.ContentObject)
			var ids []int64
			var hashes, legacy []string
			for _, row := range page {
				after = row.cursor
				prepared, _, err := prepareConversationImportRow(ctx, source, snapshot, organization, table, row, archives)
				if err != nil {
					return err
				}
				if len(prepared) != 1 {
					return errors.New("conversation source receipt is unavailable")
				}
				ids = append(ids, row.rowID)
				hashes = append(hashes, prepared[0].Receipt.RequestHash)
				legacy = append(legacy, prepared[0].legacyReceiptHash)
			}
			var matched int64
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM unnest($4::bigint[],$5::text[],$6::text[]) expected(row_id,receipt_hash,legacy_hash) JOIN conversation_history_import_rows imported ON imported.organization_id=$1 AND imported.snapshot_id=$2 AND imported.source_table=$3 AND imported.source_row_id=expected.row_id AND imported.request_hash IN(expected.receipt_hash,expected.legacy_hash)`, organization, snapshot, name, ids, hashes, legacy).Scan(&matched); err != nil {
				return err
			}
			if matched != int64(len(page)) {
				return mutation.ErrConflict
			}
			expected += int64(len(page))
		}
		var recorded int64
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM conversation_history_import_rows WHERE organization_id=$1 AND snapshot_id=$2 AND source_table=$3`, organization, snapshot, name).Scan(&recorded); err != nil {
			return err
		}
		if recorded != expected {
			return errors.New("conversation source receipt history is incomplete")
		}
	}
	return nil
}

func loadContinuationSourcePlan(ctx context.Context, tx *sql.Tx, organization, id string, head int64) (*continuationSourcePlan, bool, error) {
	if _, err := tx.ExecContext(ctx, `SELECT set_config('detent.organization_id',$1,true)`, organization); err != nil {
		return nil, false, err
	}
	var snapshot, catalog, receipt, legacy, proof string
	var row, count, sourceHead int64
	err := tx.QueryRowContext(ctx, `SELECT p.snapshot_id,p.catalog_digest,p.source_row_id,p.receipt_hash,p.legacy_receipt_hash,p.event_head,p.continue_count,p.boundary_sha256 FROM conversation_continuation_recovery p JOIN conversation_continuation_recovery seal ON seal.organization_id=p.organization_id AND seal.snapshot_id=p.snapshot_id AND seal.record_kind='source' AND seal.catalog_digest=p.catalog_digest JOIN conversation_history_imports imported ON imported.organization_id=p.organization_id AND imported.snapshot_id=p.snapshot_id AND imported.catalog_digest=p.catalog_digest AND imported.completed JOIN conversations c ON c.organization_id=p.organization_id AND c.id=p.conversation_id AND c.origin='worker' AND c.project_id=p.project_id AND c.work_item_id IS NOT DISTINCT FROM p.work_item_id WHERE p.organization_id=$1 AND p.conversation_id=$2 AND p.record_kind='conversation' AND NOT EXISTS(SELECT 1 FROM neutral_import_sources pinned WHERE pinned.organization_id=seal.organization_id AND pinned.source_id=seal.source_id AND pinned.checksum<>seal.manifest_sha256)`, organization, id).Scan(&snapshot, &catalog, &row, &receipt, &legacy, &sourceHead, &count, &proof)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if sourceHead > head {
		return nil, false, errors.New("conversation source head exceeds current history")
	}
	var valid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_history_import_rows WHERE organization_id=$1 AND source_table='conversations' AND source_row_id=$2 AND snapshot_id=$3 AND request_hash IN($4,$5))`, organization, row, snapshot, receipt, legacy).Scan(&valid); err != nil {
		return nil, false, err
	}
	if !valid {
		return nil, false, mutation.ErrConflict
	}
	actual, actualCount, err := postgresContinuationProof(ctx, tx, organization, id, sourceHead)
	if err != nil {
		return nil, false, err
	}
	if actual != proof || actualCount != count {
		return nil, false, errors.New("conversation source prefix proof differs")
	}
	return &continuationSourcePlan{head: sourceHead}, true, nil
}

func validateContinuationSourcePlans(ctx context.Context, tx *sql.Tx) error {
	var incomplete bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_continuation_recovery seal LEFT JOIN (SELECT organization_id,snapshot_id,count(*) AS plans FROM conversation_continuation_recovery WHERE record_kind='conversation' GROUP BY organization_id,snapshot_id) actual ON actual.organization_id=seal.organization_id AND actual.snapshot_id=seal.snapshot_id WHERE seal.record_kind='source' AND seal.plan_count IS DISTINCT FROM COALESCE(actual.plans,0))`).Scan(&incomplete)
	if err != nil {
		return err
	}
	if incomplete {
		return errors.New("conversation continuation source plan is incomplete")
	}
	return nil
}
