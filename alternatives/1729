package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

// conversationLogSentinel is the message text every logging test sends. No
// log record may ever contain it: conversation lines carry identifiers,
// statuses and error codes only.
const conversationLogSentinel = "sentinel-zq7-message-body"

// conversationLogSink collects the hub's JSON log records. The hub logs from
// background goroutines, so writes are serialized.
type conversationLogSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *conversationLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *conversationLogSink) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// records decodes every line the hub logged so far.
func (s *conversationLogSink) records(t *testing.T) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(s.text(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

// events returns every record whose message is name.
func (s *conversationLogSink) events(t *testing.T, name string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, record := range s.records(t) {
		if record["msg"] == name {
			found = append(found, record)
		}
	}
	return found
}

// only returns the single record named name and fails when there is not
// exactly one.
func (s *conversationLogSink) only(t *testing.T, name string) map[string]any {
	t.Helper()
	found := s.events(t, name)
	if len(found) != 1 {
		t.Fatalf("%s records = %d, want exactly 1 (log: %s)", name, len(found), s.text())
	}
	return found[0]
}

// requireNoSentinel asserts that no log record leaked message text.
func (s *conversationLogSink) requireNoSentinel(t *testing.T) {
	t.Helper()
	if strings.Contains(s.text(), conversationLogSentinel) {
		t.Fatalf("a log record carried message text: %s", s.text())
	}
}

// requireConversationLogFields asserts the component tag and every wanted
// field of one record. JSON numbers arrive as float64.
func requireConversationLogFields(t *testing.T, record map[string]any, want map[string]any) {
	t.Helper()
	if record["component"] != "conversation" {
		t.Fatalf("component = %#v, want conversation (record %#v)", record["component"], record)
	}
	for key, value := range want {
		got, ok := record[key]
		if !ok {
			t.Fatalf("record %#v is missing %q", record, key)
		}
		if got != value {
			t.Fatalf("%s = %#v, want %#v (record %#v)", key, got, value, record)
		}
	}
}

// openConversationLogService opens a hub whose logger writes JSON into sink.
func openConversationLogService(t *testing.T, cfg Config, sink *conversationLogSink) *Service {
	t.Helper()
	if cfg.Content == nil {
		cfg.Content = (&changeObjectFixture{}).call
	}
	if cfg.Conversation != nil && cfg.Conversation.ArchiveContents == nil {
		conversationConfig := *cfg.Conversation
		conversationConfig.ArchiveContents = &conversationContentFixture{}
		cfg.Conversation = &conversationConfig
	}

	cfg.Logger = slog.New(slog.NewJSONHandler(sink, nil))
	if len(cfg.InitialAdminToken) == 0 {
		cfg.InitialAdminToken = []byte(testHubAdminToken)
	}
	service := openTestService(t, cfg)
	if service.config.Postgres == nil {
		t.Fatal("logging fixture requires PostgreSQL")
	}
	return service
}

// newConversationLogFixture builds the conversation API fixture against a
// hub that logs into the returned sink.
func newConversationLogFixture(t *testing.T, cfg *ConversationConfig) (conversationAPIFixture, *conversationLogSink) {
	t.Helper()
	sink := &conversationLogSink{}
	service := openConversationLogService(t, Config{Conversation: cfg}, sink)
	f := newNativeFixture(t, service, "", "chat-logging")
	credential, status, err := service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil || status != http.StatusOK || credential.ID == "" {
		t.Fatalf("authenticate logging operator: status=%d error=%v", status, err)
	}
	return conversationAPIFixture{nativeFixture: f, ownerID: (hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}).Actor().PrincipalID}, sink
}

// TestConversationLoggingCommandPath covers the diagnostics the operator API
// emits for one conversation: a receipt line per stored receipt, a warning
// for every stale_execution rejection on both the API and the worker path,
// and a warning when the bounded control queue refuses a command.
func TestConversationLoggingCommandPath(t *testing.T) {
	t.Parallel()
	// The first message is saved for a coordinator, so the queue holds one
	// before the link.
	f, sink := newConversationLogFixture(t, &ConversationConfig{Enabled: true, ControlQueueSize: 3})
	created := f.create(t, f.token, map[string]any{"title": "Ship it", "first_message": map[string]any{"key": "first", "text": conversationLogSentinel}})
	record := created.Conversation

	t.Run("receipt", func(t *testing.T) {
		// No coordinator answers an unlinked conversation, so the first
		// message is saved.
		requireConversationLogFields(t, sink.only(t, "conversation.receipt"), map[string]any{
			"conversation_id": record.ID,
			"command_key":     "first",
			"kind":            string(conversation.CommandMessage),
			"status":          string(conversation.DeliverySaved),
			"attempt_id":      "",
		})
	})

	requireNativeStatus(t, f.link(t, f.token, record.ID, "link", true, "Ship the release"), http.StatusOK)
	requireNativeStatus(t, f.command(t, f.token, record.ID, conversation.Command{Key: "q1", Kind: conversation.CommandMessage, Text: conversationLogSentinel + " one"}), http.StatusOK)

	t.Run("queued receipt", func(t *testing.T) {
		found := sink.events(t, "conversation.receipt")
		if len(found) != 2 {
			t.Fatalf("conversation.receipt records = %d, want 2", len(found))
		}
		requireConversationLogFields(t, found[1], map[string]any{
			"conversation_id": record.ID,
			"command_key":     "q1",
			"kind":            string(conversation.CommandMessage),
			"status":          string(conversation.DeliveryQueued),
			"attempt_id":      "",
		})
		if _, ok := found[1]["error_code"]; ok {
			t.Fatalf("a receipt without an error must not log error_code: %#v", found[1])
		}
	})

	t.Run("stale execution on the api path", func(t *testing.T) {
		response := f.command(t, f.token, record.ID, conversation.Command{Key: "q-stale", Kind: conversation.CommandMessage, Text: conversationLogSentinel, Expected: conversation.Expected{AttemptID: "att_none"}})
		requireNativeError(t, response, http.StatusConflict, "stale_execution")
		requireConversationLogFields(t, sink.only(t, "API request refused"), map[string]any{
			"conversation_id":     record.ID,
			"expected_attempt_id": "att_none",
			"current_attempt_id":  "",
			"path":                string(conversation.CommandMessage),
			"level":               "WARN",
		})
	})

	t.Run("queue full", func(t *testing.T) {
		requireNativeStatus(t, f.command(t, f.token, record.ID, conversation.Command{Key: "q2", Kind: conversation.CommandMessage, Text: conversationLogSentinel + " two"}), http.StatusOK)
		response := f.command(t, f.token, record.ID, conversation.Command{Key: "q3", Kind: conversation.CommandMessage, Text: conversationLogSentinel + " three"})
		requireNativeError(t, response, http.StatusServiceUnavailable, "queue_full")
		requireConversationLogFields(t, sink.only(t, "conversation.queue_full"), map[string]any{
			"conversation_id": record.ID,
			"kind":            string(conversation.CommandMessage),
			"queued":          float64(3),
			"limit":           float64(3),
			"level":           "WARN",
		})
	})

	t.Run("stale execution on the worker path", func(t *testing.T) {
		worker := f.worker(t, "chat-logging-worker")
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/conversations/"+record.ID+"/turn-events", worker, map[string]any{
			"lease_id": "lease_absent", "fencing_token": 1, "attempt_id": "att_worker", "events": []map[string]any{},
		})
		requireNativeError(t, response, http.StatusConflict, "stale_execution")
		found := sink.events(t, "API request refused")
		if len(found) != 2 {
			t.Fatalf("conversation.stale_execution records = %d, want 2", len(found))
		}
		requireConversationLogFields(t, found[1], map[string]any{
			"conversation_id":     record.ID,
			"expected_attempt_id": "att_worker",
			"current_attempt_id":  "",
			"path":                "turn_events",
		})
	})

	sink.requireNoSentinel(t)
}

