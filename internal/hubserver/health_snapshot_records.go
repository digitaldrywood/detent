package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readHealthSnapshotRecords(ctx context.Context, organization tracker.OrganizationID, now time.Time) (healthSnapshot, error) {
	var snapshot healthSnapshot
	runners, err := s.readHealthRunnerRecords(ctx, organization, now)
	if err != nil {
		return snapshot, err
	}
	for _, runner := range runners {
		snapshot.Runners = append(snapshot.Runners, runner.healthRunner)
	}
	var projects []corestore.Project
	err = s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		projects, err = r.Projects(ctx, "", healthProjectLimit+1)
		return err
	})
	if err != nil {
		return snapshot, err
	}
	if len(projects) > healthProjectLimit {
		return snapshot, errHealthReadLimit
	}
	for _, project := range projects {
		if project.Profile != "native" {
			continue
		}
		contractProject, descriptor, err := s.readHealthContractProject(ctx, organization, project.ID)
		if err != nil {
			return snapshot, err
		}
		p := healthProject{ID: string(project.ID), CandidateIDs: map[string]bool{}}
		p.Scheduled, err = s.readHealthScheduledRecords(ctx, organization, project.ID, now)
		if err != nil {
			return snapshot, err
		}
		var after claimstore.CandidateCursor
		for {
			var page claimstore.CandidatePage
			err = s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
				var err error
				page, err = r.CandidatePage(ctx, claimstore.CandidateQuery{ProjectID: project.ID, AvailableAt: now, After: after, Limit: 200})
				return err
			})
			if err != nil {
				return snapshot, err
			}
			for _, candidate := range page.Items {
				p.Candidates++
				if p.Candidates > healthReadLimit {
					return snapshot, errHealthReadLimit
				}
				p.CandidateIDs[string(candidate.WorkItemID)] = true
				wait, err := s.healthContracts.readRecords(ctx, s, contractProject, candidate.WorkItemID, tracker.Revision(candidate.Revision), descriptor)
				if err != nil {
					return snapshot, err
				}
				if wait != nil {
					snapshot.Waits = append(snapshot.Waits, *wait)
				}
				if p.CandidateSince.IsZero() || candidate.CreatedAt.Before(p.CandidateSince) {
					p.CandidateSince = candidate.CreatedAt
				}
			}
			if len(page.Items) < 200 {
				break
			}
			after = page.Next
		}
		err = s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
			owner, ok := r.(healthstore.ObservationReader)
			if !ok {
				return workItemStorageUnavailable()
			}
			dependencies, err := owner.HealthDependencies(ctx, project.ID, healthReadLimit+1)
			if err != nil {
				return err
			}
			if len(dependencies) > healthReadLimit {
				return errHealthReadLimit
			}
			gated, held, listed := map[string]bool{}, map[string]bool{}, map[string]bool{}
			for _, dependency := range dependencies {
				gated[dependency.Dependent] = true
				if dependency.Dispatchable {
					continue
				}
				held[dependency.Blocker] = true
				if dependency.SameProject {
					listed[dependency.Blocker] = true
				}
				if p.HoldSince.IsZero() || dependency.Since.Before(p.HoldSince) {
					p.HoldSince = dependency.Since
				}
			}
			p.DependencyGated, p.HeldCount, p.HeldBlockers = len(gated), len(held), slices.Sorted(maps.Keys(listed))
			return nil
		})
		if err != nil {
			return snapshot, err
		}
		applyHealthRunnerAvailability(now, runners, &p)
		snapshot.Projects = append(snapshot.Projects, p)
	}
	if err := s.readHealthEventRecords(ctx, organization, now, &snapshot); err != nil {
		return snapshot, err
	}
	if err := s.readHealthWaitRecords(ctx, organization, now, &snapshot); err != nil {
		return snapshot, err
	}
	if err := s.readHealthRateRecords(ctx, organization, now, runners, &snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (s *Service) readHealthRunnerRecords(ctx context.Context, organization tracker.OrganizationID, now time.Time) ([]healthRunnerState, error) {
	var result []healthRunnerState
	after := ""
	for {
		var page []healthstore.RunnerObservation
		err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
			owner, ok := r.(healthstore.ObservationReader)
			if !ok {
				return workItemStorageUnavailable()
			}
			var err error
			page, err = owner.HealthRunners(ctx, after, now, 200)
			if err != nil {
				return err
			}
			for _, row := range page {
				runner := healthRunnerState{healthRunner: healthRunner{ID: row.ID, Heartbeat: row.Heartbeat, CreatedAt: row.CreatedAt, Leases: row.Leases, FreeSlots: row.FreeSlots}, MachineID: row.MachineID, Routing: runnerauth.Routing{State: row.State}, Admissions: map[string]tracker.NativeAdmissionObservation{}}
				projects, err := r.RunnerProjects(ctx, row.ID, healthProjectLimit+1)
				if err != nil {
					return err
				}
				leased, err := owner.HealthRunnerLeasedProjects(ctx, row.ID, healthProjectLimit+1)
				if err != nil {
					return err
				}
				if len(leased) > healthProjectLimit {
					return errHealthReadLimit
				}
				projects = append(projects, leased...)
				slices.Sort(projects)
				for _, project := range slices.Compact(projects) {
					runner.Projects = append(runner.Projects, string(project))
					observation, err := r.RunnerAdmissionObservation(ctx, row.ID, project)
					if err != nil && !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					if err == nil {
						runner.Admissions[string(project)] = observation
					}
				}
				result = append(result, runner)
				if len(result) > healthRunnerLimit {
					return errHealthReadLimit
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(page) < 200 {
			return result, nil
		}
		after = page[len(page)-1].ID
	}
}

func applyHealthRunnerAvailability(now time.Time, runners []healthRunnerState, p *healthProject) {
	var fallback []string
	for _, r := range runners {
		if !slices.Contains(r.Projects, p.ID) {
			continue
		}
		if r.Routing.State != "disabled" {
			refresh := r.CreatedAt
			if observed, ok := r.Admissions[p.ID]; ok {
				if !observed.Context.LastRefreshAt.IsZero() {
					refresh = observed.Context.LastRefreshAt
				}
				p.RefreshDuration = max(p.RefreshDuration, observed.Context.RefreshDuration)
			}
			if p.RefreshAt.IsZero() || refresh.Before(p.RefreshAt) {
				p.RefreshAt = refresh
			}
		}
		reason := ""
		switch {
		case r.Routing.State != "active":
			reason = "runner_inactive"
		case now.Sub(r.Heartbeat) >= runnerauth.HeartbeatTimeout:
			reason = "runner_heartbeat_stale"
		case r.FreeSlots == 0:
			reason = "runner_capacity_full"
		}
		if reason == "" {
			p.FreeSlots += r.FreeSlots
		}
		fallback = append(fallback, reason)
	}
	if len(fallback) > 0 && fallback[0] != "" && !slices.ContainsFunc(fallback, func(reason string) bool { return reason != fallback[0] }) {
		for range p.Candidates {
			p.RefusalReasons = append(p.RefusalReasons, fallback[0])
		}
	}
}

func (s *Service) readHealthEventRecords(ctx context.Context, organization tracker.OrganizationID, now time.Time, snapshot *healthSnapshot) error {
	var records []eventstore.Event
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(healthstore.ObservationReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		records, err = owner.HealthSchedulerEvents(ctx, now.Add(-healthInstanceWindow), now, healthReadLimit+1)
		return err
	})
	if err != nil {
		return err
	}
	if len(records) > healthReadLimit {
		return errHealthReadLimit
	}
	var events []healthTimeEvent
	for _, record := range records {
		event, _, err := s.readTenantCollaborationEvent(ctx, record)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(event.Data)
		if err != nil {
			return err
		}
		events = append(events, healthTimeEvent{id: record.ID, project: string(record.ProjectID), item: string(record.WorkItemID), kind: record.Type, raw: string(raw), at: record.RecordedAt})
	}
	return applyHealthEvents(ctx, now, events, snapshot)
}

func (s *Service) readHealthWaitRecords(ctx context.Context, organization tracker.OrganizationID, now time.Time, snapshot *healthSnapshot) error {
	type itemEvents struct {
		item            healthstore.OpenItemObservation
		transition, run eventstore.Event
	}
	var inputs []itemEvents
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(healthstore.ObservationReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		items, err := owner.HealthOpenItems(ctx, healthReadLimit+1)
		if err != nil {
			return err
		}
		if len(items) > healthReadLimit {
			return errHealthReadLimit
		}
		transitions, ok := r.(eventstore.WorkflowTransitionReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		for _, item := range items {
			input := itemEvents{item: item}
			input.transition, err = transitions.LatestWorkflowTransition(ctx, item.Project, item.Item)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if item.AttemptID != "" {
				input.run, err = r.LatestRunEvent(ctx, item.Project, item.AttemptID)
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
			}
			inputs = append(inputs, input)
		}
		questions, err := owner.HealthQuestions(ctx, now, healthReadLimit+1)
		if err != nil {
			return err
		}
		if len(questions) > healthReadLimit {
			return errHealthReadLimit
		}
		for _, question := range questions {
			snapshot.Waits = append(snapshot.Waits, healthWait{Item: question.Item, Project: question.Project, Attempt: question.AttemptID, Since: question.Since, Active: true})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, input := range inputs {
		if input.transition.ID == "" {
			continue
		}
		event, _, err := s.readTenantCollaborationEvent(ctx, input.transition)
		if err != nil {
			return err
		}
		detail := strings.ReplaceAll(strings.TrimSpace(event.Data.ReasonDetail), " ", "_")
		wait := healthWait{Item: string(input.item.Item), Project: string(input.item.Project), Attempt: input.item.AttemptID, Since: input.item.AttemptUpdatedAt, Active: true}
		if event.Actor.Kind == "runner" && (detail == permissionWaitTransitionReason || detail == "human_action") {
			wait.Event, wait.Since = input.transition.ID, input.transition.RecordedAt
			snapshot.Waits = append(snapshot.Waits, wait)
			continue
		}
		if input.run.ID == "" || input.item.AttemptStatus != "succeeded" || event.Actor.Kind != "runner" || event.Data.Reason != "worker_progress" || int64(event.Data.Revision) != input.item.Revision || input.transition.RecordedAt.Before(input.item.AttemptUpdatedAt) {
			continue
		}
		run, _, err := s.readTenantCollaborationEvent(ctx, input.run)
		if err != nil {
			return err
		}
		if run.Data.Run == nil || run.Data.Run.Disposition == nil {
			continue
		}
		disposition := run.Data.Run.Disposition
		if disposition.Status == "blocked" && (disposition.HumanAction || disposition.ReasonCode == "permission_wait") {
			snapshot.Waits = append(snapshot.Waits, wait)
		}
	}
	return nil
}
