package hubserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
)

func TestConversationAttachmentReplayDoesNotPublish(t *testing.T) {
	for _, test := range []struct {
		name    string
		content []byte
		status  int
	}{
		{"same body", []byte("first body"), http.StatusCreated},
		{"different body", []byte("other body"), http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := &conversationContentFixture{}
			f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects})
			created := f.create(t, f.token, map[string]any{"title": "attachment replay"})
			response := postConversationAttachment(t, f.service, f.base, f.token, created.Conversation.ID, "example.txt", "text/plain", []byte("first body"), "upload")
			requireNativeStatus(t, response, http.StatusCreated)
			objects.mu.Lock()
			before := objects.publications
			objects.mu.Unlock()
			response = postConversationAttachment(t, f.service, f.base, f.token, created.Conversation.ID, "example.txt", "text/plain", test.content, "upload")
			requireNativeStatus(t, response, test.status)
			objects.mu.Lock()
			after := objects.publications
			objects.mu.Unlock()
			if after != before {
				t.Fatalf("replayed upload published %d objects; want 0", after-before)
			}
		})
	}
}

func TestConversationAttachmentStagingChecksAllowanceBeforePublication(t *testing.T) {
	for _, test := range []struct {
		name         string
		allowance    int64
		fail         bool
		status       int
		publications int
	}{
		{"exact allowance", 0, false, http.StatusCreated, 1},
		{"exhausted allowance", -1, false, http.StatusTooManyRequests, 0},
		{"uncertain publication", 0, true, http.StatusInternalServerError, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := &conversationAttachmentRaceContent{conversationContentFixture: &conversationContentFixture{}}
			f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects})
			created := f.create(t, f.token, map[string]any{"title": "attachment allowance"})
			f.service.tokenUseWork.Wait()
			content := []byte("staged attachment")
			ref, _, err := attachment.PrepareArchive(string(f.project.OrganizationID), 1, bytes.NewReader(content))
			if err != nil {
				t.Fatal(err)
			}
			database := f.service.database
			var before int64
			if err := database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&before); err != nil {
				t.Fatal(err)
			}
			database.hostedOrganization = f.project.OrganizationID
			plans := capacityHostedPlans()
			for i := range plans.Plans {
				plans.Plans[i].Allowances["collaboration_bytes"] = before + ref.CompressedSize + test.allowance
			}
			if err := database.configureHostedPlans(t.Context(), &HostedConfig{Plans: &plans}); err != nil {
				t.Fatal(err)
			}
			objects.mu.Lock()
			initial := objects.publications
			objects.fail = test.fail
			objects.mu.Unlock()
			objects.beforePublish = func() error {
				if database.db.Stats().InUse != 0 {
					return errors.New("object I/O held writer transaction")
				}
				var size int64
				if err := database.reader.QueryRowContext(t.Context(), "SELECT size FROM attachments WHERE organization_id=? AND object_key=? AND object_deleted_at IS NULL", f.project.OrganizationID, ref.Key).Scan(&size); err != nil {
					return fmt.Errorf("read staged byte accounting: %w", err)
				}
				if size != ref.CompressedSize {
					return errors.New("staged bytes were not accounted before publication")
				}
				return nil
			}
			response := postConversationAttachment(t, f.service, f.base, f.token, created.Conversation.ID, "example.txt", "text/plain", content, "upload")
			requireNativeStatus(t, response, test.status)
			objects.mu.Lock()
			publications := objects.publications - initial
			objects.mu.Unlock()
			if publications != test.publications {
				t.Fatalf("publication calls=%d; want %d", publications, test.publications)
			}
			var after int64
			if err := database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&after); err != nil {
				t.Fatal(err)
			}
			charged := ref.CompressedSize
			if test.status == http.StatusTooManyRequests {
				charged = 0
			}
			if after-before != charged {
				t.Fatalf("staged byte charge=%d; want %d", after-before, charged)
			}
		})
	}
}

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
	requireNativeError(t, response, http.StatusConflict, "revision_conflict")
	ref, _, err := attachment.PrepareArchive(string(record.OrganizationID), 1, bytes.NewReader(attachmentPNG()))
	if err != nil {
		t.Fatal(err)
	}
	var accounted int64
	if err := store.reader.QueryRowContext(t.Context(), "SELECT size FROM attachments WHERE organization_id=? AND object_key=? AND object_deleted_at IS NULL", record.OrganizationID, ref.Key).Scan(&accounted); err != nil || accounted != ref.CompressedSize {
		t.Fatalf("rejected upload left unaccounted staged bytes: size=%d err=%v", accounted, err)
	}
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
