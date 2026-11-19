package workspacehub

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestWorkspaceOperatorHistoryKeepsCursor(t *testing.T) {
	t.Parallel()
	for _, snapshot := range []bool{false, true} {
		t.Run("unpageable/"+strconv.FormatBool(snapshot), func(t *testing.T) {
			var value any = conversations.ConversationMessagesPage{Messages: []conversations.ConversationMessageResource{{Seq: 1, Text: strings.Repeat("x", operatortool.MaxResultBytes)}}}
			if snapshot {
				value = conversations.ConversationSnapshot{Conversation: conversations.ConversationResource{Title: strings.Repeat("x", operatortool.MaxResultBytes)}}
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			bounded, err := boundOperatorHistory(raw, snapshot, nil)
			var size *operatortool.ResultTooLargeError
			if len(bounded) != 0 || !errors.As(err, &size) {
				t.Fatalf("unpageable history bytes=%d error=%v", len(bounded), err)
			}
		})
	}
	messages := []conversations.ConversationMessageResource{}
	for i := int64(1); i <= 20; i++ {
		messages = append(messages, conversations.ConversationMessageResource{Seq: i, Text: strings.Repeat("x", 16000)})
	}
	for _, snapshot := range []bool{false, true} {
		var raw []byte
		var err error
		if snapshot {
			raw, err = json.Marshal(conversations.ConversationSnapshot{Messages: messages})
		} else {
			raw, err = json.Marshal(conversations.ConversationMessagesPage{Messages: messages})
		}
		if err != nil {
			t.Fatal(err)
		}
		bounded, err := boundOperatorHistory(raw, snapshot, nil)
		if err != nil {
			t.Fatalf("snapshot=%v size=%d %v", snapshot, len(bounded), err)
		}
		if snapshot {
			var page conversations.ConversationSnapshot
			if json.Unmarshal(bounded, &page) != nil || !page.HasMore || page.Messages[0].Seq <= 1 || page.Messages[len(page.Messages)-1].Seq != 20 {
				t.Fatal("snapshot dropped history cursor")
			}
		} else {
			var page conversations.ConversationMessagesPage
			if json.Unmarshal(bounded, &page) != nil || page.NextCursor == nil || page.Messages[0].Seq <= 1 || page.Messages[len(page.Messages)-1].Seq != 20 {
				t.Fatal("history dropped cursor")
			}
		}
	}
}
