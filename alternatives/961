package conversation

import (
	"encoding/json"
	"slices"
	"time"
)

// EventType is the frame type on a conversation event stream.
type EventType string

// EventType values.
const (
	EventConversationUpdated EventType = "conversation.updated"
	EventMessageAccepted     EventType = "message.accepted"
	EventMessageDelta        EventType = "message.delta"
	EventMessageUpdated      EventType = "message.updated"
	EventQuestionOpened      EventType = "question.opened"
	EventQuestionUpdated     EventType = "question.updated"
	EventExecutionUpdated    EventType = "execution.updated"
	EventCommandReceipt      EventType = "command.receipt"
	EventHeartbeat           EventType = "heartbeat"
	EventClosed              EventType = "closed"
)

var eventTypes = []EventType{EventConversationUpdated, EventMessageAccepted, EventMessageDelta, EventMessageUpdated, EventQuestionOpened, EventQuestionUpdated, EventExecutionUpdated, EventCommandReceipt, EventHeartbeat, EventClosed}

// Valid reports whether the event type is a known value.
func (t EventType) Valid() bool { return slices.Contains(eventTypes, t) }

// Event is one committed frame in a conversation's event log.
type Event struct {
	Seq       int64           `json:"seq"`
	Type      EventType       `json:"type"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
}
