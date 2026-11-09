package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

type continuationSourceEvidence struct {
	ownership bool
	accepted  int64
	digest    string
}

func validateNeutralContinuationOwnership(ctx context.Context, source ConversationHistoryImportSource) error {
	var cursor []string
	for {
		page, err := source.neutral.Page(ctx, "conversations", cursor, continuationIndexPageRows)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		cursor = page[len(page)-1].Key
		for _, row := range page {
			if cellText(row.Values["origin"]) != "worker" {
				continue
			}
			head, err := strconv.ParseInt(cellText(row.Values["event_seq"]), 10, 64)
			if err != nil {
				return err
			}
			if _, err := readNeutralContinuation(ctx, source, cellText(row.Values["id"]), head); err != nil {
				return err
			}
		}
	}
}

func importConversationContinuation(ctx context.Context, source ConversationHistoryImportSource, id, origin string, head int64) (bool, error) {
	if origin != "worker" {
		return false, nil
	}
	evidence, err := readNeutralContinuation(ctx, source, id, head)
	return evidence.ownership, err
}

func readNeutralContinuation(ctx context.Context, source ConversationHistoryImportSource, id string, head int64) (continuationSourceEvidence, error) {
	var result continuationSourceEvidence
	if source.messages == nil || head < 0 {
		return result, ErrNeutralImport
	}
	var expected int64
	var cursor []string
	for {
		page, err := source.messages.page(ctx, id, cursor)
		if err != nil {
			return result, err
		}
		if len(page) == 0 {
			break
		}
		cursor = page[len(page)-1].Key
		for _, message := range page {
			if cellText(message.Values["role"]) != "user" || cellText(message.Values["kind"]) != "continue" || cellText(message.Values["command_key"]) == "" {
				continue
			}
			command, err := source.neutral.Lookup(ctx, "conversation_commands", []string{id, cellText(message.Values["command_key"])})
			if errors.Is(err, corestore.ErrNotFound) {
				continue
			}
			if err != nil {
				return result, err
			}
			if cellText(command.Values["kind"]) == "continue" {
				expected++
			}
		}
	}
	digest := sha256.New()
	cursor = []string{id, "0"}
	var previousMessage int64
	for {
		page, err := source.neutral.Page(ctx, "conversation_events", cursor, continuationIndexPageRows)
		if err != nil {
			return result, err
		}
		if len(page) == 0 {
			break
		}
		for _, event := range page {
			if event.Key[0] != id {
				result.digest = hex.EncodeToString(digest.Sum(nil))
				return result, validateContinuationSourceCount(result.accepted, expected)
			}
			sequence, err := strconv.ParseInt(event.Key[1], 10, 64)
			if err != nil || sequence <= 0 || sequence > head {
				return result, ErrNeutralImport
			}
			cursor = event.Key
			version, err := strconv.ParseInt(cellText(event.Values["query_metadata_version"]), 10, 64)
			if err != nil || version < 0 || version > 1 {
				return result, ErrNeutralImport
			}
			marker := struct {
				Sequence        int64
				Type            string
				MessageID       string
				MessageSequence int64
				CommandKey      string
			}{Sequence: sequence, Type: cellText(event.Values["type"])}
			switch conversation.EventType(marker.Type) {
			case conversation.EventMessageAccepted:
				var body struct {
					ID   string                   `json:"id"`
					Kind conversation.MessageKind `json:"kind"`
				}
				if version == 1 {
					body.ID = cellText(event.Values["object_id"])
					body.Kind = conversation.MessageKind(cellText(event.Values["message_kind"]))
				} else if cellText(event.Values["content_attachment_id"]) != "" || json.Unmarshal(event.Values["body_json"].Data, &body) != nil {
					return result, errors.New("imported continuation message evidence is unavailable")
				}
				if body.ID == "" || !body.Kind.Valid() {
					return result, ErrNeutralImport
				}
				if body.Kind != conversation.MessageContinue {
					continue
				}
				message, err := source.neutral.Lookup(ctx, "conversation_messages", []string{body.ID})
				if err != nil {
					return result, err
				}
				key := cellText(message.Values["command_key"])
				if cellText(message.Values["conversation_id"]) != id || cellText(message.Values["role"]) != "user" || cellText(message.Values["kind"]) != "continue" || key == "" {
					return result, ErrNeutralImport
				}
				command, err := source.neutral.Lookup(ctx, "conversation_commands", []string{id, key})
				if err != nil {
					return result, err
				}
				if cellText(command.Values["kind"]) != "continue" {
					return result, ErrNeutralImport
				}
				messageSequence, err := strconv.ParseInt(cellText(message.Values["seq"]), 10, 64)
				if err != nil || messageSequence <= previousMessage {
					return result, errors.New("imported continuation command order is invalid")
				}
				previousMessage = messageSequence
				marker.MessageID, marker.MessageSequence, marker.CommandKey = body.ID, messageSequence, key
				result.accepted++
				result.ownership = true
			case conversation.EventExecutionUpdated:
				status := conversation.ExecutionStatus(cellText(event.Values["execution_status"]))
				if version == 0 {
					var body struct {
						Status conversation.ExecutionStatus `json:"status"`
					}
					if cellText(event.Values["content_attachment_id"]) != "" || json.Unmarshal(event.Values["body_json"].Data, &body) != nil {
						return result, errors.New("imported continuation execution evidence is unavailable")
					}
					status = body.Status
				}
				if !status.Valid() {
					return result, ErrNeutralImport
				}
				if status != conversation.ExecutionCompleted {
					continue
				}
				result.ownership = false
			default:
				continue
			}
			if err := json.NewEncoder(digest).Encode(marker); err != nil {
				return result, err
			}
		}
	}
	result.digest = hex.EncodeToString(digest.Sum(nil))
	return result, validateContinuationSourceCount(result.accepted, expected)
}

func validateContinuationSourceCount(actual, expected int64) error {
	if actual != expected {
		return errors.New("imported continuation accepted history is incomplete")
	}
	return nil
}
