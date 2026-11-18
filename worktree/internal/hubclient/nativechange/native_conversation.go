package nativechange

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/conversation"
	"github.com/digitaldrywood/detent/internal/runner"
)

func (e *nativeExecution) SupportsPreStartConversationBinding(ctx context.Context) (bool, error) {
	return conversation.SupportsPreStart(ctx, e.claim.source.NativeClient())
}

// BindConversation implements runner.ConversationExecution for the native
// execution: the attempt binds with its own lease and fencing token.
func (e *nativeExecution) BindConversation(ctx context.Context, capabilities runner.ConversationCapabilities, resumeThreadID string) (runner.ConversationSession, error) {
	if e.claim.source == nil || e.claim.source.NativeClient() == nil {
		return nil, runner.ErrNoConversation
	}
	client := e.claim.source.NativeClient()
	return conversation.Bind(ctx, client, conversation.Binding{Item: e.claim.lease.WorkItemID, Lease: e.claim.lease, RunID: e.data.RunID, AttemptID: e.data.AttemptID, Capabilities: capabilities, ResumeThreadID: resumeThreadID}, conversationAuthority{e}, func(ctx context.Context, identity api.ConversationIdentity, response api.ConversationBindResponse, leaseCheck func(context.Context) error, logger *slog.Logger, ready <-chan struct{}) runner.ConversationSession {
		return api.NewConversationSession(ctx, client, e.claim.lease.WorkItemID, identity, response, leaseCheck, logger, ready)
	})
}

// leaseCheck reports whether the attempt's lease is still current locally.
func (e *nativeExecution) leaseCheck(context.Context) error {
	if e.remaining() <= 0 {
		return fmt.Errorf("%w: %w", runner.ErrStaleConversationControl, runner.ErrExecutionAuthorityUnavailable)
	}
	return nil
}
