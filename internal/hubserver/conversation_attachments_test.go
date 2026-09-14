package hubserver

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/runner"
)

// The attachment endpoints (decisions section 17.1). Uploads are sniffed and
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
	response := postConversationAttachment(t, service, base, token, id, name, media, content, newNativeID("idem"))
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
	service := f.service.conversations
	if err := service.transact(t.Context(), func(tx *sql.Tx, _ time.Time) error {
		record, err := service.store.readConversationByID(t.Context(), tx, id)
		if err != nil {
			return err
		}
		revision := record.Revision
		record.Visibility = conversation.VisibilityShared
		return service.store.updateConversation(t.Context(), tx, &record, revision)
	}); err != nil {
		t.Fatalf("share conversation: %v", err)
	}
}

func TestConversationAttachmentUploadAndRead(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	created := f.create(t, f.token, map[string]any{"title": "Dropzone"})
	id := created.Conversation.ID
	content := attachmentPNG()

	upload := f.upload(t, f.token, id, "shot.png", "image/png", content)
	if err := conversation.ValidateAttachmentID(upload.ID); err != nil {
		t.Fatalf("upload id %q: %v", upload.ID, err)
	}
	if upload.Name != "shot.png" || upload.MIME != "image/png" || upload.Size != int64(len(content)) {
		t.Fatalf("upload = %+v, want shot.png image/png %d bytes", upload, len(content))
	}
	want := f.base + "/conversations/" + id + "/attachments/" + upload.ID
	if upload.URL != want {
		t.Fatalf("upload.URL = %q, want %q", upload.URL, want)
	}
	if upload.ExpiresAt == nil {
		t.Fatal("an unsent upload must report expires_at")
	}
	if delta := upload.ExpiresAt.Sub(time.Now().UTC()); delta < 6*24*time.Hour || delta > conversation.AttachmentTTL+time.Hour {
		t.Fatalf("expires_at is %v away, want about %v", delta, conversation.AttachmentTTL)
	}

	// The blob streams back with its own media type and byte-for-byte.
	response := performHubAPIRequest(t, f.service, http.MethodGet, upload.URL, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if got := response.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if !bytes.Equal(response.Body.Bytes(), content) {
		t.Fatalf("streamed %d bytes, want %d", response.Body.Len(), len(content))
	}
	if got := response.Header().Get("Content-Disposition"); !strings.Contains(got, "shot.png") {
		t.Fatalf("Content-Disposition = %q, want the file name", got)
	}

	// The same idempotency key returns the same attachment, not a second one.
	key := newNativeID("idem")
	first := f.uploadAttachment(t, f.token, id, "again.txt", "text/plain", []byte("hello"), key)
	requireNativeStatus(t, first, http.StatusCreated)
	second := f.uploadAttachment(t, f.token, id, "again.txt", "text/plain", []byte("hello"), key)
	requireNativeStatus(t, second, http.StatusCreated)
	var one, two conversation.AttachmentUpload
	decodeHubResponse(t, first, &one)
	decodeHubResponse(t, second, &two)
	if one.ID != two.ID {
		t.Fatalf("replayed upload = %q, want %q", two.ID, one.ID)
	}
	// A different payload under the same key is a conflict.
	conflict := f.uploadAttachment(t, f.token, id, "again.txt", "text/plain", []byte("different"), key)
	requireNativeStatus(t, conflict, http.StatusConflict)
}

func TestConversationAttachmentUploadValidation(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	created := f.create(t, f.token, map[string]any{"title": "Limits"})
	id := created.Conversation.ID
	cases := []struct {
		name    string
		file    string
		media   string
		content []byte
		key     string
		status  int
	}{
		{name: "accepted image", file: "shot.png", media: "image/png", content: attachmentPNG(), key: "k1", status: http.StatusCreated},
		{name: "accepted text", file: "notes.md", media: "text/markdown", content: []byte("# Notes\n"), key: "k2", status: http.StatusCreated},
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
			requireNativeStatus(t, response, test.status)
		})
	}
}

func TestConversationAttachmentDelete(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	created := f.create(t, f.token, map[string]any{"title": "Delete"})
	id := created.Conversation.ID
	upload := f.upload(t, f.token, id, "notes.md", "text/markdown", []byte("# Notes\n"))

	// Another principal cannot delete an upload it does not own, and cannot
	// see the private conversation at all.
	response := performHubAPIRequest(t, f.service, http.MethodDelete, upload.URL, f.other, nil)
	requireNativeStatus(t, response, http.StatusNotFound)

	response = performHubAPIRequest(t, f.service, http.MethodDelete, upload.URL, f.token, nil)
	requireNativeStatus(t, response, http.StatusNoContent)
	response = performHubAPIRequest(t, f.service, http.MethodGet, upload.URL, f.token, nil)
	requireNativeStatus(t, response, http.StatusNotFound)
	// The bytes are gone, not only the row.
	var blobs int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_attachment_blobs").Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if blobs != 0 {
		t.Fatalf("blobs after delete = %d, want 0", blobs)
	}
	// Deleting twice is still not found.
	response = performHubAPIRequest(t, f.service, http.MethodDelete, upload.URL, f.token, nil)
	requireNativeStatus(t, response, http.StatusNotFound)
}

