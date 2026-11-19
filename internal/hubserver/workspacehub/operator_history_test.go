package workspacehub

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
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
	t.Run("oversized worker message preserves controls", func(t *testing.T) {
		f := newConversationWorkerFixture(t)
		if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "DELETE FROM conversations WHERE organization_id=$1 AND project_id=$2 AND id=$3", f.project.OrganizationID, f.project.ID, f.record.ID); err != nil {
			t.Fatal(err)
		}
		operator, _ := conversationOperatorToken(t, f.nativeFixture, "snapshot-operator")
		ctx := workspaceOperatorContext(t, f.service, operator, string(f.project.OrganizationID), "snapshot")
		args := map[string]any{"project_id": f.project.ID, "work_item_id": f.issue.WorkItemID}
		result, err := workspaceOperatorCall(t, f.service, ctx, "get_work_item_conversation", args)
		if err != nil || !strings.Contains(string(result.Content), f.attempt) || !strings.Contains(string(result.Content), `"status":"unavailable"`) {
			t.Fatalf("missing binding diagnostic=%s %v", result.Content, err)
		}
		var count int
		if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM conversations WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3", f.project.OrganizationID, f.project.ID, f.issue.WorkItemID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("read created a substitute conversation: count=%d err=%v", count, err)
		}
		var bound workerBindResponse
		body := f.bindBody()
		body["thread_id"] = "old-resume-thread"
		response := f.bind(t, body)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &bound)
		f.record.ID = bound.ConversationID
		requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "turn_started", "thread_id": "actual-fresh-thread", "turn_id": "actual-turn"}), http.StatusAccepted)
		for range 5 {
			requireNativeStatus(t, f.turnEvents(t, map[string]any{"type": "delta", "provider_item_id": "large-message", "text": strings.Repeat("x", 60000)}), http.StatusAccepted)
		}
		result, err = workspaceOperatorCall(t, f.service, ctx, "get_work_item_conversation", args)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot struct {
			conversations.ConversationSnapshot
			HistoryOmitted bool `json:"history_omitted"`
		}
		if err := json.Unmarshal(workspaceOperatorData(t, result), &snapshot); err != nil {
			t.Fatal(err)
		}
		current := snapshot.Conversation.Execution
		if snapshot.Conversation.ID != bound.ConversationID || current.Status != conversation.ExecutionRunning || current.AttemptID == nil || *current.AttemptID != f.attempt || current.ThreadID == nil || *current.ThreadID != "actual-fresh-thread" || current.TurnID == nil || *current.TurnID != "actual-turn" || !current.Capabilities.Steer || !snapshot.HistoryOmitted || len(snapshot.Messages) != 0 || snapshot.Cursor == 0 {
			t.Fatalf("canonical snapshot lost execution or history evidence: %+v", snapshot)
		}
	})
}
