package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
		name                 string
		allowance            int64
		fail                 bool
		status, publications int
	}{
		{"exact allowance", 0, false, http.StatusCreated, 2},
		{"exhausted allowance", -1, false, http.StatusTooManyRequests, 0},
		{"uncertain publication", 0, true, http.StatusInternalServerError, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := &conversationAttachmentRaceContent{conversationContentFixture: &conversationContentFixture{}}
			f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects})
			conversations := []conversationCreateResponse{f.create(t, f.token, map[string]any{"title": "First attachment allowance"}), f.create(t, f.token, map[string]any{"title": "Second attachment allowance"})}
			f.service.tokenUseWork.Wait()
			repository := f.service.database.postgres
			usage := func() (int64, int64) {
				t.Helper()
				var raw, archive int64
				if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					raw, err = r.CollaborationBytes(t.Context())
					if err != nil {
						return err
					}
					archive, err = r.ArchiveBytes(t.Context())
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return raw, archive
			}
			content := []byte("staged attachment")
			ref, _, err := attachment.PrepareArchive(string(f.project.OrganizationID), 1, bytes.NewReader(content))
			if err != nil {
				t.Fatal(err)
			}
			before, archiveBefore := usage()
			limit := before + 2*int64(len(content)) + test.allowance
			if test.status == http.StatusTooManyRequests {
				limit = before + int64(len(content)) + test.allowance
			}
			plans := capacityHostedPlans()
			for i := range plans.Plans {
				plans.Plans[i].Allowances["collaboration_bytes"] = limit
			}
			if err := f.service.database.configureHostedPlans(t.Context(), &HostedConfig{Plans: &plans}); err != nil {
				t.Fatal(err)
			}
			objects.mu.Lock()
			initial := objects.publications
			objects.fail = test.fail
			objects.mu.Unlock()
			for index, created := range conversations {
				objects.beforePublish = func() error {
					if repository.Stats().AcquiredConns() != 0 {
						t.Error("object I/O held database connections")
						return errors.New("object I/O held database connections")
					}
					used, archive := usage()
					if used != before+int64(index+1)*int64(len(content)) || archive != archiveBefore+ref.CompressedSize {
						t.Errorf("prepublication raw=%d archive=%d; want %d/%d", used, archive, before+int64(index+1)*int64(len(content)), archiveBefore+ref.CompressedSize)
						return errors.New("staged bytes were not accounted before publication")
					}
					return repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						object, err := r.ArchiveObjectByReference(t.Context(), ref)
						if err != nil {
							return fmt.Errorf("read staged archive metadata: %w", err)
						}
						if object.Reference != ref {
							return errors.New("staged archive reference changed")
						}
						return nil
					})
				}
				response := postConversationAttachment(t, f.service, f.base, f.token, created.Conversation.ID, "example.txt", "text/plain", content, "upload")
				requireNativeStatus(t, response, test.status)
			}
			objects.mu.Lock()
			publications := objects.publications - initial
			objects.mu.Unlock()
			if publications != test.publications {
				t.Fatalf("publication calls=%d want%d", publications, test.publications)
			}
			after, archiveAfter := usage()
			wantRaw, wantArchive := 2*int64(len(content)), ref.CompressedSize
			if test.status == http.StatusTooManyRequests {
				wantRaw, wantArchive = 0, 0
			}
			if after-before != wantRaw || archiveAfter-archiveBefore != wantArchive {
				t.Fatalf("held raw=%d archive=%d want%d/%d", after-before, archiveAfter-archiveBefore, wantRaw, wantArchive)
			}
			admin := usagePostgresAdmin(t, f.service)
			for _, created := range conversations {
				var rows int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM (SELECT 1 FROM conversation_attachments WHERE organization_id=$1 AND project_id=$2 AND conversation_id=$3 LIMIT 2) bounded", f.project.OrganizationID, f.project.ID, created.Conversation.ID).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				want := 0
				if test.status == http.StatusCreated {
					want = 1
				}
				if rows != want {
					t.Fatalf("public attachment rows=%d want%d", rows, want)
				}
			}
			if test.fail {
				objects.mu.Lock()
				beforeRetry := objects.publications
				objects.fail = false
				objects.mu.Unlock()
				for _, created := range conversations {
					response := postConversationAttachment(t, f.service, f.base, f.token, created.Conversation.ID, "changed.txt", "text/plain", content, "upload")
					requireNativeError(t, response, http.StatusConflict, "idempotency_conflict")
					for range 2 {
						response = postConversationAttachment(t, f.service, f.base, f.token, created.Conversation.ID, "example.txt", "text/plain", content, "upload")
						requireNativeStatus(t, response, http.StatusCreated)
					}
				}
				objects.mu.Lock()
				additional := objects.publications - beforeRetry
				objects.mu.Unlock()
				if additional != 1 {
					t.Fatalf("pending retries published%d objects; want1 shared body", additional)
				}
				if raw, archive := usage(); raw != after || archive != archiveAfter {
					t.Fatalf("retry changed held charge raw=%d archive=%d; want%d/%d", raw, archive, after, archiveAfter)
				}
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
	for _, consumed := range []bool{false, true} {
		t.Run(fmt.Sprintf("reservation_consumed_%t", consumed), func(t *testing.T) {
			objects := &conversationAttachmentRaceContent{conversationContentFixture: &conversationContentFixture{}}
			f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects})
			created := f.create(t, f.token, map[string]any{"title": "attachment publication"})
			f.service.tokenUseWork.Wait()
			repository := f.service.database.postgres
			recordID := created.Conversation.ID
			if consumed {
				other := f.create(t, f.token, map[string]any{"title": "shared attachment"})
				f.upload(t, f.token, other.Conversation.ID, "example.png", "image/png", attachmentPNG())
			}
			objects.writerBusy = func() bool { return repository.Stats().AcquiredConns() != 0 }
			objects.beforePublish = func() error {
				if consumed {
					nested := postConversationAttachment(t, f.service, f.base, f.token, recordID, "example.png", "image/png", attachmentPNG(), "upload")
					requireNativeStatus(t, nested, http.StatusCreated)
					var upload conversation.AttachmentUpload
					if err := json.Unmarshal(nested.Body.Bytes(), &upload); err != nil {
						t.Fatal(err)
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, upload.URL, f.token, nil), http.StatusNoContent)
					return nil
				}
				return repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					record, err := w.Conversation(t.Context(), f.project.ID, recordID)
					if err != nil {
						return err
					}
					record.Execution.Status = conversation.ExecutionRunning
					_, err = w.UpdateConversation(t.Context(), record, record.Revision)
					return err
				})
			}
			response := postConversationAttachment(t, f.service, f.base, f.token, recordID, "example.png", "image/png", attachmentPNG(), "upload")
			requireNativeError(t, response, http.StatusConflict, "revision_conflict")
			ref, _, err := attachment.PrepareArchive(string(f.project.OrganizationID), 1, bytes.NewReader(attachmentPNG()))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				object, err := r.ArchiveObjectByReference(t.Context(), ref)
				if err != nil {
					return err
				}
				if object.Reference != ref {
					return errors.New("stale upload lost staged archive metadata")
				}
				charged, err := r.CollaborationBytes(t.Context())
				if err != nil {
					return err
				}
				if charged != int64(len(attachmentPNG())) {
					return errors.New("stale upload lost raw quota reservation")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			admin := usagePostgresAdmin(t, f.service)
			count := func() int {
				t.Helper()
				var rows int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM (SELECT 1 FROM conversation_attachments WHERE organization_id=$1 AND project_id=$2 AND conversation_id=$3 LIMIT 2) bounded", f.project.OrganizationID, f.project.ID, recordID).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				return rows
			}
			if rows := count(); rows != 0 {
				t.Fatalf("stale upload committed%drows", rows)
			}
			for range 2 {
				response = postConversationAttachment(t, f.service, f.base, f.token, recordID, "example.png", "image/png", attachmentPNG(), "upload")
				requireNativeStatus(t, response, http.StatusCreated)
			}
			if rows := count(); rows != 1 {
				t.Fatalf("upload retry committed%drows, want1", rows)
			}
			if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				used, err := r.CollaborationBytes(t.Context())
				want := int64(len(attachmentPNG()))
				if consumed {
					want *= 2
				}
				if err == nil && used != want {
					return errors.New("upload retry charged raw bytes twice")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConversationUnsentAttachmentRetainsLastArchiveCharge(t *testing.T) {
	for _, test := range []struct {
		name   string
		expire bool
		shared bool
	}{
		{name: "delete"},
		{name: "expire", expire: true},
		{name: "delete shared object", shared: true},
		{name: "expire shared object", expire: true, shared: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := &conversationContentFixture{}
			base := time.Now().UTC().Truncate(time.Microsecond)
			var offset atomic.Int64
			f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects}, func() time.Time { return base.Add(time.Duration(offset.Load())) })
			first := f.create(t, f.token, map[string]any{"title": "first upload"})
			second := f.create(t, f.token, map[string]any{"title": "second upload"})
			repository := f.service.database.postgres
			usage := func() (int64, int64) {
				t.Helper()
				var raw, archive int64
				if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					raw, err = r.CollaborationBytes(t.Context())
					if err != nil {
						return err
					}
					archive, err = r.ArchiveBytes(t.Context())
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return raw, archive
			}
			before, archiveBefore := usage()
			content := []byte("unsent upload bytes")
			ref, _, err := attachment.PrepareArchive(string(f.project.OrganizationID), 1, bytes.NewReader(content))
			if err != nil {
				t.Fatal(err)
			}
			upload := f.upload(t, f.token, first.Conversation.ID, "example.txt", "text/plain", content)
			var other conversation.AttachmentUpload
			copies := int64(1)
			if test.shared {
				offset.Store(int64(conversation.AttachmentTTL / 2))
				other = f.upload(t, f.token, second.Conversation.ID, "example.txt", "text/plain", content)
				copies++
			}
			if raw, archive := usage(); raw-before != copies*int64(len(content)) || archive-archiveBefore != ref.CompressedSize {
				t.Fatalf("upload charge raw=%d archive=%d; want%d/%d", raw-before, archive-archiveBefore, copies*int64(len(content)), ref.CompressedSize)
			}
			if test.expire {
				offset.Store(int64(conversation.AttachmentTTL))
				if swept, err := f.service.conversations.sweepAttachments(t.Context()); err != nil || swept != 1 {
					t.Fatalf("expired uploads=%d; want 1: %v", swept, err)
				}
			} else {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, upload.URL, f.token, nil), http.StatusNoContent)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, upload.URL, f.token, nil), http.StatusNotFound)
			if test.shared {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, other.URL, f.token, nil), http.StatusOK)
			}
			if raw, archive := usage(); raw-before != int64(len(content)) || archive-archiveBefore != ref.CompressedSize {
				t.Fatalf("remaining charge raw=%d archive=%d; want%d/%d", raw-before, archive-archiveBefore, len(content), ref.CompressedSize)
			}
			if test.shared {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, other.URL, f.token, nil), http.StatusNoContent)
			}
			if raw, archive := usage(); raw-before != int64(len(content)) || archive-archiveBefore != ref.CompressedSize {
				t.Fatalf("last tombstone charge raw=%d archive=%d; want%d/%d", raw-before, archive-archiveBefore, len(content), ref.CompressedSize)
			}
			stored, err := objects.ReadContent(t.Context(), string(f.project.OrganizationID), ref)
			if err != nil || !bytes.Equal(stored, content) {
				t.Fatalf("retained blob=%q: %v", stored, err)
			}
			if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.ArchiveObjectByReference(t.Context(), ref)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
