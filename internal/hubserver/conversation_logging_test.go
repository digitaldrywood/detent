package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
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
// It mirrors openTestService, which pins a discarding logger.
func openConversationLogService(t *testing.T, cfg Config, sink *conversationLogSink) *Service {
	t.Helper()
	cfg.Logger = slog.New(slog.NewJSONHandler(sink, nil))
	if len(cfg.InitialAdminToken) == 0 {
		cfg.InitialAdminToken = []byte(testHubAdminToken)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return service
}

// newConversationLogFixture builds the conversation API fixture against a
// hub that logs into the returned sink.
func newConversationLogFixture(t *testing.T, cfg *ConversationConfig) (conversationAPIFixture, *conversationLogSink) {
	t.Helper()
	sink := &conversationLogSink{}
	service := openConversationLogService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Conversation: cfg}, sink)
	f := newNativeFixture(t, service, "", "chat-logging")
	var ownerID string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE name = ?", "operator-chat-logging").Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	return conversationAPIFixture{nativeFixture: f, ownerID: ownerID}, sink
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
		requireConversationLogFields(t, sink.only(t, "conversation.stale_execution"), map[string]any{
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
		found := sink.events(t, "conversation.stale_execution")
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
	linked := f.create(t, f.token, map[string]any{"title": "Linked", "first_message": map[string]any{"key": "l1", "text": conversationLogSentinel}}).Conversation
	requireNativeStatus(t, f.link(t, f.token, linked.ID, "link", true, "Ship the release"), http.StatusOK)
	requireNativeStatus(t, f.command(t, f.token, linked.ID, conversation.Command{Key: "l2", Kind: conversation.CommandMessage, Text: conversationLogSentinel}), http.StatusOK)
	requireNativeStatus(t, f.command(t, f.token, linked.ID, conversation.Command{Key: "l3", Kind: conversation.CommandMessage, Text: conversationLogSentinel}), http.StatusOK)

	// Seed the in-flight state a killed process leaves behind: one running
	// execution, one message and one receipt mid-delivery, one pending
	// question, and one saved message on each wake path.
	ctx := t.Context()
	db := f.service.database.db
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("seed %q: %v", query, err)
		}
	}
	exec("UPDATE conversations SET execution_json = json_set(execution_json, '$.status', 'running') WHERE id = ?", linked.ID)
	exec("UPDATE conversation_messages SET delivery = ? WHERE conversation_id = ? AND command_key = ?", conversation.DeliverySending, linked.ID, "l3")
	exec("UPDATE conversation_commands SET receipt_json = json_set(receipt_json, '$.status', ?) WHERE conversation_id = ? AND key = ?", conversation.DeliverySending, linked.ID, "l3")
	exec("UPDATE conversation_messages SET delivery = ? WHERE conversation_id = ? AND command_key = ?", conversation.DeliverySaved, unlinked.ID, "u1")
	var messageID string
	if err := db.QueryRowContext(ctx, "SELECT id FROM conversation_messages WHERE conversation_id = ? AND command_key = ?", linked.ID, "l2").Scan(&messageID); err != nil {
		t.Fatalf("read seeded message: %v", err)
	}
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	question := conversationQuestionRecord{Question: conversation.Question{
		ID: conversation.NewQuestionID(), ConversationID: linked.ID, MessageID: messageID,
		Status: conversation.QuestionPending, Prompts: []conversation.Prompt{{ID: "p", Question: "Continue?"}},
		CreatedAt: now, UpdatedAt: now,
	}}
	if err := f.service.conversations.store.upsertQuestion(ctx, tx, question); err != nil {
		_ = tx.Rollback()
		t.Fatalf("upsertQuestion() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	// Restart on the same database and read what the new process reported.
	databasePath := f.service.config.DatabasePath
	if err := f.service.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	sink := &conversationLogSink{}
	openConversationLogService(t, Config{DatabasePath: databasePath, Conversation: &ConversationConfig{Enabled: true, ControlQueueSize: 8}}, sink)

	requireConversationLogFields(t, sink.only(t, "conversation.restart_normalized"), map[string]any{
		"conversations": float64(1),
		"messages":      float64(1),
		"questions":     float64(1),
		"receipts":      float64(1),
	})
	requireConversationLogFields(t, sink.only(t, "conversation.wake_pending"), map[string]any{
		"controls": float64(1),
	})
	sink.requireNoSentinel(t)
}

// TestConversationNormalizeAfterRestartSummary covers the counts the store
// reports for each class of in-flight row it rewrites.
func TestConversationNormalizeAfterRestartSummary(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	now := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	rows := []struct {
		execution conversation.ExecutionStatus
		delivery  conversation.Delivery
		question  conversation.QuestionStatus
		receipt   conversation.Delivery
	}{
		{conversation.ExecutionRunning, conversation.DeliverySending, conversation.QuestionPending, conversation.DeliverySending},
		{conversation.ExecutionInterrupting, conversation.DeliveryResponding, conversation.QuestionSending, conversation.DeliveryResponding},
		{conversation.ExecutionIdle, conversation.DeliveryQueued, conversation.QuestionAnswered, conversation.DeliveryDelivered},
	}
	for i, row := range rows {
		record := f.record(f.owner, "normalize-"+strconv.Itoa(i), now)
		record.Execution = conversation.Execution{Status: row.execution, UpdatedAt: now}
		record = f.create(t, record)
		key := "k" + strconv.Itoa(i)
		tx := f.tx(t)
		message := conversationMessageRecord{ConversationID: record.ID, Role: conversation.RoleUser, Kind: conversation.MessageText, Text: conversationLogSentinel, Delivery: row.delivery, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: f.owner}, CommandKey: key, CreatedAt: now, UpdatedAt: now}
		if err := f.store.appendMessage(t.Context(), tx, &message); err != nil {
			t.Fatalf("appendMessage() error = %v", err)
		}
		question := conversationQuestionRecord{Question: conversation.Question{ID: conversation.NewQuestionID(), ConversationID: record.ID, MessageID: message.ID, Status: row.question, Prompts: []conversation.Prompt{{ID: "p", Question: "?"}}, CreatedAt: now, UpdatedAt: now}}
		if err := f.store.upsertQuestion(t.Context(), tx, question); err != nil {
			t.Fatalf("upsertQuestion() error = %v", err)
		}
		if _, _, err := f.store.reserveCommand(t.Context(), tx, record.ID, key, conversation.CommandMessage, "hash", now); err != nil {
			t.Fatalf("reserveCommand() error = %v", err)
		}
		if err := f.store.updateReceipt(t.Context(), tx, record.ID, key, conversation.Receipt{Key: key, Kind: conversation.CommandMessage, Status: row.receipt, MessageID: message.ID, UpdatedAt: now}); err != nil {
			t.Fatalf("updateReceipt() error = %v", err)
		}
		f.commit(t, tx)
	}

	summary, err := f.store.normalizeAfterRestart(t.Context(), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("normalizeAfterRestart() error = %v", err)
	}
	want := conversationNormalizeSummary{Conversations: 2, Messages: 2, Questions: 2, Receipts: 2}
	if summary != want {
		t.Fatalf("summary = %#v, want %#v", summary, want)
	}

	again, err := f.store.normalizeAfterRestart(t.Context(), now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("normalizeAfterRestart(second) error = %v", err)
	}
	if (again != conversationNormalizeSummary{}) {
		t.Fatalf("a second normalization changed %#v, want nothing", again)
	}
}

// TestConversationStaleExecutionAttempts covers the attempt identifiers the
// stale_execution log line reads off a rejection.
func TestConversationStaleExecutionAttempts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		expected string
		current  string
		want     bool
	}{
		{name: "owner mismatch", err: conversationStaleOwner("mismatch", "att_a", "att_b"), expected: "att_a", current: "att_b", want: true},
		{name: "expected only", err: conversationStaleOwner("mismatch", "att_a", ""), expected: "att_a", want: true},
		{name: "plain stale", err: conversationStale("no attempt"), want: true},
		{name: "translated stale", err: translateConversationError(conversation.ErrStaleExecution), want: true},
		{name: "other native error", err: conversationQueueFull()},
		{name: "not native", err: errors.New("boom")},
		{name: "nil", err: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expected, current, ok := staleExecutionAttempts(test.err)
			if ok != test.want {
				t.Fatalf("ok = %t, want %t", ok, test.want)
			}
			if expected != test.expected || current != test.current {
				t.Fatalf("attempts = %q/%q, want %q/%q", expected, current, test.expected, test.current)
			}
		})
	}
}
