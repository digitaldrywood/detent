package conversation_test

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type attachmentRouteFixture struct {
	project      hubfixture.Native
	token, other string
}

func newAttachmentRouteFixture(t *testing.T, h *hubfixture.Hub) attachmentRouteFixture {
	t.Helper()
	org := h.Primary
	p := hubfixture.NewNative(t, h, org, "Attachment route "+hubapi.NewID("fixture"))
	other := hubtest.Credential(t, h.Repository, org, hubtest.CredentialOptions{Projects: []tracker.ProjectID{p.Project.ID}, Now: h.Now()})
	return attachmentRouteFixture{project: p, token: p.Token, other: other.Token}
}

func (f attachmentRouteFixture) request(t *testing.T, method, path, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	return f.project.Hub.Request(t, method, path, token, payload)
}

func (f attachmentRouteFixture) create(t *testing.T, token, title string) conversations.ConversationCreatedResponse {
	t.Helper()
	payload := conversations.ConversationCreateRequest{Key: hubapi.NewID("create"), Title: title}
	response := f.request(t, http.MethodPost, f.project.Base+"/conversations", token, payload)
	hubfixture.RequireStatus(t, response, http.StatusCreated)
	var created conversations.ConversationCreatedResponse
	hubfixture.Decode(t, response, &created)
	return created
}

func (f attachmentRouteFixture) uploadAttachment(t *testing.T, token, id, name, media string, content []byte, key string) *httptest.ResponseRecorder {
	t.Helper()
	return postConversationAttachment(t, f.project.Hub.Handler(), f.project.Base, token, id, name, media, content, key)
}

func (f attachmentRouteFixture) upload(t *testing.T, token, id, name, media string, content []byte) conversation.AttachmentUpload {
	t.Helper()
	response := f.uploadAttachment(t, token, id, name, media, content, hubapi.NewID("idem"))
	hubfixture.RequireStatus(t, response, http.StatusCreated)
	var upload conversation.AttachmentUpload
	hubfixture.Decode(t, response, &upload)
	return upload
}

func (f attachmentRouteFixture) command(t *testing.T, token, id string, command conversation.Command) *httptest.ResponseRecorder {
	t.Helper()
	return f.request(t, http.MethodPost, f.project.Base+"/conversations/"+id+"/commands", token, command)
}

func (f attachmentRouteFixture) snapshot(t *testing.T, token, id string) conversations.ConversationSnapshot {
	t.Helper()
	response := f.request(t, http.MethodGet, f.project.Base+"/conversations/"+id, token, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var snapshot conversations.ConversationSnapshot
	hubfixture.Decode(t, response, &snapshot)
	return snapshot
}

func attachmentPNG() []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 64)...)
}

