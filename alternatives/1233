package hubclient

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/dispatch"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type APIError = api.APIError

type ClaimRequest = api.ClaimRequest
type Client = api.Client

type Config = api.Config
type ConversationAttachment = api.ConversationAttachment
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
var ErrNoWorkspace = api.ErrNoWorkspace
var ErrStaleConversation = api.ErrStaleConversation
var ErrStaleWorkspace = api.ErrStaleWorkspace
var ErrUnavailable = api.ErrUnavailable
var ErrWorkspacesNotServed = api.ErrWorkspacesNotServed

type FleetClient = api.FleetClient

type LeaseRequest = api.LeaseRequest
type Machine = api.Machine
type MachineHeartbeat = api.MachineHeartbeat

type NativeClient = api.NativeClient
type NativeConnector = api.NativeConnector

type NativePullRequestReference = api.NativePullRequestReference

func New(config Config) (*Client, error) { return api.New(config) }

func NewFleetClient(client *Client, organization tracker.OrganizationID, projects map[string]tracker.ProjectID) (*FleetClient, error) {
	return api.NewFleetClient(client, organization, projects)
}
func NewNativeConnector(client *NativeClient) (*NativeConnector, error) {
	return api.NewNativeConnector(client)
}
func NewRunnerReader(config Config) (*Client, error) { return api.NewRunnerReader(config) }
func NewWorkspaceClaimer(native *NativeClient, config WorkspaceLaneConfig) (*WorkspaceClaimer, error) {
	return api.NewWorkspaceClaimer(native, config)
}

func RefreshRunner(ctx context.Context, path string, rotate bool) (identity runnerauth.Identity, resultErr error) {
	return api.RefreshRunner(ctx, path, rotate)
}

type WorkItem = api.WorkItem
type WorkspaceActionRunReport = api.WorkspaceActionRunReport
type WorkspaceBindRequest = api.WorkspaceBindRequest
type WorkspaceBindResponse = api.WorkspaceBindResponse
type WorkspaceCheckout = api.WorkspaceCheckout
type WorkspaceClaimState = api.WorkspaceClaimState
type WorkspaceClaimer = api.WorkspaceClaimer

type WorkspaceHeartbeatRequest = api.WorkspaceHeartbeatRequest
type WorkspaceHeartbeatResponse = api.WorkspaceHeartbeatResponse
type WorkspaceIdentity = api.WorkspaceIdentity

const WorkspaceItemLabel = api.WorkspaceItemLabel

type WorkspaceLaneConfig = api.WorkspaceLaneConfig
type WorkspaceUnbindRequest = api.WorkspaceUnbindRequest

type Scheduler = dispatch.Scheduler
type SchedulerConfig = dispatch.SchedulerConfig

func NewScheduler(client *Client, config SchedulerConfig) (*Scheduler, error) {
	if config.ExecutionFactory == nil {
		config.ExecutionFactory = nativechange.NewExecution
	}
	if config.CompletionRestorer == nil {
		config.CompletionRestorer = nativechange.RestoreCompletion
	}
	return dispatch.NewScheduler(client, config)
}
