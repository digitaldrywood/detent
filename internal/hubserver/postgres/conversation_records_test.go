package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
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
				OrganizationID:        organization,
				ProjectID:             "prj_shared",
				ID:                    "conv_0123456789abcdef0123456789abcdef",
				CreationKey:           "create_example",
				CreationHash:          "hash_example",
				OwnerPrincipalID:      "owner",
				Origin:                "user",
				Title:                 string(organization),
				Visibility:            conversation.VisibilityPrivate,
				Status:                conversation.StatusActive,
				Revision:              1,
				ExecutionContinuation: true,
				Execution:             conversation.Execution{Status: conversation.ExecutionIdle, Owner: conversation.Owner{LeaseID: "lease_" + string(organization), FencingToken: 1}},
				Preferences:           conversation.DefaultPreferences(),
				CreatedAt:             now,
				UpdatedAt:             now,
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
		rollback := errors.New("rollback message insertion")
		err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateMessage(ctx, conversationstore.Message{OrganizationID: organization, ConversationID: "conv_0123456789abcdef0123456789abcdef", ID: "msg_abcdefabcdefabcdefabcdefabcdefab", Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "rolled back", Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman}, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				return err
			}
			return rollback
		})
		if !errors.Is(err, rollback) {
			t.Fatalf("rollback message error=%v", err)
		}
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			for _, lookup := range []struct {
				name string
				read func() (conversationstore.Conversation, error)
			}{
				{"project", func() (conversationstore.Conversation, error) { return r.Conversation(ctx, "prj_shared", "missing") }},
				{"id", func() (conversationstore.Conversation, error) { return r.ConversationByID(ctx, "missing") }},
				{"lease", func() (conversationstore.Conversation, error) { return r.ConversationByExecutionLease(ctx, "missing") }},
				{"work item", func() (conversationstore.Conversation, error) {
					return r.ConversationByWorkItem(ctx, "prj_shared", "missing")
				}},
				{"creation key", func() (conversationstore.Conversation, error) {
					return r.ConversationByCreationKey(ctx, "owner", "missing")
				}},
			} {
				if _, err := lookup.read(); !errors.Is(err, corestore.ErrNotFound) {
					t.Errorf("%s missing %s lookup error=%v, want not found", organization, lookup.name, err)
				}
			}
			contentID := contentIDs[organization]
			conversationID := "conv_0123456789abcdef0123456789abcdef"
			got, err := r.Conversation(ctx, "prj_shared", conversationID)
			if err != nil {
				return err
			}
			if got.OrganizationID != organization || got.Title != string(organization) || got.MessageCount != 1 || !got.ExecutionContinuation {
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

	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		for index, suppliedSequence := range []int64{0, 1000} {
			if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
				conversationID := "conv_0123456789abcdef0123456789abcdef"
				snapshot, err := w.LockConversation(ctx, "prj_shared", conversationID)
				if err != nil {
					return err
				}
				first, err := w.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: organization, ConversationID: conversationID, Event: conversation.Event{Type: conversation.EventExecutionUpdated, Data: json.RawMessage(`{}`), CreatedAt: now}})
				if err != nil {
					return err
				}
				wantSequence := int64(2 + index*2)
				if first.Seq != wantSequence {
					t.Fatalf("first event sequence = %d, want %d", first.Seq, wantSequence)
				}
				snapshot.EventSequence = suppliedSequence
				snapshot.ExecutionContinuation = index == 1
				snapshot.Title = string(organization) + " updated"
				updated, err := w.UpdateConversation(ctx, snapshot, snapshot.Revision)
				if err != nil {
					return err
				}
				if updated.EventSequence != wantSequence || updated.Title != snapshot.Title || updated.Revision != snapshot.Revision+1 || updated.ExecutionContinuation != (index == 1) {
					t.Fatalf("conversation update changed event ownership: %+v, want event sequence %d", updated, wantSequence)
				}
				second, err := w.AppendConversationEvent(ctx, conversationstore.Event{OrganizationID: organization, ConversationID: conversationID, Event: conversation.Event{Type: conversation.EventCommandReceipt, Data: json.RawMessage(`{}`), CreatedAt: now}})
				if err != nil {
					return err
				}
				stored, err := w.Conversation(ctx, "prj_shared", conversationID)
				if err != nil {
					return err
				}
				if second.Seq != wantSequence+1 || stored.EventSequence != wantSequence+1 || stored.Title != snapshot.Title || stored.ExecutionContinuation != (index == 1) {
					t.Fatalf("subsequent event lost sequence: event=%+v conversation=%+v", second, stored)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			events, err := r.ConversationEvents(ctx, "conv_0123456789abcdef0123456789abcdef", 0, 20)
			if err != nil {
				return err
			}
			if len(events.Items) != 5 {
				t.Fatalf("conversation history = %+v, want five events", events.Items)
			}
			for index, event := range events.Items {
				if event.OrganizationID != organization || event.Seq != int64(index+1) {
					t.Fatalf("conversation history escaped its tenant or order: %+v", event)
				}
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

func TestConversationMessageCountBackfillsAllTenants(t *testing.T) {
	const version = 20261108204849
	admin, _ := migrationDatabaseAtVersion(t, version-1)
	seedCore(t, admin)
	for index, organization := range []string{"org_first", "org_second"} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO api_tokens(organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) VALUES($1,'count_owner','Count owner',$1,$1,'operator',true,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, organization); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status) VALUES($1,'count_conversation','prj_shared','count_owner','user','private','active')`, organization); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_messages(organization_id,id,conversation_id,seq,role,kind,delivery) SELECT $1,'count_message_'||n,'count_conversation',n,'user','text','saved' FROM generate_series(1,$2::int) n`, organization, index+1); err != nil {
			t.Fatal(err)
		}
	}
	admin.SetMaxOpenConns(1)
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `SET detent.organization_id=''`); err != nil {
		t.Fatal(err)
	}
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), version); err != nil {
		t.Fatal(err)
	}
	var visible int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM conversations`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("unscoped migration role sees conversations=%d error=%v", visible, err)
	}
	if _, err := admin.ExecContext(t.Context(), `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	for index, organization := range []string{"org_first", "org_second"} {
		var count int
		if err := admin.QueryRowContext(t.Context(), `SELECT message_count FROM conversations WHERE organization_id=$1 AND id='count_conversation'`, organization).Scan(&count); err != nil || count != index+1 {
			t.Errorf("organization=%s migrated count=%d want=%d error=%v", organization, count, index+1, err)
		}
	}
}

