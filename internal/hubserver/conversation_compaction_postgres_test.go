package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type compactionInterleaveRepository struct {
	tenantstore.Repository
	tenantstore.SnapshotRepository
	afterConversation func() error
}

func (r compactionInterleaveRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(compactionInterleaveReader{Reader: reader, afterConversation: r.afterConversation})
	})
}

func (r compactionInterleaveRepository) ReadTenantSnapshot(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.SnapshotRepository.ReadTenantSnapshot(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(compactionInterleaveReader{Reader: reader, afterConversation: r.afterConversation})
	})
}

type compactionInterleaveReader struct {
	tenantstore.Reader
	afterConversation func() error
}

func (r compactionInterleaveReader) Conversation(ctx context.Context, project tracker.ProjectID, id string) (conversationstore.Conversation, error) {
	record, err := r.Reader.Conversation(ctx, project, id)
	if err == nil {
		err = r.afterConversation()
	}
	return record, err
}

func checkCompactionReplaySnapshot(t *testing.T) {
	f := newStreamFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	record := seedStreamCompactionConversation(t, f, now.Add(-31*24*time.Hour), 1)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		_, err := writer.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: f.project.OrganizationID, ConversationID: record.ID, Event: conversation.Event{Type: conversation.EventConversationUpdated, Data: []byte(`{"title":"Later snapshot"}`), CreatedAt: now}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	record = readStreamCompactionConversation(t, f, record.ID)
	scope := nativeAttemptListingScope(t, f.nativeFixture)
	fixture := conversationFixture{nativeFixture: f.nativeFixture, store: f.service.conversations.store, organization: f.project.OrganizationID}
	chat := fixture.recordChat(f.project.OrganizationID)
	triggered := false
	chat.server.config.TenantRepository = compactionInterleaveRepository{
		Repository: f.service.config.TenantRepository, SnapshotRepository: f.service.config.TenantRepository.(tenantstore.SnapshotRepository),
		afterConversation: func() error {
			if triggered {
				return nil
			}
			triggered = true
			removed, err := f.service.conversations.compactEventsPG(t.Context(), f.project.OrganizationID, now)
			if err == nil && removed != 1 {
				return fmt.Errorf("interleaved compaction removed %d events, want 1", removed)
			}
			return err
		},
	}
	response := httptest.NewRecorder()
	stream := newConversationStream(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), response))
	cursor, err := chat.server.replayPGConversationEvents(t.Context(), scope, stream, record.ID, 0, record.EventSequence)
	if !triggered || err != nil && !errors.Is(err, errConversationCursorExpired) {
		t.Fatalf("compaction interleave: triggered=%t error=%v", triggered, err)
	}
	if err == nil && (cursor != record.EventSequence || !strings.Contains(response.Body.String(), "id: 2\n")) {
		t.Fatalf("replay silently crossed compaction: cursor=%d frames=%q", cursor, response.Body.String())
	}
}

func checkCompactionSettleTick(t *testing.T) {
	f := newStreamFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	record := seedStreamCompactionConversation(t, f, now.Add(-31*24*time.Hour), 1)
	chat := f.service.conversations
	wake, unsubscribe := chat.broker.subscribe(record.ID)
	defer unsubscribe()
	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan time.Time, 1)
	lifecycle := &conversationService{
		server: f.service, store: chat.store, broker: chat.broker, config: chat.config, logger: chat.logger,
		coordinator: lifecycleSignalCoordinator{wakes: make(chan string, conversationLifecycleBatchSize)}, controls: lifecycleSignalControls{},
		settle: make(chan struct{}), settled: make(chan struct{}), settleTicks: ticks, stopSettleTicker: func() {},
	}
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		lifecycle.settleLoop(ctx)
	}()
	ticks <- now
	select {
	case <-wake.wake:
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("settle tick did not publish the compaction wake")
	}
	current := readStreamCompactionConversation(t, f, record.ID)
	if current.Compaction.ThroughSequence != record.EventSequence || current.Compaction.ContentReference == "" {
		t.Fatalf("settle tick did not commit typed compaction: %+v", current.Compaction)
	}
}

func checkCompactionPublicationSource(t *testing.T) {
	for _, mutation := range []string{"event head", "conversation revision"} {
		t.Run(mutation, func(t *testing.T) {
			f := newStreamFixture(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			record := seedStreamCompactionConversation(t, f, now.Add(-31*24*time.Hour), 1)
			contents := f.service.conversations.store.contents.(*conversationContentFixture)
			mutated := false
			contents.onIO = func() error {
				if mutated {
					return nil
				}
				mutated = true
				probe, err := conversationTestAdmin(t, f.service).BeginTx(t.Context(), nil)
				if err != nil {
					return err
				}
				defer probe.Rollback()
				var id string
				if err := probe.QueryRowContext(t.Context(), "SELECT id FROM conversations WHERE organization_id=$1 AND id=$2 FOR UPDATE NOWAIT", f.project.OrganizationID, record.ID).Scan(&id); err != nil {
					return fmt.Errorf("archive IO held a conversation lock: %w", err)
				}
				if err := probe.Rollback(); err != nil {
					return err
				}
				return f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
					if mutation == "event head" {
						_, err := writer.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: f.project.OrganizationID, ConversationID: record.ID, Event: conversation.Event{Type: conversation.EventHeartbeat, Data: []byte(`{}`), CreatedAt: now}})
						return err
					}
					current, err := writer.Conversation(t.Context(), f.project.ID, record.ID)
					if err != nil {
						return err
					}
					current.Title = "Changed source"
					_, err = writer.UpdateConversation(t.Context(), current, current.Revision)
					return err
				})
			}
			t.Cleanup(func() { contents.onIO = nil })
			removed, err := f.service.conversations.compactEventsPG(t.Context(), f.project.OrganizationID, now)
			contents.onIO = nil
			if err != nil {
				t.Fatal(err)
			}
			current := readStreamCompactionConversation(t, f, record.ID)
			events, err := streamCompactionEvents(t, f, record.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			wantEvents := 2
			if mutation == "event head" {
				wantEvents++
			}
			if !mutated || removed != 0 || current.Compaction.ThroughSequence != 0 || current.Compaction.ContentReference != "" || len(events) != wantEvents {
				t.Fatalf("changed source was compacted: mutated=%t removed=%d state=%+v events=%d", mutated, removed, current.Compaction, len(events))
			}
		})
	}
}

