package conversation_test

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
)

func openConversationTestHub(t *testing.T) *hubfixture.Hub {
	t.Helper()
	var mu sync.Mutex
	objects := map[string][]byte{}
	return hubfixture.Open(t, hubfixture.Config{
		Conversation: &conversations.ConversationConfig{Enabled: true},
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
		p := hubfixture.NewNative(t, h, h.Primary, "Reference count")
		var text strings.Builder
		var expected []string
		text.WriteString("Everything:")
		for index := range conversation.MaxReferences + 5 {
			issue := p.Create(t, "Issue "+strconv.Itoa(index))
			text.WriteString(" #" + strconv.Itoa(issue.Number))
			if index < conversation.MaxReferences {
				expected = append(expected, string(issue.WorkItemID))
			}
		}
		id := post(t, p, text.String())
		response := p.Request(t, http.MethodGet, p.Base+"/conversations/"+id, nil)
		requireStatus(t, response, http.StatusOK)
		var snapshot conversations.ConversationSnapshot
		decodeResponse(t, response, &snapshot)
		for _, message := range snapshot.Messages {
			if message.Text != text.String() {
				continue
			}
			var actual []string
			for _, reference := range message.References {
				actual = append(actual, reference.ID)
			}
			if !slices.Equal(actual, expected) {
				t.Fatalf("reference IDs = %v, want %v", actual, expected)
			}
			return
		}
		t.Fatal("submitted message is missing")
	})
	t.Run("excerpt runes", func(t *testing.T) {
		p := hubfixture.NewNative(t, h, h.Primary, "Reference excerpt")
		issue := p.Create(t, "Long reference")
		text := "#" + strconv.Itoa(issue.Number) + " " + strings.Repeat("é", 500)
		id := post(t, p, text)
		response := p.Request(t, http.MethodGet, p.Base+"/work-items/"+string(issue.WorkItemID)+"/references", nil)
		requireStatus(t, response, http.StatusOK)
		var page conversations.ConversationWorkItemReferencePage
		decodeResponse(t, response, &page)
		want := string([]rune(text)[:199]) + "…"
		for _, reference := range page.References {
			if reference.ConversationID != id {
				continue
			}
			if reference.Excerpt != want {
				t.Fatalf("excerpt = %q, want %q", reference.Excerpt, want)
			}
			return
		}
		t.Fatalf("conversation %s is missing from issue references", id)
	})
}
