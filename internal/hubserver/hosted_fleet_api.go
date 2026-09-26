package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// The fleet and spend screen (decisions section 12). Every member reads the
// fleet; a lease is only described when the member can read the project it
// runs for, so the runner list never leaks work from another project.

type hostedFleetLease struct {
	LeaseID    tracker.LeaseID          `json:"lease_id"`
	WorkItemID tracker.NativeWorkItemID `json:"work_item_id"`
	Title      string                   `json:"title"`
	ProjectID  tracker.ProjectID        `json:"project_id"`
	ExpiresAt  time.Time                `json:"expires_at"`
}

type hostedFleetRunner struct {
	ID           string `json:"id"`
	DisplayName  string `json:"display_name"`
	Hostname     string `json:"hostname"`
	Health       string `json:"health"`
	State        string `json:"state"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	// Version is the Detent build this runner's host reported. The settings
	// screen compares it against the response's Current to draw the update
	// state the footer's pill points at.
	Version          string                  `json:"version"`
	HostCapacity     int                     `json:"host_capacity"`
	HostUsed         int                     `json:"host_used"`
	CapacityLimit    int                     `json:"capacity_limit"`
	ReportedCapacity int                     `json:"reported_capacity"`
	ProviderCapacity []providercapacity.View `json:"provider_capacity"`
	LastHeartbeatAt  time.Time               `json:"last_heartbeat_at"`
	Leases           []hostedFleetLease      `json:"leases"`
}

type hostedFleetAllowance struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
}

type hostedFleetUsage struct {
	WindowEndsAt string                          `json:"window_ends_at"`
	Allowances   map[string]hostedFleetAllowance `json:"allowances"`
}

// hostedProjectSpend and hostedSpend carry the section 12 spend shape. The
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
	Runners []hostedFleetRunner `json:"runners"`
	Usage   hostedFleetUsage    `json:"usage"`
	Spend   *hostedSpend        `json:"spend"`
	// Current is the Detent build this hub runs, which is the version a runner
	// is expected to be on: the hub and the runner are the same binary, and an
	// operator upgrades a host to match the hub it enrolled against.
	Current string `json:"current"`
}

// hostedFleet answers GET /fleet for any member of the organization.
func (s *Service) hostedFleet(c echo.Context) error {
	credential, _, err := s.hostedCredential(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	ctx := c.Request().Context()
	readable, err := s.hostedReadableProjects(ctx, credential)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	visible := make(map[tracker.ProjectID]bool, len(readable))
	for _, project := range readable {
		visible[tracker.ProjectID(project.ID)] = true
	}
	runners, err := s.hostedFleetRunners(ctx, visible)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	usage, err := s.hostedFleetUsage(ctx)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.hostedAudit(ctx, credential.Hosted, "action", "GET "+c.Path(), "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, hostedFleetResponse{Runners: runners, Usage: usage, Current: detentVersion(s.config.Version)})
}

func (s *Service) hostedFleetRunners(ctx context.Context, visible map[tracker.ProjectID]bool) ([]hostedFleetRunner, error) {
	organization := tracker.OrganizationID(s.config.Hosted.OrganizationID)
	rows, err := s.database.db.QueryContext(ctx, "SELECT id FROM runner_identities WHERE organization_id = ? ORDER BY display_name, id", organization)
	if err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan runner: %w", err)
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	fleet := make([]hostedFleetRunner, 0, len(ids))
	for _, id := range ids {
		runner, err := readRunner(ctx, s.database.db, organization, id, s.config.now())
		if err != nil {
			return nil, fmt.Errorf("read runner %s: %w", id, err)
		}
		fleet = append(fleet, hostedFleetRunnerView(runner, visible))
	}
	return fleet, nil
}

func hostedFleetRunnerView(runner runnerauth.Runner, visible map[tracker.ProjectID]bool) hostedFleetRunner {
	view := hostedFleetRunner{
		ID: runner.RunnerID, DisplayName: runner.DisplayName, Hostname: runner.Hostname, Health: runner.Health,
		State: runner.State, OS: runner.OS, Architecture: runner.Architecture, Version: runner.Version, HostCapacity: runner.HostCapacity,
		HostUsed: runner.HostUsed, CapacityLimit: runner.CapacityLimit, ReportedCapacity: runner.ReportedCapacity,
		ProviderCapacity: runner.ProviderCapacity, LastHeartbeatAt: runner.LastHeartbeatAt, Leases: []hostedFleetLease{},
	}
	if view.ProviderCapacity == nil {
		view.ProviderCapacity = []providercapacity.View{}
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
	usage := hostedFleetUsage{Allowances: map[string]hostedFleetAllowance{}}
	if s.database.hostedPlans == nil {
		return usage, nil
	}
	entitlement, err := s.database.hostedPlanUsage(ctx, s.config.now())
	if err != nil {
		return usage, err
	}
	usage.WindowEndsAt = entitlement.WindowEndsAt.UTC().Format(time.RFC3339)
	for _, name := range hostedAllowanceNames() {
		usage.Allowances[name] = hostedFleetAllowance{Used: entitlement.Usage[name], Limit: entitlement.Allowances[name]}
	}
	return usage, nil
}
