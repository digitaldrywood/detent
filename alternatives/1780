package conversations

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConversationAuthorizeRead(t *testing.T) {
	service := &ConversationService{}
	base := ConversationRecord{OrganizationID: "org_a", ProjectID: "prj_a", OwnerPrincipalID: "tok_owner", Visibility: conversation.VisibilityPrivate}
	tests := []struct {
		name    string
		scope   hubauth.Input
		record  ConversationRecord
		wantErr bool
	}{
		{name: "owner reads private", scope: hubauth.Input{Organization: "org_a", Project: "prj_a", Credential: hubauth.Credential{ID: "tok_owner"}}, record: base},
		{name: "other principal denied private", scope: hubauth.Input{Organization: "org_a", Project: "prj_a", Credential: hubauth.Credential{ID: "tok_other"}}, record: base, wantErr: true},
		{name: "other principal reads shared", scope: hubauth.Input{Organization: "org_a", Project: "prj_a", Credential: hubauth.Credential{ID: "tok_other"}}, record: func() ConversationRecord {
			record := base
			record.Visibility = conversation.VisibilityShared
			return record
		}()},
		{name: "cross project denied", scope: hubauth.Input{Organization: "org_a", Project: "prj_b", Credential: hubauth.Credential{ID: "tok_owner"}}, record: base, wantErr: true},
		{name: "cross organization denied", scope: hubauth.Input{Organization: "org_b", Project: "prj_a", Credential: hubauth.Credential{ID: "tok_owner"}}, record: base, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := service.AuthorizeRead(t.Context(), nil, test.scope, test.record)
			if test.wantErr {
				var failure *hubapi.Error
				if !errors.As(err, &failure) || failure.Status != http.StatusNotFound {
					t.Fatalf("expected opaque not_found, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestTranslateConversationError(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		code   string
		status int
	}{
		{name: "linked", err: &ErrConversationLinked{ExistingConversationID: "conv_x"}, code: "conversation_already_linked", status: http.StatusConflict},
		{name: "revision", err: &ErrConversationRevision{}, code: "revision_conflict", status: http.StatusConflict},
		{name: "typed missing", err: corestore.ErrNotFound, code: "not_found", status: http.StatusNotFound},
		{name: "stale", err: conversation.ErrStaleExecution, code: "stale_execution", status: http.StatusConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var failure *hubapi.Error
			if !errors.As(TranslateConversationError(test.err), &failure) {
				t.Fatalf("expected nativeError")
			}
			if failure.Code != test.code || failure.Status != test.status {
				t.Fatalf("got %s/%d, want %s/%d", failure.Code, failure.Status, test.code, test.status)
			}
		})
	}
	var linkedErr *hubapi.Error
	if !errors.As(ConversationAlreadyLinked("conv_x"), &linkedErr) || linkedErr.Details["existing_conversation_id"] != "conv_x" {
		t.Fatalf("expected existing conversation in details, got %v", linkedErr)
	}
}

func TestProjectConversationShape(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	record := ConversationRecord{
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
	message := ProjectMessage(ConversationMessageRecord{ID: "msg_1", Delivery: conversation.DeliverySaved})
	if message.AttemptID != nil || message.CommandKey != nil {
		t.Fatal("empty message identifiers must be null")
	}
}
