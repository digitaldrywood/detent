package runnerfleet

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func RunnerLeaseMutationRoute(path string) bool {
	return path == hubapi.NativeBase+"/leases/:lease/validate" || path == hubapi.NativeBase+"/leases/:lease/renew" || path == hubapi.NativeBase+"/leases/:lease/release"
}

func (d RoutingDeps) AuthorizeRunnerLeaseProject(ctx context.Context, scope hubscope.NativeScope) error {
	if d.Repository == nil || scope.Credential.Runner.RunnerID == "" || scope.Credential.Runner.OrganizationID != scope.Organization || scope.Credential.EntryKey != nil {
		return hubapi.NativeNotFound()
	}
	return d.AuthorizeProject(ctx, scope, true)
}

func RunnerIdentityMatchesHeldLease(identity runnerstore.Identity, organization tracker.OrganizationID, runnerID, tokenID string, machine tracker.MachineID, now time.Time) bool {
	return identity.OrganizationID == organization && identity.RunnerID == runnerID && identity.TokenID == tokenID && identity.MachineID == machine && identity.RemovedAt == nil && identity.ExpiresAt.After(now)
}

func (d RoutingDeps) ValidateRunnerLeaseTenant(ctx context.Context, scope hubscope.NativeScope, leaseID tracker.LeaseID, fencing tracker.FencingToken) (runnerauth.Runner, error) {
	if d.Repository == nil || scope.Credential.Runner.RunnerID == "" || scope.Credential.EntryKey != nil {
		return runnerauth.Runner{}, hubscope.RunnerUnauthorized()
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	now := d.Now().UTC()
	var runner runnerauth.Runner
	var policyRevision policystore.Revision
	var selector *policy.Requirements
	err := d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		credential, err := reader.AccessCredential(ctx, scope.Credential.ID)
		if err != nil {
			if errors.Is(err, corestore.ErrNotFound) {
				return hubscope.RunnerUnauthorized()
			}
			return err
		}
		if credential.OrganizationID != scope.Organization || credential.Scope != string(hubapi.APIScopeWorker) || credential.Kind != "service" || !credential.Valid(scope.Credential.Hash, now) {
			return hubscope.RunnerUnauthorized()
		}
		lease, err := reader.Lease(ctx, string(leaseID))
		if err != nil {
			return err
		}
		if lease.OrganizationID != scope.Organization || lease.FencingToken != int64(fencing) || lease.ReleasedAt != nil || !lease.ExpiresAt.After(now) {
			return fmt.Errorf("%w: lease %s", tracker.ErrStaleFencingToken, leaseID)
		}
		project, err := reader.Project(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.OrganizationID != scope.Organization {
			return hubapi.NativeNotFoundAt("lease_runner_credential")
		}
		item, err := reader.WorkItemByID(ctx, scope.Project, lease.IssueID)
		if err != nil {
			return err
		}
		if item.OrganizationID != scope.Organization || item.ProjectID != scope.Project || item.ID != lease.IssueID {
			return hubapi.NativeNotFoundAt("lease_runner_credential")
		}
		identity, err := reader.RunnerForLease(ctx, string(leaseID))
		if err != nil {
			return err
		}
		if !RunnerIdentityMatchesHeldLease(identity, scope.Organization, scope.Credential.Runner.RunnerID, credential.ID, tracker.MachineID(lease.MachineID), now) {
			return hubapi.NativeNotFoundAt("lease_runner_credential")
		}
		if identity.Scope != "organization" {
			if _, err := reader.AccessTokenGrant(ctx, identity.TokenID, scope.Project, scope.Credential.HostedRole); err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return hubapi.NativeNotFoundAt("lease_runner_credential")
				}
				return err
			}
		}
		if identity.State == "disabled" {
			return RunnerExcluded([]runnerauth.Exclusion{{Code: "runner_disabled", Message: "Runner is disabled"}})
		}
		if !slices.Contains(identity.Operations, runnerauth.Claim) {
			return RunnerExcluded([]runnerauth.Exclusion{{Code: "claim_not_permitted", Message: "Runner credential does not permit claims"}})
		}
		pinned, err := reader.LeasePolicy(ctx, leaseID)
		if err != nil {
			return err
		}
		if pinned.OrganizationID != scope.Organization || pinned.ProjectID != scope.Project || pinned.LeaseID != leaseID || pinned.PolicyID == "" {
			return hubapi.PolicyMismatch("Lease policy is missing or outside the authorized project")
		}
		policyRevision, err = reader.ProjectPolicyRevision(ctx, scope.Project, pinned.PolicyID)
		if err != nil {
			return err
		}
		if policyRevision.OrganizationID != scope.Organization || policyRevision.ProjectID != scope.Project || policyRevision.PolicyID != pinned.PolicyID {
			return hubapi.PolicyMismatch("Lease policy is missing or outside the authorized project")
		}
		selector = pinned.Selector
		projects, err := reader.RunnerProjects(ctx, identity.RunnerID, runnerstore.MaxRunnerProjectList)
		if err != nil {
			return err
		}
		if !slices.Contains(projects, scope.Project) {
			projects = append(projects, scope.Project)
		}
		runner = runnerauth.Runner{
			Binding:          runnerauth.Binding{RunnerID: identity.RunnerID, MachineID: identity.MachineID},
			OrganizationID:   identity.OrganizationID,
			Revision:         identity.Revision,
			LastHeartbeatAt:  identity.LastHeartbeatAt,
			OS:               identity.OS,
			Architecture:     identity.Architecture,
			Health:           "online",
			ConnectionHealth: "online",
			Operations:       slices.Clone(identity.Operations),
			Routing: runnerauth.Routing{
				Scope:                 identity.Scope,
				ProjectIDs:            projects,
				Tags:                  slices.Clone(identity.Tags),
				State:                 identity.State,
				DisplayName:           identity.DisplayName,
				CapacityLimit:         int(identity.CapacityLimit),
				ProjectCapacityLimits: identity.ProjectCapacityLimits,
				ProjectRanks:          identity.ProjectRanks,
				MachineUse:            identity.MachineUse,
				IsolationTier:         identity.IsolationTier,
				HostServices:          slices.Clone(identity.HostServices),
				Availability: runnerauth.Availability{
					Timezone:     identity.Availability.Timezone,
					Windows:      slices.Clone(identity.Availability.Windows),
					WindowSlots:  identity.Availability.WindowSlots,
					HardDeadline: identity.Availability.HardDeadline,
				},
			},
			Leases: []runnerauth.RunnerLease{},
		}
		return nil
	})
	if err != nil {
		return runnerauth.Runner{}, err
	}
	if selector == nil {
		requirements, err := d.legacyLeaseRequirements(ctx, policyRevision)
		if err != nil {
			return runnerauth.Runner{}, err
		}
		selector = &requirements
	}
	if err := RunnerExcluded(runner.Exclusions(scope.Project, *selector, true)); err != nil {
		return runnerauth.Runner{}, err
	}
	return runner, nil
}

