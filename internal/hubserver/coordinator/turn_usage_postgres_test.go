package coordinator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/credits"
	"github.com/digitaldrywood/detent/internal/hubserver/eventbroker"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type usageCompletionWriter struct {
	tenantstore.Writer
	fail bool
}

func (w usageCompletionWriter) RecordConversationUsage(ctx context.Context, row billingstore.ConversationUsage) (bool, error) {
	if w.fail {
		return false, errors.New("example usage write failure")
	}
	return w.Writer.RecordConversationUsage(ctx, row)
}

func (w usageCompletionWriter) UpdateMessage(ctx context.Context, row conversationstore.Message, revision int64) (conversationstore.Message, error) {
	if row.Role == conversation.RoleAssistant && row.Delivery == conversation.DeliveryCompleted {
		if _, err := w.ConversationUsage(ctx, row.ID); err != nil {
			return conversationstore.Message{}, fmt.Errorf("completion must include usage: %w", err)
		}
	}
	return w.Writer.UpdateMessage(ctx, row, revision)
}

type postgresConversationObjects struct{ *hubtest.Details }

func (o postgresConversationObjects) PublishContent(ctx context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	return o.StoreDetail(ctx, org, raw)
}

func (o postgresConversationObjects) ReadContent(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	return o.ReadDetail(ctx, org, ref)
}

type usageCompletionCoordinator struct{}

func (usageCompletionCoordinator) Available() bool             { return false }
func (usageCompletionCoordinator) Cancel(string) bool          { return false }
func (usageCompletionCoordinator) Hold(string) (func(), error) { return func() {}, nil }
func (usageCompletionCoordinator) Wake(string)                 {}
func (usageCompletionCoordinator) Stop()                       {}

func TestConversationUsageCommitsWithCompletion(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	clock := func() time.Time { return now }
	for _, test := range []struct {
		name string
		fail bool
	}{{"completion sees usage", false}, {"usage failure rolls back completion", true}} {
		t.Run(test.name, func(t *testing.T) {
			org := hubtest.Organization(t, repository, hubtest.OrganizationOptions{Now: now}).OrganizationID
			project := hubtest.Project(t, repository, org, hubtest.ProjectOptions{}).ID
			owner := hubtest.Credential(t, repository, org, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project}, Now: now}).ID
			id := conversation.NewConversationID()
			var assistant conversationstore.Message
			err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateConversation(t.Context(), conversationstore.Conversation{
					OrganizationID: org, ProjectID: project, ID: id, OwnerPrincipalID: owner,
					Origin: conversations.ConversationOriginUser, Title: "Usage", Visibility: conversation.VisibilityPrivate,
					Status: conversation.StatusActive, Revision: 1, Preferences: conversation.DefaultPreferences(),
					Execution: conversation.Execution{Status: conversation.ExecutionRunning}, CreatedAt: now, UpdatedAt: now,
				}); err != nil {
					return err
				}
				var err error
				assistant, err = w.CreateMessage(t.Context(), conversationstore.Message{
					OrganizationID: org, ConversationID: id, ID: conversation.NewMessageID(), Role: conversation.RoleAssistant,
					Kind: conversation.MessageText, Actor: conversation.Actor{Kind: conversation.ActorCoordinator},
					Delivery: conversation.DeliveryResponding, CreatedAt: now, UpdatedAt: now,
				})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			details := hubtest.NewDetails()
			service := &conversations.ConversationService{
				Store:  &conversations.ConversationStore{Contents: postgresConversationObjects{details}},
				Broker: eventbroker.New("turn-usage"), Coordinator: usageCompletionCoordinator{},
			}
			service.Server = conversations.NewServer(conversations.Deps{
				Config:        func() conversations.Config { return conversations.Config{TenantRepository: repository} },
				Organization:  func() string { return string(org) },
				Conversations: func() *conversations.ConversationService { return service },
			})
			billing := credits.New(credits.Deps{
				Repository: repository, Now: clock, Details: details,
				ValidateDetail: func(ctx context.Context, store attachment.DetailStore, org tracker.OrganizationID, ref attachment.ArchiveReference, expected []byte) error {
					actual, err := store.ReadDetail(ctx, string(org), ref)
					if err != nil {
						return err
					}
					if !bytes.Equal(actual, expected) {
						return errors.New("stored usage detail differs")
					}
					return nil
				},
			})
			writer := TurnWriteDeps{
				Service: service, Now: clock,
				WriteTenant: func(ctx context.Context, org tracker.OrganizationID, write func(tenantstore.Writer) error) error {
					return repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
						return write(usageCompletionWriter{Writer: w, fail: test.fail})
					})
				},
			}
			finish := TurnFinishDeps{
				Write: writer.Write, PrepareContents: service.PrepareConversationContents,
				PrepareUsage: func(ctx context.Context, usage conversations.ConversationUsage) (func(context.Context, tenantstore.Writer) error, error) {
					prepared, err := billing.PrepareUsage(ctx, credits.Usage(usage))
					if err != nil {
						return nil, err
					}
					return func(ctx context.Context, w tenantstore.Writer) error { return billing.WriteUsage(ctx, w, prepared) }, nil
				},
			}
			usage := conversations.ConversationUsage{
				OrganizationID: org, ProjectID: project, ConversationID: id, TurnID: assistant.ID, Provider: "codex", Model: "example-model",
				Tokens: runner.AgentTokenCounts{InputTokens: 10, OutputTokens: 5}, Outcome: conversation.DeliveryCompleted, OccurredAt: now,
			}
			record := conversations.ConversationMessageFromStore(assistant)
			err = finish.Finish(t.Context(), id, "", &record, nil, false, conversation.DeliveryCompleted, nil, &usage)
			wantDelivery := conversation.DeliveryCompleted
			if test.fail {
				wantDelivery = conversation.DeliveryResponding
				if err == nil {
					t.Fatal("completion succeeded after usage write failed")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				message, err := r.Message(t.Context(), id, assistant.ID)
				if err != nil {
					return err
				}
				if message.Delivery != wantDelivery {
					return fmt.Errorf("assistant delivery = %s, want %s", message.Delivery, wantDelivery)
				}
				row, err := r.ConversationUsage(t.Context(), assistant.ID)
				if test.fail && errors.Is(err, corestore.ErrNotFound) {
					return nil
				}
				if err != nil {
					return err
				}
				if test.fail || row.ConversationID != id || row.Input != 10 || row.Output != 5 {
					return fmt.Errorf("unexpected durable usage: %+v", row)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
