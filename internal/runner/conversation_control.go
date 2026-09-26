package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AgentControlKind names a live command sent into an active provider turn.
type AgentControlKind string

const (
	// AgentControlMessage steers the active turn with an additional user message.
	AgentControlMessage AgentControlKind = "message"
	// AgentControlInterrupt asks the provider to stop the active turn.
	AgentControlInterrupt AgentControlKind = "interrupt"
	// AgentControlAnswer answers a pending provider question identified by RequestID.
	AgentControlAnswer AgentControlKind = "answer"
)

// DefaultConversationQuestionTimeout bounds how long a turn waits for a human
// answer when AgentConversationControl.QuestionTimeout is unset.
const DefaultConversationQuestionTimeout = 24 * time.Hour

var (
	// ErrStaleConversationControl reports a command that targets a thread, turn,
	// or request that is no longer (or not yet) active on the transport.
	ErrStaleConversationControl = errors.New("conversation control targets an inactive turn or request")
	// ErrConversationQuestionExpired reports that a pending provider question was
	// not answered within the configured question timeout.
	ErrConversationQuestionExpired = errors.New("conversation question expired before an answer arrived")
	// ErrUnsupportedConversationControl reports a control kind the transport
	// cannot deliver.
	ErrUnsupportedConversationControl = errors.New("unsupported conversation control")
	// ErrInvalidConversationControl reports a control that is missing required
	// delivery preconditions such as a buffered reply channel.
	ErrInvalidConversationControl = errors.New("invalid conversation control")
)

// AgentLiveBackend is optional: a plain RunTurn implementation cannot accept
// live input. Backends that can wire AgentConversationControl into the active
// turn report true.
type AgentLiveBackend interface{ SupportsLiveControl() bool }

// AgentControl is consumed by the active turn's transport owner. Check is
// called immediately before writing to the provider, after any queue delay, so
// ownership or revocation decided while the command waited is honoured. Reply
// must have capacity of at least one; exactly one error (possibly nil) is
// delivered on it once the provider acknowledges the write or the command is
// rejected.
type AgentControl struct {
	Kind      AgentControlKind
	ThreadID  string
	TurnID    string
	MessageID string
	Text      string
	// Attachments are the files the user attached to this message, already
	// fetched (decisions section 17.1).
	Attachments []AgentAttachment
	RequestID   string
	Answers     map[string][]string
	Check       func(context.Context) error
	Reply       chan error
}

// Validate reports whether the control satisfies its delivery preconditions.
func (c AgentControl) Validate() error {
	switch c.Kind {
	case AgentControlMessage, AgentControlInterrupt, AgentControlAnswer:
	default:
		return fmt.Errorf("%w: kind %q", ErrUnsupportedConversationControl, c.Kind)
	}
	if c.Reply == nil || cap(c.Reply) < 1 {
		return fmt.Errorf("%w: reply channel must be buffered", ErrInvalidConversationControl)
	}
	if c.Check == nil {
		return fmt.Errorf("%w: ownership check required", ErrInvalidConversationControl)
	}
	if c.ThreadID == "" || c.TurnID == "" {
		return fmt.Errorf("%w: thread and turn required", ErrInvalidConversationControl)
	}
	if c.Kind == AgentControlAnswer && c.RequestID == "" {
		return fmt.Errorf("%w: answer requires request id", ErrInvalidConversationControl)
	}
	return nil
}

// AgentInputRequest describes a provider question that is held pending until a
// matching AgentControlAnswer arrives.
type AgentInputRequest struct {
	ID        string
	ThreadID  string
	TurnID    string
	Questions json.RawMessage
}

// AgentConversationControl connects a live conversation to an active turn.
// Commands feeds controls into the transport; InputRequested is invoked when
// the provider asks a question that matches the bound thread and turn.
// QuestionTimeout bounds the wait for an answer (default 24 hours).
type AgentConversationControl struct {
	Commands        <-chan AgentControl
	InputRequested  func(AgentInputRequest) error
	QuestionTimeout time.Duration
}

// EffectiveQuestionTimeout returns QuestionTimeout, or the default when unset
// or non-positive. It is safe to call on a nil receiver.
func (c *AgentConversationControl) EffectiveQuestionTimeout() time.Duration {
	if c == nil || c.QuestionTimeout <= 0 {
		return DefaultConversationQuestionTimeout
	}
	return c.QuestionTimeout
}