// TestConversationLoggingRestartNormalization covers the two startup lines:
// what normalization changed, and how many conversations were re-woken on
// each path.
func TestConversationLoggingRestartNormalization(t *testing.T) {
	t.Parallel()
	f, _ := newConversationLogFixture(t, &ConversationConfig{Enabled: true, ControlQueueSize: 8})
	unlinked := f.create(t, f.token, map[string]any{"title": "Unlinked", "first_message": map[string]any{"key": "u1", "text": conversationLogSentinel}}).Conversation
	requireNativeStatus(t, f.command(t, f.token, unlinked.ID, conversation.Command{Key: "u2", Kind: conversation.CommandMessage, Text: conversationLogSentinel}), http.StatusOK)
	linked := f.create(t, f.token, map[string]any{"title": "Linked", "first_message": map[string]any{"key": "l1", "text": conversationLogSentinel}}).Conversation
	requireNativeStatus(t, f.link(t, f.token, linked.ID, "link", true, "Ship the release"), http.StatusOK)
	requireNativeStatus(t, f.command(t, f.token, linked.ID, conversation.Command{Key: "l2", Kind: conversation.CommandMessage, Text: conversationLogSentinel}), http.StatusOK)
	requireNativeStatus(t, f.command(t, f.token, linked.ID, conversation.Command{Key: "l3", Kind: conversation.CommandMessage, Text: conversationLogSentinel}), http.StatusOK)

	// Seed the in-flight state a killed process leaves behind: one running
	// execution, one message and one receipt mid-delivery, one pending
	// question, and one saved message on each wake path.
	ctx := t.Context()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	if err := f.service.config.TenantRepository.WriteTenant(ctx, f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		stored, err := w.LockConversation(ctx, f.project.ID, unlinked.ID)
		if err != nil {
			return err
		}
		stored.Execution = conversation.Execution{Status: conversation.ExecutionRunning}
		stored.ContentReference = ""
		if _, err := w.UpdateConversation(ctx, stored, stored.Revision); err != nil {
			return err
		}
		message, found, err := w.MessageByCommandKey(ctx, unlinked.ID, "u2")
		if err != nil {
			return err
		}
		if !found {
			return errors.New("normalization message was not stored")
		}
		message.Delivery = conversation.DeliverySending
		if _, err := w.UpdateMessage(ctx, message, message.Revision); err != nil {
			return err
		}
		command, found, err := w.Command(ctx, unlinked.ID, "u2")
		if err != nil {
			return err
		}
		if !found {
			return errors.New("normalization receipt was not stored")
		}
		command.Receipt.Status = conversation.DeliverySending
		if err := w.UpdateCommand(ctx, command); err != nil {
			return err
		}
		saved, found, err := w.MessageByCommandKey(ctx, unlinked.ID, "u1")
		if err != nil {
			return err
		}
		if !found {
			return errors.New("coordinator wake message was not stored")
		}
		saved.Delivery = conversation.DeliverySaved
		if _, err := w.UpdateMessage(ctx, saved, saved.Revision); err != nil {
			return err
		}
		return w.UpsertQuestion(ctx, conversationstore.Question{OrganizationID: f.project.OrganizationID, ConversationID: unlinked.ID, Question: conversation.Question{
			ID: conversation.NewQuestionID(), ConversationID: unlinked.ID, MessageID: message.ID,
			Status: conversation.QuestionPending, Prompts: []conversation.Prompt{{ID: "p", Question: "Continue?"}}, CreatedAt: now, UpdatedAt: now,
		}})
	}); err != nil {
		t.Fatalf("seed restart state: %v", err)
	}
	config := f.service.config
	databaseName := config.Postgres.Connection.Database
	previousRepository := config.TenantRepository
	config.Conversation = &ConversationConfig{Enabled: true, ControlQueueSize: 8, Backend: newFakeCoordinatorBackend(), Workspace: t.TempDir(), ArchiveContents: config.Conversation.ArchiveContents}
	if err := f.service.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	config.TenantRepository, config.AccessRepository, config.BillingRepository = nil, nil, nil
	sink := &conversationLogSink{}
	restarted := openConversationLogService(t, config, sink)
	if restarted.config.Postgres == nil || restarted.config.Postgres.Connection.Database != databaseName || restarted.config.TenantRepository == previousRepository {
		t.Fatal("restart did not reopen the same PostgreSQL database with a new repository")
	}

	requireConversationLogFields(t, sink.only(t, "conversation.restart_normalized"), map[string]any{
		"conversations": float64(1),
		"messages":      float64(1),
		"questions":     float64(1),
		"receipts":      float64(1),
	})
	requireConversationLogFields(t, sink.only(t, "conversation.wake_pending"), map[string]any{
		"coordinator": float64(1),
		"controls":    float64(1),
	})
	sink.requireNoSentinel(t)
}