var ErrLeaseSelectorUnbound = errors.New("lease selector is not bound")

func (d RoutingDeps) legacyLeaseRequirements(ctx context.Context, revision policystore.Revision) (policy.Requirements, error) {
	if d.Details() == nil {
		return policy.Requirements{}, hubapi.NativeRunEventStorageUnavailable()
	}
	descriptor, err := eventstore.NewContent(d.Details()).ProjectPolicy(ctx, revision)
	if err != nil {
		return policy.Requirements{}, err
	}
	return descriptor.Requirements, nil
}

func leaseSelectorExcluded(requirements policy.Requirements, identity runnerstore.Identity) error {
	if err := requirements.Match(identity.RunnerID, string(identity.MachineID), identity.Tags); err != nil {
		return RunnerExcluded([]runnerauth.Exclusion{{Code: "selector_no_match", Message: strings.TrimPrefix(err.Error(), "selector_no_match: ")}})
	}
	return nil
}

func CheckLeaseSelector(pinned policystore.LeasePolicy, identity runnerstore.Identity, legacyChecked bool) error {
	if pinned.Selector != nil {
		return leaseSelectorExcluded(*pinned.Selector, identity)
	}
	if legacyChecked {
		return nil
	}
	return ErrLeaseSelectorUnbound
}

func (d RoutingDeps) CheckLegacyLeaseSelector(ctx context.Context, scope hubscope.NativeScope, leaseID tracker.LeaseID) error {
	var pinned policystore.LeasePolicy
	var identity runnerstore.Identity
	var revision policystore.Revision
	err := d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		pinned, err = reader.LeasePolicy(ctx, leaseID)
		if err != nil {
			return err
		}
		if pinned.OrganizationID != scope.Organization || pinned.ProjectID != scope.Project || pinned.LeaseID != leaseID {
			return hubapi.PolicyMismatch("Lease policy is missing or outside the authorized project")
		}
		identity, err = reader.RunnerForLease(ctx, string(leaseID))
		if err != nil || pinned.Selector != nil {
			return err
		}
		revision, err = reader.ProjectPolicyRevision(ctx, scope.Project, pinned.PolicyID)
		return err
	})
	if err != nil {
		return fmt.Errorf("read legacy lease selector: %w", err)
	}
	if pinned.Selector != nil {
		return leaseSelectorExcluded(*pinned.Selector, identity)
	}
	requirements, err := d.legacyLeaseRequirements(ctx, revision)
	if err != nil {
		return fmt.Errorf("read legacy lease requirements: %w", err)
	}
	return leaseSelectorExcluded(requirements, identity)
}
