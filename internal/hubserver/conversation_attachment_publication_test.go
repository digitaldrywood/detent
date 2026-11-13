package hubserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
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
			if err := f.readConversationAttachmentUsage(t.Context(), &before); err != nil {
				t.Fatal(err)
			}
			database.hostedOrganization = f.project.OrganizationID
			plans := hostedplans.CapacityPlans()
			for i := range plans.Plans {
				plans.Plans[i].Allowances["collaboration_bytes"] = before + ref.CompressedSize + test.allowance
			}
			if err := hubapi.FromFeatureError(database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&hostedplans.HostedConfig{Plans: &plans}))); err != nil {
				t.Fatal(err)
			}
			objects.mu.Lock()
			initial := objects.publications
			objects.fail = test.fail
			objects.mu.Unlock()
			objects.beforePublish = func() error {
				if database.postgres.Stats().AcquiredConns() != 0 {
					return errors.New("object I/O held writer transaction")
				}
				var size int64
				object, err := f.stagedConversationArchive(t.Context(), ref)
				if err != nil {
					return fmt.Errorf("read staged byte accounting: %w", err)
				}
				size = object.CollaborationBytes
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
			if err := f.readConversationAttachmentUsage(t.Context(), &after); err != nil {
				t.Fatal(err)
			}
			charged := ref.CompressedSize
			if test.status == http.StatusTooManyRequests {
				charged = 0
			}
			if after-before != charged {
				t.Fatalf("staged byte charge=%d; want %d", after-before, charged)
			}
			if test.fail {
				objects.mu.Lock()
				objects.fail = false
				objects.mu.Unlock()
				retry := postConversationAttachment(t, f.service, f.base, f.token, created.Conversation.ID, "example.txt", "text/plain", content, "upload")
				requireNativeStatus(t, retry, http.StatusCreated)
				var recovered conversation.AttachmentUpload
				decodeHubResponse(t, retry, &recovered)
				if err := f.readConversationAttachmentUsage(t.Context(), &after); err != nil || after-before != charged {
					t.Fatalf("uncertain upload retry changed charge: charge=%d want=%d error=%v", after-before, charged, err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, recovered.URL, f.token, nil), http.StatusNoContent)
				if err := f.readConversationAttachmentUsage(t.Context(), &after); err != nil || after != before {
					t.Fatalf("recovered upload retained stage ownership: charge=%d error=%v", after-before, err)
				}
			}
		})
	}
	t.Run("concurrent same content retains pending upload ownership", func(t *testing.T) {
		objects := &conversationAttachmentRaceContent{conversationContentFixture: &conversationContentFixture{}}
		f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects})
		first := f.create(t, f.token, map[string]any{"title": "first concurrent upload"})
		second := f.create(t, f.token, map[string]any{"title": "second concurrent upload"})
		f.service.tokenUseWork.Wait()
		content := []byte("concurrent shared attachment")
		ref, _, err := attachment.PrepareArchive(string(f.project.OrganizationID), 1, bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		var before int64
		if err := f.readConversationAttachmentUsage(t.Context(), &before); err != nil {
			t.Fatal(err)
		}
		f.service.database.hostedOrganization = f.project.OrganizationID
		plans := hostedplans.CapacityPlans()
		for index := range plans.Plans {
			plans.Plans[index].Allowances["collaboration_bytes"] = before + ref.CompressedSize
		}
		if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&hostedplans.HostedConfig{Plans: &plans}))); err != nil {
			t.Fatal(err)
		}
		arrived := make(chan struct{}, 2)
		release := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
		var releaseOnce [2]sync.Once
		var calls atomic.Int32
		objects.beforePublishContent = func(ctx context.Context, body []byte) error {
			if !bytes.Equal(body, content) {
				return nil
			}
			index := int(calls.Add(1)) - 1
			if index >= len(release) {
				return errors.New("unexpected concurrent upload publication")
			}
			arrived <- struct{}{}
			select {
			case <-release[index]:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		responses := make(chan *httptest.ResponseRecorder, 2)
		var workers sync.WaitGroup
		t.Cleanup(func() {
			for index := range release {
				releaseOnce[index].Do(func() { close(release[index]) })
			}
			workers.Wait()
		})
		for _, id := range []string{first.Conversation.ID, second.Conversation.ID} {
			workers.Add(1)
			go func() {
				defer workers.Done()
				responses <- postConversationAttachment(t, f.service, f.base, f.token, id, "example.txt", "text/plain", content, "concurrent-upload")
			}()
		}
		for range release {
			select {
			case <-arrived:
			case response := <-responses:
				t.Fatalf("upload returned before both stages committed: status=%d body=%s", response.Code, response.Body.String())
			case <-t.Context().Done():
				t.Fatal("concurrent uploads did not reach publication")
			}
		}
		releaseOnce[0].Do(func() { close(release[0]) })
		var committed conversation.AttachmentUpload
		select {
		case response := <-responses:
			requireNativeStatus(t, response, http.StatusCreated)
			decodeHubResponse(t, response, &committed)
		case <-t.Context().Done():
			t.Fatal("first staged upload did not commit")
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, committed.URL, f.token, nil), http.StatusNoContent)
		var after int64
		if err := f.readConversationAttachmentUsage(t.Context(), &after); err != nil || after-before != ref.CompressedSize {
			t.Fatalf("pending upload lost shared archive charge: charge=%d want=%d error=%v", after-before, ref.CompressedSize, err)
		}
		object, err := f.stagedConversationArchive(t.Context(), ref)
		if err != nil || object.CollaborationBytes != ref.CompressedSize {
			t.Fatalf("pending upload lost shared archive: %+v error=%v", object, err)
		}
		releaseOnce[1].Do(func() { close(release[1]) })
		select {
		case response := <-responses:
			requireNativeStatus(t, response, http.StatusCreated)
			decodeHubResponse(t, response, &committed)
		case <-t.Context().Done():
			t.Fatal("second staged upload did not commit")
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, committed.URL, f.token, nil), http.StatusOK)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, committed.URL, f.token, nil), http.StatusNoContent)
		if err := f.readConversationAttachmentUsage(t.Context(), &after); err != nil || after != before {
			t.Fatalf("last concurrent upload retained archive charge: charge=%d error=%v", after-before, err)
		}
		if _, err := f.stagedConversationArchive(t.Context(), ref); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("last concurrent upload retained archive ownership: %v", err)
		}
	})
}