func postConversationAttachment(t *testing.T, handler http.Handler, base, token, id, name, media string, content []byte, key string) *httptest.ResponseRecorder {
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
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, base+"/conversations/"+id+"/attachments", bytes.NewReader(body.Bytes()))
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func (f attachmentRouteFixture) share(t *testing.T, id string) {
	t.Helper()
	if err := f.project.Hub.Repository.WriteTenant(t.Context(), f.project.Organization(), corestore.Serving, func(writer tenantstore.Writer) error {
		record, err := writer.Conversation(t.Context(), f.project.Project.ID, id)
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

func (f attachmentRouteFixture) storedConversationAttachment(ctx context.Context, conversationID, id string) (record conversationstore.Attachment, err error) {
	err = f.project.Hub.Repository.ReadTenant(ctx, f.project.Organization(), corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(conversationstore.AttachmentReader)
		if !ok {
			return errors.New("attachment reader is unavailable")
		}
		record, err = owner.ConversationAttachment(ctx, f.project.Project.ID, conversationID, id)
		return err
	})
	return
}

func (f attachmentRouteFixture) storedConversationAttachmentContent(ctx context.Context, record conversationstore.Attachment) ([]byte, error) {
	var object conversationstore.ContentObject
	if err := f.project.Hub.Repository.ReadTenant(ctx, f.project.Organization(), corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		object, err = r.ConversationContent(ctx, record.ContentID)
		return err
	}); err != nil {
		return nil, err
	}
	objects := conversations.ArchiveStore{Call: f.project.Hub.Config.Content}
	return objects.ReadContent(ctx, string(f.project.Organization()), object.Reference)
}

func TestConversationAttachmentRoutes(t *testing.T) {
	t.Parallel()
	hub := openConversationTestHub(t)
	t.Run("upload and read", func(t *testing.T) {
		f := newAttachmentRouteFixture(t, hub)
		created := f.create(t, f.token, "Dropzone")
		id := created.Conversation.ID
		content := attachmentPNG()

		upload := f.upload(t, f.token, id, "shot.png", "image/png", content)
		if err := conversation.ValidateAttachmentID(upload.ID); err != nil {
			t.Fatalf("upload id %q: %v", upload.ID, err)
		}
		if upload.Name != "shot.png" || upload.MIME != "image/png" || upload.Size != int64(len(content)) {
			t.Fatalf("upload = %+v, want shot.png image/png %d bytes", upload, len(content))
		}
		want := f.project.Base + "/conversations/" + id + "/attachments/" + upload.ID
		if upload.URL != want {
			t.Fatalf("upload.URL = %q, want %q", upload.URL, want)
		}
		if upload.ExpiresAt == nil {
			t.Fatal("an unsent upload must report expires_at")
		}

		response := f.request(t, http.MethodGet, upload.URL, f.token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		if got := response.Header().Get("Content-Type"); got != "image/png" {
			t.Fatalf("Content-Type = %q, want image/png", got)
		}
		if !bytes.Equal(response.Body.Bytes(), content) {
			t.Fatalf("streamed %d bytes, want %d", response.Body.Len(), len(content))
		}
		if got := response.Header().Get("Content-Disposition"); !strings.Contains(got, "shot.png") {
			t.Fatalf("Content-Disposition = %q, want the file name", got)
		}

		key := hubapi.NewID("idem")
		first := f.uploadAttachment(t, f.token, id, "again.txt", "text/plain", []byte("hello"), key)
		hubfixture.RequireStatus(t, first, http.StatusCreated)
		second := f.uploadAttachment(t, f.token, id, "again.txt", "text/plain", []byte("hello"), key)
		hubfixture.RequireStatus(t, second, http.StatusCreated)
		var one, two conversation.AttachmentUpload
		hubfixture.Decode(t, first, &one)
		hubfixture.Decode(t, second, &two)
		if one.ID != two.ID {
			t.Fatalf("replayed upload = %q, want %q", two.ID, one.ID)
		}
		conflict := f.uploadAttachment(t, f.token, id, "again.txt", "text/plain", []byte("different"), key)
		hubfixture.RequireStatus(t, conflict, http.StatusConflict)
	})
	t.Run("upload validation", func(t *testing.T) {
		f := newAttachmentRouteFixture(t, hub)
		created := f.create(t, f.token, "Limits")
		id := created.Conversation.ID
		cases := []struct {
			name    string
			file    string
			media   string
			content []byte
			key     string
			status  int
		}{
			{name: "accepted json", file: "data.json", media: "application/json", content: []byte(`{"a":1}`), key: "k3", status: http.StatusCreated},
			{name: "unsupported media", file: "doc.pdf", media: "application/pdf", content: []byte("%PDF-1.7\n"), key: "k4", status: http.StatusUnprocessableEntity},
			{name: "sniff mismatch", file: "shot.png", media: "image/png", content: []byte("plain words, not a png"), key: "k5", status: http.StatusUnprocessableEntity},
			{name: "image renamed as text", file: "shot.txt", media: "text/plain", content: attachmentPNG(), key: "k6", status: http.StatusUnprocessableEntity},
			{name: "empty file", file: "empty.txt", media: "text/plain", content: nil, key: "k7", status: http.StatusUnprocessableEntity},
			{name: "no file part", file: "", media: "", content: nil, key: "k8", status: http.StatusUnprocessableEntity},
			{name: "no idempotency key", file: "notes.md", media: "text/markdown", content: []byte("# Notes\n"), key: "", status: http.StatusUnprocessableEntity},
			{name: "over the size limit", file: "big.txt", media: "text/plain", content: bytes.Repeat([]byte("a"), conversation.MaxAttachmentBytes+1), key: "k9", status: http.StatusRequestEntityTooLarge},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				response := f.uploadAttachment(t, f.token, id, test.file, test.media, test.content, test.key)
				hubfixture.RequireStatus(t, response, test.status)
			})
		}
	})
	t.Run("delete", func(t *testing.T) {
		f := newAttachmentRouteFixture(t, hub)
		created := f.create(t, f.token, "Delete")
		id := created.Conversation.ID
		upload := f.upload(t, f.token, id, "notes.md", "text/markdown", []byte("# Notes\n"))
		stored, err := f.storedConversationAttachment(t.Context(), id, upload.ID)
		if err != nil || stored.ContentID == "" {
			t.Fatalf("uploaded attachment has no object reference: %v", err)
		}

		response := f.request(t, http.MethodDelete, upload.URL, f.other, nil)
		hubfixture.RequireStatus(t, response, http.StatusNotFound)

		response = f.request(t, http.MethodDelete, upload.URL, f.token, nil)
		hubfixture.RequireStatus(t, response, http.StatusNoContent)
		response = f.request(t, http.MethodGet, upload.URL, f.token, nil)
		hubfixture.RequireStatus(t, response, http.StatusNotFound)
		if content, err := f.storedConversationAttachmentContent(t.Context(), stored); err != nil || string(content) != "# Notes\n" {
			t.Fatalf("last charged attachment lost its retained content: %q %v", content, err)
		}
		response = f.request(t, http.MethodDelete, upload.URL, f.token, nil)
		hubfixture.RequireStatus(t, response, http.StatusNotFound)
	})
	t.Run("binds to message", func(t *testing.T) {
		f := newAttachmentRouteFixture(t, hub)
		created := f.create(t, f.token, "Bind")
		id := created.Conversation.ID
		upload := f.upload(t, f.token, id, "shot.png", "image/png", attachmentPNG())

		response := f.command(t, f.token, id, conversation.Command{Key: "cmd_bind", Kind: conversation.CommandMessage, Text: "look", Attachments: []string{upload.ID}})
		hubfixture.RequireStatus(t, response, http.StatusOK)

		snapshot := f.snapshot(t, f.token, id)
		if len(snapshot.Messages) == 0 {
			t.Fatal("snapshot has no messages")
		}
		message := snapshot.Messages[len(snapshot.Messages)-1]
		if len(message.Attachments) != 1 {
			t.Fatalf("message attachments = %+v, want one", message.Attachments)
		}
		got := message.Attachments[0]
		if got.ID != upload.ID || got.Name != "shot.png" || got.MIME != "image/png" || got.Size != upload.Size || got.URL != upload.URL {
			t.Fatalf("message attachment = %+v, want %+v", got, upload.Attachment)
		}

		stored, err := f.storedConversationAttachment(t.Context(), id, upload.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.ExpiresAt != nil {
			t.Fatalf("expires_at after binding = %q, want NULL", stored.ExpiresAt.Format(time.RFC3339Nano))
		}
		response = f.request(t, http.MethodDelete, upload.URL, f.token, nil)
		hubfixture.RequireStatus(t, response, http.StatusForbidden)
		response = f.command(t, f.token, id, conversation.Command{Key: "cmd_again", Kind: conversation.CommandMessage, Text: "again", Attachments: []string{upload.ID}})
		hubfixture.RequireStatus(t, response, http.StatusUnprocessableEntity)
		response = f.request(t, http.MethodGet, upload.URL, f.token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
	})
	t.Run("total size limit", func(t *testing.T) {
		f := newAttachmentRouteFixture(t, hub)
		created := f.create(t, f.token, "Total")
		id := created.Conversation.ID
		half := bytes.Repeat([]byte("a"), conversation.MaxAttachmentBytes/2+1)
		first := f.upload(t, f.token, id, "a.txt", "text/plain", half)
		second := f.upload(t, f.token, id, "b.txt", "text/plain", half)
		response := f.command(t, f.token, id, conversation.Command{Key: "cmd_total", Kind: conversation.CommandMessage, Text: "x", Attachments: []string{first.ID, second.ID}})
		hubfixture.RequireStatus(t, response, http.StatusUnprocessableEntity)
	})
	t.Run("read follows audience", func(t *testing.T) {
		f := newAttachmentRouteFixture(t, hub)
		created := f.create(t, f.token, "Private")
		id := created.Conversation.ID
		upload := f.upload(t, f.token, id, "notes.md", "text/markdown", []byte("# Secret\n"))

		response := f.request(t, http.MethodGet, upload.URL, f.other, nil)
		hubfixture.RequireStatus(t, response, http.StatusNotFound)

		f.share(t, id)
		response = f.request(t, http.MethodGet, upload.URL, f.other, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
	})
	t.Run("refuses expired or deleted", func(t *testing.T) {
		f := newAttachmentRouteFixture(t, hub)
		id := f.create(t, f.token, "Expiry").Conversation.ID
		cases := []struct {
			name   string
			update string
			status int
		}{
			{name: "expired", update: "UPDATE conversation_attachments SET expires_at=$3 WHERE organization_id=$1 AND id=$2", status: http.StatusUnprocessableEntity},
			{name: "deleted", update: "UPDATE conversation_attachments SET deleted_at=$3 WHERE organization_id=$1 AND id=$2", status: http.StatusUnprocessableEntity},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				uploaded := f.upload(t, f.token, id, test.name+".md", "text/markdown", []byte("# "+test.name+"\n"))
				if test.update != "" {
					if _, err := f.project.Hub.Admin.ExecContext(t.Context(), test.update, f.project.Organization(), uploaded.ID, f.project.Hub.Now().UTC().Add(-time.Minute)); err != nil {
						t.Fatal(err)
					}
				}
				response := f.command(t, f.token, id, conversation.Command{Key: "cmd_" + test.name, Kind: conversation.CommandMessage, Text: "x", Attachments: []string{uploaded.ID}})
				hubfixture.RequireStatus(t, response, test.status)
			})
		}
	})
}
