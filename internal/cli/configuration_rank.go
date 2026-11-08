package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type hubConfigurationRank struct {
	client    *api.Client
	transport *http.Transport
	path      string
}

func newHubConfigurationRank(settings globalconfig.HubClient, lookupEnv func(string) string) (*hubConfigurationRank, error) {
	settings = settings.Normalized()
	if !settings.Configured() || strings.TrimSpace(settings.OrganizationID) == "" {
		return nil, errors.New("project rank requires a configured Hub organization")
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	client, err := api.New(api.Config{URL: settings.URL, TokenSource: func() string { return lookupEnv(settings.TokenEnvironment) }, HTTPClient: &http.Client{Transport: transport, Timeout: settings.RequestTimeout()}})
	if err != nil {
		transport.CloseIdleConnections()
		return nil, fmt.Errorf("create configuration owner: %w", err)
	}
	return &hubConfigurationRank{client: client, transport: transport, path: "/api/v2/organizations/" + url.PathEscape(settings.OrganizationID) + "/configuration/project-rank"}, nil
}

func (s *hubConfigurationRank) Read(ctx context.Context) (projectsettings.Rank, error) {
	var value projectsettings.Rank
	if err := s.client.Request(ctx, http.MethodGet, s.path, nil, &value); err != nil {
		return value, fmt.Errorf("read project rank: %w", err)
	}
	return value, nil
}

func (s *hubConfigurationRank) Apply(ctx context.Context, change projectsettings.RankChange) (projectsettings.Rank, error) {
	var value projectsettings.Rank
	if err := s.client.Request(ctx, http.MethodPut, s.path, change, &value); err != nil {
		return value, fmt.Errorf("apply project rank: %w", err)
	}
	return value, nil
}

func (s *hubConfigurationRank) Close() {
	s.transport.CloseIdleConnections()
}

func promoteHubProjectRank(ctx context.Context, cfg globalconfig.Config, id string, position int, lookupEnv func(string) string) (int, error) {
	if position < 1 {
		return 0, errors.New("project priority must be greater than zero")
	}
	native := tracker.ProjectID(cfg.Client.NativeProjects[id])
	if native == "" {
		return 0, errors.New("project is not configured for the Hub configuration owner")
	}
	owner, err := newHubConfigurationRank(cfg.Client, lookupEnv)
	if err != nil {
		return 0, err
	}
	defer owner.Close()
	current, err := owner.Read(ctx)
	if err != nil {
		return 0, err
	}
	index := slices.Index(current.ProjectIDs, native)
	if index < 0 {
		return 0, errors.New("project is unavailable in the Hub configuration owner")
	}
	ids := slices.Delete(slices.Clone(current.ProjectIDs), index, index+1)
	nextPosition := min(position-1, len(ids))
	ids = slices.Insert(ids, nextPosition, native)
	if slices.Equal(ids, current.ProjectIDs) {
		return nextPosition, nil
	}
	if _, err := owner.Apply(ctx, projectsettings.RankChange{ExpectedRevision: current.Revision, ProjectIDs: ids}); err != nil {
		return 0, err
	}
	return nextPosition, nil
}
