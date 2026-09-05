package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runnerCredentialSource struct {
	mu   sync.Mutex
	path string
}

func runnerOrganizationPath(organization tracker.OrganizationID) (string, error) {
	if !strings.HasPrefix(string(organization), "org_") || strings.ContainsAny(string(organization), "/?#%\\") {
		return "", errors.New("runner organization ID is invalid")
	}
	return "/api/v2/organizations/" + string(organization), nil
}

func (c *Client) runnerRequest(ctx context.Context, token, method, path string, input, output any) error {
	if err := runnerauth.ValidateHubURL(c.baseURL.String()); err != nil {
		return err
	}
	transport := *c.httpClient
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	raw := &Client{baseURL: c.baseURL, tokenSource: func() string { return token }, httpClient: &transport}
	return raw.request(ctx, method, path, input, output)
}

func (c *Client) CreateRunnerEnrollment(ctx context.Context, organization tracker.OrganizationID, request runnerauth.EnrollmentRequest) (runnerauth.Enrollment, error) {
	var result runnerauth.Enrollment
	if c.runner != nil || c.tokenSource == nil {
		return result, errors.New("runner enrollment requires administrator credentials")
	}
	base, err := runnerOrganizationPath(organization)
	if err != nil {
		return result, err
	}
	err = c.runnerRequest(ctx, c.tokenSource(), http.MethodPost, base+"/runner-enrollments", request, &result)
	return result, err
}

func (c *Client) RevokeRunner(ctx context.Context, organization tracker.OrganizationID, binding runnerauth.Binding) error {
	if c.runner != nil || c.tokenSource == nil {
		return errors.New("runner revocation requires administrator credentials")
	}
	base, err := runnerOrganizationPath(organization)
	if err != nil {
		return err
	}
	if !binding.Valid() {
		return errors.New("runner identity is invalid")
	}
	return c.runnerRequest(ctx, c.tokenSource(), http.MethodDelete, base+"/runners/"+binding.RunnerID, nil, nil)
}

func (c *Client) runnerToken(ctx context.Context) (token string, resultErr error) {
	c.runner.mu.Lock()
	defer c.runner.mu.Unlock()
	lock, err := instancelock.Acquire(c.runner.path + ".lock")
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	file, err := runnerauth.Load(c.runner.path)
	if err != nil {
		return "", err
	}
	if file.HubURL != strings.TrimRight(c.baseURL.String(), "/") {
		return "", errors.New("runner identity belongs to a different Hub")
	}
	file, err = c.prepareRunnerCredential(ctx, c.runner.path, file, false)
	if err != nil {
		return "", err
	}
	return file.Credential, nil
}

func (c *Client) prepareRunnerCredential(ctx context.Context, path string, file runnerauth.File, forceRenew bool) (runnerauth.File, error) {
	base, err := runnerOrganizationPath(file.Identity.OrganizationID)
	if err != nil {
		return file, err
	}
	base += "/runners/" + file.Identity.RunnerID
	var identity runnerauth.Identity
	if file.PendingCredential != "" {
		err := c.runnerRequest(ctx, file.PendingCredential, http.MethodGet, base, nil, &identity)
		if err != nil {
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
				return file, err
			}
			if err := c.runnerRequest(ctx, file.Credential, http.MethodPost, base+"/rotate", runnerauth.Rotation{Credential: file.PendingCredential}, &identity); err != nil {
				return file, err
			}
		}
		if err := validateRunnerResponse(file, identity); err != nil {
			return file, err
		}
		file.Credential, file.PendingCredential, file.Identity = file.PendingCredential, "", identity
		if err := runnerauth.Save(path, file); err != nil {
			return file, err
		}
	}
	if forceRenew || !time.Now().Add(runnerauth.CredentialTTL/2).Before(file.Identity.ExpiresAt) {
		if err := c.runnerRequest(ctx, file.Credential, http.MethodPost, base+"/renew", struct{}{}, &identity); err != nil {
			return file, err
		}
		if err := validateRunnerResponse(file, identity); err != nil {
			return file, err
		}
		file.Identity = identity
		if err := runnerauth.Save(path, file); err != nil {
			return file, err
		}
	}
	return file, nil
}

