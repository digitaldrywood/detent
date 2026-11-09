package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/conversation"
)

type continuationProofQueries struct {
	events  string
	command string
}

func postgresContinuationProof(ctx context.Context, tx *sql.Tx, organization, id string, head int64) (string, int64, error) {
	return readContinuationProof(ctx, tx, organization, id, head, continuationProofQueries{
		events:  `SELECT seq,type,body_json,query_metadata_version,object_id,message_kind,execution_status,content_attachment_id FROM conversation_events WHERE organization_id=$1 AND conversation_id=$2 AND seq>$3 AND seq<=$4 ORDER BY seq LIMIT 128`,
		command: `SELECT m.seq,m.command_key FROM conversation_messages m JOIN conversation_commands c ON c.organization_id=m.organization_id AND c.conversation_id=m.conversation_id AND c.key=m.command_key WHERE m.organization_id=$1 AND m.conversation_id=$2 AND m.id=$3 AND m.role='user' AND m.kind='continue' AND c.kind='continue'`,
	})
}

func readContinuationProof(ctx context.Context, tx *sql.Tx, organization, id string, head int64, queries continuationProofQueries) (string, int64, error) {
	digest := sha256.New()
	var after, count int64
	for {
		type event struct {
			sequence, version       int64
			kind, reference         string
			body                    []byte
			id, messageKind, status sql.NullString
		}
		page, err := func() (_ []event, err error) {
			rows, err := tx.QueryContext(ctx, queries.events, organization, id, after, head)
			if err != nil {
				return nil, err
			}
			defer func() { err = errors.Join(err, rows.Close()) }()
			var page []event
			for rows.Next() {
				var e event
				if err := rows.Scan(&e.sequence, &e.kind, &e.body, &e.version, &e.id, &e.messageKind, &e.status, &e.reference); err != nil {
					return nil, err
				}
				page = append(page, e)
			}
			return page, rows.Err()
		}()
		if err != nil {
			return "", 0, err
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			after = e.sequence
			marker := struct {
				Sequence        int64
				Type            string
				MessageID       string
				MessageSequence int64
				CommandKey      string
			}{Sequence: e.sequence, Type: e.kind}
			switch conversation.EventType(e.kind) {
			case conversation.EventMessageAccepted:
				var body struct {
					ID   string                   `json:"id"`
					Kind conversation.MessageKind `json:"kind"`
				}
				if e.version == 1 {
					body.ID, body.Kind = e.id.String, conversation.MessageKind(e.messageKind.String)
				} else if e.reference != "" || json.Unmarshal(e.body, &body) != nil {
					return "", 0, errors.New("continuation source message is unavailable")
				}
				if body.ID == "" || !body.Kind.Valid() {
					return "", 0, errors.New("continuation source message is invalid")
				}
				if body.Kind != conversation.MessageContinue {
					continue
				}
				marker.MessageID = body.ID
				if err := tx.QueryRowContext(ctx, queries.command, organization, id, body.ID).Scan(&marker.MessageSequence, &marker.CommandKey); err != nil {
					return "", 0, err
				}
				count++
			case conversation.EventExecutionUpdated:
				var body struct {
					Status conversation.ExecutionStatus `json:"status"`
				}
				if e.version == 1 {
					body.Status = conversation.ExecutionStatus(e.status.String)
				} else if e.reference != "" || json.Unmarshal(e.body, &body) != nil {
					return "", 0, errors.New("continuation source execution is unavailable")
				}
				if !body.Status.Valid() {
					return "", 0, errors.New("continuation source execution is invalid")
				}
				if body.Status != conversation.ExecutionCompleted {
					continue
				}
			default:
				continue
			}
			if err := json.NewEncoder(digest).Encode(marker); err != nil {
				return "", 0, err
			}
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), count, nil
}
