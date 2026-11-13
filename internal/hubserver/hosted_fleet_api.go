package hubserver

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// The fleet and spend screen. Every member reads the
// fleet; a lease is only described when the member can read the project it
// runs for, so the runner list never leaks work from another project.

type hostedFleetLease struct {
	LeaseID    tracker.LeaseID          `json:"lease_id"`
	WorkItemID tracker.NativeWorkItemID `json:"work_item_id"`
	Title      string                   `json:"title"`
	ProjectID  tracker.ProjectID        `json:"project_id"`
	ExpiresAt  time.Time                `json:"expires_at"`
}

type hostedRunnerCheckout struct {
	Status     string `json:"status"`
	Message    string `json:"message,omitempty"`
	FixCommand string `json:"fix_command,omitempty"`
}

type hostedFleetRunner struct {
	ProjectCheckouts   map[tracker.ProjectID]hostedRunnerCheckout `json:"project_checkouts,omitempty"`
	Sprite             *hostedFleetSprite                         `json:"sprite,omitempty"`
	Update             runnerauth.UpdateView                      `json:"update"`
	Capacity           *runnerauth.CapacityView                   `json:"capacity_configuration,omitempty"`
	Problems           []runnerauth.Problem                       `json:"problems"`
	ID                 string                                     `json:"id"`
	MachineID          string                                     `json:"machine_id"`
	DisplayName        string                                     `json:"display_name"`
	Hostname           string                                     `json:"hostname"`
	ConnectionHealth   string                                     `json:"connection_health"`
	Health             string                                     `json:"health"`
	CapacityHealth     string                                     `json:"capacity_health,omitempty"`
	State              string                                     `json:"state"`
	OS                 string                                     `json:"os"`
	Architecture       string                                     `json:"architecture"`
	ClaimRefusalReason string                                     `json:"claim_refusal_reason"`
	Version            string                                     `json:"version"`
	HostCapacity       int                                        `json:"host_capacity"`
	HostUsed           int                                        `json:"host_used"`
	CapacityLimit      int                                        `json:"capacity_limit"`
	ReportedCapacity   int                                        `json:"reported_capacity"`
	ProviderCapacity   []providercapacity.View                    `json:"provider_capacity"`
	HasHeartbeat       bool                                       `json:"has_heartbeat"`
	LastHeartbeatAt    time.Time                                  `json:"last_heartbeat_at"`
	Leases             []hostedFleetLease                         `json:"leases"`
	IsolationTier      string                                     `json:"isolation_tier"`
	BackendIsolation   map[string][]string                        `json:"backend_isolation"`
	Availability       runnerauth.Availability                    `json:"availability"`
	Editable           bool                                       `json:"editable"`
	EditRefusalReason  string                                     `json:"edit_refusal_reason,omitempty"`
	CanConvertScope    bool                                       `json:"can_convert_scope"`
	CanEditProjects    bool                                       `json:"can_edit_projects"`
	Routing            *runnerauth.Routing                        `json:"routing,omitempty"`
	Revision           int64                                      `json:"revision,omitempty"`
}

type hostedFleetSprite struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	CanWake    bool   `json:"can_wake"`
	WakeFailed bool   `json:"wake_failed"`
}

type hostedFleetAllowance struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
}

type hostedFleetUsage struct {
	Unavailable  []string                        `json:"unavailable,omitempty"`
	WindowEndsAt string                          `json:"window_ends_at"`
	Allowances   map[string]hostedFleetAllowance `json:"allowances"`
}

// hostedProjectSpend and hostedSpend carry the hosted JSON API spend shape. The
// hub records allowance consumption per window, not currency per project, so
// nothing populates them yet and GET /fleet answers spend: null. The types
// exist so the shape is fixed for the first metering source that does.
type hostedProjectSpend struct {
	ProjectID string  `json:"project_id"`
	Amount    float64 `json:"amount"`
}

