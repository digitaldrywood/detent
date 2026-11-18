package scenariotest

import (
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func NewFleetClient(client *Client, organization tracker.OrganizationID, projects map[string]tracker.ProjectID) (*FleetClient, error) {
	return api.NewFleetClient(client, organization, projects)
}

var ErrUnavailable = api.ErrUnavailable

func NewNativeConnector(client *NativeClient) (*NativeConnector, error) {
	return api.NewNativeConnector(client)
}

type Client = api.Client

type NativeClient = api.NativeClient

type Config = api.Config

type FleetClient = api.FleetClient

type NativeConnector = api.NativeConnector

type Machine = api.Machine

type APIError = api.APIError

func New(config Config) (*Client, error) { return api.New(config) }