type conversationAttachmentRaceContent struct {
	*conversationContentFixture
	beforePublish        func() error
	beforePublishContent func(context.Context, []byte) error
	writerBusy           func() bool
}

func (s *conversationAttachmentRaceContent) PublishContent(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	if s.beforePublishContent != nil {
		if err := s.beforePublishContent(ctx, content); err != nil {
			return attachment.ArchiveReference{}, err
		}
	}
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
	var record conversationstore.Conversation
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.Conversation(t.Context(), f.project.ID, created.Conversation.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	record.Execution.Status = conversation.ExecutionRunning
	objects.writerBusy = func() bool { return f.service.database.postgres.Stats().AcquiredConns() != 0 }
	objects.beforePublish = func() error {
		return f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.UpdateConversation(t.Context(), record, record.Revision)
			return err
		})
	}
	response := postConversationAttachment(t, f.service, f.base, f.token, record.ID, "example.png", "image/png", attachmentPNG(), "upload")
	requireNativeError(t, response, http.StatusConflict, "revision_conflict")
	ref, _, err := attachment.PrepareArchive(string(record.OrganizationID), 1, bytes.NewReader(attachmentPNG()))
	if err != nil {
		t.Fatal(err)
	}
	var accounted int64
	object, err := f.stagedConversationArchive(t.Context(), ref)
	accounted = object.CollaborationBytes
	if err != nil || accounted != ref.CompressedSize {
		t.Fatalf("rejected upload left unaccounted staged bytes: size=%d err=%v", accounted, err)
	}
	var rows int
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_attachments WHERE organization_id=$1 AND conversation_id=$2", record.OrganizationID, record.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("stale upload committed %d rows: %v", rows, err)
	}
	response = postConversationAttachment(t, f.service, f.base, f.token, record.ID, "example.png", "image/png", attachmentPNG(), "upload")
	requireNativeStatus(t, response, http.StatusCreated)
	response = postConversationAttachment(t, f.service, f.base, f.token, record.ID, "example.png", "image/png", attachmentPNG(), "upload")
	requireNativeStatus(t, response, http.StatusCreated)
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_attachments WHERE organization_id=$1 AND conversation_id=$2", record.OrganizationID, record.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("upload retry committed %d rows: %v", rows, err)
	}
}

