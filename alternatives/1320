package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
)

func (s *conversationStore) normalizeRestartedRecords(ctx context.Context, now time.Time) (int64, int64, error) {
	conversations, err := s.normalizeRestartedExecutions(ctx, now)
	if err != nil {
		return conversations, 0, err
	}
	receipts, err := s.normalizeRestartedReceipts(ctx, now)
	return conversations, receipts, err
}

func (s *conversationStore) normalizeRestartedExecutions(ctx context.Context, now time.Time) (int64, error) {
	var cursor, changed int64
	for {
		type candidate struct {
			row, revision                         int64
			id, execution, preferences, contentID string
		}
		batch, err := func() ([]candidate, error) {
			rows, err := s.reader.QueryContext(ctx, `SELECT rowid,id,revision,execution_json,preferences_json,content_attachment_id FROM conversations WHERE rowid>? AND work_item_id IS NULL AND (execution_status IS NULL OR execution_status IN ('starting','running','waiting_input','interrupting')) ORDER BY rowid LIMIT ?`, cursor, storageConversionRows)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var batch []candidate
			for rows.Next() {
				var item candidate
				if err := rows.Scan(&item.row, &item.id, &item.revision, &item.execution, &item.preferences, &item.contentID); err != nil {
					rows.Close()
					return nil, err
				}
				batch = append(batch, item)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return nil, err
			}
			return batch, nil
		}()
		if err != nil {
			return changed, err
		}
		if len(batch) == 0 {
			return changed, nil
		}
		for _, item := range batch {
			cursor = item.row
			record, err := s.readConversationByID(ctx, s.reader, item.id)
			if err != nil {
				return changed, err
			}
			if record.Revision != item.revision || record.ContentID != item.contentID {
				continue
			}
			switch record.Execution.Status {
			case conversation.ExecutionStarting, conversation.ExecutionRunning, conversation.ExecutionWaitingInput, conversation.ExecutionInterrupting:
			default:
				continue
			}
			record.Execution = conversation.NormalizeExecutionValue(record.Execution)
			record.Execution.UpdatedAt = now.UTC()
			raw, err := recordPayload(record)
			if err != nil {
				return changed, err
			}
			content, err := messageContent(conversationMessageRecord{Data: raw})
			if err != nil {
				return changed, err
			}
			prepared, err := s.preparePayload(ctx, string(record.OrganizationID), content)
			if err != nil {
				return changed, err
			}
			count, err := func() (int64, error) {
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					return 0, err
				}
				defer tx.Rollback()
				contentID, err := s.conversationRecordStorage(prepared, tx, record)
				if err != nil {
					return 0, err
				}
				result, err := tx.ExecContext(ctx, `UPDATE conversations SET execution_json='{}',preferences_json='{}',conversion_state=2,content_attachment_id=?,execution_status=?,execution_lease_id=?,execution_settled=?,query_metadata_version=1,revision=revision+1,updated_at=? WHERE id=? AND revision=? AND content_attachment_id=? AND execution_json=? AND preferences_json=?`, contentID, record.Execution.Status, record.Execution.Owner.LeaseID, record.Execution.Settled, conversationTime(now), item.id, item.revision, item.contentID, item.execution, item.preferences)
				if err != nil {
					return 0, err
				}
				count, err := result.RowsAffected()
				if err != nil || count == 0 {
					return 0, err
				}
				return count, tx.Commit()
			}()
			if err != nil {
				return changed, err
			}
			changed += count
		}
	}
}

func (s *conversationStore) normalizeRestartedReceipts(ctx context.Context, now time.Time) (int64, error) {
	var cursor, changed int64
	for {
		type candidate struct {
			row                               int64
			conversation, key, raw, contentID string
		}
		batch, err := func() ([]candidate, error) {
			rows, err := s.reader.QueryContext(ctx, `SELECT rowid,conversation_id,key,receipt_json,content_attachment_id FROM conversation_commands WHERE rowid>? AND conversation_id IN (SELECT id FROM conversations WHERE work_item_id IS NULL) AND (receipt_status IS NULL OR receipt_status IN ('sending','sent','responding')) ORDER BY rowid LIMIT ?`, cursor, storageConversionRows)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var batch []candidate
			for rows.Next() {
				var item candidate
				if err := rows.Scan(&item.row, &item.conversation, &item.key, &item.raw, &item.contentID); err != nil {
					rows.Close()
					return nil, err
				}
				batch = append(batch, item)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return nil, err
			}
			return batch, nil
		}()
		if err != nil {
			return changed, err
		}
		if len(batch) == 0 {
			return changed, nil
		}
		for _, item := range batch {
			cursor = item.row
			receipt, err := s.decodeReceipt(ctx, s.reader, item.conversation, item.raw, item.contentID)
			if err != nil {
				return changed, err
			}
			switch receipt.Status {
			case conversation.DeliverySending, conversation.DeliverySent, conversation.DeliveryResponding:
			default:
				continue
			}
			receipt.Status, receipt.UpdatedAt = conversation.DeliveryUnknown, now.UTC()
			raw, err := json.Marshal(receipt)
			if err != nil {
				return changed, err
			}
			content, err := messageContent(conversationMessageRecord{Data: raw})
			if err != nil {
				return changed, err
			}
			var organization string
			if err := s.reader.QueryRowContext(ctx, "SELECT organization_id FROM conversations WHERE id=?", item.conversation).Scan(&organization); err != nil {
				return changed, err
			}
			prepared, err := s.preparePayload(ctx, organization, content)
			if err != nil {
				return changed, err
			}
			count, err := func() (int64, error) {
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					return 0, err
				}
				defer tx.Rollback()
				contentID, err := s.payloadStorage(prepared, tx, item.conversation, raw, now)
				if err != nil {
					return 0, err
				}
				result, err := tx.ExecContext(ctx, `UPDATE conversation_commands SET receipt_json='{}',conversion_state=2,content_attachment_id=?,receipt_status=?,query_metadata_version=1,updated_at=? WHERE rowid=? AND receipt_json=? AND content_attachment_id=?`, contentID, receipt.Status, conversationTime(now), item.row, item.raw, item.contentID)
				if err != nil {
					return 0, err
				}
				count, err := result.RowsAffected()
				if err != nil || count == 0 {
					return 0, err
				}
				return count, tx.Commit()
			}()
			if err != nil {
				return changed, err
			}
			changed += count
		}
	}
}
