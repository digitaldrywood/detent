package conversation_test

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
)

func openConversationTestHub(t *testing.T) *hubfixture.Hub {
	t.Helper()
	var mu sync.Mutex
	objects := map[string][]byte{}
	return hubfixture.Open(t, hubfixture.Config{
		Conversation: &hubserver.ConversationConfig{Enabled: true},
		Content: func(_ context.Context, req entryauthority.ContentRequest) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			if req.Write {
				objects[req.Reference.Key] = bytes.Clone(req.Data)
				return nil, nil
			}
			data, ok := objects[req.Reference.Key]
			if !ok {
				return nil, artifact.ErrMissing
			}
			return bytes.Clone(data), nil
		},
	})
}

func TestConversationReferenceBounds(t *testing.T) {
	t.Parallel()
	h := openConversationTestHub(t)
	post := func(t *testing.T, p hubfixture.Native, text string) string {
		t.Helper()
		created := p.Request(t, http.MethodPost, p.Base+"/conversations", conversations.ConversationCreateRequest{Key: "create", Title: "References"})
		requireStatus(t, created, http.StatusCreated)
		var record conversations.ConversationCreatedResponse
		decodeResponse(t, created, &record)
		id := record.Conversation.ID
		response := p.Request(t, http.MethodPost, p.Base+"/conversations/"+id+"/commands", conversation.Command{Key: "message", Kind: conversation.CommandMessage, Text: text})
		requireStatus(t, response, http.StatusOK)
		return id
	}
	t.Run("references per message", func(t *testing.T) {
		p := hubfixture.NewNative(t, h, h.Primary, "Reference bounds")
		var text strings.Builder
		text.WriteString("Everything:")
		for index := range conversation.MaxReferences + 5 {
			issue := p.Create(t, "Issue "+strconv.Itoa(index))
			text.WriteString(" #" + strconv.Itoa(issue.Number))
		}
		id := post(t, p, text.String())
		response := p.Request(t, http.MethodGet, p.Base+"/conversations/"+id, nil)
		requireStatus(t, response, http.StatusOK)
		var snapshot conversations.ConversationSnapshot
		decodeResponse(t, response, &snapshot)
		if len(snapshot.Messages) == 0 || len(snapshot.Messages[0].References) != conversation.MaxReferences {
			t.Fatalf("expected %d references in the message: %s", conversation.MaxReferences, response.Body.String())
		}
	})
	t.Run("excerpt runes", func(t *testing.T) {
		p := hubfixture.NewNative(t, h, hubfixture.Secondary, "Reference excerpt")
		issue := p.Create(t, "Long reference")
		id := post(t, p, "#"+strconv.Itoa(issue.Number)+" "+strings.Repeat("é", 500))
		response := p.Request(t, http.MethodGet, p.Base+"/work-items/"+string(issue.WorkItemID)+"/references", nil)
		requireStatus(t, response, http.StatusOK)
		var page conversations.ConversationWorkItemReferencePage
		decodeResponse(t, response, &page)
		if len(page.References) != 1 || page.References[0].ConversationID != id {
			t.Fatalf("references = %#v", page.References)
		}
		if got := len([]rune(page.References[0].Excerpt)); got != 200 {
			t.Fatalf("excerpt runes = %d, want 200", got)
		}
	})
}