func TestConversationAttachmentBindsToMessage(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	created := f.create(t, f.token, map[string]any{"title": "Bind"})
	id := created.Conversation.ID
	upload := f.upload(t, f.token, id, "shot.png", "image/png", attachmentPNG())

	response := f.command(t, f.token, id, conversation.Command{Key: "cmd_bind", Kind: conversation.CommandMessage, Text: "look", Attachments: []string{upload.ID}})
	requireNativeStatus(t, response, http.StatusOK)

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

	// A bound attachment no longer expires and cannot be deleted or reused.
	var expires sql.NullString
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT expires_at FROM conversation_attachments WHERE id = ?", upload.ID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if expires.Valid {
		t.Fatalf("expires_at after binding = %q, want NULL", expires.String)
	}
	response = performHubAPIRequest(t, f.service, http.MethodDelete, upload.URL, f.token, nil)
	requireNativeStatus(t, response, http.StatusForbidden)
	response = f.command(t, f.token, id, conversation.Command{Key: "cmd_again", Kind: conversation.CommandMessage, Text: "again", Attachments: []string{upload.ID}})
	requireNativeStatus(t, response, http.StatusUnprocessableEntity)
	// The bytes are still readable after the message is sent.
	response = performHubAPIRequest(t, f.service, http.MethodGet, upload.URL, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
}

func TestConversationAttachmentCommandValidation(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
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
			response := f.command(t, f.token, id, conversation.Command{Key: "cmd_" + test.name, Kind: conversation.CommandMessage, Text: "x", Attachments: test.attachments})
			requireNativeStatus(t, response, test.status)
		})
	}
}

func TestConversationAttachmentTotalSizeLimit(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	created := f.create(t, f.token, map[string]any{"title": "Total"})
	id := created.Conversation.ID
	half := bytes.Repeat([]byte("a"), conversation.MaxAttachmentBytes/2+1)
	first := f.upload(t, f.token, id, "a.txt", "text/plain", half)
	second := f.upload(t, f.token, id, "b.txt", "text/plain", half)
	response := f.command(t, f.token, id, conversation.Command{Key: "cmd_total", Kind: conversation.CommandMessage, Text: "x", Attachments: []string{first.ID, second.ID}})
	requireNativeStatus(t, response, http.StatusUnprocessableEntity)
}

func TestConversationAttachmentReadFollowsAudience(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	created := f.create(t, f.token, map[string]any{"title": "Private"})
	id := created.Conversation.ID
	upload := f.upload(t, f.token, id, "notes.md", "text/markdown", []byte("# Secret\n"))

	// A private conversation is invisible to another operator, attachment and
	// all; the same attachment becomes readable once the conversation is
	// shared.
	response := performHubAPIRequest(t, f.service, http.MethodGet, upload.URL, f.other, nil)
	requireNativeStatus(t, response, http.StatusNotFound)

	f.share(t, id)
	response = performHubAPIRequest(t, f.service, http.MethodGet, upload.URL, f.other, nil)
	requireNativeStatus(t, response, http.StatusOK)
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

	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversation_attachments SET expires_at = ? WHERE id = ?", "2020-01-01T00:00:00Z", expired.ID); err != nil {
		t.Fatal(err)
	}
	swept, err := f.service.conversations.sweepAttachments(t.Context())
	if err != nil {
		t.Fatalf("sweepAttachments: %v", err)
	}
	if swept != 1 {
		t.Fatalf("sweepAttachments swept %d, want 1", swept)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, expired.URL, f.token, nil), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, kept.URL, f.token, nil), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, bound.URL, f.token, nil), http.StatusOK)
	var blobs int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_attachment_blobs").Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if blobs != 2 {
		t.Fatalf("blobs after the sweep = %d, want 2", blobs)
	}
}

// The worker control envelope carries the message's attachments so the
// runner can download them, and the URL it names is one the worker's own
// token can read (decisions section 17.1).
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