func validateRunnerResponse(file runnerauth.File, identity runnerauth.Identity) error {
	if identity.Binding != file.Identity.Binding || identity.OrganizationID != file.Identity.OrganizationID || identity.ExpiresAt.IsZero() || len(identity.ProjectIDs) == 0 || !runnerauth.ValidOperations(identity.Operations) {
		return errors.New("hub returned an unexpected runner identity")
	}
	return nil
}

func EnrollRunner(ctx context.Context, path string, organization tracker.OrganizationID, enrollment string, machine Machine) (identity runnerauth.Identity, resultErr error) {
	lock, err := instancelock.Acquire(path + ".lock")
	if err != nil {
		return identity, err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	file, err := runnerauth.Load(path)
	if err != nil {
		return identity, err
	}
	if file.Identity.OrganizationID != "" && file.Identity.OrganizationID != organization {
		return identity, errors.New("runner belongs to a different organization")
	}
	file.Identity.OrganizationID = organization
	base, err := runnerOrganizationPath(organization)
	if err != nil {
		return identity, err
	}
	client, err := New(Config{URL: file.HubURL, TokenSource: func() string { return file.Credential }, HTTPClient: &http.Client{Timeout: 30 * time.Second}})
	if err != nil {
		return identity, err
	}
	if err := runnerauth.Save(path, file); err != nil {
		return identity, err
	}
	err = client.runnerRequest(ctx, file.Credential, http.MethodGet, base+"/runners/"+file.Identity.RunnerID, nil, &identity)
	if err != nil {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
			return identity, err
		}
		request := runnerauth.Redemption{Binding: file.Identity.Binding, Credential: file.Credential, Hostname: machine.Hostname, DisplayName: machine.DisplayName, Capacity: machine.Capacity, Version: machine.Version, OS: runtime.GOOS, Architecture: runtime.GOARCH}
		if err := client.runnerRequest(ctx, enrollment, http.MethodPost, base+"/runner-enrollments/redeem", request, &identity); err != nil {
			return identity, err
		}
	}
	if err := validateRunnerResponse(file, identity); err != nil {
		return identity, err
	}
	file.Identity = identity
	return identity, runnerauth.Save(path, file)
}

func RefreshRunner(ctx context.Context, path string, rotate bool) (identity runnerauth.Identity, resultErr error) {
	lock, err := instancelock.Acquire(path + ".lock")
	if err != nil {
		return identity, err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	file, err := runnerauth.Load(path)
	if err != nil {
		return identity, err
	}
	client, err := New(Config{URL: file.HubURL, TokenSource: func() string { return file.Credential }, HTTPClient: &http.Client{Timeout: 30 * time.Second}})
	if err != nil {
		return identity, err
	}
	if rotate && file.PendingCredential == "" {
		file.PendingCredential, err = apikey.GenerateToken()
		if err != nil {
			return identity, err
		}
		if err := runnerauth.Save(path, file); err != nil {
			return identity, err
		}
	}
	file, err = client.prepareRunnerCredential(ctx, path, file, !rotate)
	return file.Identity, err
}

func (c *NativeClient) HeartbeatMachine(ctx context.Context, machine Machine) error {
	request := struct {
		ProviderReports []providercapacity.Report `json:"provider_reports,omitempty"`
		DisplayName     string                    `json:"display_name"`
		Capacity        int                       `json:"capacity"`
		Version         string                    `json:"version"`
		OS              string                    `json:"os"`
		Architecture    string                    `json:"architecture"`
	}{machine.ProviderReports, machine.DisplayName, machine.Capacity, machine.Version, runtime.GOOS, runtime.GOARCH}
	return c.client.request(ctx, http.MethodPost, c.base()+"/machines/"+url.PathEscape(string(machine.ID))+"/heartbeat", request, nil)
}