type hostedSpendPoint struct {
	At     string  `json:"at"`
	Amount float64 `json:"amount"`
}

type hostedSpend struct {
	Today     float64              `json:"today"`
	Window    float64              `json:"window"`
	Currency  string               `json:"currency,omitempty"`
	ByProject []hostedProjectSpend `json:"by_project"`
	Series    []hostedSpendPoint   `json:"series,omitempty"`
}

type hostedFleetResponse struct {
	RunnerNames map[string]hostedRunnerName `json:"runner_names,omitempty"`
	Runners     []hostedFleetRunner         `json:"runners"`
	Editable    bool                        `json:"editable"`
	Usage       hostedFleetUsage            `json:"usage"`
	Spend       *hostedSpend                `json:"spend"`
	// Current is the Detent build this hub runs, which is the version a runner
	// is expected to be on: the hub and the runner are the same binary, and an
	// operator upgrades a host to match the hub it enrolled against.
	Current              string `json:"current"`
	MinimumRunnerVersion string `json:"minimum_runner_version"`
}

type hostedRunnerName struct {
	DisplayName string `json:"display_name"`
	Hostname    string `json:"hostname"`
}

type hostedRunnerNamesResponse struct {
	RunnerNames map[string]hostedRunnerName `json:"runner_names"`
}

