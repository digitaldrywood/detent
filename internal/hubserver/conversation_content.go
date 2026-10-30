package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
)

func (c *conversationService) prepareContents(ctx context.Context, organization, project, uploader string, messages ...conversationMessageRecord) (context.Context, error) {
	contents := make([][]byte, 0, len(messages))
	for _, message := range messages {
		content, err := messageContent(message)
		if err != nil {
			return ctx, err
		}
		contents = append(contents, content)
	}
	return c.store.preparePayloads(ctx, organization, contents...)
}

func (c *conversationService) prepareTurnContents(ctx context.Context, organization, id string, events []conversationTurnEvent, finishing bool) (context.Context, error) {
	if c.store.contents == nil {
		return ctx, nil
	}
	var candidates []conversationMessageRecord
	for _, event := range events {
		if event.Type == "turn_completed" {
			finishing = true
		}
		if event.Type == "item" {
			data, err := conversationItemData(event.Data)
			if err != nil {
				return ctx, err
			}
			candidates = append(candidates, conversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageKind(event.Kind), Text: conversationTruncate(event.Summary, conversationMaxSummaryBytes), Data: data, Delivery: conversation.DeliveryCompleted})
		}
	}
	if finishing {
		messages, err := c.queryMessages(ctx, c.store.reader, conversationMessageQuery+"conversation_id=? AND delivery='responding' ORDER BY seq", id)
		if err != nil {
			return ctx, err
		}
		byItem, byID := map[string]*conversationMessageRecord{}, map[string]*conversationMessageRecord{}
		tracked := make([]*conversationMessageRecord, 0, len(messages))
		for index := range messages {
			message := &messages[index]
			byItem[message.ProviderItemID] = message
			byID[message.ID] = message
			tracked = append(tracked, message)
		}
		turnID := ""
		for _, event := range events {
			switch event.Type {
			case "turn_started":
				turnID = event.TurnID
			case "delta":
				message := byItem[event.ProviderItemID]
				if event.MessageID != "" {
					message = byID[event.MessageID]
				}
				if message == nil {
					message = &conversationMessageRecord{Role: conversation.RoleAssistant, Kind: conversation.MessageText, Delivery: conversation.DeliveryResponding, TurnID: turnID}
					byItem[event.ProviderItemID] = message
					tracked = append(tracked, message)
				}
				message.Text += event.Text
			case "turn_completed":
				for _, message := range tracked {
					if message.TurnID == event.TurnID && message.Delivery == conversation.DeliveryResponding {
						message.Delivery = conversation.DeliveryCompleted
						candidates = append(candidates, *message)
					}
				}
			}
		}
		for _, message := range tracked {
			if message.Delivery == conversation.DeliveryResponding {
				message.Delivery = conversation.DeliveryCompleted
				candidates = append(candidates, *message)
			}
		}

	}

	var project, uploader string
	if err := c.store.reader.QueryRowContext(ctx, "SELECT project_id,owner_principal_id FROM conversations WHERE id=?", id).Scan(&project, &uploader); err != nil {
		return ctx, err
	}
	return c.prepareContents(ctx, organization, project, uploader, candidates...)
}

func (c *conversationService) prepareCommandContents(ctx context.Context, organization, project, uploader, id string, command conversation.Command) (context.Context, error) {
	if c.store.contents == nil {
		return ctx, nil
	}
	message := conversationMessageRecord{Role: conversation.RoleUser, Text: command.Text, Delivery: conversation.DeliverySaved}
	switch command.Kind {
	case conversation.CommandMessage:
		message.Data = coordinatorConnectionData(ctx)
	case conversation.CommandContinue:
	case conversation.CommandAnswer:
		question, err := c.store.readQuestion(ctx, c.store.reader, id, command.QuestionID)
		if err != nil {
			return ctx, err
		}
		data, err := json.Marshal(command.Answers)
		if err != nil {
			return ctx, err
		}
		message.Text, message.Data = renderAnswer(question.Question, command.Answers), data
	default:
		return ctx, nil
	}
	return c.prepareContents(ctx, organization, project, uploader, message)
}

func (c *conversationService) prepareConversationContents(ctx context.Context, id string, messages ...conversationMessageRecord) (context.Context, error) {
	if c.store.contents == nil {
		return ctx, nil
	}
	var organization, project, uploader string
	if err := c.store.reader.QueryRowContext(ctx, "SELECT organization_id,project_id,owner_principal_id FROM conversations WHERE id=?", id).Scan(&organization, &project, &uploader); err != nil {
		return ctx, err
	}
	return c.prepareContents(ctx, organization, project, uploader, messages...)
}

func (s *conversationStore) recordContentArchive(ctx context.Context, tx *sql.Tx, organization, project, uploader string, ref attachment.ArchiveReference, now time.Time) error {
	var before map[string]int64
	var err error
	if s.database != nil {
		before, err = s.database.hostedConsumption(ctx, tx, now, "collaboration_bytes")
		if err != nil {
			return err
		}
	}
	id := contentArchiveID(organization, ref)
	if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,created_at,kind,retention_class,object_key,uncompressed_size,codec,schema_version)
 VALUES(?,?,?,?,'archive.gz','application/gzip',?,?,?,'archive',?,?,?,?,?) ON CONFLICT DO NOTHING`, id, organization, project, uploader, ref.CompressedSize, ref.SHA256, conversationTime(now), ref.Retention, ref.Key, ref.UncompressedSize, ref.Codec, ref.SchemaVersion); err != nil {
		return err
	}
	stored, err := scanAttachmentArchive(tx.QueryRowContext(ctx, `SELECT object_key,sha256,size,uncompressed_size,codec,schema_version,kind,retention_class,storage_binding_id,customer_owned FROM attachments WHERE id=? AND organization_id=? AND kind='archive'`, id, organization))
	if err != nil {
		return err
	}
	if stored != ref {
		return errors.New("conversation archive reference conflicts with stored content")
	}
	if s.database != nil {
		return s.database.checkHostedGrowth(ctx, tx, before, now, false, "collaboration_bytes")
	}
	return nil
}
