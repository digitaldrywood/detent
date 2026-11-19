package hubserver

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

// The attachment endpoints. Uploads are sniffed and
// bounded, they belong to the actor that made them until a message binds
// them, reads follow the conversation's read rule, and an upload nobody sent
// is swept with its bytes.

func attachmentPNG() []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 64)...)
}

// postConversationAttachment posts one multipart upload and returns the raw
// response so a test can assert a failure as easily as a success. It takes
// the service and base path rather than a fixture: both the operator fixture
// and the worker fixture upload through it.
func postConversationAttachment(t *testing.T, service *Service, base, token, id, name, media string, content []byte, key string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if name != "" {
		header := make(map[string][]string)
		header["Content-Disposition"] = []string{`form-data; name="file"; filename="` + name + `"`}
		if media != "" {
			header["Content-Type"] = []string{media}
		}
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatalf("create multipart part: %v", err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatalf("write multipart part: %v", err)
		}
	}
	if key != "" {
		if err := writer.WriteField("idempotency_key", key); err != nil {
			t.Fatalf("write idempotency key: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, base+"/conversations/"+id+"/attachments", bytes.NewReader(body.Bytes()))
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
}

// uploadConversationAttachmentFile posts one upload and decodes the created
// resource, failing the test on any other status.
func uploadConversationAttachmentFile(t *testing.T, service *Service, base, token, id, name, media string, content []byte) conversation.AttachmentUpload {
	t.Helper()
	response := postConversationAttachment(t, service, base, token, id, name, media, content, hubapi.NewID("idem"))
	requireNativeStatus(t, response, http.StatusCreated)
	var upload conversation.AttachmentUpload
	decodeHubResponse(t, response, &upload)
	return upload
}

func (f conversationAPIFixture) uploadAttachment(t *testing.T, token, id, name, media string, content []byte, key string) *httptest.ResponseRecorder {
	t.Helper()
	return postConversationAttachment(t, f.service, f.base, token, id, name, media, content, key)
}

func (f conversationAPIFixture) upload(t *testing.T, token, id, name, media string, content []byte) conversation.AttachmentUpload {
	t.Helper()
	return uploadConversationAttachmentFile(t, f.service, f.base, token, id, name, media, content)
}

// share makes a conversation readable by every member of the project, the
// way a handoff does, without creating an issue.
func (f conversationAPIFixture) share(t *testing.T, id string) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		record, err := writer.Conversation(t.Context(), f.project.ID, id)
		if err != nil {
			return err
		}
		record.Visibility = conversation.VisibilityShared
		_, err = writer.UpdateConversation(t.Context(), record, record.Revision)
		return err
	}); err != nil {
		t.Fatal(err)
	}

}

func TestConversationAttachmentCommandValidation(t *testing.T) {
	t.Parallel()
	objects := &conversationContentFixture{}
	f := newConversationAPIFixture(t, &ConversationConfig{Enabled: true, ArchiveContents: objects})
	created := f.create(t, f.token, map[string]any{"title": "Command limits"})
	id := created.Conversation.ID
	// A second conversation of the same owner, and a principal that shares
	// the first one: the two ways an attachment can be the wrong one.
	elsewhereConversation := f.create(t, f.token, map[string]any{"title": "Another chat"})
	f.share(t, id)

	mine := f.upload(t, f.token, id, "notes.md", "text/markdown", []byte("# Notes\n"))
	elsewhere := f.upload(t, f.token, elsewhereConversation.Conversation.ID, "notes.md", "text/markdown", []byte("# Notes\n"))
	theirs := f.upload(t, f.other, id, "notes.md", "text/markdown", []byte("# Theirs\n"))

	cases := []struct {
		name        string
		attachments []string
		status      int
	}{
		{name: "own unsent attachment", attachments: []string{mine.ID}, status: http.StatusOK},
		{name: "unknown attachment", attachments: []string{conversation.NewAttachmentID()}, status: http.StatusUnprocessableEntity},
		{name: "another conversation", attachments: []string{elsewhere.ID}, status: http.StatusUnprocessableEntity},
		{name: "another principal", attachments: []string{theirs.ID}, status: http.StatusUnprocessableEntity},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			objects.mu.Lock()
			before := objects.publications
			objects.mu.Unlock()
			response := f.command(t, f.token, id, conversation.Command{Key: "cmd_" + test.name, Kind: conversation.CommandMessage, Text: "x", Attachments: test.attachments})
			requireNativeStatus(t, response, test.status)
			objects.mu.Lock()
			after := objects.publications
			objects.mu.Unlock()
			if test.status != http.StatusOK && after != before {
				t.Fatalf("rejected command published %d objects; want 0", after-before)
			}
		})
	}
}

func TestConversationAttachmentSweep(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	created := f.create(t, f.token, map[string]any{"title": "Sweep"})
	id := created.Conversation.ID
	expired := f.upload(t, f.token, id, "old.txt", "text/plain", []byte("old"))
	kept := f.upload(t, f.token, id, "new.txt", "text/plain", []byte("new"))
	bound := f.upload(t, f.token, id, "sent.txt", "text/plain", []byte("sent"))
	requireNativeStatus(t, f.command(t, f.token, id, conversation.Command{Key: "cmd_sweep", Kind: conversation.CommandMessage, Text: "x", Attachments: []string{bound.ID}}), http.StatusOK)

	if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE conversation_attachments SET expires_at=$3 WHERE organization_id=$1 AND id=$2", f.project.OrganizationID, expired.ID, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	swept, err := f.service.conversations.SweepAttachments(t.Context())
	if err != nil {
		t.Fatalf("sweepAttachments: %v", err)
	}
	if swept != 1 {
		t.Fatalf("sweepAttachments swept %d, want 1", swept)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, expired.URL, f.token, nil), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, kept.URL, f.token, nil), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, bound.URL, f.token, nil), http.StatusOK)
	for _, live := range []struct {
		upload conversation.AttachmentUpload
		text   string
	}{{kept, "new"}, {bound, "sent"}} {
		stored, err := f.storedConversationAttachment(t.Context(), id, live.upload.ID)
		if err != nil || stored.ContentID == "" {
			t.Fatalf("live attachment lost its object reference: %v", err)
		}
		content, err := f.storedConversationAttachmentContent(t.Context(), stored)
		if err != nil || string(content) != live.text {
			t.Fatalf("live attachment content=%q want=%q err=%v", content, live.text, err)
		}
	}
}

