package hubgithub

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

type appClient struct {
	config  connectorgithub.InstallationTokenConfig
	mu      sync.Mutex
	clients map[string]*connectorgithub.Client
}

func NewAppTransport(config connectorgithub.InstallationTokenConfig) *Transport {
	return &Transport{client: &appClient{config: config, clients: make(map[string]*connectorgithub.Client)}, queue: make(chan struct{}, 1), counts: make(map[string]hubserver.GitHubRequestCount), now: time.Now}
}

func (t *Transport) AppInstallation(ctx context.Context, repository string) (hubserver.GitHubAppInstallation, error) {
	app, ok := t.client.(*appClient)
	if !ok {
		return hubserver.GitHubAppInstallation{}, errors.New("GitHub App transport is unavailable")
	}
	config := app.config
	config.Repository = repository
	source, err := connectorgithub.NewInstallationTokenSource(config)
	if err != nil {
		return hubserver.GitHubAppInstallation{}, err
	}
	ctx = scopedRequests(ctx, "native", "integration")
	var result hubserver.GitHubAppInstallation
	_, err = t.request(ctx, http.MethodGet, "/app", func() (string, error) {
		var err error
		result.Slug, err = source.AppSlug(ctx)
		return "", err
	})
	if err != nil {
		return result, err
	}
	result.InstallURL = "https://github.com/apps/" + url.PathEscape(result.Slug) + "/installations/new"
	_, err = t.request(ctx, http.MethodGet, "/repos/"+repository+"/installation", func() (string, error) {
		var err error
		result.Installed, err = source.RepositoryInstalled(ctx)
		return "", err
	})
	return result, err
}

func (a *appClient) repositoryClient(path string) (*connectorgithub.Client, error) {
	parsed, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "repos" || parts[1] == "" || parts[2] == "" {
		return nil, errors.New("GitHub App requests require a repository path")
	}
	repository := strings.ToLower(parts[1] + "/" + parts[2])
	a.mu.Lock()
	defer a.mu.Unlock()
	if client := a.clients[repository]; client != nil {
		return client, nil
	}
	config := a.config
	config.Repository = repository
	token, err := connectorgithub.NewInstallationTokenSource(config)
	if err != nil {
		return nil, err
	}
	client, err := connectorgithub.NewClient(connectorgithub.ClientConfig{TokenSource: token, HTTPClient: config.HTTPClient, Endpoint: config.Endpoint})
	if err != nil {
		return nil, err
	}
	a.clients[repository] = client
	return client, nil
}

func (a *appClient) REST(ctx context.Context, method, path string, body, output any) error {
	client, err := a.repositoryClient(path)
	if err != nil {
		return err
	}
	return client.REST(ctx, method, path, body, output)
}

func (a *appClient) RESTPage(ctx context.Context, path string, output any) (string, error) {
	client, err := a.repositoryClient(path)
	if err != nil {
		return "", err
	}
	return client.RESTPage(ctx, path, output)
}
