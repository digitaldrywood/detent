package chat

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

// OperatorTranscript reads only the transport-bound conversation. It cannot
// select an unrelated browser session or approve any pending proposal.
type OperatorTranscript struct {
	Freshness      string    `json:"freshness"`
	ConnectionID   string    `json:"connection_id"`
	OrganizationID string    `json:"organization_id"`
	Client         string    `json:"client"`
	Messages       []Message `json:"messages"`
	HasMore        bool      `json:"has_more"`
	GeneratedAt    time.Time `json:"generated_at"`
}

func (s *Service) OperatorTranscript(ctx context.Context, limit int) (OperatorTranscript, error) {
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return OperatorTranscript{}, err
	}
	if err := s.CheckConnection(ctx); err != nil {
		return OperatorTranscript{}, err
	}
	current := s.Conversation(operatortool.CurrentConnection(ctx).ID)
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, operatortool.MaxItemLimit)
	more := len(current.Messages) > limit
	if more {
		current.Messages = current.Messages[len(current.Messages)-limit:]
	}
	return OperatorTranscript{"live", current.ConnectionID, current.OrganizationID, current.Client, current.Messages, more, s.now().UTC()}, nil
}

// HasProvider reflects an injected application dependency, not a tool argument.
func (s *Service) HasProvider() bool { return s.provider != nil }