func checkCompactionRestart(t *testing.T) {
	f := newStreamFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	record := seedStreamCompactionConversation(t, f, now.Add(-31*24*time.Hour), 600)
	if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE conversation_events SET compaction_group_key=NULL WHERE organization_id=$1 AND conversation_id=$2", f.project.OrganizationID, record.ID); err != nil {
		t.Fatal(err)
	}
	removed, err := f.service.conversations.compactEventsPG(t.Context(), f.project.OrganizationID, now)
	if err != nil {
		t.Fatal(err)
	}
	var projected int
	if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_events WHERE organization_id=$1 AND conversation_id=$2 AND compaction_group_key IS NOT NULL", f.project.OrganizationID, record.ID).Scan(&projected); err != nil {
		t.Fatal(err)
	}
	if removed != 0 || projected != conversationstore.CompactionBatch {
		t.Fatalf("projection exceeded its tick budget: removed=%d projected=%d", removed, projected)
	}
	reopen := func() {
		t.Helper()
		f.server.Close()
		f.service = tfConvReopenConversationService(t, f.service)
		f.server = httptest.NewServer(f.service.Handler())
		t.Cleanup(f.server.Close)
	}
	reopen()
	for range 2 {
		if _, err := f.service.conversations.compactEventsPG(t.Context(), f.project.OrganizationID, now); err != nil {
			t.Fatal(err)
		}
	}
	compacted := readStreamCompactionConversation(t, f, record.ID)
	if compacted.Compaction.ThroughSequence != record.EventSequence || compacted.Compaction.ScannedSequence != record.EventSequence || compacted.Compaction.ContentReference == "" {
		t.Fatalf("compaction did not resume projected history: %+v", compacted.Compaction)
	}
	reopen()
	if got := readStreamCompactionConversation(t, f, record.ID); got.Compaction != compacted.Compaction {
		t.Fatalf("restart lost durable watermarks: before=%+v after=%+v", compacted.Compaction, got.Compaction)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+record.ID, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var snapshot conversationSnapshot
	decodeHubResponse(t, response, &snapshot)
	if snapshot.Compaction == nil || snapshot.Compaction.RemovedEvents != 600 || snapshot.Compaction.RetainedEvents != 1 || snapshot.Cursor != record.EventSequence {
		t.Fatalf("restart compaction snapshot = %+v", snapshot)
	}
	stream := f.open(t, f.token, record.ID, 0)
	defer stream.close()
	requireClosed(t, stream.nextEvent(t), conversationClosedCursorExpired)
	if removed, err := f.service.conversations.compactEventsPG(t.Context(), f.project.OrganizationID, now); err != nil || removed != 0 {
		t.Fatalf("completed compaction was repeated: removed=%d error=%v", removed, err)
	}
}

func streamCompactionEvents(t *testing.T, f streamFixture, id string, after int64) ([]conversation.Event, error) {
	t.Helper()
	var events []conversation.Event
	err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
		for {
			page, err := reader.ConversationEvents(t.Context(), id, after, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, event := range page.Items {
				events = append(events, event.Event)
			}
			if page.Next == 0 {
				return nil
			}
			after = page.Next
		}
	})
	return events, err
}

func seedStreamCompactionConversation(t *testing.T, f streamFixture, old time.Time, heartbeats int, ids ...string) conversationstore.Conversation {
	t.Helper()
	var id string
	if len(ids) > 0 {
		id = ids[0]
	} else {
		id = f.create(t, f.token, map[string]any{"title": "Example retention"}).Conversation.ID
	}
	var record conversationstore.Conversation
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
		var err error
		record, err = writer.Conversation(t.Context(), f.project.ID, id)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(projectConversation(conversationRecordFromStore(record)))
		if err != nil {
			return err
		}
		if _, err := writer.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: f.project.OrganizationID, ConversationID: id, Event: conversation.Event{Type: conversation.EventConversationUpdated, Data: raw, CreatedAt: old}}); err != nil {
			return err
		}
		for range heartbeats {
			if _, err := writer.AppendConversationEvent(t.Context(), conversationstore.Event{OrganizationID: f.project.OrganizationID, ConversationID: id, Event: conversation.Event{Type: conversation.EventHeartbeat, Data: []byte(`{}`), CreatedAt: old}}); err != nil {
				return err
			}
		}
		record, err = writer.Conversation(t.Context(), f.project.ID, id)
		if err != nil {
			return err
		}
		record.Status, record.SettledAt, record.UpdatedAt = conversation.StatusSettled, &old, old
		record, err = writer.UpdateConversation(t.Context(), record, record.Revision)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return record
}

func readStreamCompactionConversation(t *testing.T, f streamFixture, id string) conversationstore.Conversation {
	t.Helper()
	var record conversationstore.Conversation
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		record, err = reader.Conversation(t.Context(), f.project.ID, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return record
}
