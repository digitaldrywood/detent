// Package conversation owns the domain vocabulary of the hosted conversation
// product: identifiers, delivery and execution states, command envelopes,
// receipts, validation and restart normalization. It has no database or HTTP
// code; the hub owns storage, HTTP, streaming, runner binding and control
// delivery.
package conversation

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const (
	conversationPrefix = "conv_"
	messagePrefix      = "msg_"
	questionPrefix     = "q_"
	identifierHexLen   = 32
)

// ErrInvalidID reports an identifier that does not match its expected shape.
var ErrInvalidID = errors.New("conversation: invalid identifier")

// NewConversationID returns a fresh conversation identifier ("conv_" + 32 hex).
func NewConversationID() string { return conversationPrefix + randomHex() }

// NewMessageID returns a fresh message identifier ("msg_" + 32 hex).
func NewMessageID() string { return messagePrefix + randomHex() }

// NewQuestionID returns a fresh question identifier ("q_" + 32 hex).
func NewQuestionID() string { return questionPrefix + randomHex() }

// ValidateConversationID reports whether value is a well-formed conversation identifier.
func ValidateConversationID(value string) error {
	return validateID("conversation", conversationPrefix, value)
}

// ValidateMessageID reports whether value is a well-formed message identifier.
func ValidateMessageID(value string) error {
	return validateID("message", messagePrefix, value)
}

// ValidateQuestionID reports whether value is a well-formed question identifier.
func ValidateQuestionID(value string) error {
	return validateID("question", questionPrefix, value)
}

func randomHex() string {
	var buf [identifierHexLen / 2]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand never fails on supported platforms; a failure here is
		// unrecoverable for identifier generation.
		panic(fmt.Sprintf("conversation: random identifier: %v", err))
	}
	return hex.EncodeToString(buf[:])
}

func validateID(kind, prefix, value string) error {
	rest, ok := strings.CutPrefix(value, prefix)
	if !ok {
		return fmt.Errorf("%w: %s id %q must start with %q", ErrInvalidID, kind, value, prefix)
	}
	if len(rest) != identifierHexLen {
		return fmt.Errorf("%w: %s id %q must have %d hex characters", ErrInvalidID, kind, value, identifierHexLen)
	}
	for _, r := range rest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("%w: %s id %q must be lowercase hex", ErrInvalidID, kind, value)
		}
	}
	return nil
}

// Visibility describes who can read a conversation.
type Visibility string

// Visibility values.
const (
	VisibilityPrivate Visibility = "private"
	VisibilityShared  Visibility = "shared"
)

// Valid reports whether the visibility is a known value.
func (v Visibility) Valid() bool { return v == VisibilityPrivate || v == VisibilityShared }

// Status groups a conversation for the sidebar. Settled replaces Archive
// (decisions section 14): a settled conversation is finished and idle, not
// read-only, and any accepted command or new message unsettles it.
type Status string

// Status values.
const (
	StatusActive  Status = "active"
	StatusSettled Status = "settled"
)

// Valid reports whether the status is a known value.
func (s Status) Valid() bool { return s == StatusActive || s == StatusSettled }

// Delivery is the position of a message or control on the delivery ladder.
type Delivery string

// Delivery values, in ladder order followed by the terminal outcomes.
const (
	DeliverySaved       Delivery = "saved"
	DeliveryQueued      Delivery = "queued"
	DeliverySending     Delivery = "sending"
	DeliverySent        Delivery = "sent"
	DeliveryDelivered   Delivery = "delivered"
	DeliveryResponding  Delivery = "responding"
	DeliveryCompleted   Delivery = "completed"
	DeliveryInterrupted Delivery = "interrupted"
	DeliveryRejected    Delivery = "rejected"
	DeliveryFailed      Delivery = "failed"
	DeliveryUnknown     Delivery = "unknown"
)

var deliveries = []Delivery{DeliverySaved, DeliveryQueued, DeliverySending, DeliverySent, DeliveryDelivered, DeliveryResponding, DeliveryCompleted, DeliveryInterrupted, DeliveryRejected, DeliveryFailed, DeliveryUnknown}

// Deliveries returns every delivery value in ladder order.
func Deliveries() []Delivery { return slices.Clone(deliveries) }

// Valid reports whether the delivery is a known value.
func (d Delivery) Valid() bool { return slices.Contains(deliveries, d) }

// Terminal reports whether the delivery will not change again.
func (d Delivery) Terminal() bool {
	switch d {
	case DeliveryCompleted, DeliveryInterrupted, DeliveryRejected, DeliveryFailed, DeliveryUnknown:
		return true
	default:
		return false
	}
}

// ExecutionStatus describes the runner bound to a conversation.
type ExecutionStatus string

