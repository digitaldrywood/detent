package hubserver

import (
	"context"
	"encoding/json"
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

	machine string
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
	var projects int
	if err := s.database.db.QueryRowContext(ctx, "SELECT count(*) FROM projects WHERE organization_id = ?", s.config.Hosted.OrganizationID).Scan(&projects); err != nil {
		return s.nativeAPIError(c, err)
	}
	if len(readable) < projects {
		scopeHostUsage(runners)
		reservations, err := s.hostedVisibleReservations(ctx, visible)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		scopeProviderUsage(runners, reservations)
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
	rows, err := s.database.db.QueryContext(ctx, `SELECT r.id, COALESCE(m.version, '') FROM runner_identities r LEFT JOIN machines m ON m.id = r.machine_id
WHERE r.organization_id = ? ORDER BY r.display_name, r.id`, organization)
	if err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type listed struct{ id, version string }
	runners := []listed{}
	for rows.Next() {
		var entry listed
		if err := rows.Scan(&entry.id, &entry.version); err != nil {
			return nil, fmt.Errorf("scan runner: %w", err)
		}
		runners = append(runners, entry)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	fleet := make([]hostedFleetRunner, 0, len(runners))
	machines := make([]string, 0, len(runners))
	for _, entry := range runners {
		runner, err := readRunner(ctx, s.database.db, organization, entry.id, s.config.now())
		if err != nil {
			return nil, fmt.Errorf("read runner %s: %w", entry.id, err)
		}
		fleet = append(fleet, hostedFleetRunnerView(runner, entry.version, visible))
		machines = append(machines, string(runner.MachineID))
	}
	for index := range fleet {
		fleet[index].machine = machines[index]
	}
	return fleet, nil
}

// scopeHostUsage replaces each host's in-use count with the leases the reader
// can see. The machine-wide count includes work from projects the reader has
// no grant on, and publishing it would reveal that activity.
func scopeHostUsage(runners []hostedFleetRunner) {
	used := map[string]int{}
	for _, runner := range runners {
		used[runner.machine] += len(runner.Leases)
	}
	for index := range runners {
		runners[index].HostUsed = used[runners[index].machine]
	}
}

func hostedFleetRunnerView(runner runnerauth.Runner, version string, visible map[tracker.ProjectID]bool) hostedFleetRunner {
	view := hostedFleetRunner{
		ID: runner.RunnerID, DisplayName: runner.DisplayName, Hostname: runner.Hostname, Health: runner.Health,
		State: runner.State, OS: runner.OS, Architecture: runner.Architecture, Version: version, HostCapacity: runner.HostCapacity,
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

// hostedVisibleReservations reads the live provider reservations held by
// leases in projects the reader can see.
func (s *Service) hostedVisibleReservations(ctx context.Context, visible map[tracker.ProjectID]bool) ([]providercapacity.Report, error) {
	rows, err := s.database.db.QueryContext(ctx, `SELECT p.reservation_json, l.expires_at, coalesce(i.project_id, '') FROM provider_reservations p
JOIN leases l ON l.lease_id = p.lease_id JOIN issues i ON i.id = l.issue_id
WHERE p.organization_id = ? AND l.released_at IS NULL`, s.config.Hosted.OrganizationID)
	if err != nil {
		return nil, fmt.Errorf("list provider reservations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	now := s.config.now()
	reports := []providercapacity.Report{}
	for rows.Next() {
		var raw, expiry, project string
		if err := rows.Scan(&raw, &expiry, &project); err != nil {
			return nil, fmt.Errorf("scan provider reservation: %w", err)
		}
		end, err := parseTimeValue(expiry)
		if err != nil {
			return nil, fmt.Errorf("read provider reservation expiry: %w", err)
		}
		if !end.After(now) || !visible[tracker.ProjectID(project)] {
			continue
		}
		var reservation providercapacity.Reservation
		if err := json.Unmarshal([]byte(raw), &reservation); err != nil {
			return nil, fmt.Errorf("decode provider reservation: %w", err)
		}
		reports = append(reports, reservation.Report)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list provider reservations: %w", err)
	}
	return reports, nil
}

// scopeProviderUsage recounts each provider account's reserved concurrency
// from the reservations the reader can see, and restates the reason that
// depended on the organization-wide count.
func scopeProviderUsage(runners []hostedFleetRunner, reservations []providercapacity.Report) {
	for index := range runners {
		for position := range runners[index].ProviderCapacity {
			view := &runners[index].ProviderCapacity[position]
			view.Used = 0
			for _, reservation := range reservations {
				if sharedProviderAccount(view.Report, reservation) {
					view.Used++
				}
			}
			switch {
			case view.State == "exhausted":
			case view.Used >= view.MaxConcurrent:
				view.Reason = "Shared provider concurrency is fully reserved; wait for lease release or expiry"
			case view.State == "unknown":
				view.Reason = "Quota is unknown or stale; only the declared concurrency bound is available"
			default:
				view.Reason = "Bounded concurrency available; quota is an observation, not transferable credit"
			}
		}
	}
}
