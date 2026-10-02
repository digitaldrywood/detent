package runnerauth

import (
	"encoding/hex"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
)

type CapacityRequest struct {
	ExpectedConfigRevision string `json:"expected_config_revision"`
	Capacity               int    `json:"capacity"`
	Backend                string `json:"backend,omitempty"`
}

func (r CapacityRequest) Validate() error {
	revision, err := hex.DecodeString(r.ExpectedConfigRevision)
	if err != nil || len(revision) != 32 || r.Capacity < 1 || r.Capacity > 10000 {
		return errors.New("capacity requires a configuration revision and a limit between 1 and 10000")
	}
	if r.Backend != "" {
		return (providercapacity.Requirement{Role: "code", Backend: r.Backend, Model: "capacity"}).Validate()
	}
	return nil
}

type CapacityConfig struct {
	Revision     string    `json:"revision"`
	LocalLimit   int       `json:"local_limit"`
	ClientLimit  int       `json:"client_limit"`
	RuntimeLimit int       `json:"runtime_limit"`
	Manageable   bool      `json:"manageable"`
	Constraint   string    `json:"constraint,omitempty"`
	ObservedAt   time.Time `json:"observed_at"`
}

func (c CapacityConfig) Validate() error {
	if c.LocalLimit < 1 || c.LocalLimit > 10000 || c.ClientLimit < 1 || c.ClientLimit > 10000 || c.RuntimeLimit < 1 || c.RuntimeLimit > 10000 || c.ObservedAt.IsZero() {
		return errors.New("capacity evidence requires bounded configured and runtime limits")
	}
	if err := (CapacityRequest{ExpectedConfigRevision: c.Revision, Capacity: c.LocalLimit}).Validate(); err != nil {
		return err
	}
	switch c.Constraint {
	case "", "The selected configuration changed; read capacity and retry with its current revision.", "The selected configuration could not be written; check the enrolled runner configuration permissions.", "Waiting for the existing configuration reload to apply the saved limits.":
		return nil
	default:
		return errors.New("invalid capacity application constraint")
	}
}

type CapacityLimit struct {
	Availability string    `json:"availability,omitempty"`
	Owner        string    `json:"owner"`
	Limit        *int      `json:"limit"`
	Constraint   string    `json:"constraint,omitempty"`
	ObservedAt   time.Time `json:"observed_at,omitzero"`
}

type CapacityView struct {
	Scope              string          `json:"scope"`
	DispatchConstraint string          `json:"dispatch_constraint"`
	RunnerID           string          `json:"runner_id"`
	Revision           int64           `json:"revision"`
	Desired            int             `json:"desired"`
	Applied            *CapacityConfig `json:"applied"`
	Effective          *int            `json:"effective"`
	Status             string          `json:"status"`
	Limits             []CapacityLimit `json:"limits"`
}

func (r Runner) CapacityRequiresApplication(capacity int) bool {
	return capacity > 0 && r.CapacityConfig != nil && (capacity != r.CapacityConfig.LocalLimit || capacity != r.CapacityConfig.ClientLimit)
}

func (r Runner) CapacityView(backend string, now time.Time) CapacityView {
	view := CapacityView{Scope: "runner_configuration_ceiling", DispatchConstraint: "Actual claims also require current project, pool, lane, policy, provider quota and plan admission.", RunnerID: r.RunnerID, Revision: r.Revision, Desired: r.CapacityLimit, Status: "unknown"}
	add := func(owner string, limit *int, constraint string, observed time.Time) {
		view.Limits = append(view.Limits, CapacityLimit{Owner: owner, Limit: limit, Constraint: constraint, ObservedAt: observed})
	}
	if backend == "" && r.CapacityRequest != nil {
		backend = r.CapacityRequest.Backend
	}
	cloud, host := r.CapacityLimit, r.HostCapacity
	add("cloud_runner", &cloud, "", now)
	add("shared_host", &host, "Change the existing host capacity setting if this operator-owned ceiling is too small.", now)
	fresh := !now.Before(r.LastHeartbeatAt) && now.Before(r.LastHeartbeatAt.Add(HeartbeatTimeout)) && r.ConnectionHealth != "offline" && r.Health != "revoked" && r.Health != "expired"
	if !fresh {
		add("local_runner", nil, "Runner heartbeat is stale; wait for the enrolled runner to report fresh evidence.", r.LastHeartbeatAt)
		return view
	}
	effective := min(cloud, host, r.ReportedCapacity)
	reported := r.ReportedCapacity
	add("reported_runner", &reported, "", r.LastHeartbeatAt)
	config := r.CapacityConfig
	if config == nil || now.Before(config.ObservedAt) || !now.Before(config.ObservedAt.Add(HeartbeatTimeout)) {
		add("local_configuration", nil, "The enrolled runner has not reported current configuration authority; upgrade or reconnect the runner.", time.Time{})
		return view
	}
	copy := *config
	view.Applied = &copy
	local, client, runtime := config.LocalLimit, config.ClientLimit, config.RuntimeLimit
	add("local_configuration", &local, config.Constraint, config.ObservedAt)
	add("client_configuration", &client, "", config.ObservedAt)
	add("runtime_configuration", &runtime, "", config.ObservedAt)
	effective = min(effective, local, client, runtime)
	knownProvider, unknownProvider := false, false
	matchingProviders := 0
	for _, provider := range r.ProviderCapacity {
		if backend != "" && provider.Backend != backend {
			continue
		}
		knownProvider = true
		matchingProviders++
		owner := "provider:" + provider.Backend + ":" + provider.AccountAlias
		if now.Before(provider.ObservedAt) || !now.Before(provider.ObservedAt.Add(providercapacity.MaxAge)) {
			unknownProvider = true
			add(owner, nil, "Provider report is stale; refresh its configured producer. Detent cannot manage an external producer.", provider.ObservedAt)
			continue
		}
		limit := provider.MaxConcurrent
		add(owner, &limit, "The configured external provider-capacity producer is unmanaged. Change its concurrency setting and let its normal refresh publish evidence; editing the report is ineffective.", provider.ObservedAt)
		view.Limits[len(view.Limits)-1].Availability = provider.State
		if provider.State == "exhausted" {
			view.Limits[len(view.Limits)-1].Constraint = "Provider account is exhausted; wait for a fresh observation or reset hint."
			limit = 0
		}
		effective = min(effective, limit)
	}
	if !knownProvider || unknownProvider || matchingProviders > 1 {
		add("provider", nil, "Select a backend with a single fresh provider report; a combined account or provider ceiling is unknown.", time.Time{})
		return view
	}
	view.Effective = &effective
	view.Status = "partially_applied"
	if local == cloud && client == cloud && runtime == cloud && effective >= cloud && config.Constraint == "" {
		view.Status = "applied"
	}
	return view
}
