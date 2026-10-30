package hubconversation

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectConversationShape(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	record := Record{
		ID: "conv_1", OrganizationID: tracker.OrganizationID("org_a"), ProjectID: tracker.ProjectID("prj_a"),
		OwnerPrincipalID: "tok_owner", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive,
		Execution: conversation.Execution{Owner: conversation.Owner{LeaseID: "lease_secret", FencingToken: 7, AttemptID: "attempt_1"}, UpdatedAt: now},
		Revision:  3, CreatedAt: now, UpdatedAt: now,
	}
	encoded, err := json.Marshal(ProjectConversation(record))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["work_item_id"] != nil {
		t.Fatalf("unlinked conversation must report null work_item_id, got %v", decoded["work_item_id"])
	}
	execution := decoded["execution"].(map[string]any)
	if execution["status"] != "idle" {
		t.Fatalf("empty execution status must project as idle, got %v", execution["status"])
	}
	if _, leaked := execution["lease_id"]; leaked {
		t.Fatal("lease and fencing token must not be exposed on the public resource")
	}
	if execution["attempt_id"] != "attempt_1" {
		t.Fatalf("attempt id missing: %v", execution)
	}
	message := ProjectMessage(MessageRecord{ID: "msg_1", Delivery: conversation.DeliverySaved})
	if message.AttemptID != nil || message.CommandKey != nil {
		t.Fatal("empty message identifiers must be null")
	}
}
