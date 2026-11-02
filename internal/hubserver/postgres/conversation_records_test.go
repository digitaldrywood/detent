package postgres_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConversationRecordsAreTenantScopedAndTransactional(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	repository, _ := repositoryFor(t, admin, smallPool())
	ctx := t.Context()
	now := time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC)
	contentIDs := make(map[tracker.OrganizationID]string)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, organization)
		ref, _, err := attachment.PrepareArchive(string(organization), 1, bytes.NewReader([]byte(`{"example":true}`)))
		if err != nil {
			t.Fatal(err)
		}
		contentID := "att_" + artifact.Digest([]byte(string(organization) + "\n" + ref.Key))[:32]
		contentIDs[organization] = contentID
		if _, err := admin.ExecContext(ctx, `INSERT INTO api_tokens (organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) VALUES ($1,'owner','owner','hash','fingerprint','operator',true,$2,$2)`, organization, now); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.PutConversationContent(ctx, conversationstore.ContentObject{OrganizationID: organization, ID: contentID, Reference: ref, CreatedAt: now}); err != nil {
				return err
			}
			_, err := w.CreateConversation(ctx, conversationstore.Conversation{
				OrganizationID:   organization,
				ProjectID:        "prj_shared",
				ID:               "conv_0123456789abcdef0123456789abcdef",
				CreationKey:      "create_example",
				CreationHash:     "hash_example",
				OwnerPrincipalID: "owner",
				Origin:           "user",
				Title:            string(organization),
				Visibility:       conversation.VisibilityPrivate,
				Status:           conversation.StatusActive,
				Revision:         1,
				Execution:        conversation.Execution{Status: conversation.ExecutionIdle, Owner: conversation.Owner{LeaseID: "lease_" + string(organization), FencingToken: 1}},
				Preferences:      conversation.DefaultPreferences(),
				CreatedAt:        now,
				UpdatedAt:        now,
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			conversationID := "conv_0123456789abcdef0123456789abcdef"
			price := billingstore.ConversationPrice{OrganizationID: organization, ID: 1, Provider: "example-provider", Model: "example-model", EffectiveAt: now.Add(-time.Hour), Input: .1, CachedInput: .01, Output: .5}
			if err := w.CreateConversationPrice(ctx, price); err != nil {
				return err
			}
			if err := w.CreateConversationPrice(ctx, price); err != nil {
				return err
			}
			for _, turn := range []string{"turn-first", "turn-second"} {
				usage := billingstore.ConversationUsage{OrganizationID: organization, ProjectID: "prj_shared", ConversationID: conversationID, TurnID: turn, Provider: price.Provider, Model: price.Model, OccurredAt: now, Input: 100, CachedInput: 20, Output: 30, Outcome: "delivered", PriceID: new(int64(1)), CostUSD: new(.0000232), CacheSavingsUSD: new(.0000018)}
				created, err := w.RecordConversationUsage(ctx, usage)
				if err != nil {
					return err
				}
				if !created {
					t.Fatal("first usage insert replayed")
				}
				created, err = w.RecordConversationUsage(ctx, usage)
				if err != nil {
					return err
				}
				if created {
					t.Fatal("usage replay inserted twice")
				}
				usage.Input++
				if _, err := w.RecordConversationUsage(ctx, usage); !errors.Is(err, mutation.ErrConflict) {
					t.Fatalf("changed usage replay error=%v", err)
				}
				stored, err := w.ConversationUsage(ctx, turn)
				if err != nil {
					return err
				}
				if stored.OrganizationID != organization || stored.Input != 100 || stored.TurnID != turn {
					t.Fatalf("stored usage=%+v", stored)
				}
			}

			message, err := w.CreateMessage(ctx, conversationstore.Message{
				OrganizationID: organization,
				ConversationID: conversationID,
				ID:             "msg_0123456789abcdef0123456789abcdef",
				Role:           conversation.RoleUser,
				Kind:           conversation.MessageText,
				Text:           string(organization),
				Data:           json.RawMessage(`{"text":"example"}`),
				Delivery:       conversation.DeliverySaved,
				Actor:          conversation.Actor{Kind: conversation.ActorHuman},
				CreatedAt:      now,
				UpdatedAt:      now,
			})
			if err != nil {
				return err
			}
			if message.Sequence != 1 || message.Revision != 1 {
				t.Fatalf("message sequence/revision = %d/%d", message.Sequence, message.Revision)
			}
			receipt := conversation.Receipt{Key: "command_example", Kind: conversation.CommandMessage, Status: conversation.DeliverySaved, MessageID: message.ID, UpdatedAt: now}
			command := conversationstore.Command{OrganizationID: organization, ConversationID: conversationID, Key: receipt.Key, Kind: receipt.Kind, RequestHash: "hash_example", Receipt: receipt, CreatedAt: now, UpdatedAt: now}
			if _, created, err := w.ReserveCommand(ctx, command); err != nil || !created {
				return errors.Join(err, errors.New("first command reservation was not created"))
			}
			if _, created, err := w.ReserveCommand(ctx, command); err != nil || created {
				return errors.Join(err, errors.New("duplicate command reservation was not replayed"))
			}
			if _, _, err := w.ReserveCommand(ctx, conversationstore.Command{OrganizationID: organization, ConversationID: conversationID, Key: receipt.Key, Kind: receipt.Kind, RequestHash: "different_hash", Receipt: receipt, CreatedAt: now, UpdatedAt: now}); err == nil {
				return errors.New("conflicting duplicate command was accepted")
			}
			_, err = w.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: organization, ConversationID: conversationID, Event: conversation.Event{Type: conversation.EventMessageAccepted, Data: json.RawMessage(`{"message_id":"msg_0123456789abcdef0123456789abcdef"}`), CreatedAt: now}})
			if err != nil {
				return err
			}
			return w.RecordTurnBatch(ctx, "attempt_"+string(organization), "batch_"+string(organization), 1, now)
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			contentID := contentIDs[organization]
			conversationID := "conv_0123456789abcdef0123456789abcdef"
			got, err := r.Conversation(ctx, "prj_shared", conversationID)
			if err != nil {
				return err
			}
			if got.OrganizationID != organization || got.Title != string(organization) || got.MessageCount != 1 {
				t.Fatalf("conversation escaped tenant scope: %+v", got)
			}
			created, err := r.ConversationByCreationKey(ctx, "owner", "create_example")
			if err != nil || created.ID != conversationID || created.CreationHash != "hash_example" {
				return errors.Join(err, errors.New("conversation creation receipt was not preserved"))
			}
			byLease, err := r.ConversationByExecutionLease(ctx, "lease_"+string(organization))
			if err != nil || byLease.ID != conversationID {
				return errors.Join(err, errors.New("conversation execution lease lookup was not preserved"))
			}
			content, err := r.ConversationContent(ctx, contentID)
			if err != nil || content.ID != contentID || content.Reference.Key == "" {
				return errors.Join(err, errors.New("conversation content reference was not preserved"))
			}
			messages, err := r.Messages(ctx, conversationID, 0, 20)
			if err != nil {
				return err
			}
			if len(messages.Items) != 1 || messages.Items[0].OrganizationID != organization || messages.Items[0].Text != string(organization) {
				t.Fatalf("message escaped tenant scope: %+v", messages.Items)
			}
			filtered, err := r.FilteredMessages(ctx, conversationstore.MessageFilter{
				ConversationID: conversationID, Role: conversation.RoleUser,
				Deliveries: []conversation.Delivery{conversation.DeliverySaved}, Limit: 1,
			})
			if err != nil || len(filtered.Items) != 1 || filtered.Items[0].Text != string(organization) {
				return errors.Join(err, errors.New("filtered message query did not preserve tenant-scoped message"))
			}
			command, found, err := r.Command(ctx, conversationID, "command_example")
			if err != nil {
				return err
			}
			if !found || command.Receipt.MessageID != messages.Items[0].ID {
				t.Fatalf("command receipt = %+v, found=%v", command, found)
			}
			events, err := r.ConversationEvents(ctx, conversationID, 0, 20)
			if err != nil {
				return err
			}
			if len(events.Items) != 1 || events.Items[0].Seq != 1 || events.Items[0].Type != conversation.EventMessageAccepted {
				t.Fatalf("events = %+v", events.Items)
			}
			sequence, found, err := r.TurnBatch(ctx, "attempt_"+string(organization), "batch_"+string(organization))
			if err != nil || !found || sequence != 1 {
				return errors.Join(err, errors.New("turn batch replay was not tenant-scoped"))
			}
			otherOrganization := tracker.OrganizationID("org_first")
			if organization == otherOrganization {
				otherOrganization = "org_second"
			}
			if _, found, err := r.TurnBatch(ctx, "attempt_"+string(otherOrganization), "batch_"+string(otherOrganization)); err != nil || found {
				return errors.Join(err, errors.New("turn batch escaped tenant scope"))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	rollback := errors.New("reject after message write")
	rollbackRef, _, err := attachment.PrepareArchive("org_first", 1, bytes.NewReader([]byte(`{"rollback":true}`)))
	if err != nil {
		t.Fatal(err)
	}
	rollbackContentID := "att_" + artifact.Digest([]byte("org_first\n" + rollbackRef.Key))[:32]
	err = repository.WriteTenant(ctx, "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.PutConversationContent(ctx, conversationstore.ContentObject{OrganizationID: "org_first", ID: rollbackContentID, Reference: rollbackRef, CreatedAt: now}); err != nil {
			return err
		}
		_, err := w.CreateMessage(ctx, conversationstore.Message{OrganizationID: "org_first", ConversationID: "conv_0123456789abcdef0123456789abcdef", ID: "msg_abcdefabcdefabcdefabcdefabcdefab", Role: conversation.RoleUser, Kind: conversation.MessageText, Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman}, CreatedAt: now, UpdatedAt: now})
		if err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("write error = %v", err)
	}
	if err := repository.ReadTenant(ctx, "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		if _, err := r.ConversationContent(ctx, rollbackContentID); !errors.Is(err, corestore.ErrNotFound) {
			return errors.New("content reference remained after transaction rollback")
		}
		messages, err := r.Messages(ctx, "conv_0123456789abcdef0123456789abcdef", 1, 20)
		if err != nil {
			return err
		}
		if len(messages.Items) != 0 {
			t.Fatalf("rolled back message remains: %+v", messages.Items)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
