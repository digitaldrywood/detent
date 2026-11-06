package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) projectOnboardingBatchTenant(ctx context.Context, organization tracker.OrganizationID, projects []appBootstrapProject) (map[tracker.ProjectID]onboarding.Project, error) {
	if len(projects) > corestore.MaxPageSize {
		return nil, corestore.ErrInvalidPage
	}
	ctx = s.withRequestDetails(ctx)
	ids := make([]tracker.ProjectID, len(projects))
	for i, project := range projects {
		ids[i] = tracker.ProjectID(project.ID)
	}
	var rows []projectstore.OnboardingRecord
	var records []runnerstore.FleetRecord
	var leases []runnerstore.FleetLease
	var urgent runnerstore.UrgentUpdate
	var checks map[tracker.ProjectID]map[string]runnerauth.LocalChecks
	now := s.config.now().UTC()
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(projectstore.OnboardingBatchReader)
		if !ok {
			return errors.New("project onboarding batch reader unavailable")
		}
		var err error
		rows, err = reader.ProjectOnboardingRecords(ctx, ids)
		if err != nil {
			return err
		}
		if len(rows) != len(ids) {
			return corestore.ErrNotFound
		}
		records, err = reader.FleetRecords(ctx, 10000)
		if err != nil {
			return err
		}
		leases, err = reader.FleetLeases(ctx, now, 10000)
		if err != nil {
			return err
		}
		urgent, err = r.RunnerUrgentUpdate(ctx)
		if err != nil {
			return err
		}
		checks, err = reader.ProjectRunnerChecksForProjects(ctx, ids, 10000)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read project onboarding batch: %w", err)
	}
	values := make([]onboarding.Project, len(rows))
	for i, row := range rows {
		if row.Progress.Artifacts == "customer" {
			return nil, &nativeError{Code: "storage_owner_unavailable", Message: "Project artifact services are unavailable", status: http.StatusServiceUnavailable, publicMessage: true}
		}
		values[i] = onboarding.Project{Repository: row.Repository, Progress: row.Progress, Runners: []runnerauth.Eligibility{}}
		if row.Repository == "" {
			values[i].RepositoryBindingReason = &onboarding.RepositoryBindingReason{Code: "repository_binding_required", Message: "Choose and connect a repository in the Repository step before retrying checkout."}
		}

	}
	references := []attachment.ArchiveReference{}
	seen := map[attachment.ArchiveReference]bool{}
	var compressed, content int64
	for _, row := range rows {
		if row.Policy == nil {
			continue
		}
		revision := row.Policy
		ref := eventstore.Reference(revision.PayloadObjectKey, revision.PayloadSHA256, revision.PayloadBytes, revision.PayloadContentBytes)
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if ref.Validate(string(organization)) != nil {
			return nil, attachment.ErrInvalid
		}
		compressed += ref.CompressedSize
		content += ref.UncompressedSize
		if compressed > attachment.MaxArchiveBytes || content > attachment.MaxArchiveBytes {
			return nil, corestore.ErrInvalidPage
		}
		references = append(references, ref)
	}
	s.prefetchDetails(ctx, string(organization), references)
	policyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int, len(rows))
	for i := range rows {
		if rows[i].Policy != nil {
			jobs <- i
		}
	}
	close(jobs)
	var workers sync.WaitGroup
	var failure error
	var failureMu sync.Mutex
	for range min(4, len(rows)) {
		workers.Go(func() {
			for i := range jobs {
				if policyCtx.Err() != nil {
					return
				}
				revision := rows[i].Policy
				raw, err := eventstore.NewContent(s.runDetailStoreFor(policyCtx)).Receipt(policyCtx, eventstore.Receipt{OrganizationID: organization, PayloadObjectKey: revision.PayloadObjectKey, PayloadSHA256: revision.PayloadSHA256, PayloadBytes: revision.PayloadBytes, PayloadContentBytes: revision.PayloadContentBytes})
				var approval policy.Approval
				if err == nil {
					approval, err = decodeProjectPolicy(policyCtx, string(organization)+"/"+string(rows[i].ProjectID), revision.PolicyID, string(raw), policy.Approval{ApprovedBy: revision.ApprovedBy, ApprovedAt: formatHubTime(revision.ApprovedAt)})
				}
				if err != nil {
					failureMu.Lock()
					if failure == nil {
						failure = err
					}
					failureMu.Unlock()
					cancel()
					return
				}
				values[i].Policy = &approval
			}
		})
	}
	workers.Wait()
	if failure != nil {
		return nil, fmt.Errorf("read project onboarding policy: %w", failure)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	providerViews := tenantFleetProviderViews(records, leases, now)
	hostSlots := map[tracker.MachineID]map[string]bool{}
	runnerSlots := map[string]map[string]bool{}
	projectUsed := map[string]map[tracker.ProjectID]int{}
	for _, lease := range leases {
		key := string(lease.Lease.ID)
		if lease.LandingCapacity > 0 {
			if group := landingCapacityKey(lease.MachineID, string(lease.Lease.ProjectID), lease.SessionID); group != "" {
				key = group
			}
		}
		if hostSlots[lease.MachineID] == nil {
			hostSlots[lease.MachineID] = map[string]bool{}
		}
		hostSlots[lease.MachineID][key] = true
		if runnerSlots[lease.RunnerID] == nil {
			runnerSlots[lease.RunnerID] = map[string]bool{}
			projectUsed[lease.RunnerID] = map[tracker.ProjectID]int{}
		}
		if !runnerSlots[lease.RunnerID][key] {
			projectUsed[lease.RunnerID][lease.Lease.ProjectID]++
		}
		runnerSlots[lease.RunnerID][key] = true
	}
	runners := make([]runnerauth.Runner, len(records))
	for i, record := range records {
		runner := runnerFromFleetRecord(record, now, urgent)
		runner.HostUsed = len(hostSlots[runner.MachineID])
		runner.Used = len(runnerSlots[runner.RunnerID])
		runner.ProjectUsed = projectUsed[runner.RunnerID]
		runner.ProviderCapacity = providerViews[runner.RunnerID]
		runners[i] = runner
	}
	result := make(map[tracker.ProjectID]onboarding.Project, len(rows))
	for i, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		value := values[i]
		var requirements policy.Requirements
		if value.Policy != nil {
			requirements = value.Policy.Policy.Requirements
		}
		for _, runner := range runners {
			if !runner.AllowsProject(row.ProjectID) {
				continue
			}
			eligibility := runnerauth.Eligibility{Runner: runner, Exclusions: runner.Exclusions(row.ProjectID, requirements, false)}
			if check, ok := checks[row.ProjectID][runner.RunnerID]; ok {
				eligibility.LocalChecks = &check
			}
			if value.ObserveRunner(eligibility) {
				break
			}
		}
		value.Evaluate()
		result[row.ProjectID] = value
	}
	return result, nil
}
