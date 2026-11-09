package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func importConversationContinuation(ctx context.Context, source *sql.Tx, organization tracker.OrganizationID, id, origin string) (bool, error) {
	if origin != "worker" {
		return false, nil
	}
	var expected int64
	if err := source.QueryRowContext(ctx, `SELECT count(*) FROM conversation_messages m JOIN conversation_commands cmd ON cmd.conversation_id=m.conversation_id AND cmd.key=m.command_key JOIN conversations c ON c.id=m.conversation_id WHERE c.organization_id=? AND c.id=? AND m.role='user' AND m.kind='continue' AND cmd.kind='continue'`, organization, id).Scan(&expected); err != nil {
		return false, err
	}
	var after, previousMessage, accepted int64
	ownership := false
	for {
		rows, err := source.QueryContext(ctx, `SELECT e.seq,e.type,e.body_json,e.query_metadata_version,e.object_id,e.message_kind,e.execution_status FROM conversation_events e JOIN conversations c ON c.id=e.conversation_id WHERE c.organization_id=? AND e.conversation_id=? AND e.seq>? ORDER BY e.seq LIMIT 128`, organization, id, after)
		if err != nil {
			return false, err
		}
		type event struct {
			sequence, metadataVersion int64
			kind, body                string
			id, messageKind, status   sql.NullString
		}
		var page []event
		for rows.Next() {
			var e event
			if err := rows.Scan(&e.sequence, &e.kind, &e.body, &e.metadataVersion, &e.id, &e.messageKind, &e.status); err != nil {
				return false, errors.Join(err, rows.Close())
			}
			page = append(page, e)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return false, err
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			after = e.sequence
			switch conversation.EventType(e.kind) {
			case conversation.EventMessageAccepted:
				messageID, kind := e.id.String, conversation.MessageKind(e.messageKind.String)
				if e.metadataVersion != 1 {
					var body struct {
						ID   string                   `json:"id"`
						Kind conversation.MessageKind `json:"kind"`
					}
					if json.Unmarshal([]byte(e.body), &body) != nil {
						return false, errors.New("imported continuation message evidence is unavailable")
					}
					messageID, kind = body.ID, body.Kind
				}
				if messageID == "" || !kind.Valid() {
					return false, errors.New("imported continuation message evidence is unavailable")
				}
				if kind != conversation.MessageContinue {
					continue
				}
				var sequence int64
				if err := source.QueryRowContext(ctx, `SELECT m.seq FROM conversation_messages m JOIN conversation_commands cmd ON cmd.conversation_id=m.conversation_id AND cmd.key=m.command_key JOIN conversations c ON c.id=m.conversation_id WHERE c.organization_id=? AND c.id=? AND m.id=? AND m.role='user' AND m.kind='continue' AND cmd.kind='continue'`, organization, id, messageID).Scan(&sequence); err != nil {
					return false, err
				}
				if sequence <= previousMessage {
					return false, errors.New("imported continuation command order is invalid")
				}
				previousMessage, accepted, ownership = sequence, accepted+1, true
			case conversation.EventExecutionUpdated:
				status := conversation.ExecutionStatus(e.status.String)
				if e.metadataVersion != 1 {
					var body struct {
						Status conversation.ExecutionStatus `json:"status"`
					}
					if json.Unmarshal([]byte(e.body), &body) != nil {
						return false, errors.New("imported continuation execution evidence is unavailable")
					}
					status = body.Status
				}
				if !status.Valid() {
					return false, errors.New("imported continuation execution evidence is unavailable")
				}
				if status == conversation.ExecutionCompleted {
					ownership = false
				}
			}
		}
	}
	if accepted != expected {
		return false, errors.New("imported continuation accepted history is incomplete")
	}
	return ownership, nil
}
