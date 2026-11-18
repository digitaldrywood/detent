package conversation

import (
	"context"
	"log/slog"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Client interface {
	HubFeature(context.Context, string) (bool, error)
	BindConversation(context.Context, tracker.NativeWorkItemID, api.ConversationBindRequest) (api.ConversationBindResponse, error)
}
type Binding struct {
	Item           tracker.NativeWorkItemID
	Lease          tracker.NativeLease
	RunID          string
	AttemptID      string
	Capabilities   runner.ConversationCapabilities
	ResumeThreadID string
}
type Authority interface {
	Ready() <-chan struct{}
	LeaseCheck(context.Context) error
	Bound(bool)
}
type SessionFactory func(context.Context, api.ConversationIdentity, api.ConversationBindResponse, func(context.Context) error, *slog.Logger, <-chan struct{}) runner.ConversationSession

func SupportsPreStart(ctx context.Context, client Client) (bool, error) {
	return client.HubFeature(ctx, tracker.NativePreStartConversationBindingCapability)
}
func Bind(ctx context.Context, client Client, binding Binding, host Authority, newSession SessionFactory) (runner.ConversationSession, error) {
	if binding.AttemptID == "" {
		return nil, runner.ErrNoConversation
	}
	identity := api.ConversationIdentity{LeaseID: binding.Lease.ID, FencingToken: binding.Lease.FencingToken, AttemptID: binding.AttemptID}
	response, err := client.BindConversation(ctx, binding.Item, api.ConversationBindRequest{ConversationIdentity: identity, RunID: binding.RunID, Capabilities: binding.Capabilities, ThreadID: strings.TrimSpace(binding.ResumeThreadID)})
	if err != nil {
		return nil, err
	}
	logger := slog.Default().With("work_item", binding.Item, "attempt", binding.AttemptID)
	session := newSession(ctx, identity, response, host.LeaseCheck, logger, host.Ready())
	host.Bound(session.Continuation())
	return session, nil
}
