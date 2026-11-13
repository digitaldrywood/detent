package hubserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func runnerLeaseMutationRoute(path string) bool {
	return path == hubapi.NativeBase+"/leases/:lease/validate" || path == hubapi.NativeBase+"/leases/:lease/renew" || path == hubapi.NativeBase+"/leases/:lease/release"
}

func (s *Service) authorizeRunnerLeaseProject(ctx context.Context, scope hubscope.NativeScope) error {
	if s.config.TenantRepository == nil || scope.Credential.Runner.RunnerID == "" || scope.Credential.Runner.OrganizationID != scope.Organization || scope.Credential.EntryKey != nil {
		return hubapi.NativeNotFound()
	}
	return s.authorizeProjectRecord(ctx, scope, true)
}

func runnerIdentityMatchesHeldLease(identity runnerstore.Identity, organization tracker.OrganizationID, runnerID, tokenID string, machine tracker.MachineID, now time.Time) bool {
	return identity.OrganizationID == organization && identity.RunnerID == runnerID && identity.TokenID == tokenID && identity.MachineID == machine && identity.RemovedAt == nil && identity.ExpiresAt.After(now)
}

func (s *Service) validateRunnerLeaseTenant(ctx context.Context, scope hubscope.NativeScope, leaseID tracker.LeaseID, fencing tracker.FencingToken) (runnerauth.Runner, error) {
	if s.config.TenantRepository == nil || scope.Credential.Runner.RunnerID == "" || scope.Credential.EntryKey != nil {
		return runnerauth.Runner{}, hubscope.RunnerUnauthorized()
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	now := s.config.now().UTC()
	var runner runnerauth.Runner
	var policyRevision policystore.Revision
	err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
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
		if !runnerIdentityMatchesHeldLease(identity, scope.Organization, scope.Credential.Runner.RunnerID, credential.ID, tracker.MachineID(lease.MachineID), now) {
			return hubapi.NativeNotFoundAt("lease_runner_credential")
		}
		if identity.Scope != "organization" {
			if _, err := reader.AccessTokenGrant(ctx, identity.TokenID, scope.Project); err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return hubapi.NativeNotFoundAt("lease_runner_credential")
				}
				return err
			}
		}
		if identity.State == "disabled" {
			return runnerExcluded([]runnerauth.Exclusion{{Code: "runner_disabled", Message: "Runner is disabled"}})
		}
		if !slices.Contains(identity.Operations, runnerauth.Claim) {
			return runnerExcluded([]runnerauth.Exclusion{{Code: "claim_not_permitted", Message: "Runner credential does not permit claims"}})
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
	if s.runDetailStore() == nil {
		return runnerauth.Runner{}, hubapi.NativeRunEventStorageUnavailable()
	}
	descriptor, err := eventstore.NewContent(s.runDetailStore()).ProjectPolicy(ctx, policyRevision)
	if err != nil {
		return runnerauth.Runner{}, err
	}
	if err := runnerExcluded(runner.Exclusions(scope.Project, descriptor.Requirements, true)); err != nil {
		return runnerauth.Runner{}, err
	}
	return runner, nil
}