// ExecutionStatus values.
const (
	ExecutionIdle             ExecutionStatus = "idle"
	ExecutionWaitingForRunner ExecutionStatus = "waiting_for_runner"
	ExecutionStarting         ExecutionStatus = "starting"
	ExecutionRunning          ExecutionStatus = "running"
	ExecutionWaitingInput     ExecutionStatus = "waiting_input"
	ExecutionInterrupting     ExecutionStatus = "interrupting"
	ExecutionCompleted        ExecutionStatus = "completed"
	ExecutionInterrupted      ExecutionStatus = "interrupted"
	ExecutionFailed           ExecutionStatus = "failed"
	ExecutionUnknown          ExecutionStatus = "unknown"
)

var executionStatuses = []ExecutionStatus{ExecutionIdle, ExecutionWaitingForRunner, ExecutionStarting, ExecutionRunning, ExecutionWaitingInput, ExecutionInterrupting, ExecutionCompleted, ExecutionInterrupted, ExecutionFailed, ExecutionUnknown}

// ExecutionStatuses returns every execution status value.
func ExecutionStatuses() []ExecutionStatus { return slices.Clone(executionStatuses) }

// Valid reports whether the execution status is a known value.
func (s ExecutionStatus) Valid() bool { return slices.Contains(executionStatuses, s) }

// Terminal reports whether the execution has ended.
func (s ExecutionStatus) Terminal() bool {
	switch s {
	case ExecutionCompleted, ExecutionInterrupted, ExecutionFailed, ExecutionUnknown:
		return true
	default:
		return false
	}
}

// QuestionStatus describes a pending request for user input.
type QuestionStatus string

// QuestionStatus values.
const (
	QuestionPending  QuestionStatus = "pending"
	QuestionSending  QuestionStatus = "sending"
	QuestionSent     QuestionStatus = "sent"
	QuestionAnswered QuestionStatus = "answered"
	QuestionExpired  QuestionStatus = "expired"
	QuestionUnknown  QuestionStatus = "unknown"
)

var questionStatuses = []QuestionStatus{QuestionPending, QuestionSending, QuestionSent, QuestionAnswered, QuestionExpired, QuestionUnknown}

// QuestionStatuses returns every question status value.
func QuestionStatuses() []QuestionStatus { return slices.Clone(questionStatuses) }

// Valid reports whether the question status is a known value.
func (s QuestionStatus) Valid() bool { return slices.Contains(questionStatuses, s) }

// Role is the author role of a message.
type Role string

// Role values.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// Valid reports whether the role is a known value.
func (r Role) Valid() bool { return r == RoleUser || r == RoleAssistant || r == RoleSystem }

// MessageKind describes what a message carries.
type MessageKind string

// MessageKind values.
const (
	MessageText      MessageKind = "text"
	MessageAnswer    MessageKind = "answer"
	MessageInterrupt MessageKind = "interrupt"
	MessageContinue  MessageKind = "continue"
	MessageTool      MessageKind = "tool"
	MessageStatus    MessageKind = "status"
)

var messageKinds = []MessageKind{MessageText, MessageAnswer, MessageInterrupt, MessageContinue, MessageTool, MessageStatus}

// Valid reports whether the message kind is a known value.
func (k MessageKind) Valid() bool { return slices.Contains(messageKinds, k) }

// CommandKind describes a client command envelope.
type CommandKind string

// CommandKind values.
const (
	CommandMessage   CommandKind = "message"
	CommandAnswer    CommandKind = "answer"
	CommandInterrupt CommandKind = "interrupt"
	CommandContinue  CommandKind = "continue"
	CommandCancel    CommandKind = "cancel"
	// CommandRetry asks the hub to hand a message that could not be
	// established to the next worker. Nothing is ever replayed
	// automatically (decisions section 10.3).
	CommandRetry CommandKind = "retry"
)

var commandKinds = []CommandKind{CommandMessage, CommandAnswer, CommandInterrupt, CommandContinue, CommandCancel, CommandRetry}

// Valid reports whether the command kind is a known value.
func (k CommandKind) Valid() bool { return slices.Contains(commandKinds, k) }

// ActorKind describes who produced a message.
type ActorKind string

// ActorKind values.
const (
	ActorHuman       ActorKind = "human"
	ActorRunner      ActorKind = "runner"
	ActorCoordinator ActorKind = "coordinator"
)

// Valid reports whether the actor kind is a known value.
func (k ActorKind) Valid() bool {
	return k == ActorHuman || k == ActorRunner || k == ActorCoordinator
}

// Actor identifies the author of a message.
type Actor struct {
	Kind        ActorKind `json:"kind"`
	PrincipalID string    `json:"principal_id"`
}
