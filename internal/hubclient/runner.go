package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/instancelock"
	isolationpolicy "github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type runnerCredentialSource struct {
	problems         []runnerauth.Problem
	settingsRejected bool
	routingMu        sync.Mutex
	routing          *runnerauth.RoutingSnapshot
	routingChanged   chan struct{}
	localPolicies    map[tracker.ProjectID]string
	heartbeat        nativeMachineHeartbeat
	heartbeatPath    string
	mu               sync.Mutex
	path             string
}

func runnerSpriteName(hostname string) string {
	if info, err := os.Stat("/.sprite/api.sock"); err == nil && info.Mode()&os.ModeSocket != 0 {
		return hostname
	}
	return ""
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
	if identity.Binding != file.Identity.Binding || identity.OrganizationID != file.Identity.OrganizationID || identity.ExpiresAt.IsZero() || !runnerauth.ValidOperations(identity.Operations) {
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
		request := runnerauth.Redemption{IsolationTier: machine.IsolationTier, BackendIsolation: machine.BackendIsolation, Binding: file.Identity.Binding, Credential: file.Credential, Hostname: machine.Hostname, DisplayName: machine.DisplayName, Capacity: machine.Capacity, Version: machine.Version, OS: runtime.GOOS, Architecture: runtime.GOARCH}
		request.SpriteName = runnerSpriteName(machine.Hostname)
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
	if machine.ProjectConfiguration != nil {
		supported, err := c.HubFeature(ctx, tracker.NativeProjectConfigurationCapability)
		if err != nil {
			return err
		}
		if !supported {
			machine.ProjectConfiguration = nil
		}
	}
	if machine.ProjectConfiguration != nil && machine.ProjectConfiguration.Diagnostics != nil {
		supported, err := c.HubFeature(ctx, tracker.NativeProjectDiagnosticsCapability)
		if err != nil {
			return err
		}
		if !supported {
			machine.ProjectConfiguration.Diagnostics = nil
		}
	}
	if machine.Update != nil {
		supported, err := c.HubFeature(ctx, tracker.NativeRunnerUpdateCapability)
		if err != nil {
			return err
		}
		if !supported {
			machine.Update = nil
		}
	}
	// Optional observations must never exceed the Hub's advertised schema.
	// Omission leaves diagnostic authority absent; it does not attest success.
	if machine.CapacityConfig != nil {
		supported, err := c.HubFeature(ctx, tracker.NativeRunnerCapacityCapability)
		if err != nil {
			return err
		}
		if !supported {
			machine.CapacityConfig = nil
		}
	}
	if machine.LocalChecks != nil {
		supported, err := c.HubFeature(ctx, tracker.NativeLocalChecksCapability)
		if err != nil {
			return err
		}
		if !supported {
			machine.LocalChecks = nil
		}
	}
	if machine.LocalChecks != nil && (machine.LocalChecks.CheckoutMessage != "" || machine.LocalChecks.CheckoutFix != "") {
		supported, err := c.HubFeature(ctx, tracker.NativeProjectCheckoutCapability)
		if err != nil {
			return err
		}
		if !supported {
			checks := *machine.LocalChecks
			checks.CheckoutMessage, checks.CheckoutFix = "", ""
			machine.LocalChecks = &checks
		}
	}
	if machine.LocalChecks != nil && machine.LocalChecks.RunnerSetupDeclared != nil {
		supported, err := c.HubFeature(ctx, tracker.NativeRunnerSetupDeclarationCapability)
		if err != nil {
			return err
		}
		if !supported {
			checks := *machine.LocalChecks
			checks.RunnerSetupDeclared = nil
			machine.LocalChecks = &checks
		}
	}
	if machine.LocalChecks != nil && machine.LocalChecks.Setup != "" {
		supported, err := c.HubFeature(ctx, tracker.NativeRunnerSetupCapability)
		if err != nil {
			return err
		}
		if !supported {
			checks := *machine.LocalChecks
			checks.Setup = ""
			machine.LocalChecks = &checks
		}
	}
	if machine.Admission != nil {
		supported, err := c.HubFeature(ctx, tracker.NativeAdmissionObservationCapability)
		if err != nil {
			return err
		}
		if !supported || machine.Admission.Context.Validate() != nil {
			machine.Admission = nil
		} else if raw, err := json.Marshal(machine.Admission); err != nil || len(raw) > 8192-64 {
			machine.Admission = nil
		}
	}
	capacity := machine.Capacity
	if c.client.runner != nil {
		availability, err := c.client.runner.availability()
		if err != nil {
			machine.Capacity = 0
		} else {
			status, err := availability.Evaluate(time.Now())
			if err != nil {
				return err
			}
			if !status.Open {
				machine.Capacity = 0
			}
		}
	}
	return c.heartbeatMachine(ctx, machine, capacity, true)
}

func (c *NativeClient) heartbeatMachine(ctx context.Context, machine Machine, capacity int, refresh bool) error {
	var changeCursor *string
	if c.client.runner != nil {
		supported, err := c.HubFeature(ctx, tracker.NativeHeartbeatChangesCapability)
		if err != nil {
			return err
		}
		if supported {
			cursor := c.changeCursor()
			changeCursor = &cursor
		}
	}
	problems := machine.Problems
	var rejected bool
	if c.client.runner != nil {
		problems, rejected = c.client.runner.heartbeatProblems(ctx, machine)
	}
	request := machineHeartbeatPayload(machine, problems, rejected)
	request.ChangeCursor = changeCursor
	if c.client.runner == nil {
		return c.client.request(ctx, http.MethodPost, c.base()+"/machines/"+url.PathEscape(string(machine.ID))+"/heartbeat", request, nil)
	}
	var snapshot runnerauth.RoutingSnapshot
	if err := c.client.request(ctx, http.MethodPost, c.base()+"/machines/"+url.PathEscape(string(machine.ID))+"/heartbeat", request, &snapshot); err != nil {
		c.client.capabilitiesMu.Lock()
		c.client.capabilitiesDigest, c.client.capabilitiesAt = "", time.Time{}
		c.client.capabilitiesMu.Unlock()
		return err
	}
	c.client.runner.routingMu.Lock()
	c.client.runner.heartbeat = request
	c.client.runner.heartbeatPath = c.base() + "/machines/" + url.PathEscape(string(machine.ID)) + "/heartbeat"
	c.client.runner.routingMu.Unlock()
	snapshot.Routing = snapshot.Routing.Normalized()
	identity, err := runnerauth.Load(c.client.runner.path)
	if err != nil {
		return err
	}
	if snapshot.RunnerID != identity.Identity.RunnerID || snapshot.Revision < 1 {
		return errors.Join(ErrUnavailable, errors.New("hub returned an unexpected runner routing identity"))
	}
	if err := snapshot.Routing.Validate(); err != nil {
		c.client.runner.rejectSettings(true)
		return errors.Join(ErrUnavailable, err)
	}
	c.applyHeartbeatChanges(snapshot.Changes)
	c.client.runner.setRouting(snapshot)
	if err := runnerauth.SaveRoutingCache(c.client.runner.path, snapshot); err != nil {
		c.client.runner.rejectSettings(true)
		slog.Default().Warn("runner routing cache not updated", "error", err)
	} else {
		c.client.runner.rejectSettings(false)
	}
	status, err := snapshot.Routing.Availability.Evaluate(time.Now())
	if err != nil {
		return err
	}
	reported := capacity
	if !status.Open {
		reported = 0
	}
	if refresh && reported != machine.Capacity {
		machine.Capacity = reported
		return c.heartbeatMachine(ctx, machine, capacity, false)
	}

	if snapshot.GitHubIntake != nil && c.githubBatch != nil {
		return c.githubBatch(ctx, *snapshot.GitHubIntake)
	}
	return nil
}

func (r *runnerCredentialSource) setRouting(snapshot runnerauth.RoutingSnapshot) {
	snapshot.Routing.Availability.Windows = slices.Clone(snapshot.Routing.Availability.Windows)
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	previous := r.routing
	changes := snapshot.Changes
	modeChanged := previous != nil && (previous.Changes == nil) != (changes == nil)
	if modeChanged || changes != nil && (changes.Reset || len(changes.Items) > 0 || changes.Claimable || previous == nil || previous.Changes == nil || changes.Claimable != previous.Changes.Claimable) {
		r.notifyClaimChange()
	}
	if snapshot.ClaimState != nil {
		state := *snapshot.ClaimState
		state.Slots = slices.Clone(state.Slots)
		slices.SortFunc(state.Slots, func(a, b runnerauth.ClaimSlot) int { return strings.Compare(string(a.ID), string(b.ID)) })
		state.PolicyIDs = maps.Clone(state.PolicyIDs)
		snapshot.ClaimState = &state
	}
	if previous != nil && previous.ClaimState != nil && snapshot.ClaimState != nil {
		policies := maps.Clone(previous.ClaimState.PolicyIDs)
		if policies == nil {
			policies = make(map[tracker.ProjectID]string)
		}
		maps.Copy(policies, snapshot.ClaimState.PolicyIDs)
		snapshot.ClaimState.PolicyIDs = policies
	}
	if previous != nil && (previous.Routing.State != snapshot.Routing.State || !reflect.DeepEqual(previous.ClaimState, snapshot.ClaimState) || previous.Routing.Availability.Timezone != snapshot.Routing.Availability.Timezone || previous.Routing.Availability.HardDeadline != snapshot.Routing.Availability.HardDeadline || !slices.Equal(previous.Routing.Availability.Windows, snapshot.Routing.Availability.Windows)) {
		r.notifyClaimChange()
	}
	r.routing = &snapshot
}

func (r *runnerCredentialSource) availability() (runnerauth.Availability, error) {
	availability, _, err := r.availabilityState()
	return availability, err
}

func (r *runnerCredentialSource) availabilityState() (runnerauth.Availability, <-chan struct{}, error) {
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	if r.routing == nil {
		snapshot, err := runnerauth.LoadRoutingCache(r.path)
		if err != nil {
			return runnerauth.Availability{}, nil, errors.Join(ErrUnavailable, err)
		}
		r.routing = &snapshot
	}
	if r.routingChanged == nil {
		r.routingChanged = make(chan struct{})
	}
	return r.routing.Routing.Availability, r.routingChanged, nil
}

func (r *runnerCredentialSource) capacityRequest() *runnerauth.CapacityRequest {
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	if r.routing == nil || r.routing.Routing.CapacityRequest == nil || r.routing.Routing.CapacityRequest.Capacity != r.routing.Routing.CapacityLimit {
		return nil
	}
	copy := *r.routing.Routing.CapacityRequest
	return &copy
}

func (r *runnerCredentialSource) updateRequest() *runnerauth.UpdateRequest {
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	if r.routing == nil || r.routing.Routing.UpdateRequest == nil {
		return nil
	}
	copy := *r.routing.Routing.UpdateRequest
	return &copy
}

func (r *runnerCredentialSource) projectConfigurationRequest() *runnerauth.ProjectConfigurationRequest {
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	if r.routing == nil || r.routing.ProjectConfigurationRequest == nil {
		return nil
	}
	copy := *r.routing.ProjectConfigurationRequest
	return &copy
}

func (r *runnerCredentialSource) notifyClaimChange() {
	if r.routingChanged != nil {
		close(r.routingChanged)
	}
	r.routingChanged = make(chan struct{})
}

func (r *runnerCredentialSource) setLocalPolicy(project tracker.ProjectID, id string) {
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	if r.localPolicies == nil {
		r.localPolicies = make(map[tracker.ProjectID]string)
	}
	if r.localPolicies[project] != id {
		r.localPolicies[project] = id
		r.notifyClaimChange()
	}
}

func (r *runnerCredentialSource) claimSlot(lease tracker.NativeLease, acquired bool) {
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	if r.routing == nil || r.routing.ClaimState == nil {
		return
	}
	state := *r.routing.ClaimState
	slots := slices.Clone(state.Slots)
	index := slices.IndexFunc(slots, func(slot runnerauth.ClaimSlot) bool { return slot.ID == lease.ID })
	if acquired && index < 0 {
		state.Slots = append(slots, runnerauth.ClaimSlot{ID: lease.ID, RunnerID: r.routing.RunnerID})
		slices.SortFunc(state.Slots, func(a, b runnerauth.ClaimSlot) int { return strings.Compare(string(a.ID), string(b.ID)) })
		r.routing.ClaimState = &state
		r.notifyClaimChange()
	} else if !acquired && index >= 0 {
		state.Slots = slices.Delete(slots, index, index+1)
		r.routing.ClaimState = &state
		r.notifyClaimChange()
	}
}

func (r *runnerCredentialSource) setApprovedPolicy(project tracker.ProjectID, id string) {
	r.routingMu.Lock()
	defer r.routingMu.Unlock()
	if r.routing == nil || r.routing.ClaimState == nil {
		return
	}
	state := *r.routing.ClaimState
	if current, ok := state.PolicyIDs[project]; ok && current == id {
		return
	}
	state.PolicyIDs = maps.Clone(state.PolicyIDs)
	if state.PolicyIDs == nil {
		state.PolicyIDs = make(map[tracker.ProjectID]string)
	}
	state.PolicyIDs[project] = id
	r.routing.ClaimState = &state
	r.notifyClaimChange()
}

func (c *Client) RunnerIdentity(ctx context.Context) (runnerauth.Identity, error) {
	var identity runnerauth.Identity
	if c.runner == nil {
		return identity, errors.New("enrolled runner identity is required")
	}
	file, err := runnerauth.Load(c.runner.path)
	if err != nil {
		return identity, err
	}
	base, err := runnerOrganizationPath(file.Identity.OrganizationID)
	if err != nil {
		return identity, err
	}
	if err := c.request(ctx, http.MethodGet, base+"/runners/"+file.Identity.RunnerID, nil, &identity); err != nil {
		return identity, err
	}
	return identity, validateRunnerResponse(file, identity)
}

type nativeMachineHeartbeat struct {
	HostMetrics          []hostmetrics.Summary               `json:"host_metrics,omitempty"`
	Admission            *tracker.NativeAdmissionObservation `json:"admission,omitempty"`
	SpriteName           string                              `json:"sprite_name,omitempty"`
	Update               *runnerauth.UpdateObservation       `json:"update,omitempty"`
	CapacityConfig       *runnerauth.CapacityConfig          `json:"capacity_configuration,omitempty"`
	ProjectConfiguration *runnerauth.ProjectConfiguration    `json:"project_configuration,omitempty"`
	LocalChecks          *runnerauth.LocalChecks             `json:"local_checks,omitempty"`
	Problems             []runnerauth.Problem                `json:"problems"`
	ProtocolMajor        int                                 `json:"protocol_major,omitempty"`
	SettingsRejected     bool                                `json:"settings_rejected,omitempty"`
	BackendIsolation     isolationpolicy.Report              `json:"backend_isolation"`
	ProviderReports      []providercapacity.Report           `json:"provider_reports,omitempty"`
	DisplayName          string                              `json:"display_name"`
	Capacity             int                                 `json:"capacity"`
	Version              string                              `json:"version"`
	OS                   string                              `json:"os"`
	Architecture         string                              `json:"architecture"`
	// The workspace claim gate matches these against a workspace's
	// requires set and checks the heartbeat that carried them is fresh.
	WorkspaceCapabilities *workspacesession.Capabilities `json:"workspace_capabilities,omitempty"`
	WorkspaceIsolation    string                         `json:"workspace_isolation,omitempty"`
	CheckoutRepository    *string                        `json:"checkout_repository,omitempty"`
	ChangeCursor          *string                        `json:"change_cursor,omitempty"`
}

func machineHeartbeatPayload(machine Machine, problems []runnerauth.Problem, rejected bool) nativeMachineHeartbeat {
	capabilities, isolation := machine.workspaceReport()
	return nativeMachineHeartbeat{machine.HostMetrics, machine.Admission, runnerSpriteName(machine.Hostname), machine.Update, machine.CapacityConfig, machine.ProjectConfiguration, machine.LocalChecks, problems, 2, rejected, machine.BackendIsolation, machine.ProviderReports, machine.DisplayName, machine.Capacity, machine.Version, runtime.GOOS, runtime.GOARCH, capabilities, isolation, machine.CheckoutRepository, nil}
}
