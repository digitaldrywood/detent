package conversation

import (
	"regexp"
	"strings"
	"testing"
)

func TestNewIdentifiers(t *testing.T) {
	t.Parallel()
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	tests := []struct {
		name     string
		generate func() string
		prefix   string
		validate func(string) error
	}{
		{name: "conversation", generate: NewConversationID, prefix: "conv_", validate: ValidateConversationID},
		{name: "message", generate: NewMessageID, prefix: "msg_", validate: ValidateMessageID},
		{name: "question", generate: NewQuestionID, prefix: "q_", validate: ValidateQuestionID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			first := test.generate()
			second := test.generate()
			if !strings.HasPrefix(first, test.prefix) {
				t.Fatalf("%s id = %q, want prefix %q", test.name, first, test.prefix)
			}
			if !hex32.MatchString(strings.TrimPrefix(first, test.prefix)) {
				t.Fatalf("%s id = %q, want 32 hex characters after prefix", test.name, first)
			}
			if first == second {
				t.Fatalf("generated identifiers collide: %q", first)
			}
			if err := test.validate(first); err != nil {
				t.Fatalf("validate(%q) error = %v", first, err)
			}
		})
	}
}

func TestValidateIdentifiers(t *testing.T) {
	t.Parallel()
	valid := strings.Repeat("ab", 16)
	tests := []struct {
		name     string
		validate func(string) error
		value    string
		wantErr  bool
	}{
		{name: "conversation valid", validate: ValidateConversationID, value: "conv_" + valid},
		{name: "conversation wrong prefix", validate: ValidateConversationID, value: "msg_" + valid, wantErr: true},
		{name: "conversation empty", validate: ValidateConversationID, value: "", wantErr: true},
		{name: "conversation short", validate: ValidateConversationID, value: "conv_abc", wantErr: true},
		{name: "conversation uppercase", validate: ValidateConversationID, value: "conv_" + strings.ToUpper(valid), wantErr: true},
		{name: "conversation non hex", validate: ValidateConversationID, value: "conv_" + strings.Repeat("zz", 16), wantErr: true},
		{name: "message valid", validate: ValidateMessageID, value: "msg_" + valid},
		{name: "message wrong prefix", validate: ValidateMessageID, value: "conv_" + valid, wantErr: true},
		{name: "message too long", validate: ValidateMessageID, value: "msg_" + valid + "a", wantErr: true},
		{name: "question valid", validate: ValidateQuestionID, value: "q_" + valid},
		{name: "question wrong prefix", validate: ValidateQuestionID, value: "msg_" + valid, wantErr: true},
		{name: "question empty", validate: ValidateQuestionID, value: "", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.validate(test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("validate(%q) error = %v, wantErr %v", test.value, err, test.wantErr)
			}
		})
	}
}

