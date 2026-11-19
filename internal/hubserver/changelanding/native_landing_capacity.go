package changelanding

import (
	"context"
	"reflect"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/changerules"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const CapacityUnknown = -1

func LandingCapacityKey(machine tracker.MachineID, project, session string) string {
	parts := strings.Split(session, "/")
	if len(parts) != 3 || parts[0] != "landing" || parts[1] == "" || parts[2] == "" || project == "" {
		return ""
	}
	return string(machine) + "/" + project + "/" + parts[1]
}

type Renewal struct {
	lease       leasestore.Lease
	change      eventstore.Change
	version     eventstore.Version
	policy      hubrecords.CurrentChangePolicyRows
	until       time.Time
	evaluatedAt time.Time
}

func (s *Capacity) PrepareRenewal(ctx context.Context, scope hubscope.NativeScope, leaseID string, capacity int) (*Renewal, error) {
	if capacity <= 0 {
		return nil, nil
	}
	p := &Renewal{evaluatedAt: s.deps.Clock().UTC()}
	var item corestore.WorkItem
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		p.lease, err = r.Lease(ctx, leaseID)
		if err != nil || p.lease.LandingCapacity > 0 {
			return err
		}
		item, err = r.WorkItemByID(ctx, scope.Project, p.lease.IssueID)
		if err != nil {
			return err
		}
		p.change, err = r.LatestChange(ctx, scope.Project, item.NativeID)
		if err != nil {
			return err
		}
		if p.change.CurrentVersionID == nil {
			return s.deps.NoClaimableWork
		}
		p.version, err = r.Version(ctx, p.change.ID, *p.change.CurrentVersionID)
		if err != nil {
			return err
		}
		p.policy, err = hubrecords.ReadChangeSummaryPolicyRows(ctx, r, scope)
		return err
	})
	if err != nil {
		return nil, err
	}
	if p.lease.LandingCapacity > 0 {
		return p, nil
	}
	detail, err := s.deps.HydrateCurrentChangeDetail(ctx, scope, item.NativeID, p.change, &p.version, p.policy, p.evaluatedAt)
	if err != nil {
		return nil, err
	}
	var version *tracker.ChangeVersion
	for i := range detail.Versions {
		if detail.Versions[i].ID == detail.Change.CurrentVersion {
			version = &detail.Versions[i]
			break
		}
	}
	if version == nil || !version.Policy.Gates.GitHubPullRequest || !changerules.LandingReady("Merging", &detail) {
		return nil, s.deps.NoClaimableWork
	}
	for _, expected := range version.Checks {
		for _, check := range detail.Checks {
			if check.CheckRunID == expected.CheckRunID {
				expires := check.CompletedAt.Add(time.Duration(expected.MaxAgeSeconds) * time.Second)
				if p.until.IsZero() || expires.Before(p.until) {
					p.until = expires
				}
			}
		}
	}
	return p, nil
}

func (s *Capacity) ReserveRenewal(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *Renewal, lease leasestore.Lease, capacity int64) (int64, error) {
	if p == nil {
		return capacity, nil
	}
	if lease.LeaseID != p.lease.LeaseID || lease.FencingToken != p.lease.FencingToken {
		return 0, tracker.ErrStaleFencingToken
	}
	now := s.deps.Clock().UTC()
	if lease.LandingCapacity == 0 {
		current, err := w.LatestChange(ctx, scope.Project, p.change.WorkItemID)
		if err != nil {
			return 0, err
		}
		expected := p.change
		expected.ReviewSummarySequence = current.ReviewSummarySequence
		if !reflect.DeepEqual(current, expected) {
			return 0, corestore.ErrRevisionConflict
		}
		version, err := w.Version(ctx, p.change.ID, p.version.ID)
		if err != nil {
			return 0, err
		}
		policy, err := hubrecords.ReadChangeSummaryPolicyRows(ctx, w, scope)
		if err != nil {
			return 0, err
		}
		if !reflect.DeepEqual(version, p.version) || !reflect.DeepEqual(policy, p.policy) {
			return 0, corestore.ErrRevisionConflict
		}
		if now.Before(p.evaluatedAt) || !p.until.IsZero() && !now.Before(p.until) {
			return 0, s.deps.NoClaimableWork
		}
	} else if lease.ExpiresAt.After(now) {
		return capacity, nil
	}
	if !lease.ExpiresAt.After(now) {
		identity, err := w.RunnerIdentity(ctx, scope.Credential.Runner.RunnerID)
		if err != nil {
			return 0, err
		}
		capacityLimit := min(identity.CapacityLimit, identity.ReportedCapacity, int64(identity.CapacityConfig.LocalLimit), int64(identity.CapacityConfig.ClientLimit), int64(identity.CapacityConfig.RuntimeLimit))
		projectLimit, projectLimited := identity.ProjectCapacityLimits[scope.Project]
		if projectLimited {
			capacityLimit = min(capacityLimit, int64(projectLimit))
		}
		availability := runnerauth.Availability{Timezone: identity.Availability.Timezone, Windows: identity.Availability.Windows, WindowSlots: identity.Availability.WindowSlots, HardDeadline: identity.Availability.HardDeadline}
		available, err := availability.Capacity(now, int(capacityLimit))
		if err != nil {
			return 0, err
		}
		machine, err := w.LockMachine(ctx, lease.MachineID)
		if err != nil {
			return 0, err
		}
		usage, err := w.ClaimCapacityUsage(ctx, lease.MachineID, identity.RunnerID, scope.Project, "", now)
		if err != nil {
			return 0, err
		}
		if usage.Host >= machine.Capacity || usage.Runner >= int64(available) || projectLimited && usage.Project >= int64(projectLimit) {
			return 0, tracker.ErrLeaseConflict
		}
	}
	usage, err := w.LandingCapacityUsageExcludingLease(ctx, scope.Project, now, lease.LeaseID)
	if err != nil {
		return 0, err
	}
	if usage.Limit > 0 && (capacity <= 0 || usage.Limit < capacity) {
		capacity = usage.Limit
	}
	if usage.Unknown || capacity > 0 && usage.Active >= capacity {
		return 0, tracker.ErrLeaseConflict
	}
	return capacity, nil
}

func ExistingRenewal(lease leasestore.Lease) *Renewal {
	return &Renewal{lease: lease}
}
