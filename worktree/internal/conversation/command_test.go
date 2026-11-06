package conversation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func isStaleExecution(err error) bool { return errors.Is(err, ErrStaleExecution) }

func TestValidateCommand(t *testing.T) {
	t.Parallel()
	long := func(n int) string { return strings.Repeat("x", n) }
	tests := []struct {
		name    string
		command Command
		wantErr string
	}{
		{name: "valid message", command: Command{Key: "k1", Kind: CommandMessage, Text: "hello"}},
		{name: "valid message at limit", command: Command{Key: long(128), Kind: CommandMessage, Text: long(16000)}},
		{name: "empty key", command: Command{Kind: CommandMessage, Text: "hello"}, wantErr: "key"},
		{name: "blank key", command: Command{Key: "   ", Kind: CommandMessage, Text: "hello"}, wantErr: "key"},
		{name: "key too long", command: Command{Key: long(129), Kind: CommandMessage, Text: "hello"}, wantErr: "key"},
		{name: "unknown kind", command: Command{Key: "k", Kind: CommandKind("restart"), Text: "hello"}, wantErr: "kind"},
		{name: "empty kind", command: Command{Key: "k", Text: "hello"}, wantErr: "kind"},
		{name: "message without text", command: Command{Key: "k", Kind: CommandMessage}, wantErr: "text"},
		{name: "message blank text", command: Command{Key: "k", Kind: CommandMessage, Text: " \n"}, wantErr: "text"},
		{name: "message text too long", command: Command{Key: "k", Kind: CommandMessage, Text: long(16001)}, wantErr: "text"},
		{name: "valid answer", command: Command{Key: "k", Kind: CommandAnswer, QuestionID: "q_" + long(32), Answers: map[string][]string{"p1": {"yes"}}}},
		{name: "answer without question", command: Command{Key: "k", Kind: CommandAnswer, Answers: map[string][]string{"p1": {"yes"}}}, wantErr: "question_id"},
		{name: "answer without answers", command: Command{Key: "k", Kind: CommandAnswer, QuestionID: "q_1"}, wantErr: "answers"},
		{name: "answer with empty answer set", command: Command{Key: "k", Kind: CommandAnswer, QuestionID: "q_1", Answers: map[string][]string{"p1": {}}}, wantErr: "answers"},
		{name: "answer value too long", command: Command{Key: "k", Kind: CommandAnswer, QuestionID: "q_1", Answers: map[string][]string{"p1": {long(4001)}}}, wantErr: "answers"},
		{name: "answer value at limit", command: Command{Key: "k", Kind: CommandAnswer, QuestionID: "q_1", Answers: map[string][]string{"p1": {long(4000)}}}},
		{name: "answer blank prompt id", command: Command{Key: "k", Kind: CommandAnswer, QuestionID: "q_1", Answers: map[string][]string{"": {"yes"}}}, wantErr: "answers"},
		{name: "valid interrupt", command: Command{Key: "k", Kind: CommandInterrupt, Expected: Expected{AttemptID: "att_1"}}},
		{name: "interrupt without attempt", command: Command{Key: "k", Kind: CommandInterrupt}, wantErr: "expected.attempt_id"},
		{name: "valid continue without attempt", command: Command{Key: "k", Kind: CommandContinue}},
		{name: "valid continue with attempt", command: Command{Key: "k", Kind: CommandContinue, Expected: Expected{AttemptID: "att_1"}}},
		{name: "continue text at the limit", command: Command{Key: "k", Kind: CommandContinue, Text: long(16000)}},
		{name: "continue text too long", command: Command{Key: "k", Kind: CommandContinue, Text: long(16001)}, wantErr: "text"},
		{name: "valid cancel", command: Command{Key: "k", Kind: CommandCancel}},
		{name: "valid retry", command: Command{Key: "k", Kind: CommandRetry, MessageID: "msg_" + strings.Repeat("a", 32)}},
		{name: "retry without message", command: Command{Key: "k", Kind: CommandRetry}, wantErr: "message_id"},
		{name: "retry with blank message", command: Command{Key: "k", Kind: CommandRetry, MessageID: "  "}, wantErr: "message_id"},
		{name: "retry with a conversation id", command: Command{Key: "k", Kind: CommandRetry, MessageID: "conv_" + strings.Repeat("a", 32)}, wantErr: "message_id"},
		{name: "retry with a bare prefix", command: Command{Key: "k", Kind: CommandRetry, MessageID: "msg_"}, wantErr: "message_id"},
		{name: "valid retry with a short id", command: Command{Key: "k", Kind: CommandRetry, MessageID: "msg_6a20f1c4"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateCommand(test.command)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateCommand() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateCommand() error = nil, want error mentioning %q", test.wantErr)
			}
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("ValidateCommand() error = %v, want ErrInvalidCommand", err)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ValidateCommand() error = %q, want mention of %q", err.Error(), test.wantErr)
			}
		})
	}
}

