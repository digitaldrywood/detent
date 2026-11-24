package conversations

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventbroker"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type lifecycleDetailStore struct{ *hubtest.Details }

func (s lifecycleDetailStore) PublishContent(ctx context.Context, org string, value []byte) (attachment.ArchiveReference, error) {
	return s.StoreDetail(ctx, org, value)
}

func (s lifecycleDetailStore) ReadContent(ctx context.Context, org string, ref attachment.ArchiveReference) ([]byte, error) {
	return s.ReadDetail(ctx, org, ref)
}

type postgresConversationLifecycle struct {
	repository tenantstore.Repository
	project    tracker.NativeProject
	owner      string
	now        time.Time
}

func newPostgresConversationLifecycle(t *testing.T, repository tenantstore.Repository) postgresConversationLifecycle {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	org := hubtest.Organization(t, repository, hubtest.OrganizationOptions{Now: now}).OrganizationID
	project := hubtest.Project(t, repository, org, hubtest.ProjectOptions{})
	owner := hubtest.Credential(t, repository, org, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.ID}, Now: now}).ID
	return postgresConversationLifecycle{repository: repository, project: project, owner: owner, now: now}
}

func (f postgresConversationLifecycle) service(logger *slog.Logger, coordinator ConversationCoordinator, controls ConversationControlRouter) *ConversationService {
	service := &ConversationService{
		Store: &ConversationStore{Contents: lifecycleDetailStore{hubtest.NewDetails()}}, Broker: eventbroker.New("lifecycle"),
		Config: ConversationConfig{Enabled: true}.Normalized(), Logger: logger, Coordinator: coordinator, Controls: controls,
		Settle: make(chan struct{}), Settled: make(chan struct{}),
	}
	service.Server = NewServer(Deps{
		Config:        func() Config { return Config{TenantRepository: f.repository, Now: func() time.Time { return f.now }} },
		Organization:  func() string { return string(f.project.OrganizationID) },
		Conversations: func() *ConversationService { return service },
	})
	return service
}

func (f postgresConversationLifecycle) write(t *testing.T, fn func(tenantstore.Writer) error) {
	t.Helper()
	if err := f.repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, fn); err != nil {
		t.Fatal(err)
	}
}

func (f postgresConversationLifecycle) read(t *testing.T, fn func(tenantstore.Reader) error) {
	t.Helper()
	if err := f.repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Background, fn); err != nil {
		t.Fatal(err)
	}
}

func (f postgresConversationLifecycle) conversation(title string, at time.Time) conversationstore.Conversation {
	return conversationstore.Conversation{
		OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ID: conversation.NewConversationID(), OwnerPrincipalID: f.owner,
		Origin: ConversationOriginUser, Title: title, Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive,
		Revision: 1, Execution: conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: at},
		Preferences: conversation.DefaultPreferences(), CreatedAt: at, UpdatedAt: at,
	}
}

func (f postgresConversationLifecycle) message(id, text string, role conversation.Role, delivery conversation.Delivery, attempt string) conversationstore.Message {
	kind := conversation.ActorRunner
	if role == conversation.RoleUser {
		kind = conversation.ActorHuman
	}
	return ConversationMessageToStore(f.project.OrganizationID, ConversationMessageRecord{
		ID: conversation.NewMessageID(), ConversationID: id, Role: role, Kind: conversation.MessageText,
		Text: text, Delivery: delivery, AttemptID: attempt, Actor: conversation.Actor{Kind: kind, PrincipalID: f.owner},
		CreatedAt: f.now, UpdatedAt: f.now,
	})
}

func (f postgresConversationLifecycle) stored(t *testing.T, id string) conversationstore.Conversation {
	t.Helper()
	var row conversationstore.Conversation
	f.read(t, func(r tenantstore.Reader) error {
		var err error
		row, err = r.Conversation(t.Context(), f.project.ID, id)
		return err
	})
	return row
}

