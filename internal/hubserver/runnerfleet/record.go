package runnerfleet

import (
	"maps"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerupdate"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func FromRecord(record runnerstore.FleetRecord, now time.Time, urgent runnerstore.UrgentUpdate) runnerauth.Runner {
	identity, machine := record.Identity, record.Machine
	runner := runnerauth.Runner{
		Binding:             runnerauth.Binding{RunnerID: identity.RunnerID, MachineID: identity.MachineID},
		OrganizationID:      identity.OrganizationID,
		Revision:            identity.Revision,
		LastHeartbeatAt:     identity.LastHeartbeatAt,
		Hostname:            machine.Hostname,
		HostDisplayName:     machine.DisplayName,
		HostCapacity:        int(machine.Capacity),
		HostRevision:        record.HostRevision,
		ReportedCapacity:    int(identity.ReportedCapacity),
		CapacityConfig:      identity.CapacityConfig,
		OS:                  identity.OS,
		Architecture:        identity.Architecture,
		BackendIsolation:    record.Isolation,
		Update:              record.Update.Observation,
		UpdateTargetVersion: record.Update.TargetVersion,
		UpdateTargetHeld:    record.Update.TargetHeld,
		Lifecycle:           record.Lifecycle,
		Problems:            slices.Clone(record.Problems),
		Operations:          slices.Clone(identity.Operations),
		Routing: runnerauth.Routing{
			ProjectRankOverrides:  maps.Clone(identity.ProjectRanks),
			UpdateRequest:         record.Update.Desired,
			CapacityRequest:       identity.CapacityRequest,
			Scope:                 identity.Scope,
			State:                 identity.State,
			DisplayName:           identity.DisplayName,
			ProjectIDs:            slices.Clone(identity.ProjectIDs),
			Tags:                  slices.Clone(identity.Tags),
			CapacityLimit:         int(identity.CapacityLimit),
			ProjectCapacityLimits: maps.Clone(identity.ProjectCapacityLimits),
			ProjectRanks:          maps.Clone(identity.ProjectRanks),
			MachineUse:            identity.MachineUse,
			IsolationTier:         identity.IsolationTier,
			HostServices:          slices.Clone(identity.HostServices),
			Availability: runnerauth.Availability{
				Timezone:     identity.Availability.Timezone,
				Windows:      slices.Clone(identity.Availability.Windows),
				WindowSlots:  maps.Clone(identity.Availability.WindowSlots),
				HardDeadline: identity.Availability.HardDeadline,
			},
		},
	}
	runner.Health = "online"
	if record.CredentialRevoked {
		runner.Health = "revoked"
	} else if !identity.ExpiresAt.After(now) || now.Before(record.CredentialCreatedAt) || record.CredentialExpiresAt != nil && !record.CredentialExpiresAt.After(now) {
		runner.Health = "expired"
	} else if now.Before(identity.LastHeartbeatAt) || !now.Before(identity.LastHeartbeatAt.Add(runnerauth.HeartbeatTimeout)) {
		runner.Health = "offline"
	}
	runner.ConnectionHealth = runner.Health
	runner.ApplyLifecycle(now)
	if !record.Update.UrgentPolicyChecked || record.Update.UrgentAllowed {
		runnerupdate.ApplyUrgentRouting(&runner, runnerupdate.UrgentUpdate{Revision: urgent.Revision, Request: urgent.Request}, machine.Version)
	}
	ApplyProblemView(&runner, machine.SpriteName, now)
	return runner
}
