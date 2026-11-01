package dispatch

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type APIError = api.APIError

type ClaimRequest = api.ClaimRequest
type Client = api.Client

type Config = api.Config

type ConversationControl = api.ConversationControl

type ConversationIdentity = api.ConversationIdentity

func EnrollRunner(ctx context.Context, path string, organization tracker.OrganizationID, enrollment string, machine Machine) (identity runnerauth.Identity, resultErr error) {
	return api.EnrollRunner(ctx, path, organization, enrollment, machine)
}

var ErrNoClaimableWork = api.ErrNoClaimableWork

var ErrUnavailable = api.ErrUnavailable

type FleetClient = api.FleetClient

type Machine = api.Machine
type MachineHeartbeat = api.MachineHeartbeat

type NativeClient = api.NativeClient
type NativeConnector = api.NativeConnector

func NativeItemPath(id tracker.NativeWorkItemID) (string, error) { return api.NativeItemPath(id) }

func New(config Config) (*Client, error) { return api.New(config) }

func NewFleetClient(client *Client, organization tracker.OrganizationID, projects map[string]tracker.ProjectID) (*FleetClient, error) {
	return api.NewFleetClient(client, organization, projects)
}
func NewNativeConnector(client *NativeClient) (*NativeConnector, error) {
	return api.NewNativeConnector(client)
}

func RefreshRunner(ctx context.Context, path string, rotate bool) (identity runnerauth.Identity, resultErr error) {
	return api.RefreshRunner(ctx, path, rotate)
}

type SessionTimings = api.SessionTimings

func WithMutationAuthority(ctx context.Context, scope string, lease tracker.NativeLease, cancellation func() error) context.Context {
	return api.WithMutationAuthority(ctx, scope, lease, cancellation)
}

type WorkItem = api.WorkItem