func TestConversationUnsentAttachmentReclaimsArchiveCharge(t *testing.T) {
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
			f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects})
			first := f.create(t, f.token, map[string]any{"title": "first upload"})
			second := f.create(t, f.token, map[string]any{"title": "second upload"})
			usage := func() int64 {
				t.Helper()
				var size int64
				if err := f.readConversationAttachmentUsage(t.Context(), &size); err != nil {
					t.Fatal(err)
				}
				return size
			}
			before := usage()
			content := []byte("unsent upload bytes")
			ref, _, err := attachment.PrepareArchive(string(f.project.OrganizationID), 1, bytes.NewReader(content))
			if err != nil {
				t.Fatal(err)
			}
			upload := f.upload(t, f.token, first.Conversation.ID, "example.txt", "text/plain", content)
			var other conversation.AttachmentUpload
			if test.shared {
				other = f.upload(t, f.token, second.Conversation.ID, "example.txt", "text/plain", content)
			}
			if got := usage() - before; got != ref.CompressedSize {
				t.Fatalf("upload charge=%d; want %d", got, ref.CompressedSize)
			}
			if test.expire {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE conversation_attachments SET expires_at=$3 WHERE organization_id=$1 AND id=$2", f.project.OrganizationID, upload.ID, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
					t.Fatal(err)
				}
				if swept, err := f.service.conversations.SweepAttachments(t.Context()); err != nil || swept != 1 {
					t.Fatalf("expired uploads=%d; want 1: %v", swept, err)
				}
			} else {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, upload.URL, f.token, nil), http.StatusNoContent)
			}
			want := int64(0)
			if test.shared {
				want = ref.CompressedSize
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, other.URL, f.token, nil), http.StatusOK)
			}
			if got := usage() - before; got != want {
				t.Fatalf("remaining archive charge=%d; want %d", got, want)
			}
			if test.shared {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, other.URL, f.token, nil), http.StatusNoContent)
			}
			if got := usage() - before; got != 0 {
				t.Fatalf("last upload left archive charge=%d; want 0", got)
			}
			var rows int
			_, err = f.stagedConversationArchive(t.Context(), ref)
			if !errors.Is(err, corestore.ErrNotFound) {
				rows = 1
				t.Fatalf("unreferenced archive rows=%d; want 0: %v", rows, err)
			}
		})
	}
}

func (f conversationAPIFixture) readConversationAttachmentUsage(ctx context.Context, used *int64) error {
	return f.service.config.TenantRepository.ReadTenant(ctx, f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		*used, err = r.CollaborationBytes(ctx)
		return err
	})
}

func (f conversationAPIFixture) stagedConversationArchive(ctx context.Context, ref attachment.ArchiveReference) (object artifactstore.ArchiveObject, err error) {
	err = f.service.config.TenantRepository.ReadTenant(ctx, f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		object, err = r.ArchiveObjectByReference(ctx, ref)
		return err
	})
	return
}
