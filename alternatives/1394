package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
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

func readHostedRunnerNames(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, includeActive bool) (map[string]hostedRunnerName, error) {
	rows, err := query.QueryContext(ctx, `SELECT r.id, r.display_name, m.hostname FROM runner_identities r JOIN machines m ON m.id = r.machine_id WHERE r.organization_id = ? AND (r.removed_at IS NOT NULL OR ?)`, organization, includeActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := make(map[string]hostedRunnerName)
	for rows.Next() {
		var id string
		var name hostedRunnerName
		if err := rows.Scan(&id, &name.DisplayName, &name.Hostname); err != nil {
			return nil, err
		}
		names[id] = name
	}
	return names, rows.Err()
}

// hostedFleet answers GET /fleet for any member of the organization.
func (s *Service) hostedFleet(c echo.Context) error {
	ctx := c.Request().Context()
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	credential, _, err := s.appCredential(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	var result any
	if c.QueryParam("include") == "names" {
		var names map[string]hostedRunnerName
		names, err = readHostedRunnerNames(readCtx, s.database.reader, tracker.OrganizationID(s.appOrganizationID()), true)
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
					return s.nativeAPIError(c, nativePublicInvalid("Host history timestamps must use RFC3339"))
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

func (s *Service) readHostedFleet(ctx context.Context, credential apiCredential) (hostedFleetResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	readable, err := s.hostedReadableProjects(ctx, credential)
	if err != nil {
		return hostedFleetResponse{}, err
	}
	visible := make(map[tracker.ProjectID]bool, len(readable))
	for _, project := range readable {
		visible[tracker.ProjectID(project.ID)] = true
	}
	editable := credential.HostedRole != "viewer" && s.hostedAllRunnerGrants(ctx, credential)
	runners, err := s.hostedFleetRunners(ctx, credential, visible)
	if err != nil {
		return hostedFleetResponse{}, err
	}
	if credential.HostedRole == "reporter" {
		visibleIssues, err := reporterIssueIDs(ctx, s.database.reader, nativeScope{organization: tracker.OrganizationID(s.appOrganizationID()), credential: credential})
		if err != nil {
			return hostedFleetResponse{}, err
		}
		for index := range runners {
			runners[index].Leases = slices.DeleteFunc(runners[index].Leases, func(lease hostedFleetLease) bool { return !visibleIssues[string(lease.WorkItemID)] })
		}
	}
	var projects int
	if err := s.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM projects WHERE deleted_at IS NULL AND organization_id = ?", s.appOrganizationID()).Scan(&projects); err != nil {
		return hostedFleetResponse{}, err
	}
	if credential.HostedRole == "reporter" || len(readable) < projects {
		scopeHostUsage(runners)
	}
	if len(readable) < projects {
		reservations, err := s.hostedVisibleReservations(ctx, visible)
		if err != nil {
			return hostedFleetResponse{}, err
		}
		scopeProviderUsage(runners, reservations)
	}
	usage, err := s.hostedFleetUsage(ctx)
	if err != nil {
		return hostedFleetResponse{}, err
	}
	names, err := readHostedRunnerNames(ctx, s.database.reader, tracker.OrganizationID(s.appOrganizationID()), false)
	if err != nil {
		return hostedFleetResponse{}, err
	}
	return hostedFleetResponse{Runners: runners, RunnerNames: names, Editable: editable, Usage: usage, Current: detentVersion(s.config.Version), MinimumRunnerVersion: minimumRunnerVersion(s.config.Version)}, nil
}

func (s *Service) hostedFleetRunners(ctx context.Context, credential apiCredential, visible map[tracker.ProjectID]bool) ([]hostedFleetRunner, error) {
	organization := tracker.OrganizationID(s.appOrganizationID())
	rows, err := s.database.reader.QueryContext(ctx, `SELECT r.id, COALESCE(m.version, ''), r.local_checks_json, r.created_at FROM runner_identities r LEFT JOIN machines m ON m.id = r.machine_id
WHERE r.organization_id = ? AND r.removed_at IS NULL ORDER BY r.display_name, r.id`, organization)
	if err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type listed struct{ id, version, checks, created string }
	runners := []listed{}
	for rows.Next() {
		var entry listed
		if err := rows.Scan(&entry.id, &entry.version, &entry.checks, &entry.created); err != nil {
			return nil, fmt.Errorf("scan runner: %w", err)
		}
		runners = append(runners, entry)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	fleet := make([]hostedFleetRunner, 0, len(runners))
	spriteContext, cancelSprites := context.WithTimeout(ctx, 5*time.Second)
	defer cancelSprites()
	for _, entry := range runners {
		runner, err := readRunnerWithClock(ctx, s.database.reader, organization, entry.id, s.config.now)
		if err != nil {
			return nil, fmt.Errorf("read runner %s: %w", entry.id, err)
		}
		createdAt, err := parseTimeValue(entry.created)
		if err != nil {
			return nil, fmt.Errorf("read runner enrollment time: %w", err)
		}
		view := hostedFleetRunnerView(runner, entry.version, visible, s.config.now())
		view.HasHeartbeat = runner.LastHeartbeatAt.After(createdAt)
		var checks map[tracker.ProjectID]runnerauth.LocalChecks
		if err := json.Unmarshal([]byte(entry.checks), &checks); err != nil {
			return nil, err
		}
		view.ProjectCheckouts = make(map[tracker.ProjectID]hostedRunnerCheckout)
		for id, check := range checks {
			if !visible[id] || !slices.Contains(runner.ProjectIDs, id) {
				continue
			}
			status := "preparing"
			if check.Checkout == "failed" {
				status = "missing"
			} else if check.Setup == "failed" {
				status = "setup_failed"
			} else if check.Checkout == "passed" && check.Setup == "passed" {
				status = "ready"
			}
			view.ProjectCheckouts[id] = hostedRunnerCheckout{Status: status, Message: check.CheckoutMessage, FixCommand: check.CheckoutFix}
		}
		view.ClaimRefusalReason = runnerClaimRefusal(minimumRunnerVersion(s.config.Version), entry.version)
		if err := s.hostedFleetSprite(ctx, spriteContext, credential, &view, runner, visible); err != nil {
			return nil, err
		}
		routing := runner.Routing
		routing.ProjectCapacityLimits = maps.Clone(routing.ProjectCapacityLimits)
		for project := range routing.ProjectCapacityLimits {
			if !visible[project] {
				delete(routing.ProjectCapacityLimits, project)
			}
		}
		routing.UpdateRequest = nil
		routing.CapacityRequest = nil
		routing.ProjectConfigurationCommand = nil
		view.Routing = &routing
		view.Revision = runner.Revision
		authorityErr := s.requireHostedRunnerProjects(ctx, s.database.reader, credential, runnerManagementProjects(runner))
		if authorityErr != nil && !errors.Is(authorityErr, operatortool.ErrAccessDenied) {
			return nil, authorityErr
		}
		view.Editable = authorityErr == nil
		if authorityErr != nil {
			view.EditRefusalReason = strings.TrimPrefix(authorityErr.Error(), operatortool.ErrAccessDenied.Error()+": ")
		}
		if view.Editable {
			owned, err := runnerOwnedBy(ctx, s.database.reader, organization, runner.RunnerID, credential)
			if err != nil {
				return nil, err
			}
			view.CanEditProjects = owned
			view.CanConvertScope = owned || canManageProjectSecrets(credential)
			capacity, err := s.runnerCapacityView(ctx, s.database.reader, runner, "", s.config.now())
			if err != nil {
				return nil, err
			}
			view.Capacity = &capacity
		}
		fleet = append(fleet, view)
	}
	return fleet, nil
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
	usage := hostedFleetUsage{Allowances: map[string]hostedFleetAllowance{}}
	if s.database.plans() == nil {
		return usage, nil
	}
	entitlement, err := s.database.hostedPlanUsage(ctx, s.config.now(), hostedAllowanceNames()...)
	if err != nil {
		return usage, err
	}
	usage.WindowEndsAt = entitlement.WindowEndsAt.UTC().Format(time.RFC3339)
	for _, name := range hostedAllowanceNames() {
		if limit, limited := entitlement.Allowances[name]; limited {
			usage.Allowances[name] = hostedFleetAllowance{Used: entitlement.Usage[name], Limit: limit}
		}
	}
	return usage, nil
}

// hostedVisibleReservations reads the live provider reservations held by
// leases in projects the reader can see.
func (s *Service) hostedVisibleReservations(ctx context.Context, visible map[tracker.ProjectID]bool) ([]scopedProviderAccount, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	rows, err := s.database.reader.QueryContext(ctx, `SELECT p.reservation_json, l.expires_at, coalesce(i.project_id, ''), coalesce(lr.runner_id, '') FROM provider_reservations p
JOIN leases l ON l.lease_id = p.lease_id JOIN issues i ON i.id = l.issue_id
LEFT JOIN lease_runners lr ON lr.lease_id = l.lease_id
WHERE p.organization_id = ? AND l.released_at IS NULL`, s.appOrganizationID())
	if err != nil {
		return nil, fmt.Errorf("list provider reservations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	now := s.config.now()
	reports := []scopedProviderAccount{}
	for rows.Next() {
		var raw, expiry, project, runnerID string
		if err := rows.Scan(&raw, &expiry, &project, &runnerID); err != nil {
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
		reports = append(reports, scopedProviderAccount{runnerID: runnerID, Report: reservation.Report})
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list provider reservations: %w", err)
	}
	return reports, nil
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
			case view.State == "exhausted":
			case view.MaxConcurrent > 0 && view.Used >= view.MaxConcurrent:
				view.Reason = "Shared provider concurrency is fully reserved; wait for lease release or expiry"
			case view.MaxConcurrent == 0:
				view.Reason = "No provider account concurrency limit is configured; runner and host limits apply"
			case view.State == "unknown":
				view.Reason = "Quota is unknown or stale; only the declared concurrency bound is available"
			default:
				view.Reason = "Bounded concurrency available; quota is an observation, not transferable credit"
			}
		}
	}
}

func (s *Service) readHostedRunnerHostHistory(ctx context.Context, credential apiCredential, runnerID string, from, to time.Time) ([]runnerHostHour, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if runnerID == "" {
		return nil, nativePublicInvalid("Runner ID is required for host history")
	}
	readable, err := s.hostedReadableProjects(ctx, credential)
	if err != nil {
		return nil, err
	}
	var projects int
	if err := s.database.reader.QueryRowContext(ctx, "SELECT count(*) FROM projects WHERE deleted_at IS NULL AND organization_id = ?", s.appOrganizationID()).Scan(&projects); err != nil {
		return nil, err
	}
	if len(readable) < projects {
		return nil, nativeNotFound()
	}
	fleet, err := s.readHostedFleet(ctx, credential)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(fleet.Runners, func(runner hostedFleetRunner) bool { return runner.ID == runnerID }) {
		return nil, nativeNotFound()
	}
	return s.readRunnerHostHistory(ctx, tracker.OrganizationID(s.appOrganizationID()), runnerID, from, to)
}