// The worker control envelope carries the message's attachments so the
// runner can download them, and the URL it names is one the worker's own
// token can read.
func TestConversationControlCarriesAttachments(t *testing.T) {
	t.Parallel()
	f := newConversationWorkerFixture(t)
	requireNativeStatus(t, f.bind(t, nil), http.StatusOK)

	content := attachmentPNG()
	upload := uploadConversationAttachmentFile(t, f.service, f.base, f.token, f.record.ID, "shot.png", "image/png", content)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+f.record.ID+"/commands", f.token,
		conversation.Command{Key: "cmd_control", Kind: conversation.CommandMessage, Text: "look at this", Attachments: []string{upload.ID}}), http.StatusOK)

	response := f.controls(t, 0, 0)
	requireNativeStatus(t, response, http.StatusOK)
	var page workerControlsResponse
	decodeHubResponse(t, response, &page)
	if len(page.Controls) != 1 {
		t.Fatalf("controls = %+v, want one", page.Controls)
	}
	control := page.Controls[0]
	if control.Kind != "message" || control.Text != "look at this" {
		t.Fatalf("control = %+v, want the user message", control)
	}
	if len(control.Attachments) != 1 {
		t.Fatalf("control attachments = %+v, want one", control.Attachments)
	}
	if control.Attachments[0] != upload.Attachment {
		t.Fatalf("control attachment = %+v, want %+v", control.Attachments[0], upload.Attachment)
	}

	// The URL the control names is the one a worker token can read, and it
	// streams the bytes the runner hands the provider as image input.
	download := performHubAPIRequest(t, f.service, http.MethodGet, control.Attachments[0].URL, f.worker, nil)
	requireNativeStatus(t, download, http.StatusOK)
	if !bytes.Equal(download.Body.Bytes(), content) {
		t.Fatalf("worker download = %d bytes, want %d", download.Body.Len(), len(content))
	}
	if got := download.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("worker download Content-Type = %q, want image/png", got)
	}
}

// The upload accepts exactly two fields and exactly one file. Anything else
// in the body is refused before a byte is stored.
func TestConversationAttachmentUploadRefusesExtraParts(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	created := f.create(t, f.token, map[string]any{"title": "Parts"})
	id := created.Conversation.ID
	var initialBlobs int
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM archive_objects WHERE organization_id=$1", f.project.OrganizationID).Scan(&initialBlobs); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		build func(*multipart.Writer)
	}{
		{
			name: "a second file part",
			build: func(writer *multipart.Writer) {
				for range 2 {
					part, err := writer.CreateFormFile("file", "notes.md")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := part.Write([]byte("# Notes\n")); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "an unknown field",
			build: func(writer *multipart.Writer) {
				part, err := writer.CreateFormFile("file", "notes.md")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := part.Write([]byte("# Notes\n")); err != nil {
					t.Fatal(err)
				}
				if err := writer.WriteField("conversation_id", "cnv_elsewhere"); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			test.build(writer)
			if err := writer.WriteField("idempotency_key", hubapi.NewID("idem")); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, f.base+"/conversations/"+id+"/attachments", bytes.NewReader(body.Bytes()))
			request.Header.Set("Content-Type", writer.FormDataContentType())
			request.Header.Set("Authorization", "Bearer "+f.token)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
		})
	}

	// A body that is not multipart at all, and a truncated one, are refused
	// the same way rather than reaching the store.
	request := httptest.NewRequest(http.MethodPost, f.base+"/conversations/"+id+"/attachments", strings.NewReader(`{"file":"notes.md"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+f.token)
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	requireNativeStatus(t, response, http.StatusUnprocessableEntity)

	var blobs int
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM archive_objects WHERE organization_id=$1", f.project.OrganizationID).Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	blobs -= initialBlobs
	if blobs != 0 {
		t.Fatalf("blobs after refused uploads = %d, want 0", blobs)
	}
}

// conversationAttachmentBodyError keeps the two failures apart: a body over
// the reader's limit is 413, anything else is a 422.

func (f conversationAPIFixture) storedConversationAttachment(ctx context.Context, conversationID, id string) (record conversationstore.Attachment, err error) {
	err = f.service.config.TenantRepository.ReadTenant(ctx, f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(conversationstore.AttachmentReader)
		if !ok {
			return conversationContentUnavailable()
		}
		record, err = owner.ConversationAttachment(ctx, f.project.ID, conversationID, id)
		return err
	})
	return
}

func (f conversationAPIFixture) storedConversationAttachmentContent(ctx context.Context, record conversationstore.Attachment) ([]byte, error) {
	var object conversationstore.ContentObject
	if err := f.service.config.TenantRepository.ReadTenant(ctx, f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		object, err = r.ConversationContent(ctx, record.ContentID)
		return err
	}); err != nil {
		return nil, err
	}
	return f.service.conversations.Store.Contents.ReadContent(ctx, string(f.project.OrganizationID), object.Reference)
}
