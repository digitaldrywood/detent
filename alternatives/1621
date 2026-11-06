package hubserver

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConversationReferenceReadsUseTenantState(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ids := map[tracker.OrganizationID][]string{}
	for _, org := range []tracker.OrganizationID{"org_reference_first", "org_reference_second"} {
		project := tracker.ProjectID("prj_" + string(org))
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "owner", Name: "Example", Hash: "hash_" + string(org), Fingerprint: "fingerprint_" + string(org), Scope: "operator", Kind: "personal", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example", Slug: "example", Profile: "native", CreatedAt: now}, Settings: projectstore.Settings{OrganizationID: org, ProjectID: project}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			for i := 0; i < 2; i++ {
				id := conversation.NewConversationID()
				ids[org] = append(ids[org], id)
				if _, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: org, ProjectID: project, ID: id, OwnerPrincipalID: "owner", Origin: conversationOriginUser, Title: "Example", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if _, err := w.RecordConversationUsage(t.Context(), billingstore.ConversationUsage{OrganizationID: org, ProjectID: project, ConversationID: id, TurnID: "turn_" + id, Provider: "example", Model: "example", OccurredAt: now, Input: 1, Output: 1, Outcome: "completed"}); err != nil {
					return err
				}
				message, err := w.CreateMessage(t.Context(), conversationstore.Message{OrganizationID: org, ConversationID: id, ID: conversation.NewMessageID(), Role: conversation.RoleUser, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: "owner"}, Kind: conversation.MessageText, Text: "Example", Delivery: conversation.DeliverySaved, CreatedAt: now, UpdatedAt: now})
				if err != nil {
					return err
				}
				if err := w.ReplaceMessageReferences(t.Context(), id, message.ID, []conversationstore.MessageReference{{OrganizationID: org, ConversationID: id, MessageID: message.ID, Kind: conversation.ReferenceIssue, TargetID: "wi_example", Label: "Example"}}, now.Add(time.Duration(i)*time.Second)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_reference_first", "org_reference_second"} {
		err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			owner, ok := r.(conversationstore.WorkItemReferenceReader)
			if !ok {
				return fmt.Errorf("typed work item reference owner missing")
			}
			rows, err := owner.WorkItemReferences(t.Context(), tracker.ProjectID("prj_"+string(org)), "wi_example", 1)
			if err != nil {
				return err
			}
			if len(rows) != 1 || rows[0].ConversationID != ids[org][1] || rows[0].OwnerPrincipalID != "owner" || rows[0].Visibility != conversation.VisibilityPrivate {
				return fmt.Errorf("unexpected references: %+v", rows)
			}
			rows, err = owner.WorkItemReferences(t.Context(), "prj_other", "wi_example", 1)
			if err == nil && len(rows) != 0 {
				return fmt.Errorf("cross-project references: %+v", rows)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_reference_first", "org_reference_second"} {
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			owner, ok := w.(conversationstore.RemovalWriter)
			if !ok {
				return fmt.Errorf("typed removal owner missing")
			}
			id := ids[org][1]
			if err := owner.DeleteConversation(t.Context(), tracker.ProjectID("prj_"+string(org)), id, now); err != nil {
				return err
			}
			if _, err := w.ConversationByID(t.Context(), id); !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("removed conversation remains readable: %v", err)
			}
			usage, err := w.ConversationUsage(t.Context(), "turn_"+id)
			if err != nil {
				return err
			}
			if usage.Input != 1 || usage.Output != 1 {
				return fmt.Errorf("usage lost after removal: %+v", usage)
			}
			if _, err := w.ConversationByID(t.Context(), ids[org][0]); err != nil {
				return err
			}
			rows, err := w.(conversationstore.WorkItemReferenceReader).WorkItemReferences(t.Context(), tracker.ProjectID("prj_"+string(org)), "wi_example", 2)
			if err == nil && (len(rows) != 1 || rows[0].ConversationID != ids[org][0]) {
				return fmt.Errorf("references after removal: %+v", rows)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}

}