type pendingConversationQueryTrace struct {
	statement string
	args      []any
}

func (trace *pendingConversationQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "conversation_messages") && strings.Contains(data.SQL, "conversation_commands") {
		trace.statement, trace.args = data.SQL, append([]any(nil), data.Args...)
	}
	return ctx
}

func (*pendingConversationQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestPendingConversationsBoundsSparseTenantHistory(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	base := rolePool(t, admin, "detent_runtime_v2", "")
	config := base.Config()
	base.Close()
	trace := &pendingConversationQueryTrace{}
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, smallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	const history, limit = 4096, 32
	id := func(number int) string { return fmt.Sprintf("conv_%032x", number) }
	organizations := []tracker.OrganizationID{"org_first", "org_second"}
	for index, organization := range organizations {
		seedTenantAccess(t, repository, organization)
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversations (organization_id,id,project_id,owner_principal_id,origin,visibility,status,execution_status,execution_settled,deleted_at) SELECT $1,'conv_'||lpad(to_hex(n),32,'0'),'prj_shared','first','user','private','active','idle',true,CASE WHEN n%3=0 AND n<=$2 THEN CURRENT_TIMESTAMP END FROM generate_series(1,$2+2) n`, organization, history); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_messages (organization_id,id,conversation_id,seq,role,kind,delivery,actor_kind,actor_principal_id) SELECT $1,'msg_'||n,'conv_'||lpad(to_hex(n),32,'0'),1,'user','text','completed','human','first' FROM generate_series(1,$2) n`, organization, history); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_messages (organization_id,id,conversation_id,seq,role,kind,delivery,actor_kind,actor_principal_id) SELECT $1,'dense_completed_'||n,$3,n+1,'user','text','completed','human','first' FROM generate_series(1,$2) n`, organization, history, id(1)); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_messages (organization_id,id,conversation_id,seq,role,kind,delivery,actor_kind,actor_principal_id) SELECT $1,'dense_irrelevant_'||n,$3,$2+n+1,CASE WHEN n%3=0 THEN 'user' ELSE 'assistant' END,'text',CASE n%3 WHEN 0 THEN 'sending' WHEN 1 THEN 'queued' ELSE 'responding' END,'human','first' FROM generate_series(1,$2) n`, organization, history, id(1)); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_commands (organization_id,conversation_id,key,kind,request_hash,receipt_json,receipt_status,target_turn_id) SELECT $1,'conv_'||lpad(to_hex(n),32,'0'),'completed','cancel','completed_'||n,$3,'delivered','turn_'||n FROM generate_series(1,$2) n`, organization, history, "{}"); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			now := historyAt()
			_, _, err := w.ReserveCommand(t.Context(), conversationstore.Command{OrganizationID: organization, ConversationID: id(history + index + 1), Key: "cancel", Kind: conversation.CommandCancel, TargetTurnID: "turn_" + string(organization), RequestHash: "cancel_" + string(organization), Receipt: conversation.Receipt{Key: "cancel", Kind: conversation.CommandCancel, Status: conversation.DeliverySaved, UpdatedAt: now}, CreatedAt: now, UpdatedAt: now})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"conversations", "conversation_messages", "conversation_commands"} {
		if _, err := admin.ExecContext(t.Context(), "ANALYZE "+pgx.Identifier{table}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	for index, organization := range organizations {
		t.Run(string(organization), func(t *testing.T) {
			var first, second, last conversationstore.PendingConversationPage
			if err := repository.ReadTenant(t.Context(), organization, corestore.Background, func(r tenantstore.Reader) error {
				var err error
				first, err = r.PendingConversations(t.Context(), "", limit)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(first.Items) != 0 || first.Next != id(limit) {
				t.Errorf("sparse first examined page=%+v, want empty items and boundary %s", first, id(limit))
			}
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id',$1,true)`, organization); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+trace.statement, trace.args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var plans []struct{ Plan searchPlanNode }
			if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatalf("decode pending scan plan: %v", err)
			}
			work := make(map[string]float64)
			var visit func(searchPlanNode)
			visit = func(node searchPlanNode) {
				work[node.Relation] += (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
				for _, child := range node.Plans {
					visit(child)
				}
			}
			visit(plans[0].Plan)
			t.Logf("history=%d dense_completed=%d irrelevant_active=%d limit=%d natural row work=%v plan=%s", history, history, history, limit, work, raw)
			if work["conversations"] > limit+1 || work["conversation_messages"]+work["conversation_commands"] > 2*(limit+1) {
				t.Errorf("pending scan exceeded examined-row budget: %v", work)
			}
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), organization, corestore.Background, func(r tenantstore.Reader) error {
				var err error
				second, err = r.PendingConversations(t.Context(), id(limit), limit)
				if err != nil {
					return err
				}
				last, err = r.PendingConversations(t.Context(), id(history), limit)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(second.Items) != 0 || second.Next != id(2*limit) {
				t.Errorf("sparse second examined page=%+v, want boundary %s", second, id(2*limit))
			}
			if len(last.Items) != 1 || last.Items[0].ID != id(history+index+1) || last.Next != "" {
				t.Errorf("tenant pending tail=%+v", last)
			}
		})
	}
}
