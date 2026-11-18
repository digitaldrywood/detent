package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/conversation"
)

const conversationContinuationBackfillVersion = 20261108224927

func ConversationMigrations(source fs.FS) []*goose.Migration {
	if _, err := fs.Stat(source, "20261108224926_conversation_execution_continuation.sql"); err != nil {
		return nil
	}
	return []*goose.Migration{goose.NewGoMigration(conversationContinuationBackfillVersion, &goose.GoFunc{RunTx: migrateConversationContinuation}, nil)}
}

func migrateConversationContinuation(ctx context.Context, db *sql.Tx) error {
	if err := validateContinuationSourcePlans(ctx, db); err != nil {
		return err
	}
	var organization, id string
	var unproven, sequences int64
	for {
		err := db.QueryRowContext(ctx, `SELECT organization_id,id FROM conversations WHERE execution_continuation IS NULL AND (organization_id,id)>($1,$2) ORDER BY organization_id,id LIMIT 1`, organization, id).Scan(&organization, &id)
		if errors.Is(err, sql.ErrNoRows) && unproven > 0 {
			return fmt.Errorf("backfill conversation continuation: %d conversations with %d missing event sequences cannot be proven compacted", unproven, sequences)
		} else if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("read conversation continuation upgrade page: %w", err)
		}
		missing, err := backfillConversationContinuation(ctx, db, organization, id)
		if err != nil {
			return fmt.Errorf("backfill conversation continuation: %w", err)
		}
		unproven, sequences = unproven+min(missing, 1), sequences+missing
	}
}

func backfillConversationContinuation(ctx context.Context, tx *sql.Tx, organization, id string) (int64, error) {
	var origin, currentStatus string
	var head int64
	if err := tx.QueryRowContext(ctx, `SELECT origin,event_seq,COALESCE(execution_status,'') FROM conversations WHERE organization_id=$1 AND id=$2 AND execution_continuation IS NULL FOR UPDATE`, organization, id).Scan(&origin, &head, &currentStatus); errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	var accepted int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM conversation_messages m JOIN conversation_commands c ON c.organization_id=m.organization_id AND c.conversation_id=m.conversation_id AND c.key=m.command_key WHERE m.organization_id=$1 AND m.conversation_id=$2 AND m.role='user' AND m.kind='continue' AND c.kind='continue'`, organization, id).Scan(&accepted); err != nil {
		return 0, err
	}
	ownership, after, missing := false, int64(0), int64(0)
	if origin == "worker" {
		plan, hasPlan, err := loadContinuationSourcePlan(ctx, tx, organization, id, head)
		if err != nil {
			return 0, err
		}
		var verifiedCount, previousMessage, continuedAt int64
		var latestStatus conversation.ExecutionStatus
		for after < head {
			type event struct {
				sequence                               int64
				metadataVersion                        int64
				objectID, messageKind, executionStatus sql.NullString
				kind, reference                        string
				body                                   []byte
			}
			events, err := func() (_ []event, err error) {
				rows, err := tx.QueryContext(ctx, `SELECT seq,type,body_json,content_attachment_id,query_metadata_version,object_id,message_kind,execution_status FROM conversation_events WHERE organization_id=$1 AND conversation_id=$2 AND seq>$3 ORDER BY seq LIMIT 128`, organization, id, after)
				if err != nil {
					return nil, err
				}
				defer func() { err = errors.Join(err, rows.Close()) }()
				var events []event
				for rows.Next() {
					var e event
					if err := rows.Scan(&e.sequence, &e.kind, &e.body, &e.reference, &e.metadataVersion, &e.objectID, &e.messageKind, &e.executionStatus); err != nil {
						return nil, err
					}
					events = append(events, e)
				}
				return events, rows.Err()
			}()
			if err != nil {
				return 0, err
			}
			if len(events) == 0 {
				if hasPlan {
					after = max(after, plan.head)
				}
				missing += head - after
				break
			}
			for _, e := range events {
				if hasPlan && after < plan.head && e.sequence > plan.head {
					after = plan.head
				}
				verifiedPrefix := hasPlan && e.sequence <= plan.head
				if e.sequence <= after || e.sequence > head {
					return 0, errors.New("conversation continuation history is incomplete")
				}
				if !verifiedPrefix {
					missing += e.sequence - after - 1
				}
				after = e.sequence
				switch conversation.EventType(e.kind) {
				case conversation.EventMessageAccepted:
					var message struct {
						ID   string                   `json:"id"`
						Kind conversation.MessageKind `json:"kind"`
					}
					if e.metadataVersion == 1 {
						message.ID, message.Kind = e.objectID.String, conversation.MessageKind(e.messageKind.String)
					} else if e.reference != "" || json.Unmarshal(e.body, &message) != nil {
						return 0, errors.New("conversation continuation message evidence is unavailable")
					}
					if message.ID == "" || !message.Kind.Valid() {
						return 0, errors.New("conversation continuation message evidence is unavailable")
					}
					if message.Kind == conversation.MessageContinue {
						var sequence int64
						if err := tx.QueryRowContext(ctx, `SELECT m.seq FROM conversation_messages m JOIN conversation_commands c ON c.organization_id=m.organization_id AND c.conversation_id=m.conversation_id AND c.key=m.command_key WHERE m.organization_id=$1 AND m.conversation_id=$2 AND m.id=$3 AND m.role='user' AND m.kind='continue' AND c.kind='continue'`, organization, id, message.ID).Scan(&sequence); err != nil {
							return 0, err
						}
						if sequence <= previousMessage {
							return 0, errors.New("conversation continuation command evidence is unavailable")
						}
						verifiedCount++
						previousMessage, ownership, continuedAt = sequence, true, missing
					}
				case conversation.EventExecutionUpdated:
					var execution struct {
						Status conversation.ExecutionStatus `json:"status"`
					}
					if e.metadataVersion == 1 {
						execution.Status = conversation.ExecutionStatus(e.executionStatus.String)
					} else if e.reference != "" || json.Unmarshal(e.body, &execution) != nil {
						return 0, errors.New("conversation continuation execution evidence is unavailable")
					}
					if !execution.Status.Valid() {
						return 0, errors.New("conversation continuation execution evidence is unavailable")
					}
					if latestStatus = execution.Status; execution.Status == conversation.ExecutionCompleted {
						ownership = false
					}
				}
			}
		}
		if verifiedCount != accepted {
			return 0, errors.New("conversation continuation accepted history is incomplete")
		}
		if missing > 0 && (string(latestStatus) != currentStatus && (latestStatus != "" || currentStatus != string(conversation.ExecutionIdle)) || ownership && missing > continuedAt) {
			return missing, nil
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET execution_continuation=$1 WHERE organization_id=$2 AND id=$3 AND execution_continuation IS NULL`, ownership, organization, id); err != nil {
		return 0, err
	}
	return 0, nil
}
