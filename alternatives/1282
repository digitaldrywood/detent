package nativechange

import (
	"context"
	"log/slog"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type APIError = api.APIError

type Client = api.Client

type Config = api.Config

type ConversationBindRequest = api.ConversationBindRequest
type ConversationBindResponse = api.ConversationBindResponse
type ConversationControl = api.ConversationControl
type ConversationControlsPage = api.ConversationControlsPage
type ConversationExpected = api.ConversationExpected
type ConversationIdentity = api.ConversationIdentity
type ConversationTurnEventsRequest = api.ConversationTurnEventsRequest
type ConversationUnbindRequest = api.ConversationUnbindRequest

func EnrollRunner(ctx context.Context, path string, organization tracker.OrganizationID, enrollment string, machine Machine) (identity runnerauth.Identity, resultErr error) {
	return api.EnrollRunner(ctx, path, organization, enrollment, machine)
}

var ErrNoClaimableWork = api.ErrNoClaimableWork

var ErrStaleConversation = api.ErrStaleConversation

var ErrUnavailable = api.ErrUnavailable

type Machine = api.Machine

func MutationEffectError(ctx context.Context) error { return api.MutationEffectError(ctx) }

type NativeClient = api.NativeClient
type NativeConnector = api.NativeConnector

func NativeItemPath(id tracker.NativeWorkItemID) (string, error) { return api.NativeItemPath(id) }

type NativePullRequestReference = api.NativePullRequestReference

func New(config Config) (*Client, error) { return api.New(config) }
func NewConversationSession(ctx context.Context, client *NativeClient, item tracker.NativeWorkItemID, identity ConversationIdentity, response ConversationBindResponse, leaseCheck func(context.Context) error, logger *slog.Logger, ready <-chan struct{}) runner.ConversationSession {
	return api.NewConversationSession(ctx, client, item, identity, response, leaseCheck, logger, ready)
}
func NewFleetClient(client *Client, organization tracker.OrganizationID, projects map[string]tracker.ProjectID) (*FleetClient, error) {
	return api.NewFleetClient(client, organization, projects)
}
func NewNativeConnector(client *NativeClient) (*NativeConnector, error) {
	return api.NewNativeConnector(client)
}

type SessionTimings = api.SessionTimings

func WithMutationAuthority(ctx context.Context, scope string, lease tracker.NativeLease, cancellation func() error) context.Context {
	return api.WithMutationAuthority(ctx, scope, lease, cancellation)
}

type WorkItem = api.WorkItem

type FleetClient = api.FleetClient