func TestEnumValidity(t *testing.T) {
	t.Parallel()
	t.Run("visibility", func(t *testing.T) {
		t.Parallel()
		for _, value := range []Visibility{VisibilityPrivate, VisibilityShared} {
			if !value.Valid() {
				t.Fatalf("%q should be valid", value)
			}
		}
		if Visibility("public").Valid() || Visibility("").Valid() {
			t.Fatal("unknown visibility should be invalid")
		}
	})
	t.Run("status", func(t *testing.T) {
		t.Parallel()
		for _, value := range []Status{StatusActive, StatusSettled} {
			if !value.Valid() {
				t.Fatalf("%q should be valid", value)
			}
		}
		if Status("deleted").Valid() {
			t.Fatal("unknown status should be invalid")
		}
	})
	t.Run("delivery", func(t *testing.T) {
		t.Parallel()
		want := []Delivery{DeliverySaved, DeliveryQueued, DeliverySending, DeliverySent, DeliveryDelivered, DeliveryResponding, DeliveryCompleted, DeliveryInterrupted, DeliveryRejected, DeliveryFailed, DeliveryUnknown}
		if len(Deliveries()) != len(want) {
			t.Fatalf("Deliveries() = %v, want %v", Deliveries(), want)
		}
		for i, value := range want {
			if Deliveries()[i] != value || !value.Valid() {
				t.Fatalf("delivery %d = %q, want %q valid", i, Deliveries()[i], value)
			}
		}
		if Delivery("lost").Valid() {
			t.Fatal("unknown delivery should be invalid")
		}
		for _, value := range []Delivery{DeliveryCompleted, DeliveryInterrupted, DeliveryRejected, DeliveryFailed, DeliveryUnknown} {
			if !value.Terminal() {
				t.Fatalf("%q should be terminal", value)
			}
		}
		for _, value := range []Delivery{DeliverySaved, DeliveryQueued, DeliverySending, DeliverySent, DeliveryDelivered, DeliveryResponding} {
			if value.Terminal() {
				t.Fatalf("%q should not be terminal", value)
			}
		}
	})
	t.Run("execution status", func(t *testing.T) {
		t.Parallel()
		want := []ExecutionStatus{ExecutionIdle, ExecutionWaitingForRunner, ExecutionStarting, ExecutionRunning, ExecutionWaitingInput, ExecutionInterrupting, ExecutionCompleted, ExecutionInterrupted, ExecutionFailed, ExecutionUnknown}
		if len(ExecutionStatuses()) != len(want) {
			t.Fatalf("ExecutionStatuses() = %v", ExecutionStatuses())
		}
		for i, value := range want {
			if ExecutionStatuses()[i] != value || !value.Valid() {
				t.Fatalf("execution status %d = %q, want %q valid", i, ExecutionStatuses()[i], value)
			}
		}
		if ExecutionStatus("paused").Valid() {
			t.Fatal("unknown execution status should be invalid")
		}
		for _, value := range []ExecutionStatus{ExecutionCompleted, ExecutionInterrupted, ExecutionFailed, ExecutionUnknown} {
			if !value.Terminal() {
				t.Fatalf("%q should be terminal", value)
			}
		}
		if ExecutionRunning.Terminal() || ExecutionIdle.Terminal() {
			t.Fatal("running and idle are not terminal")
		}
	})
	t.Run("question status", func(t *testing.T) {
		t.Parallel()
		want := []QuestionStatus{QuestionPending, QuestionSending, QuestionSent, QuestionAnswered, QuestionExpired, QuestionUnknown}
		if len(QuestionStatuses()) != len(want) {
			t.Fatalf("QuestionStatuses() = %v", QuestionStatuses())
		}
		for i, value := range want {
			if QuestionStatuses()[i] != value || !value.Valid() {
				t.Fatalf("question status %d = %q, want %q valid", i, QuestionStatuses()[i], value)
			}
		}
		if QuestionStatus("open").Valid() {
			t.Fatal("unknown question status should be invalid")
		}
	})
	t.Run("role", func(t *testing.T) {
		t.Parallel()
		for _, value := range []Role{RoleUser, RoleAssistant, RoleSystem} {
			if !value.Valid() {
				t.Fatalf("%q should be valid", value)
			}
		}
		if Role("tool").Valid() {
			t.Fatal("unknown role should be invalid")
		}
	})
	t.Run("message kind", func(t *testing.T) {
		t.Parallel()
		for _, value := range []MessageKind{MessageText, MessageAnswer, MessageInterrupt, MessageContinue, MessageTool, MessageStatus} {
			if !value.Valid() {
				t.Fatalf("%q should be valid", value)
			}
		}
		if MessageKind("image").Valid() {
			t.Fatal("unknown message kind should be invalid")
		}
	})
	t.Run("command kind", func(t *testing.T) {
		t.Parallel()
		for _, value := range []CommandKind{CommandMessage, CommandAnswer, CommandInterrupt, CommandContinue, CommandCancel} {
			if !value.Valid() {
				t.Fatalf("%q should be valid", value)
			}
		}
		if CommandKind("restart").Valid() || CommandKind("").Valid() {
			t.Fatal("unknown command kind should be invalid")
		}
	})
	t.Run("actor kind", func(t *testing.T) {
		t.Parallel()
		for _, value := range []ActorKind{ActorHuman, ActorRunner, ActorCoordinator} {
			if !value.Valid() {
				t.Fatalf("%q should be valid", value)
			}
		}
		if ActorKind("bot").Valid() {
			t.Fatal("unknown actor kind should be invalid")
		}
	})
	t.Run("event types", func(t *testing.T) {
		t.Parallel()
		want := []EventType{EventConversationUpdated, EventMessageAccepted, EventMessageDelta, EventMessageUpdated, EventQuestionOpened, EventQuestionUpdated, EventExecutionUpdated, EventCommandReceipt, EventHeartbeat, EventClosed}
		wantNames := []string{"conversation.updated", "message.accepted", "message.delta", "message.updated", "question.opened", "question.updated", "execution.updated", "command.receipt", "heartbeat", "closed"}
		for i, value := range want {
			if string(value) != wantNames[i] {
				t.Fatalf("event type %d = %q, want %q", i, value, wantNames[i])
			}
			if !value.Valid() {
				t.Fatalf("%q should be valid", value)
			}
		}
		if EventType("message.deleted").Valid() {
			t.Fatal("unknown event type should be invalid")
		}
	})
}

func TestOwnerMatches(t *testing.T) {
	t.Parallel()
	owner := Owner{AttemptID: "att_1", RunID: "run_1", LeaseID: "lease_1", FencingToken: 7, RunnerID: "runner_1", MachineID: "machine_1", ThreadID: "thread_1", TurnID: "turn_1"}
	tests := []struct {
		name     string
		owner    Owner
		expected Expected
		wantErr  bool
	}{
		{name: "wildcards match anything", owner: owner, expected: Expected{}},
		{name: "attempt matches", owner: owner, expected: Expected{AttemptID: "att_1"}},
		{name: "attempt and turn match", owner: owner, expected: Expected{AttemptID: "att_1", TurnID: "turn_1"}},
		{name: "turn only matches", owner: owner, expected: Expected{TurnID: "turn_1"}},
		{name: "attempt mismatch", owner: owner, expected: Expected{AttemptID: "att_2"}, wantErr: true},
		{name: "turn mismatch", owner: owner, expected: Expected{AttemptID: "att_1", TurnID: "turn_2"}, wantErr: true},
		{name: "no owner but attempt expected", owner: Owner{}, expected: Expected{AttemptID: "att_1"}, wantErr: true},
		{name: "no owner and wildcard", owner: Owner{}, expected: Expected{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.owner.Matches(test.expected)
			if (err != nil) != test.wantErr {
				t.Fatalf("Matches() error = %v, wantErr %v", err, test.wantErr)
			}
			if err != nil && !isStaleExecution(err) {
				t.Fatalf("Matches() error = %v, want ErrStaleExecution", err)
			}
		})
	}
}
