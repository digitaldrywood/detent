package hubconversation

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// Config enables the conversation product on the hub.
type Config struct {
	ArchiveContents attachment.ArchiveContentStore `yaml:"-"`
	// Enabled mounts the conversation API and streams.
	Enabled bool
	// Backend runs coordinator turns for conversations without a linked
	// issue. When nil ordinary chat reports the coordinator as unavailable
	// and messages in unlinked conversations are saved until a link.
	Backend      runner.AgentBackend
	UsageSink    UsageSink
	ModelBackend func() (runner.AgentBackend, string, error)
	// Workspace is the directory coordinator turns run in. It must exist
	// when Backend is set.
	Workspace string
	// Model is the model "auto" resolves to for this hub, offered beside the
	// models the runners report, and the model coordinator turns use.
	Model string
	// ReasoningEffort is the effort coordinator turns use and the effort
	// "auto" resolves to in the composer.
	ReasoningEffort string
	// QuestionTimeout bounds how long a pending runner question waits for
	// an answer before it expires. Zero selects the runner default.
	QuestionTimeout time.Duration
	// ControlQueueSize bounds pending controls per conversation. Zero
	// selects 64.
	ControlQueueSize int
	// SettleWindow is how long a conversation whose execution has ended or
	// never started may sit without activity before it settles. Zero selects
	// the default settle window.
	SettleWindow time.Duration
}

type Usage struct {
	OccurredAt     time.Time
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	ConversationID string
	TurnID         string
	Provider       string
	Model          string
	Tokens         runner.AgentTokenCounts
	Outcome        conversation.Delivery
}

type UsageSink interface {
	RecordConversationUsage(context.Context, Usage) error
}