func TestRequestHash(t *testing.T) {
	t.Parallel()
	base := Command{Key: "k", Kind: CommandAnswer, QuestionID: "q_1", Answers: map[string][]string{"b": {"2"}, "a": {"1"}}, Expected: Expected{AttemptID: "att_1"}}
	same := Command{Key: "k", Kind: CommandAnswer, QuestionID: "q_1", Answers: map[string][]string{"a": {"1"}, "b": {"2"}}, Expected: Expected{AttemptID: "att_1"}}
	first := RequestHash(base)
	if len(first) != 64 {
		t.Fatalf("RequestHash() = %q, want 64 hex characters", first)
	}
	if first != RequestHash(same) {
		t.Fatal("map key order must not change the hash")
	}
	if first != RequestHash(base) {
		t.Fatal("hash must be deterministic")
	}
	tests := []struct {
		name   string
		mutate func(Command) Command
	}{
		{name: "text", mutate: func(c Command) Command { c.Text = "x"; return c }},
		{name: "kind", mutate: func(c Command) Command { c.Kind = CommandMessage; return c }},
		{name: "question", mutate: func(c Command) Command { c.QuestionID = "q_2"; return c }},
		{name: "answers", mutate: func(c Command) Command { c.Answers = map[string][]string{"a": {"1"}}; return c }},
		{name: "expected", mutate: func(c Command) Command { c.Expected.TurnID = "turn"; return c }},
		{name: "message id", mutate: func(c Command) Command { c.MessageID = "msg_1"; return c }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if RequestHash(test.mutate(base)) == first {
				t.Fatalf("changing %s must change the hash", test.name)
			}
		})
	}
	// The key itself is part of the identity but not the payload; two commands
	// with different keys and the same payload share a hash.
	other := base
	other.Key = "k2"
	if RequestHash(other) != first {
		t.Fatal("key must not participate in the payload hash")
	}
}

func TestCommandJSONShape(t *testing.T) {
	t.Parallel()
	var command Command
	raw := `{"key":"abc","kind":"answer","text":"","question_id":"q_1","answers":{"p":["x"]},"expected":{"attempt_id":"att_1","turn_id":null}}`
	if err := json.Unmarshal([]byte(raw), &command); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if command.Key != "abc" || command.Kind != CommandAnswer || command.QuestionID != "q_1" || len(command.Answers["p"]) != 1 || command.Answers["p"][0] != "x" || command.Expected.AttemptID != "att_1" || command.Expected.TurnID != "" {
		t.Fatalf("command = %#v", command)
	}
	encoded, err := json.Marshal(Receipt{Key: "abc", Kind: CommandMessage, Status: DeliverySaved, MessageID: "msg_1", UpdatedAt: time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	want := `{"key":"abc","kind":"message","status":"saved","message_id":"msg_1","question_id":null,"error":null,"updated_at":"2026-09-09T10:00:00Z"}`
	if string(encoded) != want {
		t.Fatalf("receipt json = %s, want %s", encoded, want)
	}
	encoded, err = json.Marshal(Receipt{Key: "abc", Kind: CommandAnswer, Status: DeliveryRejected, QuestionID: "q_1", Error: &ReceiptError{Code: "stale_execution", Message: "gone"}})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(encoded), `"question_id":"q_1"`) || !strings.Contains(string(encoded), `"error":{"code":"stale_execution","message":"gone"}`) || !strings.Contains(string(encoded), `"message_id":null`) {
		t.Fatalf("receipt json = %s", encoded)
	}
}
