package hubserver

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
)

type conversationAttachmentRaceContent struct {
	*conversationContentFixture
	beforePublish func() error
	writerBusy    func() bool
}

func (s *conversationAttachmentRaceContent) PublishContent(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	if s.beforePublish != nil {
		fn := s.beforePublish
		s.beforePublish = nil
		if err := fn(); err != nil {
			return attachment.ArchiveReference{}, err
		}
	}
	return s.conversationContentFixture.PublishContent(ctx, organization, content)
}

func (s *conversationAttachmentRaceContent) ReadContent(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if s.writerBusy != nil && s.writerBusy() {
		return nil, errors.New("object read held writer transaction")
	}
	return s.conversationContentFixture.ReadContent(ctx, organization, ref)
}

func TestConversationAttachmentPublicationRechecksRevision(t *testing.T) {
	objects := &conversationAttachmentRaceContent{conversationContentFixture: &conversationContentFixture{}}
	f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	created := f.create(t, f.token, map[string]any{"title": "attachment publication"})
	f.service.tokenUseWork.Wait()
	store := f.service.conversations.store
	record, err := store.readConversationByID(t.Context(), store.reader, created.Conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.Execution.Status = conversation.ExecutionRunning
	prepared := prepareTestConversationRecord(t, store, record)
	objects.writerBusy = func() bool { return store.db.Stats().InUse != 0 }
	objects.beforePublish = func() error {
		tx, err := store.db.BeginTx(prepared, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := store.updateConversation(prepared, tx, &record, record.Revision); err != nil {
			return err
		}
		return tx.Commit()
	}
	response := postConversationAttachment(t, f.service, f.base, f.token, record.ID, "example.png", "image/png", attachmentPNG(), "upload")
	requireNativeError(t, response, http.StatusConflict, "stale_execution")
	var rows int
	if err := store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_attachments WHERE conversation_id=?", record.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("stale upload committed %d rows: %v", rows, err)
	}
	response = postConversationAttachment(t, f.service, f.base, f.token, record.ID, "example.png", "image/png", attachmentPNG(), "upload")
	requireNativeStatus(t, response, http.StatusCreated)
	response = postConversationAttachment(t, f.service, f.base, f.token, record.ID, "example.png", "image/png", attachmentPNG(), "upload")
	requireNativeStatus(t, response, http.StatusCreated)
	if err := store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_attachments WHERE conversation_id=?", record.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("upload retry committed %d rows: %v", rows, err)
	}
}
