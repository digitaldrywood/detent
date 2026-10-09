package runnerauth

import (
	"errors"
	"fmt"
	"net"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/activehours"
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const HeartbeatTimeout = 2 * time.Minute

type Routing struct {
	ProjectConfigurationCommand *ProjectConfigurationCommand `json:"-"`
	UpdateRequest               *UpdateRequest               `json:"update_request,omitempty"`
	CapacityRequest             *CapacityRequest             `json:"capacity_request,omitempty"`
	DisplayName                 string                       `json:"display_name"`
	Tags                        []string                     `json:"tags"`
	State                       string                       `json:"state"`
	CapacityLimit               int                          `json:"capacity_limit"`
	ProjectRanks                map[tracker.ProjectID]int    `json:"project_ranks,omitempty"`
	ProjectIDs                  []tracker.ProjectID          `json:"project_ids"`
	IsolationTier               string                       `json:"isolation_tier"`
	HostServices                []string                     `json:"host_services"`
	Availability                Availability                 `json:"availability"`
}

type Availability struct {
	Timezone     string   `json:"timezone"`
	Windows      []string `json:"windows"`
	HardDeadline string   `json:"hard_deadline"`
}

type ClaimSlot struct {
	ID       tracker.LeaseID `json:"lease_id"`
	RunnerID string          `json:"runner_id"`
}

type ClaimState struct {
	HostCapacity   int                          `json:"host_capacity"`
	RunnerCapacity int                          `json:"runner_capacity"`
	Slots          []ClaimSlot                  `json:"slots"`
	PolicyIDs      map[tracker.ProjectID]string `json:"policy_ids"`
}

type RoutingSnapshot struct {
	HostMetricsAcknowledged     []hostmetrics.Acknowledgment `json:"host_metrics_acknowledged,omitempty"`
	Changes                     *HeartbeatChanges            `json:"changes,omitempty"`
	TargetRunnerVersion         string                       `json:"target_runner_version,omitempty"`
	ClaimState                  *ClaimState                  `json:"claim_state,omitempty"`
	ProjectConfigurationRequest *ProjectConfigurationRequest `json:"project_configuration_request,omitempty"`
	GitHubIntake                *tracker.GitHubBatchTask     `json:"github_intake,omitempty"`
	RunnerID                    string                       `json:"runner_id"`
	Revision                    int64                        `json:"revision"`
	Routing                     Routing                      `json:"routing"`
}

type HeartbeatChanges struct {
	Cursor             string          `json:"cursor"`
	Reset              bool            `json:"reset"`
	Items              []HeartbeatItem `json:"items"`
	PolicyID           string          `json:"policy_id"`
	CapabilitiesDigest string          `json:"capabilities_digest"`
	Claimable          bool            `json:"claimable"`
}

type HeartbeatItem struct {
	ID             tracker.NativeWorkItemID `json:"work_item_id"`
	Revision       tracker.Revision         `json:"revision,string"`
	LastActivityAt time.Time                `json:"last_activity_at"`
}

type RoutingChange struct {
	Routing
	ExpectedRevision int64 `json:"expected_revision"`
}

type HostChange struct {
	ExpectedRevision int64  `json:"expected_revision"`
	DisplayName      string `json:"display_name"`
	Capacity         int    `json:"capacity"`
}

type Runner struct {
	BackendIsolation map[string][]string     `json:"backend_isolation,omitempty"`
	Update           *UpdateObservation      `json:"update,omitempty"`
	CapacityConfig   *CapacityConfig         `json:"capacity_configuration,omitempty"`
	Problems         []Problem               `json:"problems"`
	ProviderCapacity []providercapacity.View `json:"provider_capacity,omitempty"`
	Binding
	Routing
	OrganizationID   tracker.OrganizationID `json:"organization_id"`
	Revision         int64                  `json:"revision"`
	Hostname         string                 `json:"hostname"`
	HostDisplayName  string                 `json:"host_display_name"`
	HostRevision     int64                  `json:"host_revision"`
	HostCapacity     int                    `json:"host_capacity"`
	HostUsed         int                    `json:"host_used"`
	ReportedCapacity int                    `json:"reported_capacity"`
	Used             int                    `json:"used"`
	OS               string                 `json:"os"`
	Architecture     string                 `json:"architecture"`
	Health           string                 `json:"health"`
	ConnectionHealth string                 `json:"connection_health"`
	LastHeartbeatAt  time.Time              `json:"last_heartbeat_at"`
	Operations       []string               `json:"operations"`
	Leases           []RunnerLease          `json:"leases"`
}

type RunnerLease struct {
	ProviderReservation *providercapacity.Reservation `json:"provider_reservation,omitempty"`
	ID                  tracker.LeaseID               `json:"lease_id"`
	FencingToken        tracker.FencingToken          `json:"fencing_token,string"`
	WorkItemID          tracker.NativeWorkItemID      `json:"work_item_id"`
	Title               string                        `json:"title"`
	ProjectID           tracker.ProjectID             `json:"project_id"`
	Policy              policy.Descriptor             `json:"policy"`
	ExpiresAt           time.Time                     `json:"expires_at"`
	Exclusions          []Exclusion                   `json:"exclusions"`
}

type Exclusion struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Eligibility struct {
	LocalChecks *LocalChecks `json:"local_checks,omitempty"`
	Runner      Runner       `json:"runner"`
	Exclusions  []Exclusion  `json:"exclusions"`
}

type Fleet struct {
	Runners  []Runner                     `json:"runners"`
	Editable bool                         `json:"editable"`
	Projects map[string]tracker.ProjectID `json:"projects"`
}

type ProjectEligibility struct {
	Project    string            `json:"project"`
	Policy     policy.Descriptor `json:"policy"`
	Runners    []Eligibility     `json:"runners"`
	Exclusions []Exclusion       `json:"exclusions"`
}

func (r Routing) Normalized() Routing {
	r.DisplayName = strings.TrimSpace(r.DisplayName)
	r.Tags = (policy.Requirements{RequiredTags: r.Tags}).Normalized().RequiredTags
	if r.Tags == nil {
		r.Tags = []string{}
	}
	r.ProjectIDs = slices.Clone(r.ProjectIDs)
	slices.Sort(r.ProjectIDs)
	if r.IsolationTier == "" {
		r.IsolationTier = "sandbox"
	}
	r.HostServices = slices.Clone(r.HostServices)
	if r.HostServices == nil {
		r.HostServices = []string{}
	}
	for i := range r.HostServices {
		r.HostServices[i] = strings.TrimSpace(r.HostServices[i])
	}
	config := (activehours.Config{Timezone: r.Availability.Timezone, Windows: r.Availability.Windows}).Normalize()
	r.Availability.Timezone = config.Timezone
	r.Availability.Windows = config.Windows
	r.Availability.HardDeadline = strings.TrimSpace(r.Availability.HardDeadline)
	return r
}

func (r Routing) Validate() error {
	if r.UpdateRequest != nil {
		if err := r.UpdateRequest.Validate(); err != nil {
			return err
		}
	}
	if r.CapacityRequest != nil {
		if err := r.CapacityRequest.Validate(); err != nil {
			return err
		}
	}
	if r.DisplayName == "" || len(r.DisplayName) > 200 || strings.ContainsAny(r.DisplayName, "\r\n\x00") {
		return errors.New("runner display name must contain 1 to 200 characters on one line")
	}
	if err := (policy.Requirements{RequiredTags: r.Tags}).Validate(); err != nil {
		return err
	}
	if !slices.Contains([]string{"active", "draining", "disabled"}, r.State) || r.CapacityLimit < 0 || r.CapacityLimit > 10000 {
		return errors.New("runner state must be active, draining or disabled and capacity must be between 0 and 10000")
	}
	if len(r.ProjectIDs) > 100 {
		return errors.New("runner access is limited to 100 projects")
	}
	for i, id := range r.ProjectIDs {
		if id == "" || slices.Contains(r.ProjectIDs[:i], id) {
			return errors.New("runner project access must contain unique project IDs")
		}
	}
	if r.IsolationTier != "sandbox" && r.IsolationTier != "native-trusted" {
		return errors.New("runner isolation tier must be sandbox or native-trusted")
	}
	if len(r.HostServices) > 32 {
		return errors.New("runner host services are limited to 32")
	}
	for _, service := range r.HostServices {
		if err := validateHostService(service); err != nil {
			return err
		}
	}
	if len(r.Availability.Windows) > 64 {
		return errors.New("runner availability is limited to 64 windows")
	}
	config := activehours.Config{Timezone: r.Availability.Timezone, Windows: r.Availability.Windows}
	if problems := config.ValidateNonOverlapping("availability"); len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	if r.Availability.HardDeadline != "" {
		deadline, err := time.ParseDuration(r.Availability.HardDeadline)
		if err != nil || deadline <= 0 || config.IsZero() {
			return errors.New("availability.hard_deadline requires a positive duration and a window")
		}
	}
	return nil
}

func validateHostService(service string) error {
	if strings.HasPrefix(service, "tcp:") {
		host, rawPort, err := net.SplitHostPort(strings.TrimPrefix(service, "tcp:"))
		port, portErr := strconv.Atoi(rawPort)
		if err != nil || portErr != nil || host != "127.0.0.1" || port < 1 || port > 65535 {
			return errors.New("host services require tcp:127.0.0.1:<port>")
		}
		return nil
	}
	socketPath, ok := strings.CutPrefix(service, "unix:")
	if !ok || !path.IsAbs(socketPath) || path.Clean(socketPath) != socketPath {
		return errors.New("host services require an absolute unix socket path")
	}
	name := strings.ToLower(path.Base(socketPath))
	for _, privileged := range []string{"docker", "podman", "containerd", "crio", "buildkit", "libvirt", "kubelet"} {
		if strings.Contains(name, privileged) {
			return fmt.Errorf("host service %q grants host administration", service)
		}
	}
	return nil
}

func (r Runner) Exclusions(project tracker.ProjectID, requirements policy.Requirements, activeLease bool) []Exclusion {
	result := []Exclusion{}
	add := func(code, message string) { result = append(result, Exclusion{Code: code, Message: message}) }
	if !slices.Contains(r.ProjectIDs, project) {
		add("project_access_denied", "Runner owner has not allowed this project")
	}
	if !slices.Contains(r.Operations, Claim) {
		add("claim_not_permitted", "Runner credential does not permit claims")
	}
	if r.State == "disabled" {
		add("runner_disabled", "Runner is disabled")
	}
	if !activeLease && r.State == "draining" {
		add("runner_draining", "Runner is draining; active leases may finish")
	}
	if r.Health == "revoked" || r.Health == "expired" {
		add("runner_"+r.Health, "Runner credential is "+r.Health)
	}
	if !activeLease && (r.Health == "offline" || r.ConnectionHealth == "offline") {
		add("runner_offline", "Runner heartbeat is stale; work stays queued for this target")
	}
	if err := requirements.Match(r.RunnerID, string(r.MachineID), r.Tags); err != nil {
		add("selector_no_match", strings.TrimPrefix(err.Error(), "selector_no_match: "))
	}
	if !activeLease && r.HostUsed >= r.HostCapacity {
		add("host_capacity", "Shared host capacity is full or paused")
	}
	if !activeLease && r.Used >= min(r.CapacityLimit, r.ReportedCapacity) {
		add("runner_capacity", "Runner capacity is full or paused")
	}
	if !activeLease && len(r.ProviderCapacity) > 0 {
		available := slices.ContainsFunc(r.ProviderCapacity, func(view providercapacity.View) bool {
			return view.Available()
		})
		if !available {
			add("provider_capacity", "All reported provider accounts are exhausted or fully reserved; work stays queued")
		}
	}
	return result
}

func (a Availability) Evaluate(now time.Time) (activehours.Status, error) {
	return activehours.Evaluate(activehours.Config{Timezone: a.Timezone, Windows: a.Windows}, now, time.Time{})
}

func (a Availability) Deadline(started time.Time) (time.Time, error) {
	if a.HardDeadline == "" {
		return time.Time{}, nil
	}
	delay, err := time.ParseDuration(a.HardDeadline)
	if err != nil || delay <= 0 {
		return time.Time{}, errors.New("availability hard deadline must be a positive duration")
	}
	status, err := a.Evaluate(started)
	if err != nil {
		return time.Time{}, err
	}
	if status.NextClose.IsZero() {
		return time.Time{}, nil
	}
	if !status.Open {
		cursor := started.AddDate(0, 0, -8)
		var closeTime time.Time
		for cursor.Before(started) {
			previous, err := a.Evaluate(cursor)
			if err != nil {
				return time.Time{}, err
			}
			if previous.NextClose.IsZero() || previous.NextClose.After(started) {
				break
			}
			closeTime = previous.NextClose
			cursor = closeTime
		}
		if closeTime.IsZero() {
			return time.Time{}, nil
		}
		return closeTime.Add(delay), nil
	}
	return status.NextClose.Add(delay), nil
}

func (r Runner) Status(now time.Time) string {
	if r.Health == "online" {
		status, err := r.Availability.Evaluate(now)
		if err == nil && !status.Open {
			return "outside_hours"
		}
	}
	return r.Health
}