func TestConversationPostgresLifecycleUsesTenantStateWithoutSQLite(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	t.Run("restart and stale execution reconcile durable work", func(t *testing.T) {
		f := newPostgresConversationLifecycle(t, repository)
		var logs bytes.Buffer
		controls := &pendingWaker{}
		service := f.service(slog.New(slog.NewJSONHandler(&logs, nil)), &pendingWaker{}, controls)
		const private = "Private interrupted conversation text sentinel"
		const itemID tracker.NativeWorkItemID = "wi_lifecycle"
		org := f.project.OrganizationID
		restart := f.conversation(private, f.now)
		restart.Execution = conversation.Execution{Status: conversation.ExecutionRunning, Owner: conversation.Owner{AttemptID: "attempt_restart", RunID: "run_restart", LeaseID: "lease_restart", FencingToken: 1}, Capabilities: conversation.Capabilities{Steer: true}, UpdatedAt: f.now}
		stale := f.conversation("Expired worker", f.now)
		stale.Origin, stale.Visibility, stale.WorkItemID, stale.LinkedAt = ConversationOriginWorker, conversation.VisibilityShared, string(itemID), &f.now
		stale.Execution = conversation.Execution{Status: conversation.ExecutionRunning, Owner: conversation.Owner{AttemptID: "attempt_missing", RunID: "run_missing", LeaseID: "lease_missing", FencingToken: 1}, UpdatedAt: f.now}
		idle := f.conversation("Idle conversation", f.now.Add(-2*service.Config.SettleWindow))
		var restartMessages, staleMessages, staleQuestions []string
		restartQuestion := conversation.NewQuestionID()
		f.write(t, func(w tenantstore.Writer) error {
			ctx := t.Context()
			if _, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: org, ProjectID: f.project.ID, NativeID: itemID, Number: 1, Title: "Lifecycle item", ActorKind: new("human"), ActorPrincipalID: &f.owner, WorkflowStateName: new("Todo"), CreatedAt: f.now}); err != nil {
				return err
			}
			for _, row := range []conversationstore.Conversation{restart, stale, idle} {
				if _, err := w.CreateConversation(ctx, row); err != nil {
					return err
				}
			}
			addQuestion := func(row conversationstore.Conversation, id string) error {
				return w.UpsertQuestion(ctx, conversationstore.Question{OrganizationID: org, ConversationID: row.ID, Question: conversation.Question{ID: id, Status: conversation.QuestionPending, Owner: row.Execution.Owner, Prompts: []conversation.Prompt{{ID: "prompt", Question: "Example question?"}}, CreatedAt: f.now, UpdatedAt: f.now}})
			}
			if err := addQuestion(restart, restartQuestion); err != nil {
				return err
			}
			if _, err := w.CreateMessage(ctx, f.message(stale.ID, "Queued control", conversation.RoleUser, conversation.DeliveryQueued, "")); err != nil {
				return err
			}
			for range ConversationLifecycleBatchSize + 1 {
				message := f.message(restart.ID, private, conversation.RoleAssistant, conversation.DeliverySending, "")
				if _, err := w.CreateMessage(ctx, message); err != nil {
					return err
				}
				restartMessages = append(restartMessages, message.ID)
				message = f.message(stale.ID, "Interrupted response", conversation.RoleAssistant, conversation.DeliveryResponding, stale.Execution.Owner.AttemptID)
				if _, err := w.CreateMessage(ctx, message); err != nil {
					return err
				}
				staleMessages = append(staleMessages, message.ID)
				questionID := conversation.NewQuestionID()
				if err := addQuestion(stale, questionID); err != nil {
					return err
				}
				staleQuestions = append(staleQuestions, questionID)
			}
			_, _, err := w.ReserveCommand(ctx, conversationstore.Command{OrganizationID: org, ConversationID: restart.ID, Key: "receipt_restart", Kind: conversation.CommandMessage, RequestHash: "hash_restart", Receipt: conversation.Receipt{Key: "receipt_restart", Kind: conversation.CommandMessage, Status: conversation.DeliverySending, UpdatedAt: f.now}, CreatedAt: f.now, UpdatedAt: f.now})
			return err
		})
		if err := service.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(controls.wakes, stale.ID) {
			t.Fatal("restart omitted the queued worker control")
		}
		for _, event := range []string{"conversation.restart_normalized", "conversation.wake_pending"} {
			if !strings.Contains(logs.String(), event) {
				t.Errorf("startup omitted %s", event)
			}
		}
		if strings.Contains(logs.String(), private) {
			t.Error("conversation restart logs exposed private text")
		}
		execution := f.stored(t, restart.ID).Execution
		if execution.Status != conversation.ExecutionUnknown || execution.Owner.LeaseID != restart.Execution.Owner.LeaseID || execution.Capabilities.Steer {
			t.Fatalf("restart left an active execution: %+v", execution)
		}
		f.read(t, func(r tenantstore.Reader) error {
			ctx := t.Context()
			for _, id := range restartMessages {
				message, err := r.Message(ctx, restart.ID, id)
				if err != nil {
					return err
				}
				if message.Delivery != conversation.DeliveryUnknown {
					t.Errorf("restart left message %s in %s", id, message.Delivery)
				}
			}
			question, err := r.Question(ctx, restart.ID, restartQuestion)
			if err != nil {
				return err
			}
			if question.Status != conversation.QuestionExpired {
				t.Errorf("restart question remains %s", question.Status)
			}
			command, ok, err := r.Command(ctx, restart.ID, "receipt_restart")
			if err == nil && (!ok || command.Receipt.Status != conversation.DeliveryUnknown) {
				t.Errorf("restart left the command receipt unresolved: %+v", command.Receipt)
			}
			return err
		})
		candidate := f.stored(t, stale.ID)
		if candidate.Execution.Status != conversation.ExecutionInterrupted {
			t.Fatalf("stale execution remains %s", candidate.Execution.Status)
		}
		for !candidate.Execution.Settled {
			changed, err := service.ReconcileExecutionPG(t.Context(), org, candidate)
			if err != nil || !changed {
				t.Fatalf("stale execution stopped progressing: changed=%t error=%v", changed, err)
			}
			candidate = f.stored(t, stale.ID)
		}
		f.read(t, func(r tenantstore.Reader) error {
			for _, id := range staleMessages {
				message, err := r.Message(t.Context(), stale.ID, id)
				if err != nil {
					return err
				}
				if message.Delivery != conversation.DeliveryInterrupted {
					t.Errorf("stale response %s remains %s", id, message.Delivery)
				}
			}
			for _, id := range staleQuestions {
				question, err := r.Question(t.Context(), stale.ID, id)
				if err != nil {
					return err
				}
				if question.Status == conversation.QuestionPending {
					t.Errorf("stale question %s remains pending", id)
				}
			}
			return nil
		})
		if _, err := service.SettleIdleConversationsPG(t.Context(), org, f.now); err != nil {
			t.Fatal(err)
		}
		if row := f.stored(t, idle.ID); row.Status != conversation.StatusSettled || row.SettledAt == nil {
			t.Fatalf("idle conversation remains %s", row.Status)
		}
	})

	t.Run("shared restart respects the host start time and item budget", func(t *testing.T) {
		f := newPostgresConversationLifecycle(t, repository)
		service := f.service(slog.New(slog.DiscardHandler), &pendingWaker{}, &pendingWaker{})
		before := f.now.Add(time.Second)
		row := f.conversation("Example", f.now)
		row.Execution.Status = conversation.ExecutionRunning
		f.write(t, func(w tenantstore.Writer) error {
			if _, err := w.CreateConversation(t.Context(), row); err != nil {
				return err
			}
			for i := range 6 {
				m := f.message(row.ID, "Example", conversation.RoleAssistant, conversation.DeliverySending, "")
				if i == 0 {
					m.UpdatedAt = before
				}
				if _, err := w.CreateMessage(t.Context(), m); err != nil {
					return err
				}
			}
			return nil
		})
		after := ""
		for tick := 0; ; tick++ {
			next, done, used, err := service.RecoverPage(t.Context(), f.project.OrganizationID, after, before, 2)
			if err != nil || used > 2 || tick > 6 {
				t.Fatalf("restart tick %d used %d: %v", tick, used, err)
			}
			if done {
				break
			}
			after = next
		}
		f.read(t, func(r tenantstore.Reader) error {
			page, err := r.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: row.ID, Deliveries: []conversation.Delivery{conversation.DeliverySending}, Limit: 2})
			if err == nil && (len(page.Items) != 1 || !page.Items[0].UpdatedAt.Equal(before)) {
				t.Error("expected only the message at the host start boundary to remain sending")
			}
			return err
		})
	})
	t.Run("settle wakes each pending conversation across pages", func(t *testing.T) {
		f := newPostgresConversationLifecycle(t, repository)
		wakes := make(lifecycleWakeRecorder, 2*ConversationLifecycleBatchSize+1)
		service := f.service(slog.New(slog.NewTextHandler(io.Discard, nil)), wakes, &pendingWaker{})
		pending := map[string]bool{}
		f.write(t, func(w tenantstore.Writer) error {
			for range ConversationLifecycleBatchSize + 1 {
				row := f.conversation("Pending message", f.now)
				if _, err := w.CreateConversation(t.Context(), row); err != nil {
					return err
				}
				message := f.message(row.ID, "Saved message", conversation.RoleUser, conversation.DeliverySaved, "")
				if _, err := w.CreateMessage(t.Context(), message); err != nil {
					return err
				}
				pending[row.ID] = true
			}
			return nil
		})
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		ticks := make(chan time.Time, 2)
		service.SettleTicks, service.SettleCancel = ticks, cancel
		go service.SettleLoop(ctx)
		t.Cleanup(service.Stop)
		ticks <- f.now
		ticks <- f.now
		for len(pending) > 0 {
			select {
			case id := <-wakes:
				if !pending[id] {
					t.Fatalf("settle woke an unexpected conversation: %s", id)
				}
				delete(pending, id)
			case <-ctx.Done():
				t.Fatal("settle skipped pending conversations across pages")
			}
		}
	})

	t.Run("shutdown cancels and joins the active read", func(t *testing.T) {
		f := newPostgresConversationLifecycle(t, repository)
		blocked := &lifecycleBlockedRead{Repository: repository, started: make(chan struct{}), canceled: make(chan struct{})}
		f.repository = blocked
		service := f.service(slog.New(slog.NewTextHandler(io.Discard, nil)), &pendingWaker{}, &pendingWaker{})
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		ticks := make(chan time.Time, 1)
		service.SettleTicks, service.SettleCancel = ticks, cancel
		go service.SettleLoop(ctx)
		t.Cleanup(service.Stop)
		ticks <- f.now
		select {
		case <-blocked.started:
		case <-ctx.Done():
			t.Fatal("settle did not start its PostgreSQL read")
		}
		service.Stop()
		select {
		case <-blocked.canceled:
		default:
			t.Fatal("shutdown returned without canceling its PostgreSQL read")
		}
		select {
		case <-service.Settled:
		default:
			t.Fatal("shutdown returned before the settle loop stopped")
		}
	})
}

type lifecycleWakeRecorder chan string

func (lifecycleWakeRecorder) Available() bool             { return true }
func (lifecycleWakeRecorder) Cancel(string) bool          { return false }
func (lifecycleWakeRecorder) Hold(string) (func(), error) { return func() {}, nil }
func (w lifecycleWakeRecorder) Wake(id string)            { w <- id }
func (lifecycleWakeRecorder) Stop()                       {}

type lifecycleBlockedRead struct {
	tenantstore.Repository
	started, canceled   chan struct{}
	startOnce, stopOnce sync.Once
}

func (r *lifecycleBlockedRead) ReadTenant(ctx context.Context, _ tracker.OrganizationID, _ corestore.Workload, _ func(tenantstore.Reader) error) error {
	r.startOnce.Do(func() { close(r.started) })
	<-ctx.Done()
	r.stopOnce.Do(func() { close(r.canceled) })
	return ctx.Err()
}
