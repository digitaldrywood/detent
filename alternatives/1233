package conversation

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ErrInvalidAnswers reports answers that do not match a question's prompts.
var ErrInvalidAnswers = errors.New("invalid answers")

// ErrInvalidPrompt reports a request for input the hub will not store.
var ErrInvalidPrompt = errors.New("invalid prompt")

// Limits on a request for user input. A runner is trusted to be correct,
// not to be bounded, and every prompt is rendered in a browser.
const (
	MaxPrompts             = 20
	MaxPromptOptions       = 10
	MaxPromptIDBytes       = 200
	MaxPromptHeaderBytes   = 500
	MaxPromptQuestionBytes = 4000
	MaxOptionLabelBytes    = 500
	MaxOptionDetailBytes   = 2000
)

// ValidatePrompts enforces the bounds on one request for user input.
func ValidatePrompts(prompts []Prompt) error {
	if len(prompts) == 0 {
		return fmt.Errorf("%w: at least one prompt is required", ErrInvalidPrompt)
	}
	if len(prompts) > MaxPrompts {
		return fmt.Errorf("%w: at most %d prompts are supported", ErrInvalidPrompt, MaxPrompts)
	}
	seen := make([]string, 0, len(prompts))
	for _, prompt := range prompts {
		id := strings.TrimSpace(prompt.ID)
		if id == "" || len(prompt.ID) > MaxPromptIDBytes {
			return fmt.Errorf("%w: every prompt needs an id of at most %d bytes", ErrInvalidPrompt, MaxPromptIDBytes)
		}
		if slices.Contains(seen, id) {
			return fmt.Errorf("%w: prompt %q is repeated", ErrInvalidPrompt, id)
		}
		seen = append(seen, id)
		if len(prompt.Header) > MaxPromptHeaderBytes || len(prompt.Question) > MaxPromptQuestionBytes {
			return fmt.Errorf("%w: prompt %q has a header or question that is too long", ErrInvalidPrompt, id)
		}
		if len(prompt.Options) > MaxPromptOptions {
			return fmt.Errorf("%w: prompt %q offers more than %d options", ErrInvalidPrompt, id, MaxPromptOptions)
		}
		for _, option := range prompt.Options {
			if strings.TrimSpace(option.Label) == "" || len(option.Label) > MaxOptionLabelBytes || len(option.Description) > MaxOptionDetailBytes {
				return fmt.Errorf("%w: prompt %q has an option label or description that is empty or too long", ErrInvalidPrompt, id)
			}
		}
	}
	return nil
}

// Option is one selectable answer for a prompt.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Prompt is one question inside a request for user input.
type Prompt struct {
	ID       string   `json:"id"`
	Header   string   `json:"header"`
	Question string   `json:"question"`
	Options  []Option `json:"options"`
	FreeText bool     `json:"free_text"`
	// Multiple allows more than one option to be selected. It is optional
	// and absent means a single choice: the answers map already carries an
	// array per prompt, but only a prompt that says so renders as
	// checkboxes rather than radio buttons.
	Multiple bool `json:"multiple,omitempty"`
}

// Question is a pending request for user input bound to an owner generation.
type Question struct {
	ID             string
	ConversationID string
	MessageID      string
	Status         QuestionStatus
	Owner          Owner
	Prompts        []Prompt
	Answers        map[string][]string
	AnsweredBy     string
	ExpiresAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// questionWire is the contract shape of a question resource.
type questionWire struct {
	ID             string              `json:"id"`
	ConversationID string              `json:"conversation_id"`
	MessageID      string              `json:"message_id"`
	Status         QuestionStatus      `json:"status"`
	Owner          Expected            `json:"owner"`
	Questions      []Prompt            `json:"questions"`
	Answers        map[string][]string `json:"answers"`
	AnsweredBy     *string             `json:"answered_by"`
	ExpiresAt      *time.Time          `json:"expires_at"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
}

// MarshalJSON renders the contract shape; the owner exposes only the attempt
// and turn, never lease or fencing details.
func (q Question) MarshalJSON() ([]byte, error) {
	prompts := q.Prompts
	if prompts == nil {
		prompts = []Prompt{}
	}
	answers := q.Answers
	if answers == nil {
		answers = map[string][]string{}
	}
	return marshalJSON(questionWire{
		ID:             q.ID,
		ConversationID: q.ConversationID,
		MessageID:      q.MessageID,
		Status:         q.Status,
		Owner:          Expected{AttemptID: q.Owner.AttemptID, TurnID: q.Owner.TurnID},
		Questions:      prompts,
		Answers:        answers,
		AnsweredBy:     nullable(q.AnsweredBy),
		ExpiresAt:      q.ExpiresAt,
		CreatedAt:      q.CreatedAt,
		UpdatedAt:      q.UpdatedAt,
	})
}

// ValidateAnswers requires exactly one non-empty answer set per prompt,
// rejects answers for prompts the question does not have, and rejects a
// value that is not one of the prompt's option labels unless the prompt
// allows free text (decisions section 10.9).
func ValidateAnswers(question Question, answers map[string][]string) error {
	known := make([]string, 0, len(question.Prompts))
	for _, prompt := range question.Prompts {
		known = append(known, prompt.ID)
		values, ok := answers[prompt.ID]
		if !ok || len(values) == 0 {
			return fmt.Errorf("%w: prompt %q has no answer", ErrInvalidAnswers, prompt.ID)
		}
		if err := validatePromptValues(prompt, values); err != nil {
			return err
		}
	}
	unknown := make([]string, 0)
	for id := range answers {
		if !slices.Contains(known, id) {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return fmt.Errorf("%w: unknown prompt %s", ErrInvalidAnswers, strings.Join(unknown, ", "))
	}
	return nil
}

// validatePromptValues rejects a value the prompt does not offer. A prompt
// that allows free text, or offers no options at all, accepts any value.
func validatePromptValues(prompt Prompt, values []string) error {
	if prompt.FreeText || len(prompt.Options) == 0 {
		return nil
	}
	labels := make([]string, 0, len(prompt.Options))
	for _, option := range prompt.Options {
		labels = append(labels, option.Label)
	}
	for _, value := range values {
		if !slices.Contains(labels, value) {
			return fmt.Errorf("%w: prompt %q does not offer %q; choose one of %s", ErrInvalidAnswers, prompt.ID, value, strings.Join(labels, ", "))
		}
	}
	return nil
}
