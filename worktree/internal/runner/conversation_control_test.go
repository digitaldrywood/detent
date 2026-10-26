package runner

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAgentControlValidate(t *testing.T) {
	t.Parallel()

	check := func(context.Context) error { return nil }
	tests := []struct {
		name    string
		control AgentControl
		wantErr error
	}{
		{
			name:    "message",
			control: AgentControl{Kind: AgentControlMessage, ThreadID: "thread", TurnID: "turn", Text: "hi", Check: check, Reply: make(chan error, 1)},
		},
		{
			name:    "interrupt",
			control: AgentControl{Kind: AgentControlInterrupt, ThreadID: "thread", TurnID: "turn", Check: check, Reply: make(chan error, 1)},
		},
		{
			name:    "answer",
			control: AgentControl{Kind: AgentControlAnswer, ThreadID: "thread", TurnID: "turn", RequestID: "1001", Check: check, Reply: make(chan error, 1)},
		},
		{
			name:    "nil reply",
			control: AgentControl{Kind: AgentControlMessage, ThreadID: "thread", TurnID: "turn", Check: check},
			wantErr: ErrInvalidConversationControl,
		},
		{
			name:    "unbuffered reply",
			control: AgentControl{Kind: AgentControlMessage, ThreadID: "thread", TurnID: "turn", Check: check, Reply: make(chan error)},
			wantErr: ErrInvalidConversationControl,
		},
		{
			name:    "missing check",
			control: AgentControl{Kind: AgentControlMessage, ThreadID: "thread", TurnID: "turn", Reply: make(chan error, 1)},
			wantErr: ErrInvalidConversationControl,
		},
		{
			name:    "missing thread",
			control: AgentControl{Kind: AgentControlMessage, TurnID: "turn", Check: check, Reply: make(chan error, 1)},
			wantErr: ErrInvalidConversationControl,
		},
		{
			name:    "missing turn",
			control: AgentControl{Kind: AgentControlMessage, ThreadID: "thread", Check: check, Reply: make(chan error, 1)},
			wantErr: ErrInvalidConversationControl,
		},
		{
			name:    "answer without request",
			control: AgentControl{Kind: AgentControlAnswer, ThreadID: "thread", TurnID: "turn", Check: check, Reply: make(chan error, 1)},
			wantErr: ErrInvalidConversationControl,
		},
		{
			name:    "unknown kind",
			control: AgentControl{Kind: AgentControlKind("steer"), ThreadID: "thread", TurnID: "turn", Check: check, Reply: make(chan error, 1)},
			wantErr: ErrUnsupportedConversationControl,
		},
		{
			name:    "empty kind",
			control: AgentControl{ThreadID: "thread", TurnID: "turn", Check: check, Reply: make(chan error, 1)},
			wantErr: ErrUnsupportedConversationControl,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.control.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestAgentConversationControlEffectiveQuestionTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		control *AgentConversationControl
		want    time.Duration
	}{
		{name: "nil control", want: DefaultConversationQuestionTimeout},
		{name: "zero", control: &AgentConversationControl{}, want: DefaultConversationQuestionTimeout},
		{name: "negative", control: &AgentConversationControl{QuestionTimeout: -time.Second}, want: DefaultConversationQuestionTimeout},
		{name: "explicit", control: &AgentConversationControl{QuestionTimeout: time.Minute}, want: time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.control.EffectiveQuestionTimeout(); got != tt.want {
				t.Fatalf("EffectiveQuestionTimeout() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestConversationControlSentinelsAreDistinct(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		ErrStaleConversationControl,
		ErrConversationQuestionExpired,
		ErrUnsupportedConversationControl,
		ErrInvalidConversationControl,
	}
	for i, left := range sentinels {
		for j, right := range sentinels {
			if i != j && errors.Is(left, right) {
				t.Fatalf("sentinel %d matches sentinel %d: %v", i, j, left)
			}
		}
	}
	if DefaultConversationQuestionTimeout != 24*time.Hour {
		t.Fatalf("DefaultConversationQuestionTimeout = %s, want 24h", DefaultConversationQuestionTimeout)
	}
}