// The hub-side coordinator turn carries the attachments as provider input:
// the images ride the turn request and the text files become one delimited
// data block on the prompt (decisions section 17.1).
func TestCoordinatorTurnCarriesAttachments(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "coordinator-attachments")
	defer f.coordinator().Stop()

	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations", f.token,
		map[string]any{"key": newNativeID("cck"), "title": "Attachments"})
	requireNativeStatus(t, response, http.StatusCreated)
	var created conversationCreateResponse
	decodeHubResponse(t, response, &created)
	id := created.Conversation.ID

	image := attachmentPNG()
	notes := []byte("# Notes\nThe lease lapses under load.\n")
	uploadedImage := uploadConversationAttachmentFile(t, f.service, f.base, f.token, id, "shot.png", "image/png", image)
	uploadedNotes := uploadConversationAttachmentFile(t, f.service, f.base, f.token, id, "notes.md", "text/markdown", notes)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+id+"/commands", f.token,
		conversation.Command{Key: "cmd_coordinator", Kind: conversation.CommandMessage, Text: "What do these say?",
			Attachments: []string{uploadedImage.ID, uploadedNotes.ID}}), http.StatusOK)

	waitUntil(t, "the coordinator turn", func() bool { return f.backend.turns() > 0 })
	request := f.backend.request(t, 0)
	if len(request.Attachments) != 2 {
		t.Fatalf("turn attachments = %+v, want two", request.Attachments)
	}
	byName := map[string]runner.AgentAttachment{}
	for _, attachment := range request.Attachments {
		byName[attachment.Name] = attachment
	}
	if got := byName["shot.png"]; got.MIME != "image/png" || !bytes.Equal(got.Content, image) || got.Size != int64(len(image)) {
		t.Fatalf("image attachment = %+v, want the uploaded bytes", got)
	}
	if got := byName["notes.md"]; !bytes.Equal(got.Content, notes) {
		t.Fatalf("text attachment = %+v, want the uploaded bytes", got)
	}
	if images := runner.AttachmentImages(request.Attachments); len(images) != 1 || images[0].Name != "shot.png" {
		t.Fatalf("provider image input = %+v, want only the png", images)
	}
	// The text file is data on the prompt; the image never is.
	if !strings.Contains(request.Prompt, "notes.md") || !strings.Contains(request.Prompt, "The lease lapses under load.") {
		t.Fatalf("prompt does not carry the text attachment: %q", request.Prompt)
	}
	if !strings.Contains(request.Prompt, "What do these say?") {
		t.Fatalf("prompt does not carry the user message: %q", request.Prompt)
	}
	if strings.Index(request.Prompt, "What do these say?") > strings.Index(request.Prompt, "<attachments>") {
		t.Fatalf("the data block must follow the instructions: %q", request.Prompt)
	}
	if strings.Contains(request.Prompt, "shot.png") {
		t.Fatalf("the image must not be inlined as text: %q", request.Prompt)
	}
}

// An attachment whose bytes went away between the command and the turn is
// left out rather than failing the turn.
func TestCoordinatorTurnSkipsMissingAttachmentBytes(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "coordinator-missing-attachment")
	defer f.coordinator().Stop()

	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations", f.token,
		map[string]any{"key": newNativeID("cck"), "title": "Missing"})
	requireNativeStatus(t, response, http.StatusCreated)
	var created conversationCreateResponse
	decodeHubResponse(t, response, &created)
	id := created.Conversation.ID

	gone := uploadConversationAttachmentFile(t, f.service, f.base, f.token, id, "gone.md", "text/markdown", []byte("# Gone\n"))
	kept := uploadConversationAttachmentFile(t, f.service, f.base, f.token, id, "kept.md", "text/markdown", []byte("# Kept\n"))
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+id+"/commands", f.token,
		conversation.Command{Key: "cmd_missing", Kind: conversation.CommandMessage, Text: "read these",
			Attachments: []string{gone.ID, kept.ID}}), http.StatusOK)
	if _, err := f.service.database.db.ExecContext(t.Context(),
		`DELETE FROM conversation_attachment_blobs WHERE artifact_ref IN (SELECT artifact_ref FROM conversation_attachments WHERE id = ?)`, gone.ID); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, "the coordinator turn", func() bool { return f.backend.turns() > 0 })
	request := f.backend.request(t, 0)
	if len(request.Attachments) != 1 || request.Attachments[0].Name != "kept.md" {
		t.Fatalf("turn attachments = %+v, want only the file that still has bytes", request.Attachments)
	}
}

// The upload accepts exactly two fields and exactly one file. Anything else
// in the body is refused before a byte is stored (decisions section 17.1).
func TestConversationAttachmentUploadRefusesExtraParts(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	created := f.create(t, f.token, map[string]any{"title": "Parts"})
	id := created.Conversation.ID

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
			if err := writer.WriteField("idempotency_key", newNativeID("idem")); err != nil {
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
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_attachment_blobs").Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if blobs != 0 {
		t.Fatalf("blobs after refused uploads = %d, want 0", blobs)
	}
}

// conversationAttachmentBodyError keeps the two failures apart: a body over
// the reader's limit is 413, anything else is a 422.
func TestConversationAttachmentBodyError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{name: "over the limit", err: &http.MaxBytesError{Limit: conversation.MaxAttachmentBytes}, status: http.StatusRequestEntityTooLarge},
		{name: "wrapped over the limit", err: fmt.Errorf("read part: %w", &http.MaxBytesError{Limit: 1}), status: http.StatusRequestEntityTooLarge},
		{name: "truncated", err: io.ErrUnexpectedEOF, status: http.StatusUnprocessableEntity},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var failure *nativeError
			if !errors.As(conversationAttachmentBodyError(test.err), &failure) {
				t.Fatalf("conversationAttachmentBodyError(%v) is not an API error", test.err)
			}
			if failure.status != test.status {
				t.Fatalf("status = %d, want %d", failure.status, test.status)
			}
		})
	}
}
