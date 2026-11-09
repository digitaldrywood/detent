package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/pressly/goose/v3"
)

const conversationContinuationBackfillVersion = 20261108224927

func ConversationMigrations(source fs.FS) []*goose.Migration {
	if _, err := fs.Stat(source, "20261108224926_conversation_execution_continuation.sql"); err != nil {
		return nil
	}
	return []*goose.Migration{goose.NewGoMigration(conversationContinuationBackfillVersion, &goose.GoFunc{RunTx: migrateConversationContinuation}, nil)}
}

func migrateConversationContinuation(ctx context.Context, db *sql.Tx) error {
	type identity struct{ organization, conversation string }
	for {
		rows, err := db.QueryContext(ctx, `SELECT organization_id,id FROM conversations WHERE execution_continuation IS NULL ORDER BY organization_id,id LIMIT 32`)
		if err != nil {
			return fmt.Errorf("read conversation continuation upgrade page: %w", err)
		}
		var page []identity
		for rows.Next() {
			var id identity
			if err := rows.Scan(&id.organization, &id.conversation); err != nil {
				return errors.Join(err, rows.Close())
			}
			page = append(page, id)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, id := range page {
			if err := backfillConversationContinuation(ctx, db, id.organization, id.conversation); err != nil {
				return fmt.Errorf("backfill conversation continuation: %w", err)
			}
		}
	}
}

func backfillConversationContinuation(ctx context.Context, tx *sql.Tx, organization, id string) error {
	var origin string
	var head int64
	if err := tx.QueryRowContext(ctx, `SELECT origin,event_seq FROM conversations WHERE organization_id=$1 AND id=$2 AND execution_continuation IS NULL FOR UPDATE`, organization, id).Scan(&origin, &head); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	var accepted int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM conversation_messages m JOIN conversation_commands c ON c.organization_id=m.organization_id AND c.conversation_id=m.conversation_id AND c.key=m.command_key WHERE m.organization_id=$1 AND m.conversation_id=$2 AND m.role='user' AND m.kind='continue' AND c.kind='continue'`, organization, id).Scan(&accepted); err != nil {
		return err
	}
	ownership, after := false, int64(0)
	if origin == "worker" {
		var verifiedCount, previousMessage int64
		for after < head {
			rows, err := tx.QueryContext(ctx, `SELECT seq,type,body_json,content_attachment_id,query_metadata_version,object_id,message_kind,execution_status FROM conversation_events WHERE organization_id=$1 AND conversation_id=$2 AND seq>$3 ORDER BY seq LIMIT 128`, organization, id, after)
			if err != nil {
				return err
			}
			type event struct {
				sequence                               int64
				metadataVersion                        int64
				objectID, messageKind, executionStatus sql.NullString
				kind, reference                        string
				body                                   []byte
			}
			var events []event
			for rows.Next() {
				var e event
				if err := rows.Scan(&e.sequence, &e.kind, &e.body, &e.reference, &e.metadataVersion, &e.objectID, &e.messageKind, &e.executionStatus); err != nil {
					return errors.Join(err, rows.Close())
				}
				events = append(events, e)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			if len(events) == 0 {
				return errors.New("conversation continuation history is incomplete")
			}
			for _, e := range events {
				if e.sequence != after+1 || e.sequence > head {
					return errors.New("conversation continuation history is incomplete")
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
						return errors.New("conversation continuation message evidence is unavailable")
					}
					if message.ID == "" || !message.Kind.Valid() {
						return errors.New("conversation continuation message evidence is unavailable")
					}
					if message.Kind == conversation.MessageContinue {
						var sequence int64
						if err := tx.QueryRowContext(ctx, `SELECT m.seq FROM conversation_messages m JOIN conversation_commands c ON c.organization_id=m.organization_id AND c.conversation_id=m.conversation_id AND c.key=m.command_key WHERE m.organization_id=$1 AND m.conversation_id=$2 AND m.id=$3 AND m.role='user' AND m.kind='continue' AND c.kind='continue'`, organization, id, message.ID).Scan(&sequence); err != nil {
							return err
						}
						if sequence <= previousMessage {
							return errors.New("conversation continuation command evidence is unavailable")
						}
						verifiedCount++
						previousMessage = sequence
						ownership = true
					}
				case conversation.EventExecutionUpdated:
					var execution struct {
						Status conversation.ExecutionStatus `json:"status"`
					}
					if e.metadataVersion == 1 {
						execution.Status = conversation.ExecutionStatus(e.executionStatus.String)
					} else if e.reference != "" || json.Unmarshal(e.body, &execution) != nil {
						return errors.New("conversation continuation execution evidence is unavailable")
					}
					if !execution.Status.Valid() {
						return errors.New("conversation continuation execution evidence is unavailable")
					}
					if execution.Status == conversation.ExecutionCompleted {
						ownership = false
					}
				}
			}
		}
		if verifiedCount != accepted {
			return errors.New("conversation continuation accepted history is incomplete")
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversations SET execution_continuation=$1 WHERE organization_id=$2 AND id=$3 AND execution_continuation IS NULL`, ownership, organization, id); err != nil {
		return err
	}
	return nil
}
