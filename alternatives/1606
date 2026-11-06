package hubserver

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConversationPostgresLifecycleUsesTenantStateWithoutSQLite(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	repository := projectRecordPostgres(t)
	f := newConversationTenantFixture(t, repository, now)
	f.service.config.TenantRepository = repository
	f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID)}
	f.service.config.now = func() time.Time { return now }
	owner := conversationLifecyclePostgresOwner(t, f, now)

	var issue corestore.WorkItem
	var restartID, staleID, idleID, restartQuestionID, staleQuestionID string
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := writer.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: f.project.OrganizationID, Name: string(f.project.OrganizationID), CreatedAt: now}); err != nil {
			return fmt.Errorf("create organization: %w", err)
		}
		if err := writer.CreateAccessCredential(t.Context(), owner); err != nil {
			return fmt.Errorf("create owner credential: %w", err)
		}
		if _, err := writer.CreateProjectRecord(t.Context(), projectstore.Record{
			Core:     corestore.Project{OrganizationID: f.project.OrganizationID, ID: f.project.ID, Name: f.project.Name, Slug: f.project.Slug, Profile: "native", CreatedAt: now},
			Settings: projectstore.Settings{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID},
			States:   f.project.States,
		}); err != nil {
			return fmt.Errorf("create project record: %w", err)
		}
		created, err := writer.CreateWorkItem(t.Context(), corestore.WorkItem{
			OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, NativeID: "wi_pg_lifecycle", Number: 1,
			Title: "Example lifecycle item", ActorKind: new("human"), ActorPrincipalID: new(owner.ID), WorkflowStateName: new("Todo"), CreatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("create work item: %w", err)
		}
		issue = created

		restartID = conversation.NewConversationID()
		if _, err := writer.CreateConversation(t.Context(), conversationstore.Conversation{
			OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ID: restartID, OwnerPrincipalID: owner.ID, Origin: conversationOriginUser,
			Title: "Restart recovery", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1,
			Execution:   conversation.Execution{Status: conversation.ExecutionRunning, Owner: conversation.Owner{AttemptID: "attempt_restart", RunID: "run_restart", LeaseID: "lease_restart", FencingToken: 1}, Capabilities: conversation.Capabilities{Steer: true}, UpdatedAt: now},
			Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return fmt.Errorf("create restart conversation: %w", err)
		}
		for range conversationLifecycleBatchSize + 1 {
			message := conversationMessageToStore(f.project.OrganizationID, conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: restartID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: "Example interrupted response", Delivery: conversation.DeliverySending, Actor: conversation.Actor{Kind: conversation.ActorRunner}, CreatedAt: now, UpdatedAt: now})
			if _, err := writer.CreateMessage(t.Context(), message); err != nil {
				return fmt.Errorf("create restart message: %w", err)
			}
		}
		restartQuestionID = conversation.NewQuestionID()
		if err := writer.UpsertQuestion(t.Context(), conversationstore.Question{OrganizationID: f.project.OrganizationID, ConversationID: restartID, Question: conversation.Question{ID: restartQuestionID, Status: conversation.QuestionPending, Owner: conversation.Owner{AttemptID: "attempt_restart"}, Prompts: []conversation.Prompt{{ID: "prompt", Question: "Example question?"}}, CreatedAt: now, UpdatedAt: now}}); err != nil {
			return fmt.Errorf("create restart question: %w", err)
		}
		if _, _, err := writer.ReserveCommand(t.Context(), conversationstore.Command{OrganizationID: f.project.OrganizationID, ConversationID: restartID, Key: "receipt_restart", Kind: conversation.CommandMessage, RequestHash: "hash_restart", Receipt: conversation.Receipt{Key: "receipt_restart", Kind: conversation.CommandMessage, Status: conversation.DeliverySending, UpdatedAt: now}, CreatedAt: now, UpdatedAt: now}); err != nil {
			return fmt.Errorf("create restart receipt: %w", err)
		}

		staleID = conversation.NewConversationID()
		if _, err := writer.CreateConversation(t.Context(), conversationstore.Conversation{
			OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ID: staleID, OwnerPrincipalID: owner.ID, Origin: conversationOriginWorker,
			Title: "Expired worker", Visibility: conversation.VisibilityShared, Status: conversation.StatusActive, WorkItemID: string(issue.NativeID), LinkedAt: &now, Revision: 1,
			Execution:   conversation.Execution{Status: conversation.ExecutionRunning, Owner: conversation.Owner{AttemptID: "attempt_missing", RunID: "run_missing", LeaseID: "lease_missing", FencingToken: 1}, UpdatedAt: now},
			Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return fmt.Errorf("create stale conversation: %w", err)
		}
		if _, err := writer.CreateMessage(t.Context(), conversationMessageToStore(f.project.OrganizationID, conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: staleID, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "Example queued control", Delivery: conversation.DeliveryQueued, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: owner.ID}, CreatedAt: now, UpdatedAt: now})); err != nil {
			return fmt.Errorf("create stale message: %w", err)
		}
		for range conversationLifecycleBatchSize + 1 {
			message := conversationMessageToStore(f.project.OrganizationID, conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: staleID, Role: conversation.RoleAssistant, Kind: conversation.MessageText, Text: "Example interrupted response", Delivery: conversation.DeliveryResponding, AttemptID: "attempt_missing", Actor: conversation.Actor{Kind: conversation.ActorRunner}, CreatedAt: now, UpdatedAt: now})
			if _, err := writer.CreateMessage(t.Context(), message); err != nil {
				return fmt.Errorf("create stale response: %w", err)
			}
		}
		for index := range conversationLifecycleBatchSize + 1 {
			questionID := conversation.NewQuestionID()
			if index == 0 {
				staleQuestionID = questionID
			}
			if err := writer.UpsertQuestion(t.Context(), conversationstore.Question{OrganizationID: f.project.OrganizationID, ConversationID: staleID, Question: conversation.Question{ID: questionID, Status: conversation.QuestionPending, Owner: conversation.Owner{AttemptID: "attempt_missing"}, Prompts: []conversation.Prompt{{ID: "prompt", Question: "Example question?"}}, CreatedAt: now, UpdatedAt: now}}); err != nil {
				return fmt.Errorf("create stale question: %w", err)
			}
		}

		idleID = conversation.NewConversationID()
		old := now.Add(-2 * f.service.conversations.config.SettleWindow)
		_, err = writer.CreateConversation(t.Context(), conversationstore.Conversation{
			OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ID: idleID, OwnerPrincipalID: owner.ID, Origin: conversationOriginUser,
			Title: "Idle conversation", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1,
			Execution: conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: old}, Preferences: conversation.DefaultPreferences(), CreatedAt: old, UpdatedAt: old,
		})
		return err
	}); err != nil {
		t.Fatalf("seed PostgreSQL lifecycle records: %v", err)
	}

	staleWake, cancelWake := f.service.conversations.broker.subscribe(staleID)
	defer cancelWake()
	if err := f.service.conversations.start(t.Context()); err != nil {
		t.Fatalf("selected PostgreSQL startup lifecycle: %v", err)
	}
	select {
	case <-staleWake.wake:
	default:
		t.Fatal("startup did not wake the queued linked conversation")
	}

	err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Background, func(reader tenantstore.Reader) error {
		restarted, err := reader.Conversation(t.Context(), f.project.ID, restartID)
		if err != nil {
			return err
		}
		if restarted.Execution.Status != conversation.ExecutionUnknown || restarted.Execution.Owner.LeaseID != "lease_restart" || restarted.Execution.Capabilities.Steer {
			return fmt.Errorf("restart execution was not normalized: %+v", restarted.Execution)
		}
		messages, err := reader.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: restartID, Deliveries: []conversation.Delivery{conversation.DeliveryUnknown}, Limit: conversationLifecycleBatchSize})
		if err != nil {
			return err
		}
		if len(messages.Items) != conversationLifecycleBatchSize || messages.Next == 0 {
			return fmt.Errorf("restart messages were not normalized: %+v", messages.Items)
		}
		tail, err := reader.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: restartID, Deliveries: []conversation.Delivery{conversation.DeliveryUnknown}, BeforeSequence: messages.Next, Limit: conversationLifecycleBatchSize})
		if err != nil {
			return err
		}
		if len(tail.Items) != 1 || tail.Items[0].Delivery != conversation.DeliveryUnknown {
			return fmt.Errorf("restart normalization did not finish its overflow batch: %+v", tail.Items)
		}
		question, err := reader.Question(t.Context(), restartID, restartQuestionID)
		if err != nil {
			return err
		}
		if question.Status != conversation.QuestionExpired {
			return fmt.Errorf("restart question status = %s", question.Status)
		}
		command, ok, err := reader.Command(t.Context(), restartID, "receipt_restart")
		if err != nil {
			return err
		}
		if !ok || command.Receipt.Status != conversation.DeliveryUnknown {
			return fmt.Errorf("restart command receipt = %+v, found=%t", command.Receipt, ok)
		}
		stale, err := reader.Conversation(t.Context(), f.project.ID, staleID)
		if err != nil {
			return err
		}
		if stale.Execution.Status != conversation.ExecutionInterrupted {
			return fmt.Errorf("stale execution status = %s", stale.Execution.Status)
		}
		if stale.Execution.Settled {
			return fmt.Errorf("stale execution settled before its owned rows drained")
		}
		staleQuestion, err := reader.Question(t.Context(), staleID, staleQuestionID)
		if err != nil {
			return err
		}
		if staleQuestion.Status != conversation.QuestionPending {
			return fmt.Errorf("stale question status after first bounded batch = %s", staleQuestion.Status)
		}
		responding, err := reader.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: staleID, AttemptID: "attempt_missing", Role: conversation.RoleAssistant, Deliveries: []conversation.Delivery{conversation.DeliveryResponding}, Limit: 10})
		if err != nil {
			return err
		}
		if len(responding.Items) != 1 {
			return fmt.Errorf("stale responses remaining after first bounded batch = %d, want 1", len(responding.Items))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var staleCandidate conversationstore.Conversation
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Background, func(reader tenantstore.Reader) error {
		var err error
		staleCandidate, err = reader.Conversation(t.Context(), f.project.ID, staleID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		changed, err := f.service.conversations.reconcileExecutionPG(t.Context(), f.project.OrganizationID, staleCandidate)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("stale execution stopped progressing before all owned rows settled")
		}
	}
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Background, func(reader tenantstore.Reader) error {
		stale, err := reader.Conversation(t.Context(), f.project.ID, staleID)
		if err != nil {
			return err
		}
		if !stale.Execution.Settled {
			return fmt.Errorf("stale execution remained unsettled after draining bounded batches")
		}
		questions, err := reader.OutstandingQuestions(t.Context(), staleID, "attempt_missing", "", "", 1)
		if err != nil {
			return err
		}
		if len(questions.Items) != 0 {
			return fmt.Errorf("outstanding questions remain after reconciliation: %d", len(questions.Items))
		}
		responses, err := reader.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: staleID, AttemptID: "attempt_missing", Role: conversation.RoleAssistant, Deliveries: []conversation.Delivery{conversation.DeliveryInterrupted}, Limit: conversationLifecycleBatchSize})
		if err != nil {
			return err
		}
		if len(responses.Items) != conversationLifecycleBatchSize || responses.Next == 0 {
			return fmt.Errorf("settled response batch = %d next=%d", len(responses.Items), responses.Next)
		}
		tail, err := reader.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: staleID, AttemptID: "attempt_missing", Role: conversation.RoleAssistant, Deliveries: []conversation.Delivery{conversation.DeliveryInterrupted}, BeforeSequence: responses.Next, Limit: conversationLifecycleBatchSize})
		if err != nil {
			return err
		}
		if len(tail.Items) != 1 {
			return fmt.Errorf("settled response overflow batch = %d, want 1", len(tail.Items))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	settled, err := f.service.conversations.settleIdleConversationsPG(t.Context(), f.project.OrganizationID, now)
	if err != nil {
		t.Fatal(err)
	}
	if settled != 1 {
		t.Fatalf("settled conversations = %d, want 1", settled)
	}
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Background, func(reader tenantstore.Reader) error {
		idle, err := reader.Conversation(t.Context(), f.project.ID, idleID)
		if err != nil {
			return err
		}
		if idle.Status != conversation.StatusSettled || idle.SettledAt == nil {
			return fmt.Errorf("idle conversation did not settle: status=%s settled_at=%v", idle.Status, idle.SettledAt)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		messages, err := writer.FilteredMessages(t.Context(), conversationstore.MessageFilter{ConversationID: staleID, Deliveries: []conversation.Delivery{conversation.DeliveryQueued}, Limit: 10})
		if err != nil {
			return err
		}
		for _, message := range messages.Items {
			message.Delivery, message.UpdatedAt = conversation.DeliveryUnknown, now
			if _, err := writer.UpdateMessage(t.Context(), message, message.Revision); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		for range conversationLifecycleBatchSize + 1 {
			id := conversation.NewConversationID()
			created, err := writer.CreateConversation(t.Context(), conversationstore.Conversation{
				OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, ID: id, OwnerPrincipalID: owner.ID, Origin: conversationOriginUser,
				Title: "Pending message", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1,
				Execution: conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: now}, Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now,
			})
			if err != nil {
				return err
			}
			message := conversationMessageToStore(f.project.OrganizationID, conversationMessageRecord{ID: conversation.NewMessageID(), ConversationID: created.ID, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: "Example pending message", Delivery: conversation.DeliverySaved, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: owner.ID}, CreatedAt: now, UpdatedAt: now})
			if _, err := writer.CreateMessage(t.Context(), message); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed pending PostgreSQL conversations: %v", err)
	}
	var firstPending, secondPending conversationstore.PendingConversationPage
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Background, func(reader tenantstore.Reader) error {
		var err error
		firstPending, err = reader.PendingConversations(t.Context(), "", conversationLifecycleBatchSize)
		if err != nil {
			return err
		}
		secondPending, err = reader.PendingConversations(t.Context(), firstPending.Next, conversationLifecycleBatchSize)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if firstPending.Next == "" || len(firstPending.Items) != conversationLifecycleBatchSize || len(secondPending.Items) == 0 {
		t.Fatalf("pending pages did not cross the lifecycle batch: first=%d next=%q second=%d", len(firstPending.Items), firstPending.Next, len(secondPending.Items))
	}
	wakeSignals := make(chan string, conversationLifecycleBatchSize*2)
	coordinator := lifecycleSignalCoordinator{wakes: wakeSignals}
	settleTicks := make(chan time.Time, 2)
	settleContext, settleCancel := context.WithCancel(t.Context())
	lifecycle := &conversationService{
		server: f.service, broker: f.service.conversations.broker, config: f.service.conversations.config,
		logger: f.service.conversations.logger, coordinator: coordinator, controls: f.service.conversations.controls,
		settle: make(chan struct{}), settled: make(chan struct{}),
		settleCancel: settleCancel, settleTicks: settleTicks, stopSettleTicker: func() {},
	}
	loopDone := make(chan struct{})
	go func() {
		lifecycle.settleLoop(settleContext)
		close(loopDone)
	}()
	var stopOnce sync.Once
	stopLoop := func() {
		stopOnce.Do(func() {
			close(lifecycle.settle)
			settleCancel()
		})
		<-loopDone
	}
	t.Cleanup(stopLoop)
	settleTicks <- now
	for range len(firstPending.Items) {
		id := <-wakeSignals
		if id == secondPending.Items[0].ID {
			t.Fatal("first settle tick advanced beyond its pending page")
		}
	}
	select {
	case id := <-wakeSignals:
		t.Fatalf("first settle tick woke extra conversation %s beyond its bounded page", id)
	default:
	}
	settleTicks <- now
	for range len(secondPending.Items) {
		id := <-wakeSignals
		if id != secondPending.Items[0].ID {
			t.Fatalf("second settle tick woke %s, want %s", id, secondPending.Items[0].ID)
		}
	}
	stopLoop()

	blockedRepository := &blockingLifecycleRepository{delegate: repository, started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	previousRepository := f.service.config.TenantRepository
	f.service.config.TenantRepository = blockedRepository
	t.Cleanup(func() {
		blockedRepository.releaseNow()
		f.service.config.TenantRepository = previousRepository
	})
	blockedContext, blockedCancel := context.WithCancel(t.Context())
	blockedTicks := make(chan time.Time, 1)
	blockedLifecycle := &conversationService{
		server: f.service, broker: newNotificationBroker(), config: f.service.conversations.config,
		logger: f.service.conversations.logger, coordinator: coordinator, controls: lifecycleSignalControls{},
		settle: make(chan struct{}), settled: make(chan struct{}),
		settleCancel: blockedCancel, settleTicks: blockedTicks, stopSettleTicker: func() {},
	}
	blockedDone := make(chan struct{})
	go func() {
		blockedLifecycle.settleLoop(blockedContext)
		close(blockedDone)
	}()
	blockedTicks <- now
	<-blockedRepository.started
	stopDone := make(chan struct{})
	go func() {
		blockedLifecycle.stop()
		close(stopDone)
	}()
	select {
	case <-blockedRepository.canceled:
	case <-t.Context().Done():
		blockedRepository.releaseNow()
		<-stopDone
		t.Fatal("service stop did not cancel its active PostgreSQL lifecycle read")
	}
	<-stopDone
	select {
	case <-blockedDone:
	default:
		t.Fatal("service stop returned before joining the lifecycle loop")
	}
}

func conversationLifecyclePostgresOwner(t *testing.T, f conversationAPIFixture, now time.Time) accessstore.Credential {
	t.Helper()
	return accessstore.Credential{OrganizationID: f.project.OrganizationID, ID: f.ownerID, Name: "Example owner", Hash: "example-owner-hash", Scope: "operator", ProjectAccess: "all", Kind: "personal", CreatedAt: now, UpdatedAt: now}
}

type lifecycleSignalCoordinator struct {
	wakes chan<- string
}

func (c lifecycleSignalCoordinator) Available() bool           { return true }
func (c lifecycleSignalCoordinator) Wake(id string)            { c.wakes <- id }
func (lifecycleSignalCoordinator) Cancel(string) bool          { return false }
func (lifecycleSignalCoordinator) Hold(string) (func(), error) { return func() {}, nil }
func (lifecycleSignalCoordinator) Stop()                       {}

type lifecycleSignalControls struct{}

func (lifecycleSignalControls) Wake(string) {}
func (lifecycleSignalControls) Stop()       {}

type blockingLifecycleRepository struct {
	delegate   tenantstore.Repository
	started    chan struct{}
	canceled   chan struct{}
	release    chan struct{}
	once       sync.Once
	cancelOnce sync.Once
}

func (r *blockingLifecycleRepository) ReadTenant(ctx context.Context, _ tracker.OrganizationID, _ corestore.Workload, _ func(tenantstore.Reader) error) error {
	r.once.Do(func() { close(r.started) })
	select {
	case <-ctx.Done():
		r.cancelOnce.Do(func() { close(r.canceled) })
		return ctx.Err()
	case <-r.release:
		return context.Canceled
	}
}

func (r *blockingLifecycleRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.delegate.WriteTenant(ctx, organization, workload, write)
}

func (r *blockingLifecycleRepository) releaseNow() {
	select {
	case <-r.release:
	default:
		close(r.release)
	}
}

func newConversationTenantFixture(t *testing.T, repository tenantstore.Repository, now time.Time) conversationAPIFixture {
	t.Helper()
	service := &Service{
		config:   Config{TenantRepository: repository, AccessRepository: repository, Logger: discardLogger(), now: func() time.Time { return now }, Hosted: &HostedConfig{OrganizationID: "org_conversation"}},
		database: &database{}, notifications: newNotificationBroker(),
	}
	service.ready.Store(true)
	chat := &conversationService{server: service, store: &conversationStore{contents: &conversationContentFixture{}, database: service.database}, broker: service.notifications, logger: service.config.Logger, config: (ConversationConfig{Enabled: true}).normalized(), settle: make(chan struct{}), settled: make(chan struct{})}
	close(chat.settled)
	chat.coordinator = newConversationCoordinator(chat)
	chat.controls = &conversationWorkerRouter{service: chat, stop: make(chan struct{})}
	service.conversations = chat
	t.Cleanup(service.conversations.stop)
	return conversationAPIFixture{nativeFixture: nativeFixture{service: service, project: tracker.NativeProject{OrganizationID: "org_conversation", ID: "prj_conversation", Name: "Example project", Slug: "example-project", Profile: "native", States: nativeFixtureStates()}}, ownerID: "owner_conversation"}
}
