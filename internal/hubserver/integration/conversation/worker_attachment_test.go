package conversation_test

import (
	"bytes"
	"net/http"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
)

func TestConversationControlCarriesAttachments(t *testing.T) {
	t.Parallel()
	h := openConversationTestHub(t)
	f := newWorkerConversationProject(t, h, "Worker attachment delivery")
	content := attachmentPNG()
	uploaded := postConversationAttachment(t, h.Handler(), f.project.Base, f.project.Token, f.id, "shot.png", "image/png", content, hubapi.NewID("upload"))
	hubfixture.RequireStatus(t, uploaded, http.StatusCreated)
	var attachment conversation.AttachmentUpload
	hubfixture.Decode(t, uploaded, &attachment)
	response := f.project.Request(t, http.MethodPost, f.path()+"/commands", conversation.Command{
		Key: "worker-attachment", Kind: conversation.CommandMessage, Text: "Look at this", Attachments: []string{attachment.ID},
	})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	response = f.controls(t, 0, 0)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var page conversations.WorkerControlsResponse
	hubfixture.Decode(t, response, &page)
	index := slices.IndexFunc(page.Controls, func(control conversations.WorkerControl) bool {
		return control.Key == "worker-attachment" && control.Kind == "message" && control.Text == "Look at this"
	})
	if index < 0 {
		t.Fatal("the worker did not receive the submitted message")
	}
	control := page.Controls[index]
	for _, delivered := range control.Attachments {
		if delivered == attachment.Attachment {
			download := h.Request(t, http.MethodGet, delivered.URL, f.worker, nil)
			hubfixture.RequireStatus(t, download, http.StatusOK)
			if !bytes.Equal(download.Body.Bytes(), content) || download.Header().Get("Content-Type") != "image/png" {
				t.Fatalf("worker download has type %q and bytes %x", download.Header().Get("Content-Type"), download.Body.Bytes())
			}
			return
		}
	}
	t.Fatalf("worker message is missing attachment %s", attachment.ID)
}
