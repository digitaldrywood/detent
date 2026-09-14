package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Limits on command envelopes.
const (
	MaxCommandKeyBytes  = 128
	MaxMessageTextBytes = 16000
	MaxAnswerValueBytes = 4000
)

// ErrInvalidCommand reports a command envelope that fails validation.
var ErrInvalidCommand = errors.New("invalid command")

// Command is the client command envelope.
type Command struct {
	Key  string      `json:"key"`
	Kind CommandKind `json:"kind"`
	Text string      `json:"text,omitempty"`
	// Attachments names the uploads bound to a message command. The hub
	// checks ownership, the unsent state and the total size; the envelope
	// only checks the shape (decisions section 17.1).
	Attachments []string            `json:"attachments,omitempty"`
	QuestionID  string              `json:"question_id,omitempty"`
	Answers     map[string][]string `json:"answers,omitempty"`
	// MessageID names the message a retry re-queues.
	MessageID string   `json:"message_id,omitempty"`
	Expected  Expected `json:"expected"`
}

// ValidateCommand enforces the envelope rules from the contract.
func ValidateCommand(command Command) error {
	if strings.TrimSpace(command.Key) == "" || len(command.Key) > MaxCommandKeyBytes {
		return fmt.Errorf("%w: key is required and must be at most %d bytes", ErrInvalidCommand, MaxCommandKeyBytes)
	}
	if !command.Kind.Valid() {
		return fmt.Errorf("%w: kind %q is not supported", ErrInvalidCommand, command.Kind)
	}
	if len(command.Attachments) > 0 && command.Kind != CommandMessage {
		return fmt.Errorf("%w: only a message carries attachments", ErrInvalidCommand)
	}
	switch command.Kind {
	case CommandMessage:
		// A message carries text, attachments, or both: a dropped file with
		// no words is still a message (decisions section 17.1).
		if strings.TrimSpace(command.Text) == "" && len(command.Attachments) == 0 {
			return fmt.Errorf("%w: text or attachments are required for a message", ErrInvalidCommand)
		}
		if len(command.Text) > MaxMessageTextBytes {
			return fmt.Errorf("%w: text must be at most %d bytes", ErrInvalidCommand, MaxMessageTextBytes)
		}
		if err := ValidateAttachmentSelection(command.Attachments); err != nil {
			return err
		}
	case CommandAnswer:
		if strings.TrimSpace(command.QuestionID) == "" {
			return fmt.Errorf("%w: question_id is required for an answer", ErrInvalidCommand)
		}
		if len(command.Answers) == 0 {
			return fmt.Errorf("%w: answers are required for an answer", ErrInvalidCommand)
		}
		for prompt, values := range command.Answers {
			if strings.TrimSpace(prompt) == "" {
				return fmt.Errorf("%w: answers must name a prompt", ErrInvalidCommand)
			}
			if len(values) == 0 {
				return fmt.Errorf("%w: answers for %q must not be empty", ErrInvalidCommand, prompt)
			}
			for _, value := range values {
				if len(value) > MaxAnswerValueBytes {
					return fmt.Errorf("%w: answers for %q must be at most %d bytes each", ErrInvalidCommand, prompt, MaxAnswerValueBytes)
				}
			}
		}
	case CommandInterrupt:
		if strings.TrimSpace(command.Expected.AttemptID) == "" {
			return fmt.Errorf("%w: expected.attempt_id is required for an interrupt", ErrInvalidCommand)
		}
	case CommandRetry:
		if strings.TrimSpace(command.MessageID) == "" {
			return fmt.Errorf("%w: message_id is required for a retry", ErrInvalidCommand)
		}
		// The envelope only checks the shape; the hub resolves the message
		// in the conversation and refuses one that is not retryable.
		if !strings.HasPrefix(command.MessageID, messagePrefix) || len(command.MessageID) <= len(messagePrefix) {
			return fmt.Errorf("%w: message_id must name a message (%q prefix)", ErrInvalidCommand, messagePrefix)
		}
	case CommandContinue:
		// A continue may carry guidance; it is stored as a message, so it
		// is bounded like one.
		if len(command.Text) > MaxMessageTextBytes {
			return fmt.Errorf("%w: text must be at most %d bytes", ErrInvalidCommand, MaxMessageTextBytes)
		}
	case CommandCancel:
	}
	return nil
}

// RequestHash returns the sha256 hex digest of the command's canonical
// payload (sorted keys, key excluded) so that a replay with the same key can
// be told apart from a conflicting payload.
func RequestHash(command Command) string {
	payload := struct {
		Kind        CommandKind         `json:"kind"`
		Text        string              `json:"text"`
		Attachments []string            `json:"attachments"`
		QuestionID  string              `json:"question_id"`
		Answers     map[string][]string `json:"answers"`
		MessageID   string              `json:"message_id"`
		Expected    Expected            `json:"expected"`
	}{command.Kind, command.Text, command.Attachments, command.QuestionID, command.Answers, command.MessageID, command.Expected}
	// encoding/json sorts map keys, which gives a canonical form for the
	// answers map; struct fields are emitted in declaration order.
	encoded, err := json.Marshal(payload)
	if err != nil {
		// The payload only contains strings and maps of strings; Marshal
		// cannot fail. Hash the error text so callers still get a value.
		encoded = []byte(err.Error())
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// ReceiptError explains why a command was rejected or failed.
type ReceiptError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Receipt is the durable outcome of a command keyed by its idempotency key.
type Receipt struct {
	Key        string        `json:"key"`
	Kind       CommandKind   `json:"kind"`
	Status     Delivery      `json:"status"`
	MessageID  string        `json:"message_id"`
	QuestionID string        `json:"question_id"`
	Error      *ReceiptError `json:"error"`
	UpdatedAt  time.Time     `json:"updated_at"`
}

// MarshalJSON renders empty identifiers as null to match the contract.
func (r Receipt) MarshalJSON() ([]byte, error) {
	type wire struct {
		Key        string        `json:"key"`
		Kind       CommandKind   `json:"kind"`
		Status     Delivery      `json:"status"`
		MessageID  *string       `json:"message_id"`
		QuestionID *string       `json:"question_id"`
		Error      *ReceiptError `json:"error"`
		UpdatedAt  time.Time     `json:"updated_at"`
	}
	return json.Marshal(wire{r.Key, r.Kind, r.Status, nullable(r.MessageID), nullable(r.QuestionID), r.Error, r.UpdatedAt})
}

func nullable(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
