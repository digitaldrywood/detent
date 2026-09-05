package runnerauth

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const HeartbeatTimeout = 2 * time.Minute

type Routing struct {
	DisplayName   string              `json:"display_name"`
	Tags          []string            `json:"tags"`
	State         string              `json:"state"`
	CapacityLimit int                 `json:"capacity_limit"`
	ProjectIDs    []tracker.ProjectID `json:"project_ids"`
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
	LastHeartbeatAt  time.Time              `json:"last_heartbeat_at"`
	Operations       []string               `json:"operations"`
	Leases           []RunnerLease          `json:"leases"`
}

type RunnerLease struct {
	ID         tracker.LeaseID          `json:"lease_id"`
	WorkItemID tracker.NativeWorkItemID `json:"work_item_id"`
	Title      string                   `json:"title"`
	ProjectID  tracker.ProjectID        `json:"project_id"`
	Policy     policy.Descriptor        `json:"policy"`
	ExpiresAt  time.Time                `json:"expires_at"`
	Exclusions []Exclusion              `json:"exclusions"`
}

type Exclusion struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Eligibility struct {
	Runner     Runner      `json:"runner"`
	Exclusions []Exclusion `json:"exclusions"`
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
	return r
}

func (r Routing) Validate() error {
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
	return nil
}

func (r Runner) Exclusions(project tracker.ProjectID, requirements policy.Requirements, activeLease bool) []Exclusion {
	result := []Exclusion{}
	add := func(code, message string) { result = append(result, Exclusion{Code: code, Message: message}) }
	if !slices.Contains(r.ProjectIDs, project) {
		add("project_access_denied", "Runner has no administrator-approved access to this project")
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
	if !activeLease && r.Health == "offline" {
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
	return result
}
