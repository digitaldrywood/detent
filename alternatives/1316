package hubclient

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/url"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type FleetClient struct {
	client       *Client
	organization tracker.OrganizationID
	projects     map[string]tracker.ProjectID
}

func NewFleetClient(client *Client, organization tracker.OrganizationID, projects map[string]tracker.ProjectID) (*FleetClient, error) {
	if client == nil {
		return nil, errors.New("hub client is required")
	}
	if _, err := runnerOrganizationPath(organization); err != nil {
		return nil, err
	}
	return &FleetClient{client: client, organization: organization, projects: maps.Clone(projects)}, nil
}

func (f *FleetClient) base() string {
	return "/api/v2/organizations/" + url.PathEscape(string(f.organization))
}

func (f *FleetClient) Fleet(ctx context.Context) (runnerauth.Fleet, error) {
	result := runnerauth.Fleet{Runners: []runnerauth.Runner{}, Projects: maps.Clone(f.projects)}
	if f.client.runner != nil {
		file, err := runnerauth.Load(f.client.runner.path)
		if err != nil {
			return result, err
		}
		var runner runnerauth.Runner
		if err := f.client.request(ctx, http.MethodGet, f.base()+"/runners/"+url.PathEscape(file.Identity.RunnerID)+"/routing", nil, &runner); err != nil {
			return result, err
		}
		result.Runners = append(result.Runners, runner)
		return result, nil
	}
	err := f.client.request(ctx, http.MethodGet, f.base()+"/runners", nil, &result.Runners)
	result.Editable = err == nil
	return result, err
}

func (f *FleetClient) UpdateRunner(ctx context.Context, id string, change runnerauth.RoutingChange) error {
	if f.client.runner != nil {
		return errors.New("runner routing edits require administrator credentials")
	}
	return f.client.request(ctx, http.MethodPut, f.base()+"/runners/"+url.PathEscape(id)+"/routing", change, nil)
}

func (f *FleetClient) UpdateHost(ctx context.Context, id tracker.MachineID, change runnerauth.HostChange) error {
	if f.client.runner != nil {
		return errors.New("host edits require administrator credentials")
	}
	return f.client.request(ctx, http.MethodPut, f.base()+"/machines/"+url.PathEscape(string(id))+"/routing", change, nil)
}

func (f *FleetClient) ProjectEligibility(ctx context.Context, project string) (runnerauth.ProjectEligibility, error) {
	result := runnerauth.ProjectEligibility{Project: project, Runners: []runnerauth.Eligibility{}, Exclusions: []runnerauth.Exclusion{}}
	id, ok := f.projects[project]
	if !ok {
		return result, errors.New("project has no configured Hub runner mapping")
	}
	native, err := f.client.Native(f.organization, id)
	if err != nil {
		return result, err
	}
	approval, err := native.ProjectPolicy(ctx)
	if err != nil {
		return result, err
	}
	result.Policy = approval.Policy
	fleet, err := f.Fleet(ctx)
	if err != nil {
		return result, err
	}
	eligible := false
	for _, r := range fleet.Runners {
		exclusions := r.Exclusions(id, result.Policy.Requirements, false)
		eligible = eligible || len(exclusions) == 0
		result.Runners = append(result.Runners, runnerauth.Eligibility{Runner: r, Exclusions: exclusions})
	}
	if !eligible {
		result.Exclusions = append(result.Exclusions, runnerauth.Exclusion{Code: "no_eligible_runner", Message: "Work stays queued until an authorized runner matches every selector and has available capacity"})
	}
	return result, nil
}
