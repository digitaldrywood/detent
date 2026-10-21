package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
)

type conversationContent struct {
	Text string          `json:"text"`
	Data json.RawMessage `json:"data"`
}

func messageContent(message conversationMessageRecord) ([]byte, error) {
	data := message.Data
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	return json.Marshal(conversationContent{Text: message.Text, Data: data})
}

type preparedConversationContents map[string]attachment.ArchiveReference
type conversationContentsKey struct{}

func externalConversationContent(message conversationMessageRecord) bool {
	return message.Role == conversation.RoleSystem || (message.Delivery != conversation.DeliveryResponding && len(message.Text)+len(message.Data) > 2048)
}

func (c *conversationService) prepareContents(ctx context.Context, organization, project, uploader string, messages ...conversationMessageRecord) (context.Context, error) {
	if c.store.contents == nil {
		return ctx, nil
	}
	prepared := preparedConversationContents{}
	if existing, ok := ctx.Value(conversationContentsKey{}).(preparedConversationContents); ok {
		for key, ref := range existing {
			prepared[key] = ref
		}
	}
	for _, message := range messages {
		if !externalConversationContent(message) {
			continue
		}
		content, err := messageContent(message)
		if err != nil {
			return ctx, err
		}
		key := organization + "/" + artifact.Digest(content)
		if _, ok := prepared[key]; ok {
			continue
		}
		expected, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(content))
		if err != nil {
			return ctx, err
		}
		tx, err := c.store.db.BeginTx(ctx, nil)
		if err != nil {
			return ctx, err
		}
		err = c.store.recordContentArchive(ctx, tx, organization, project, uploader, expected, c.server.config.now())
		if err == nil {
			err = tx.Commit()
		}
		rollbackErr := tx.Rollback()
		if rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
		if err != nil {
			return ctx, err
		}
		ref, err := c.store.contents.PublishContent(ctx, organization, content)
		if err != nil {
			return ctx, fmt.Errorf("publish conversation content: %w", err)
		}
		if ref != expected {
			return ctx, errors.New("conversation archive reference changed during publication")
		}
		prepared[key] = ref
	}
	return context.WithValue(ctx, conversationContentsKey{}, prepared), nil
}

func (s *conversationStore) messageStorage(ctx context.Context, tx *sql.Tx, message conversationMessageRecord) (string, string, string, error) {
	if s.contents == nil || !externalConversationContent(message) {
		return message.Text, string(message.Data), "", nil
	}
	content, err := messageContent(message)
	if err != nil {
		return "", "", "", err
	}
	digest := artifact.Digest(content)
	if message.ContentID != "" && digest == message.contentHash {
		return conversationTruncate(message.Text, 256), "{}", message.ContentID, nil
	}
	var organization, project string
	if err := tx.QueryRowContext(ctx, "SELECT organization_id,project_id FROM conversations WHERE id=?", message.ConversationID).Scan(&organization, &project); err != nil {
		return "", "", "", err
	}
	prepared, ok := ctx.Value(conversationContentsKey{}).(preparedConversationContents)
	if !ok {
		return "", "", "", errors.New("conversation content was not prepared")
	}
	ref, found := prepared[organization+"/"+digest]
	if !found || ref.Validate(organization) != nil {
		return "", "", "", errors.New("conversation content was not published before the transaction")
	}
	id := contentArchiveID(organization, ref)
	if err := s.recordContentArchive(ctx, tx, organization, project, message.Actor.PrincipalID, ref, message.CreatedAt); err != nil {
		return "", "", "", err
	}

	return conversationTruncate(message.Text, 256), "{}", id, nil
}

type conversationReadContentsKey struct{}
type readConversationContent struct {
	content conversationContent
	hash    string
}
type conversationContentReads map[string]readConversationContent

func (s *conversationStore) resolveMessageContents(ctx context.Context, query nativeQueryer, messages []*conversationMessageRecord) error {
	cache, ok := ctx.Value(conversationReadContentsKey{}).(conversationContentReads)
	if !ok || cache == nil {
		cache = conversationContentReads{}
	}
	for _, message := range messages {
		if message.ContentID == "" {
			continue
		}
		var organization string
		if err := query.QueryRowContext(ctx, "SELECT organization_id FROM conversations WHERE id=?", message.ConversationID).Scan(&organization); err != nil {
			return err
		}
		key := organization + "/" + message.ContentID
		if cached, ok := cache[key]; ok {
			message.Text, message.Data, message.contentHash = cached.content.Text, cached.content.Data, cached.hash
			continue
		}
		ref, err := scanAttachmentArchive(query.QueryRowContext(ctx, `SELECT object_key,sha256,size,uncompressed_size,codec,schema_version,kind,retention_class,storage_binding_id,customer_owned FROM attachments WHERE id=? AND organization_id=? AND kind='archive'`, message.ContentID, organization))
		if err != nil {
			return err
		}
		if s.contents == nil {
			return errors.New("conversation content storage is unavailable")
		}
		content, err := s.contents.ReadContent(ctx, organization, ref)
		if err != nil {
			return fmt.Errorf("read conversation content: %w", err)
		}
		var decoded conversationContent
		if err := json.Unmarshal(content, &decoded); err != nil {
			return fmt.Errorf("decode conversation content: %w", err)
		}
		message.Text, message.Data, message.contentHash = decoded.Text, decoded.Data, artifact.Digest(content)
		cache[key] = readConversationContent{content: decoded, hash: message.contentHash}
	}
	return nil
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

func contentArchiveID(organization string, ref attachment.ArchiveReference) string {
	return "att_" + artifact.Digest([]byte(organization + "\n" + ref.Key))[:32]
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

func (c *conversationService) prepareBindNotice(ctx context.Context, scope nativeScope, record conversationRecord, request conversationBindRequest) (context.Context, error) {
	messages, err := c.pendingControlMessages(ctx, c.store.reader, record.ID, 0)
	if err != nil {
		return ctx, err
	}
	var pending []conversationControl
	for _, message := range messages {
		if len(pending) >= c.config.ControlQueueSize {
			break
		}
		if message.Delivery == conversation.DeliverySending && !record.Execution.Status.Terminal() && record.Execution.Owner.AttemptID != "" && record.Execution.Owner.AttemptID != request.AttemptID {
			continue
		}
		if message.Kind == conversation.MessageAnswer {
			if _, _, err := c.resolveAnswer(ctx, c.store.reader, record.ID, message); err != nil {
				continue
			}
		}
		pending = append(pending, conversationControl{MessageID: message.ID})
	}
	thread := strings.TrimSpace(request.ThreadID)
	named := thread != ""
	if !named {
		thread = record.ProviderThreadID
	} else {
		record.ProviderThreadRunnerID = scope.credential.Runner.RunnerID
		record.ProviderThreadOrigin = conversation.ThreadOriginWorker
	}
	resume, err := c.resumeFor(ctx, c.store.reader, record, scope, thread, named, false, pending)
	if err != nil {
		return ctx, err
	}
	if conversationResumeKind(resume) != conversation.ResumeTranscript {
		return ctx, nil
	}
	notice := conversationMessageRecord{Role: conversation.RoleSystem, Kind: conversation.MessageStatus, Text: conversationTranscriptNotice(len(resume.Transcript)), Delivery: conversation.DeliverySaved}
	return c.prepareContents(ctx, string(scope.organization), string(scope.project), scope.credential.ID, notice)
}
