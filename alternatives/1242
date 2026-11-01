package scenariotest

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
)

func (s runnerSnapshot) attempt() string {
	if s.Conversation.Execution.AttemptID == nil {
		return ""
	}
	return *s.Conversation.Execution.AttemptID
}

func (s runnerSnapshot) delivery(key string) conversation.Delivery {
	for _, message := range s.Messages {
		if message.CommandKey != nil && *message.CommandKey == key {
			return message.Delivery
		}
	}
	return ""
}

func (s runnerSnapshot) assistantText() string {
	var text strings.Builder
	for _, message := range s.Messages {
		if message.Role == conversation.RoleAssistant && message.Kind == conversation.MessageText {
			text.WriteString(message.Text)
		}
	}
	return text.String()
}

type runnerSnapshot struct {
	Conversation struct {
		ID        string `json:"id"`
		Execution struct {
			Status       conversation.ExecutionStatus `json:"status"`
			AttemptID    *string                      `json:"attempt_id"`
			TurnID       *string                      `json:"turn_id"`
			Capabilities conversation.Capabilities    `json:"capabilities"`
		} `json:"execution"`
	} `json:"conversation"`
	Messages []struct {
		ID         string                   `json:"id"`
		AttemptID  *string                  `json:"attempt_id"`
		Role       conversation.Role        `json:"role"`
		Kind       conversation.MessageKind `json:"kind"`
		Text       string                   `json:"text"`
		Delivery   conversation.Delivery    `json:"delivery"`
		CommandKey *string                  `json:"command_key"`
	} `json:"messages"`
	Questions []struct {
		ID     string                      `json:"id"`
		Status conversation.QuestionStatus `json:"status"`
	} `json:"questions"`
}

type conversationArchiveFixture struct {
	mu      sync.Mutex
	content map[string][]byte
}

func NewConversationArchive() attachment.ArchiveContentStore {
	return &conversationArchiveFixture{}
}

func (f *conversationArchiveFixture) ReadContent(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := ref.Validate(organization); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.content[ref.Key]
	if !ok {
		return nil, errors.New("archive content missing")
	}
	return bytes.Clone(content), nil
}

func (f *conversationArchiveFixture) PublishContent(_ context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(content))
	if err != nil {
		return ref, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.content == nil {
		f.content = map[string][]byte{}
	}
	f.content[ref.Key] = bytes.Clone(content)
	return ref, nil
}