// hostedFleet answers GET /fleet for any member of the organization.
func (s *Service) hostedFleet(c echo.Context) error {
	ctx := c.Request().Context()
	readCtx, cancelRead := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancelRead()
	credential, _, err := s.appCredential(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	var result any
	if c.QueryParam("include") == "names" {
		if s.config.TenantRepository == nil {
			return s.nativeAPIError(c, nativeUnavailable("Runner fleet storage is unavailable"))
		}
		var names map[string]hostedRunnerName
		names, err = s.readHostedRunnerNamesTenant(readCtx, true)
		result = hostedRunnerNamesResponse{RunnerNames: names}
	} else if c.QueryParam("include") == "host_metrics" {
		var from, to time.Time
		for _, bound := range []struct {
			name  string
			value *time.Time
		}{{"from", &from}, {"to", &to}} {
			if raw := c.QueryParam(bound.name); raw != "" {
				*bound.value, err = time.Parse(time.RFC3339, raw)
				if err != nil {
					return s.nativeAPIError(c, hubapi.NativePublicInvalid("Host history timestamps must use RFC3339"))
				}
			}
		}
		result, err = s.readHostedRunnerHostHistory(ctx, credential, c.QueryParam("runner_id"), from, to)
	} else {
		result, err = s.readHostedFleet(ctx, credential)
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.hostedAudit(ctx, credential.Hosted, "action", "GET "+c.Path(), "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) readHostedFleet(ctx context.Context, credential hubapi.APICredential) (hostedFleetResponse, error) {
	if s.config.TenantRepository == nil {
		return hostedFleetResponse{}, nativeUnavailable("Runner fleet storage is unavailable")
	}
	return s.readHostedFleetTenant(ctx, credential)
}

// scopeHostUsage replaces each host's in-use count with the leases the reader
// can see. The machine-wide count includes work from projects the reader has
// no grant on, and publishing it would reveal that activity.
func scopeHostUsage(runners []hostedFleetRunner) {
	used := map[string]int{}
	for _, runner := range runners {
		used[runner.MachineID] += len(runner.Leases)
	}
	for index := range runners {
		runners[index].HostUsed = used[runners[index].MachineID]
	}
}

func hostedFleetRunnerView(runner runnerauth.Runner, version string, visible map[tracker.ProjectID]bool, now time.Time) hostedFleetRunner {
	view := hostedFleetRunner{
		Update: runner.UpdateView(now),
		ID:     runner.RunnerID, MachineID: string(runner.MachineID), DisplayName: runner.DisplayName, Hostname: runner.Hostname, Health: runner.Status(now),
		State: runner.State, OS: runner.OS, Architecture: runner.Architecture, Version: version, HostCapacity: runner.HostCapacity,
		HostUsed: runner.HostUsed, CapacityLimit: runner.CapacityLimit, ReportedCapacity: runner.ReportedCapacity,
		ProviderCapacity: runner.ProviderCapacity, LastHeartbeatAt: runner.LastHeartbeatAt, Leases: []hostedFleetLease{},
		IsolationTier: runner.IsolationTier, BackendIsolation: make(map[string][]string, len(runner.BackendIsolation)), Availability: runner.Availability,
		Problems: runner.Problems, ConnectionHealth: runner.ConnectionHealth,
	}
	for backend, tiers := range runner.BackendIsolation {
		view.BackendIsolation[backend] = append([]string{}, tiers...)
	}
	if view.ProviderCapacity == nil {
		view.ProviderCapacity = []providercapacity.View{}
	}
	if len(runner.Problems) > 0 && !slices.ContainsFunc(runner.Problems, func(problem runnerauth.Problem) bool { return problem.ProjectID == "" }) {
		capacityRunner := runner
		capacityRunner.Health = runner.ConnectionHealth
		view.CapacityHealth = capacityRunner.Status(now)
	}
	if view.Update.Status == "refused" || view.Update.Status == "uncertain" {
		if view.CapacityHealth == "" {
			view.CapacityHealth = view.Health
		}
		view.Health = "needs_attention"
	}
	view.Problems = slices.Clone(runner.Problems)
	if view.Problems == nil {
		view.Problems = []runnerauth.Problem{}
	}
	for _, lease := range runner.Leases {
		if !visible[lease.ProjectID] {
			continue
		}
		view.Leases = append(view.Leases, hostedFleetLease{LeaseID: lease.ID, WorkItemID: lease.WorkItemID, Title: lease.Title, ProjectID: lease.ProjectID, ExpiresAt: lease.ExpiresAt})
	}
	return view
}

// hostedFleetUsage pairs the current consumption with the effective allowance.
// A hub without hosted plans reports an empty window rather than failing.
func (s *Service) hostedFleetUsage(ctx context.Context) (hostedFleetUsage, error) {
	if s.database.plansService().Plans() == nil {
		return hostedFleetUsage{Allowances: map[string]hostedFleetAllowance{}}, nil
	}
	if s.config.TenantRepository == nil {
		return hostedFleetUsage{}, nativeUnavailable("Runner fleet storage is unavailable")
	}
	return s.hostedFleetUsageTenant(ctx)
}

// scopeProviderUsage recounts each provider account's reserved concurrency
// from the reservations the reader can see, and restates the reason that
// depended on the organization-wide count.
func scopeProviderUsage(runners []hostedFleetRunner, reservations []scopedProviderAccount) {
	for index := range runners {
		for position := range runners[index].ProviderCapacity {
			view := &runners[index].ProviderCapacity[position]
			view.Used = 0
			for _, reservation := range reservations {
				if sharedProviderAccount(scopedProviderAccount{runnerID: runners[index].ID, Report: view.Report}, reservation) {
					view.Used++
				}
			}
			switch {
			case view.MaxConcurrent > 0 && view.Used >= view.MaxConcurrent:
				view.Reason = "Shared provider concurrency is fully reserved; wait for lease release or expiry"
			case view.MaxConcurrent == 0:
				view.Reason = "No provider account concurrency limit is configured; runner and host limits apply"
			default:
				view.Reason = "Declared provider concurrency is available"
			}
		}
	}
}

func (s *Service) readHostedRunnerHostHistory(ctx context.Context, credential hubapi.APICredential, runnerID string, from, to time.Time) ([]runnerHostHour, error) {
	if s.config.TenantRepository == nil {
		return nil, nativeUnavailable("Runner fleet storage is unavailable")
	}
	return s.readHostedRunnerHostHistoryTenant(ctx, credential, runnerID, from, to)
}
