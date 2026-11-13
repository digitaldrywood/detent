package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/detailbatch"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) HealthPageMaintenanceFeature() hubmaintenance.Feature {
	var tenants sync.Map
	return hubmaintenance.Feature{Name: "health-pages", Bytes: hubmaintenance.ByteLimit, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, budget hubmaintenance.Budget) error {
		if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
			return err
		}
		value, _ := tenants.LoadOrStore(tenant.Organization, &healthMaintenanceState{})
		state := value.(*healthMaintenanceState)
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.generation != tenant.Generation {
			state.cursor = healthMaintenanceCursor{}
			state.generation = tenant.Generation
		}
		organization := tracker.OrganizationID(tenant.Organization)
		observed := s.maintainHealthSources(ctx, organization, &state.cursor, budget)
		_, projected := s.MaintainHealthPage(ctx, organization)
		return errors.Join(observed, projected)
	}}
}

func (s *Service) MaintainHealthPage(ctx context.Context, organization tracker.OrganizationID) (bool, error) {
	if err := hubmaintenance.RequireAllowance(ctx, string(organization), hubmaintenance.ByteLimit); err != nil {
		return false, err
	}
	var candidate healthstore.Finding
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(healthstore.PageMaintainer)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		candidate, err = owner.NextHealthPage(ctx)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		owner, ok := w.(healthstore.PageMaintainer)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		return owner.AdvanceHealthPage(ctx, candidate)
	})
	if err != nil {
		return false, err
	}
	var evidence healthstore.Evidence
	if candidate.Evidence.Key != "" {
		raw, err := hubmaintenance.LimitDetails(s.runDetailStore()).ReadDetail(ctx, string(organization), candidate.Evidence)
		if err != nil {
			return false, err
		}
		if err := json.Unmarshal(raw, &evidence); err != nil {
			return false, err
		}
	}
	published := false
	err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		owner, ok := w.(healthstore.PageMaintainer)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		published, err = owner.PublishHealthPage(ctx, candidate, evidence)
		return err
	})
	if published && s.config.Logger != nil {
		s.config.Logger.LogAttrs(ctx, slog.LevelDebug, "project health page", slog.String("organization_id", string(organization)), slog.Int64("generation", candidate.Generation), slog.Duration("projection_lag", s.config.now().Sub(candidate.SourceAt)))
	}
	return published, err
}

type healthMaintenanceState struct {
	mu         sync.Mutex
	generation int64
	cursor     healthMaintenanceCursor
}

type healthWaitItemCursor struct {
	afterProject tracker.ProjectID
	project      tracker.ProjectID
	state        string
	id           int64
}

type healthProjectWaitCursor struct {
	finding                postgres.HealthRecord
	conversation, question string
}

type healthMaintenanceCursor struct {
	project                tracker.ProjectID
	runner                 runnerstore.UpdateProjectionCursor
	item                   healthWaitItemCursor
	conversation, question string
	finding                string
	cycleFailed            bool
	completed              [5]bool
	projectWait            healthProjectWaitCursor
}

func healthFingerprint(signal, kind, id string) string {
	return newHealthFinding(signal, "", kind, id, "", "", nil, healthEvidence{}).Fingerprint
}

func (s *Service) maintainHealthSources(ctx context.Context, organization tracker.OrganizationID, cursor *healthMaintenanceCursor, budget hubmaintenance.Budget) error {
	if budget.Items < 1 {
		return corestore.ErrInvalidPage
	}
	if s.runDetailStore() == nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	ctx = detailbatch.WithStore(ctx, hubmaintenance.LimitDetails(s.runDetailStore()))

	projectErr := s.maintainHealthProject(ctx, organization, &cursor.project)
	runnerErr := s.maintainHealthRunner(ctx, organization, &cursor.runner)
	itemErr := s.maintainHealthWaitItems(ctx, organization, &cursor.item, min(budget.Items, corestore.MaxPageSize))
	questionErr := s.maintainHealthQuestions(ctx, organization, &cursor.conversation, &cursor.question, min(budget.Items, corestore.MaxPageSize))
	findingErr := s.maintainRetiredHealthSubjects(ctx, organization, &cursor.finding, &cursor.projectWait, min(budget.Items, corestore.MaxPageSize))
	cursor.completed[0] = cursor.completed[0] || cursor.project == ""
	cursor.completed[1] = cursor.completed[1] || cursor.runner.RunnerID == ""
	cursor.completed[2] = cursor.completed[2] || cursor.item == (healthWaitItemCursor{})
	cursor.completed[3] = cursor.completed[3] || cursor.conversation == ""
	cursor.completed[4] = cursor.completed[4] || cursor.finding == "" && cursor.projectWait.finding.ID == ""
	combined := errors.Join(projectErr, runnerErr, itemErr, questionErr, findingErr)
	cursor.cycleFailed = cursor.cycleFailed || combined != nil
	if cursor.completed == ([5]bool{true, true, true, true, true}) && cursor.projectWait.finding.ID == "" {
		if !cursor.cycleFailed {
			combined = s.database.postgres.WriteHealth(ctx, organization, corestore.Background, func(writer postgres.HealthWriter) error {
				return writer.PutHealthTick(ctx, postgres.PutHealthTickInput{OrganizationID: organization, LastTickAt: s.config.now().UTC()})
			})
		}
		*cursor = healthMaintenanceCursor{}
	}

	return combined
}

func healthScheduledHistory(history healthstore.ScheduledHistory) healthScheduledValidation {
	out := healthScheduledValidation{FirstReport: history.FirstReport, LastGreen: history.LastGreen}
	intervals := []time.Duration{}
	for index, report := range history.Reports {
		if index == 0 {
			out.LastReport, out.LastRunURL = report.RecordedAt, report.RunURL
		}
		if report.Conclusion == "failure" && report.RecordedAt.After(out.LastGreen) {
			out.Failures++
		}
		if index > 0 && len(intervals) < 20 {
			if interval := history.Reports[index-1].RecordedAt.Sub(report.RecordedAt); interval > 0 {
				intervals = append(intervals, interval)
			}
		}
	}
	if len(intervals) > 0 {
		slices.Sort(intervals)
		out.Cadence = intervals[len(intervals)/2]
	}
	out.LastFailed = out.Failures > 0
	return out
}

func (s *Service) maintainHealthRunner(ctx context.Context, organization tracker.OrganizationID, after *runnerstore.UpdateProjectionCursor) error {
	now := s.config.now().UTC()
	var runner healthRunner
	found := false
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		owner, ok := reader.(runnerstore.UpdateProjectionPageReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		page, err := owner.RunnerUpdateProjectionPage(ctx, *after, 1)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			*after = runnerstore.UpdateProjectionCursor{}
			return nil
		}
		row := page[0]
		*after = runnerstore.UpdateProjectionCursor{DisplayName: row.DisplayName, RunnerID: row.RunnerID}
		identity, err := reader.RunnerIdentity(ctx, row.RunnerID)
		if err != nil {
			return err
		}
		runner = healthRunner{ID: identity.RunnerID, CreatedAt: identity.CreatedAt, Heartbeat: identity.LastHeartbeatAt}
		found = true
		credential, err := reader.AccessCredential(ctx, identity.TokenID)
		if err != nil {
			return err
		}
		if !credential.Valid(credential.Hash, now) || credential.Scope != "worker" || credential.Kind != "service" || identity.RemovedAt != nil || !identity.ExpiresAt.After(now) {
			return nil
		}
		projects, err := reader.RunnerProjects(ctx, identity.RunnerID, healthProjectLimit)
		if err != nil {
			return err
		}
		for _, project := range projects {
			runner.Projects = append(runner.Projects, string(project))
		}
		leases, err := reader.RunnerActiveLeaseIDs(ctx, identity.RunnerID, now, corestore.MaxPageSize)
		if err != nil {
			return err
		}
		runner.Leases = len(leases)
		return nil
	})
	if err != nil || !found {
		return err
	}
	finding, active := heartbeatFinding(now, runner)
	scope := healthEvaluationScope{runners: map[string]bool{runner.ID: true}, fingerprints: map[string]bool{finding.Fingerprint: true}}
	findings := []healthFinding{}
	if active {
		findings = append(findings, finding)
	}
	if err := s.publishHealthEvaluationScope(ctx, organization, now, findings, scope); err != nil {
		return err
	}
	projects := []healthRateProject{}
	complete := true
	var failures []error
	for _, id := range runner.Projects {
		value, err := s.readHealthProjectSource(ctx, organization, tracker.ProjectID(id), runner.ID, now)
		if errors.Is(err, corestore.ErrNotFound) {
			continue
		}
		if err != nil {
			failures = append(failures, err)
			complete = false
			continue
		}
		if !value.capacityKnown || !value.attemptsKnown || value.rate.ResidencePartial {
			complete = false
		}
		value.rate.Attempts = slices.DeleteFunc(value.rate.Attempts, func(attempt healthRateAttempt) bool { return attempt.Runner != runner.ID })
		value.rate.Runners = slices.DeleteFunc(value.rate.Runners, func(selected healthRunner) bool { return selected.ID != runner.ID })
		projects = append(projects, value.rate)
	}
	if complete {
		values := evaluateHealthRates(now, projects)
		values = slices.DeleteFunc(values, func(finding healthFinding) bool {
			return finding.Subject.Kind != "runner" || finding.Subject.ID != runner.ID
		})
		scope.fingerprints = map[string]bool{healthFingerprint("runner_setup_failures", "runner", runner.ID): true, healthFingerprint("capacity_starvation", "runner", runner.ID): true}
		if err := s.publishHealthEvaluationScope(ctx, organization, now, values, scope); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

type healthProjectSource struct {
	project                                                                    projectstore.Record
	observation                                                                healthProject
	rate                                                                       healthRateProject
	capacity                                                                   runnerstore.RuntimeCapacitySnapshot
	candidates                                                                 claimstore.CandidatePage
	diagnostics                                                                diagnosticstore.Snapshot
	residence                                                                  eventstore.AnalyticsResidencePage
	questions                                                                  []healthstore.QuestionSource
	capacityKnown, attemptsKnown, dependencyKnown, refusalKnown, landingsKnown bool
}

func (s *Service) readHealthProjectSource(ctx context.Context, organization tracker.OrganizationID, projectID tracker.ProjectID, runnerID string, now time.Time) (healthProjectSource, error) {
	var project projectstore.Record
	var scheduled healthstore.ScheduledHistory
	var diagnostics diagnosticstore.Snapshot
	var residence eventstore.AnalyticsResidencePage
	var candidates claimstore.CandidatePage
	var capacity runnerstore.RuntimeCapacitySnapshot
	var costs []reportstore.CompletionCostFact
	var admissions map[string]tracker.NativeAdmissionObservation
	var dependency healthProject
	dependencyKnown := true
	decisions := []eventstore.Event{}
	refusalKnown := true
	var selectedQuestions []healthstore.QuestionSource
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return healthProjectSource{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		var err error
		project, err = reader.ProjectRecord(ctx, projectID)
		if err != nil {
			return err
		}
		if project.Core.Profile != "native" || project.Core.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		if runnerID == "" {
			owner, ok := reader.(healthstore.ScheduledReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			scheduled, err = owner.HealthScheduledHistory(ctx, project.Core.ID, now)
			if err != nil {
				return err
			}
			candidates, err = reader.CandidatePage(ctx, claimstore.CandidateQuery{ProjectID: project.Core.ID, Scope: string(project.Core.ID), AvailableAt: now, Limit: corestore.MaxPageSize})
			if err != nil {
				return err
			}
			transitions, ok := reader.(eventstore.WorkflowTransitionReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			for _, candidate := range candidates.Items {
				row, err := transitions.LatestSchedulerDecision(ctx, project.Core.ID, candidate.WorkItemID)
				if errors.Is(err, corestore.ErrNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				decisions = append(decisions, row)
			}

		}
		capacityOwner, ok := reader.(runnerstore.RuntimeCapacityReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		capacity, err = capacityOwner.RuntimeCapacitySnapshot(ctx, runnerstore.RuntimeCapacityQuery{ProjectID: project.Core.ID, RunnerID: runnerID, At: now})
		if err != nil {
			return err
		}
		if runnerID == "" {
			ids := make([]string, len(capacity.Records))
			for index, row := range capacity.Records {
				ids[index] = row.Runner.RunnerID
			}
			admissionOwner, ok := reader.(runnerstore.AdmissionReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			admissions, err = admissionOwner.RunnerAdmissionObservations(ctx, project.Core.ID, ids)
			if err != nil {
				return err
			}
		}
		from := now.Add(-healthBaselineWindow - healthLongWindow)
		diagnosticOwner, ok := reader.(diagnosticstore.Reader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		diagnostics, err = diagnosticOwner.DiagnosticsSnapshot(ctx, []tracker.ProjectID{project.Core.ID}, from, now, now, "")
		if err != nil {
			return err
		}
		residenceOwner, ok := reader.(eventstore.AnalyticsResidenceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		residence, err = residenceOwner.ReadAnalyticsResidencePage(ctx, eventstore.AnalyticsResidenceQuery{ProjectID: project.Core.ID, From: from, To: now, Live: true, Limit: 1000})
		if err != nil {
			return err
		}
		if runnerID == "" {
			requested := make([]reportstore.CompletionCostItem, len(residence.Issues))
			for index, item := range residence.Issues {
				requested[index] = reportstore.CompletionCostItem{ProjectID: project.Core.ID, WorkItemID: item.WorkItemID}
			}
			costOwner, ok := reader.(reportstore.CompletionCostReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			costs, err = costOwner.ReadCompletionCosts(ctx, reportstore.CompletionCostQuery{Items: requested, CurrentTo: now, PriorTo: now.Add(-healthLongWindow)})
			if err != nil && !errors.Is(err, reportstore.ErrPopulationExceeded) {
				return err
			}
			if errors.Is(err, reportstore.ErrPopulationExceeded) {
				costs = nil
				err = nil
			}
			dependency, dependencyKnown, err = readHealthDependencySource(ctx, reader, project)
			if err != nil {
				return err
			}
			sourceOwner, ok := reader.(healthstore.SourceReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			selectedQuestions, err = sourceOwner.HealthSourceQuestions(ctx, "", "", corestore.MaxPageSize)
		}
		return err
	})
	if err != nil {
		return healthProjectSource{}, err
	}
	p := healthProject{ID: string(project.Core.ID), Scheduled: healthScheduledHistory(scheduled), Candidates: len(candidates.Items), CandidateIDs: map[string]bool{}}
	p.DependencyGated, p.HeldCount, p.HeldBlockers, p.HoldSince = dependency.DependencyGated, dependency.HeldCount, dependency.HeldBlockers, dependency.HoldSince

	for _, candidate := range candidates.Items {
		p.CandidateIDs[string(candidate.WorkItemID)] = true
		if candidate.DispatchWaitSince != nil && (p.CandidateSince.IsZero() || candidate.DispatchWaitSince.Before(p.CandidateSince)) {
			p.CandidateSince = *candidate.DispatchWaitSince
		}
	}
	capacityKnown := !capacity.Truncated
	runners := runtimeRunnersFromSnapshot(capacity, nativeScope{organization: organization, project: project.Core.ID}, now)
	rate := healthRateProject{ID: p.ID, Candidates: p.Candidates, ResidencePartial: residence.IssuesMore || residence.EventsMore, Costs: map[string]healthItemCost{}, BaselineCosts: map[string]healthItemCost{}}
	for _, runner := range runners {
		view := runner.CapacityView("", now)
		if view.Effective == nil {
			capacityKnown = false
		} else if runner.State == "active" {
			free := max(0, min(*view.Effective-runner.Used, runner.HostCapacity-runner.HostUsed))
			if limit, ok := runner.ProjectCapacityLimits[project.Core.ID]; ok {
				free = min(free, max(0, limit-runner.ProjectUsed[project.Core.ID]))
			}
			p.FreeSlots += free
			rate.Runners = append(rate.Runners, healthRunner{ID: runner.RunnerID, Heartbeat: runner.LastHeartbeatAt, Leases: runner.Used, FreeSlots: free})
		}
		if observation, ok := admissions[runner.RunnerID]; ok && observation.Context.LastRefreshAt.After(p.RefreshAt) {
			p.RefreshAt, p.RefreshDuration = observation.Context.LastRefreshAt, observation.Context.RefreshDuration
		}
	}
	for _, landing := range diagnostics.Landings {
		if landing.At.After(p.LastLanding) {
			p.LastLanding = landing.At
		}
	}
	content := eventstore.NewContent(hubmaintenance.LimitDetails(s.runDetailStore()))
	for _, row := range decisions {
		value, err := content.Event(ctx, row)
		if errors.Is(err, artifact.ErrMissing) || errors.Is(err, fs.ErrNotExist) {
			refusalKnown = false
			continue
		}
		if err != nil {
			return healthProjectSource{}, err
		}
		if value.ProjectID != project.Core.ID || !p.CandidateIDs[string(value.AggregateID)] || value.Data.Decision == nil {
			return healthProjectSource{}, artifact.ErrIntegrity
		}
		decision := value.Data.Decision
		candidateIndex := slices.IndexFunc(candidates.Items, func(candidate claimstore.Candidate) bool { return candidate.WorkItemID == value.AggregateID })
		if candidateIndex < 0 || decision.Source != "native_claim" || int64(decision.WorkItemRevision) != candidates.Items[candidateIndex].Revision || p.RefreshAt.IsZero() || value.RecordedAt.Before(p.RefreshAt) {
			refusalKnown = false
			continue
		}
		if decision.Outcome == "claimed" {
			p.Claims++
		} else if decision.Reason != "" {
			p.RefusalReasons = append(p.RefusalReasons, decision.Reason)
			p.RefusalEvents = append(p.RefusalEvents, value.ID)
		} else {
			refusalKnown = false
		}
	}

	attemptValues, attemptErrors := readDiagnosticObjects(diagnostics.AttemptRecords, func(attempt diagnosticstore.Attempt) (tracker.NativeRunData, error) {
		if attempt.Event.ID == "" {
			return tracker.NativeRunData{}, nil
		}
		value, err := content.Event(ctx, attempt.Event)
		if err != nil {
			return tracker.NativeRunData{}, err
		}
		if value.Data.Run == nil || value.Data.Run.AttemptID != attempt.ID || value.AggregateID != attempt.WorkItemID || value.ProjectID != project.Core.ID {
			return tracker.NativeRunData{}, artifact.ErrIntegrity
		}
		return *value.Data.Run, nil
	})
	attemptsKnown := !diagnostics.AttemptsPartial
	usage := map[string]float64{}
	for _, row := range diagnostics.Usage {
		usage[row.AttemptID] += row.Cost
	}
	landingsKnown := len(diagnostics.Landings) < diagnosticstore.Limit
	canonical := map[string]eventstore.VersionLanding{}
	missingRunner := map[string]string{}
	if err := repository.ReadTenantSnapshot(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		for index, attempt := range diagnostics.AttemptRecords {
			if attempt.Event.ID == "" || attemptErrors[index] != nil {
				continue
			}
			data := attemptValues[index]
			if data.RunnerID == "" {
				row, err := reader.Attempt(ctx, attempt.ID)
				if err != nil {
					return err
				}
				identity, err := reader.RunnerForLease(ctx, row.LeaseID)
				if err == nil {
					missingRunner[attempt.ID] = identity.RunnerID
				} else if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
			}
			if data.Runtime == nil || data.Runtime.Landing == nil || !data.Runtime.Landing.Landed {
				continue
			}
			landing := data.Runtime.Landing
			owner, ok := reader.(eventstore.VersionLandingReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			row, err := owner.VersionLanding(ctx, landing.ChangeID, landing.VersionID)
			if errors.Is(err, corestore.ErrNotFound) {
				landingsKnown = false
				continue
			}
			if err != nil {
				return err
			}
			if row.OrganizationID != organization || row.ProjectID != project.Core.ID || row.WorkItemID != attempt.WorkItemID || row.HeadSHA != landing.HeadSHA || row.MergeSHA != landing.MergeSHA {
				return artifact.ErrIntegrity
			}
			canonical[attempt.ID] = row
		}
		return nil
	}); err != nil {
		return healthProjectSource{}, err
	}

	landed := map[string]bool{}
	for index, row := range diagnostics.AttemptRecords {
		if row.Event.ID == "" {
			attemptsKnown = false
			continue
		}
		if err := attemptErrors[index]; errors.Is(err, artifact.ErrMissing) || errors.Is(err, fs.ErrNotExist) {
			attemptsKnown = false
			continue
		}
		if err := attemptErrors[index]; err != nil {
			return healthProjectSource{}, fmt.Errorf("read health attempt: %w", err)
		}
		a := healthRateAttempt{ID: row.ID, Item: string(row.WorkItemID), Status: row.Status, Started: row.StartedAt, At: row.UpdatedAt, Runner: attemptValues[index].RunnerID, Cost: usage[row.ID]}
		applyHealthAttemptData(&a, attemptValues[index])
		if a.Runner == "" {
			a.Runner = missingRunner[row.ID]
		}
		if a.Landed {
			if proof, ok := canonical[row.ID]; ok {
				a.LandingAt = proof.At
			} else {
				a.Landing, a.Landed = false, false
			}
		}

		if a.Landing && a.LandingAt.IsZero() {
			a.LandingAt = a.At
		}
		if a.Landed && a.LandingKey != "" {
			if landed[a.LandingKey] {
				a.Landing, a.Landed = false, false
			}
			landed[a.LandingKey] = true
		}
		rate.Attempts = append(rate.Attempts, a)
	}
	for _, landing := range diagnostics.Landings {
		duplicate := slices.ContainsFunc(rate.Attempts, func(attempt healthRateAttempt) bool {
			proof, ok := canonical[attempt.ID]
			return ok && proof.ChangeID == landing.ChangeID && proof.At.Equal(landing.At)
		})
		if !duplicate {
			rate.Attempts = append(rate.Attempts, healthRateAttempt{ID: landing.ChangeID, Item: string(landing.WorkItemID), Landing: true, Landed: true, LandingAt: landing.At})
		}
	}
	slices.SortFunc(rate.Attempts, func(a, b healthRateAttempt) int {
		if order := a.Started.Compare(b.Started); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	states := map[string]tracker.NativeState{}
	for _, state := range residence.States {
		states[state.Name] = state
	}
	issues := make([]analyticsResidenceIssue, len(residence.Issues))
	positions := map[tracker.NativeWorkItemID]int{}
	for index, row := range residence.Issues {
		issues[index] = analyticsResidenceIssue{Id: string(row.WorkItemID), Created: row.CreatedAt, Initial: row.InitialState}
		positions[row.WorkItemID] = index
	}
	for _, row := range residence.Events {
		index, ok := positions[row.WorkItemID]
		if !ok {
			return healthProjectSource{}, artifact.ErrIntegrity
		}
		value, err := content.Event(ctx, row)
		if errors.Is(err, artifact.ErrMissing) || errors.Is(err, fs.ErrNotExist) {
			rate.ResidencePartial = true
			continue
		}
		if err != nil {
			return healthProjectSource{}, err
		}
		if value.AggregateID != row.WorkItemID || value.ProjectID != project.Core.ID {
			return healthProjectSource{}, artifact.ErrIntegrity
		}
		if value.Type == "scheduler.decision" && (value.Data.Decision == nil || value.Data.Decision.Source != "native_claim" || value.Data.Decision.Outcome != "claimed") {
			continue
		}
		issues[index].Events = append(issues[index].Events, analyticsResidenceEvent{At: value.RecordedAt, Kind: value.Type, Data: value.Data})
	}
	for _, issue := range issues {
		issue.Partial = rate.ResidencePartial
		rate.Timelines = append(rate.Timelines, healthResidenceTimeline(buildAnalyticsResidenceTimeline(issue, states, now)))
	}
	for _, cost := range costs {
		if cost.Complete {
			rate.Costs[string(cost.WorkItemID)] = healthItemCost{Cost: cost.Cost, Known: true}
			rate.BaselineCosts[string(cost.WorkItemID)] = healthItemCost{Cost: cost.PriorCost, Known: true}
		}
	}
	return healthProjectSource{project: project, observation: p, rate: rate, capacity: capacity, candidates: candidates, diagnostics: diagnostics, residence: residence, questions: selectedQuestions, capacityKnown: capacityKnown, attemptsKnown: attemptsKnown, dependencyKnown: dependencyKnown, refusalKnown: refusalKnown, landingsKnown: landingsKnown}, nil
}

func (s *Service) maintainHealthProject(ctx context.Context, organization tracker.OrganizationID, after *tracker.ProjectID) error {
	now := s.config.now().UTC()
	var selected corestore.Project
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		rows, err := reader.Projects(ctx, *after, 1)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			*after = ""
			return nil
		}
		selected = rows[0]
		*after = selected.ID
		return nil
	})
	if err != nil || selected.ID == "" || selected.Profile != "native" || selected.DeletedAt != nil {
		return err
	}
	source, err := s.readHealthProjectSource(ctx, organization, selected.ID, "", now)
	if err != nil {
		return err
	}
	project, p, rate := source.project, source.observation, source.rate
	capacity, candidates, residence, selectedQuestions := source.capacity, source.candidates, source.residence, source.questions
	capacityKnown, attemptsKnown := source.capacityKnown, source.attemptsKnown
	runners := runtimeRunnersFromSnapshot(capacity, nativeScope{organization: organization, project: project.Core.ID}, now)

	scope := healthEvaluationScope{projects: map[tracker.ProjectID]bool{project.Core.ID: true}, fingerprints: map[string]bool{}}
	findings := []healthFinding{}
	gaps := []healthBaselineUnavailable{}
	add := func(finding healthFinding, active, complete bool) {
		if active {
			findings = append(findings, finding)
			scope.fingerprints[finding.Fingerprint] = true
		} else if complete {
			scope.fingerprints[finding.Fingerprint] = true
		} else {
			gaps = append(gaps, healthBaselineUnavailable{Project: p.ID, Signal: finding.Signal})
		}
	}
	flowKnown := capacityKnown && candidates.Next.WorkItemID == "" && source.landingsKnown
	dead, active := deadManFinding(now, p)
	if !flowKnown {
		delete(dead.Evidence.Counts, "candidates")
		delete(dead.Evidence.Counts, "free_slots")
		delete(dead.Evidence.Counts, "landing_stalled")
		dead.Evidence.Reason = "flow_coverage_unavailable"
		scheduledOnly := healthProject{ID: p.ID, Scheduled: p.Scheduled}
		dead, active = deadManFinding(now, scheduledOnly)
		delete(dead.Evidence.Counts, "candidates")
		delete(dead.Evidence.Counts, "free_slots")
		delete(dead.Evidence.Counts, "landing_stalled")
		dead.Evidence.Reason = "flow_coverage_unavailable"
	}
	add(dead, active, flowKnown)
	hold, held := dependencyHoldFinding(now, p)
	if !source.dependencyKnown {
		delete(hold.Evidence.Counts, "dependency_gated")
		delete(hold.Evidence.Counts, "held_blockers")
		hold.Evidence.Reason = "dependency_coverage_unavailable"
	}
	add(hold, held && candidates.Next.WorkItemID == "" && capacityKnown && source.landingsKnown, source.dependencyKnown && flowKnown)
	scheduler, schedulerActive := schedulerFinding(now, p)
	add(scheduler, schedulerActive, !capacity.Truncated)
	if !capacity.Truncated && len(p.RefusalReasons) == 0 {
		fallback := ""
		allSame := len(runners) > 0
		for _, runner := range runners {
			reason := ""
			if runner.State != "active" {
				reason = "runner_inactive"
			} else if now.Sub(runner.LastHeartbeatAt) >= runnerauth.HeartbeatTimeout {
				reason = "runner_heartbeat_stale"
			} else if capacityKnown && p.FreeSlots == 0 {
				reason = "runner_capacity_full"
			}
			if reason == "" || fallback != "" && reason != fallback {
				allSame = false
			}
			fallback = reason
		}
		if allSame {
			source.refusalKnown = true
			for range p.Candidates {
				p.RefusalReasons = append(p.RefusalReasons, fallback)
			}
		}
	}
	refusal, refused := refusalFinding(p)
	add(refusal, refused && candidates.Next.WorkItemID == "" && !capacity.Truncated, source.refusalKnown && !capacity.Truncated && candidates.Next.WorkItemID == "")
	if attemptsKnown {
		rateFindings := evaluateHealthRates(now, []healthRateProject{rate})
		for _, finding := range rateFindings {
			if finding.Subject.Kind == "runner" {
				continue
			}
			if !source.landingsKnown && (finding.Signal == "conflict_burn" || finding.Signal == "throughput_collapse" || finding.Signal == "activity_without_delivery") {
				continue
			}
			if rate.ResidencePartial && (finding.Signal == "cost_anomaly" || finding.Signal == "throughput_collapse" || finding.Signal == "stalled_active_item" || finding.Signal == "activity_without_delivery") {
				continue
			}
			if source.diagnostics.UsagePartial && finding.Signal == "retry_storm" {
				delete(finding.Evidence.Values, "wasted_cost_usd")
			}
			findings = append(findings, finding)
			scope.fingerprints[finding.Fingerprint] = true
		}
		for _, issue := range residence.Issues {
			for _, signal := range []string{"retry_storm", "recovery_loop", "unproductive_attempts"} {
				scope.fingerprints[healthFingerprint(signal, "work_item", string(issue.WorkItemID))] = true
			}
			if !rate.ResidencePartial {
				scope.fingerprints[healthFingerprint("stalled_active_item", "work_item", string(issue.WorkItemID))] = true
				if rate.Costs[string(issue.WorkItemID)].Known && len(buildHealthRateBaseline(now, rate).Costs) >= healthBaselineSamples {
					scope.fingerprints[healthFingerprint("cost_anomaly", "work_item", string(issue.WorkItemID))] = true
				}
			}
		}
		if !rate.ResidencePartial && source.landingsKnown {
			for _, signal := range []string{"conflict_burn", "throughput_collapse", "activity_without_delivery"} {
				scope.fingerprints[healthFingerprint(signal, "project", p.ID)] = true
			}
		}
	} else {
		for _, signal := range []string{"retry_storm", "recovery_loop", "unproductive_attempts", "activity_without_delivery", "conflict_burn", "throughput_collapse", "stalled_active_item"} {
			gaps = append(gaps, healthBaselineUnavailable{Project: p.ID, Signal: signal})
		}
	}
	gaps = append(gaps, healthBaselineGaps(rate, buildHealthRateBaseline(now, rate))...)
	questionComplete := len(selectedQuestions) < corestore.MaxPageSize
	questionActive := false
	for _, row := range selectedQuestions {
		if row.ProjectID != project.Core.ID || row.WorkItemID != "" {
			continue
		}
		if healthQuestionActive(row, now) {
			wait := healthWait{Project: p.ID, Since: row.CreatedAt, Attempt: row.AttemptID, Active: true}
			finding, active := humanWaitFinding(now, wait)
			if active {
				questionActive = true
				add(finding, true, false)
			}
		}
	}
	if questionComplete && !questionActive {
		scope.fingerprints[healthFingerprint("unanswered_human_wait", "project", p.ID)] = true
	}
	if !questionComplete {
		gaps = append(gaps, healthBaselineUnavailable{Project: p.ID, Signal: "unanswered_human_wait"})
	}
	byFingerprint := map[string]healthFinding{}
	for _, finding := range findings {
		if old, ok := byFingerprint[finding.Fingerprint]; !ok || finding.Signal == "unanswered_human_wait" && finding.ConditionSince.Before(old.ConditionSince) {
			byFingerprint[finding.Fingerprint] = finding
		}
	}
	findings = findings[:0]
	for _, finding := range byFingerprint {
		findings = append(findings, finding)
	}
	slices.SortFunc(findings, func(a, b healthFinding) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
	if err := s.publishHealthEvaluationScope(ctx, organization, now, findings, scope); err != nil {
		return err
	}
	if !source.landingsKnown {
		for _, signal := range []string{"conflict_burn", "throughput_collapse", "activity_without_delivery"} {
			gaps = append(gaps, healthBaselineUnavailable{Project: p.ID, Signal: signal})
		}
	}
	if rate.ResidencePartial {
		for _, signal := range []string{"stalled_active_item", "cost_anomaly", "capacity_starvation", "throughput_collapse", "activity_without_delivery"} {
			gaps = append(gaps, healthBaselineUnavailable{Project: p.ID, Signal: signal})
		}
	}
	if !attemptsKnown {
		gaps = append(gaps, healthBaselineUnavailable{Project: p.ID, Signal: "runner_setup_failures"})
	}
	return s.publishHealthSourceBaselines(ctx, organization, project.Core.ID, now, gaps)
}

func healthQuestionActive(row healthstore.QuestionSource, now time.Time) bool {
	return !row.ConversationDeleted && !row.ConversationArchived && row.ConversationStatus == "active" && slices.Contains([]string{"pending", "sending", "sent", "unknown"}, row.Status) && (row.ExpiresAt == nil || row.ExpiresAt.After(now))
}

func (s *Service) publishHealthSourceBaselines(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, now time.Time, gaps []healthBaselineUnavailable) error {
	wanted := map[[2]string]bool{}
	for _, gap := range gaps {
		wanted[[2]string{gap.Signal, gap.Lane}] = true
	}
	current := map[[2]string]bool{}
	if err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		owner, ok := reader.(healthstore.SourceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		rows, err := owner.HealthSourceBaselines(ctx, project)
		if err != nil {
			return err
		}
		for _, row := range rows {
			current[[2]string{row.Signal, row.Lane}] = true
		}
		return nil
	}); err != nil {
		return err
	}

	return s.database.postgres.WriteHealth(ctx, organization, corestore.Background, func(writer postgres.HealthWriter) error {
		for _, signal := range []string{"dead_man", "scheduler_loop_behind", "every_candidate_refused", "dependency_hold", "unanswered_human_wait", "runner_setup_failures", "retry_storm", "recovery_loop", "unproductive_attempts", "activity_without_delivery", "conflict_burn", "throughput_collapse", "stalled_active_item", "cost_anomaly", "capacity_starvation"} {
			lanes := []string{""}
			if signal == "stalled_active_item" {
				lanes = []string{"", "In Progress", "Rework", "Merging"}
			}
			if signal == "capacity_starvation" {
				lanes = []string{"", "Todo"}
			}
			for _, lane := range lanes {
				if current[[2]string{signal, lane}] == wanted[[2]string{signal, lane}] {
					continue
				}
				input := postgres.PutHealthBaselineInput{OrganizationID: organization, ProjectID: project, Signal: signal, Lane: lane}
				if wanted[[2]string{signal, lane}] {
					if err := writer.PutHealthBaseline(ctx, input); err != nil {
						return err
					}
				} else {
					if err := writer.ClearHealthBaseline(ctx, postgres.ClearHealthBaselineInput(input)); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

func (s *Service) maintainHealthWaitItems(ctx context.Context, organization tracker.OrganizationID, cursor *healthWaitItemCursor, limit int) error {
	var items []healthstore.ItemSource
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		var project projectstore.Record
		if cursor.project == "" {
			projects, err := reader.Projects(ctx, cursor.afterProject, 1)
			if err != nil {
				return err
			}
			if len(projects) == 0 {
				*cursor = healthWaitItemCursor{}
				return nil
			}
			cursor.project, cursor.afterProject = projects[0].ID, projects[0].ID
			if projects[0].Profile != "native" || projects[0].DeletedAt != nil {
				cursor.project = ""
				return nil
			}
		}
		var err error
		project, err = reader.ProjectRecord(ctx, cursor.project)
		if errors.Is(err, corestore.ErrNotFound) {
			cursor.project = ""
			cursor.state = ""
			cursor.id = 0
			return nil
		}
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil || project.Core.Profile != "native" {
			cursor.project = ""
			cursor.state = ""
			cursor.id = 0
			return nil
		}
		var states []string
		for _, state := range project.States {
			if !state.Terminal {
				states = append(states, state.Name)
			}
		}
		slices.Sort(states)
		if cursor.state == "" && len(states) > 0 {
			cursor.state = states[0]
		}
		if !slices.Contains(states, cursor.state) {
			next := slices.IndexFunc(states, func(state string) bool { return state > cursor.state })
			if next < 0 {
				cursor.project = ""
				cursor.state = ""
				cursor.id = 0
				return nil
			}
			cursor.state, cursor.id = states[next], 0
		}
		owner, ok := reader.(healthstore.SourceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		items, err = owner.HealthSourceItems(ctx, cursor.project, cursor.state, cursor.id, limit)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			next := slices.IndexFunc(states, func(state string) bool { return state > cursor.state })
			cursor.id = 0
			if next < 0 {
				cursor.project, cursor.state = "", ""
			} else {
				cursor.state = states[next]
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, item := range items {
		cursor.id = item.ID
		if err := s.maintainHealthWaitItem(ctx, organization, item.WorkItemReference); err != nil {
			failures = append(failures, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(failures...)
}

func (s *Service) maintainHealthWaitItem(ctx context.Context, organization tracker.OrganizationID, reference corestore.WorkItemReference) error {
	now := s.config.now().UTC()
	var item corestore.WorkItem
	var version eventstore.CollaborationVersion
	var project projectstore.Record
	var attempt leasestore.Attempt
	var run, transition eventstore.Event
	var questions []healthstore.QuestionSource
	var conversationRecord conversationstore.Conversation
	var plan *nativeClaimPlan
	complete := true
	found := false
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		var err error
		project, err = reader.ProjectRecord(ctx, reference.ProjectID)
		if err != nil {
			return err
		}
		if project.Core.Profile != "native" || project.Core.DeletedAt != nil {
			return nil
		}
		item, err = reader.WorkItem(ctx, reference.ProjectID, reference.WorkItemID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		version, err = reader.CollaborationVersion(ctx, string(item.NativeID), item.Revision)
		if err != nil {
			return err
		}
		attempt, err = reader.LatestAttempt(ctx, item.ID)
		if err == nil {
			run, err = reader.LatestRunEvent(ctx, reference.ProjectID, attempt.ID)
			if errors.Is(err, corestore.ErrNotFound) {
				complete = false
				err = nil
			}
		} else if errors.Is(err, corestore.ErrNotFound) {
			err = nil
		}
		if err != nil {
			return err
		}
		transitions, ok := reader.(eventstore.WorkflowTransitionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		transition, err = transitions.LatestWorkflowTransition(ctx, reference.ProjectID, item.NativeID)
		if errors.Is(err, corestore.ErrNotFound) {
			err = nil
		}
		if err != nil {
			return err
		}
		conversationRecord, err = reader.ConversationByWorkItem(ctx, reference.ProjectID, string(item.NativeID))
		if err == nil {
			questionOwner, ok := reader.(healthstore.SourceReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			questions, err = questionOwner.HealthSourceConversationQuestions(ctx, conversationRecord.ID, "", corestore.MaxPageSize)
			if len(questions) == corestore.MaxPageSize {
				complete = false
			}
		} else if errors.Is(err, corestore.ErrNotFound) {
			err = nil
		}
		if err != nil {
			return err
		}
		if item.Archived {
			return nil
		}
		page, err := reader.CandidatePage(ctx, claimstore.CandidateQuery{ProjectID: reference.ProjectID, Scope: string(reference.ProjectID), WorkItemID: item.NativeID, AvailableAt: now, Limit: 1})
		if err != nil {
			return err
		}
		if len(page.Items) > 0 {
			policyRecord, err := reader.ProjectPolicy(ctx, reference.ProjectID)
			if errors.Is(err, corestore.ErrNotFound) {
				complete = false
				return nil
			}
			if err != nil {
				return err
			}
			scope := nativeScope{organization: organization, project: reference.ProjectID}
			value, err := s.readNativeClaimPlanRows(ctx, scope, tracker.NativeClaim{PolicyID: policyRecord.PolicyID, WorkItemID: item.NativeID}, now, reader, &page.Items[0], nativeClaimPlan{})
			if err != nil {
				return err
			}
			plan = &value
		}
		return nil
	})
	if err != nil || !found {
		return err
	}
	issue, err := s.hydrateWorkItemRecord(ctx, item, version)
	if err != nil {
		return err
	}
	waits := []healthWait{}
	for _, question := range questions {
		if healthQuestionActive(question, now) {
			waits = append(waits, healthWait{Item: string(item.NativeID), Project: string(reference.ProjectID), Since: question.CreatedAt, Attempt: question.AttemptID, Active: true})
		}
	}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	var data tracker.NativeRunData
	if run.ID != "" {
		value, err := content.Event(ctx, run)
		if errors.Is(err, artifact.ErrMissing) || errors.Is(err, fs.ErrNotExist) {
			complete = false
		} else if err != nil {
			return err
		} else if value.Data.Run == nil || value.Data.Run.AttemptID != attempt.ID || value.AggregateID != item.NativeID || value.ProjectID != reference.ProjectID {
			return artifact.ErrIntegrity
		} else {
			data = *value.Data.Run
		}
	}
	terminal := false
	for _, state := range project.States {
		if state.Name == issue.State {
			terminal = state.Terminal
		}
	}
	if !item.Archived && !terminal && transition.ID != "" {
		value, err := content.Event(ctx, transition)
		if errors.Is(err, artifact.ErrMissing) || errors.Is(err, fs.ErrNotExist) {
			complete = false
		} else if err != nil {
			return err
		} else {
			if value.AggregateID != item.NativeID || value.ProjectID != reference.ProjectID {
				return artifact.ErrIntegrity
			}
			detail := strings.ReplaceAll(strings.TrimSpace(value.Data.ReasonDetail), " ", "_")
			if value.Actor.Kind == "runner" && (detail == "permission_wait" || detail == "human_action") && int64(value.Data.Revision) == item.Revision {
				waits = append(waits, healthWait{Item: string(item.NativeID), Project: string(reference.ProjectID), Since: value.RecordedAt, Attempt: attempt.ID, Event: value.ID, Active: true})
			} else if disposition := data.Disposition; disposition != nil && disposition.Status == "blocked" && (disposition.HumanAction || disposition.ReasonCode == "permission_wait") && attempt.Status == "succeeded" && value.Actor.Kind == "runner" && value.Data.Reason == "worker_progress" && int64(value.Data.Revision) == item.Revision && !value.RecordedAt.Before(attempt.UpdatedAt) {
				waits = append(waits, healthWait{Item: string(item.NativeID), Project: string(reference.ProjectID), Since: attempt.UpdatedAt, Attempt: attempt.ID, Active: true})
			}
		}
	}
	if plan != nil {
		scope := nativeScope{organization: organization, project: reference.ProjectID}
		plans := []nativeClaimPlan{*plan}
		if err := s.prepareNativeClaimLegacyAuditFacts(ctx, scope, plans); err != nil {
			return err
		}
		if err := s.prepareNativeClaimApprovals(ctx, scope, plans); err != nil {
			return err
		}
		value, err := s.hydrateNativeClaimPlan(ctx, scope, tracker.NativeClaim{PolicyID: plan.policy.PolicyID, WorkItemID: item.NativeID}, now, plans[0], nil)
		if err != nil && !errors.Is(err, ErrNoClaimableWork) {
			return err
		}
		if err == nil {
			evaluation, err := nativeRuntimeIssueContractEvaluation(value)
			if err != nil {
				return err
			}
			if !evaluation.Allowed && evaluation.ReasonCode == "issue_contract_unsatisfied" {
				waits = append(waits, healthWait{Item: string(item.NativeID), Project: string(reference.ProjectID), Since: item.NativeUpdatedAt, Reason: evaluation.Reason, Active: true})
			}
		}
	}
	findings := []healthFinding{}
	var selected *healthFinding
	for _, wait := range waits {
		finding, active := humanWaitFinding(now, wait)
		if active && (selected == nil || finding.ConditionSince.Before(selected.ConditionSince)) {
			selected = &finding
		}
	}
	if selected != nil {
		findings = append(findings, *selected)
	}
	if !complete {
		if err := s.database.postgres.WriteHealth(ctx, organization, corestore.Background, func(writer postgres.HealthWriter) error {
			return writer.PutHealthBaseline(ctx, postgres.PutHealthBaselineInput{OrganizationID: organization, ProjectID: reference.ProjectID, Signal: "unanswered_human_wait"})
		}); err != nil {
			return err
		}
		if selected == nil {
			return nil
		}
	}
	fingerprint := healthFingerprint("unanswered_human_wait", "work_item", string(reference.WorkItemID))
	return s.publishHealthEvaluationScope(ctx, organization, now, findings, healthEvaluationScope{projects: map[tracker.ProjectID]bool{reference.ProjectID: true}, fingerprints: map[string]bool{fingerprint: true}})
}

func (s *Service) maintainHealthQuestions(ctx context.Context, organization tracker.OrganizationID, afterConversation, afterQuestion *string, limit int) error {
	now := s.config.now().UTC()
	var rows []healthstore.QuestionSource
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		owner, ok := reader.(healthstore.SourceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		rows, err = owner.HealthSourceQuestions(ctx, *afterConversation, *afterQuestion, limit)
		return err
	})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		*afterConversation, *afterQuestion = "", ""
		return nil
	}
	byFingerprint := map[string]healthFinding{}
	projects := map[tracker.ProjectID]bool{}
	for _, row := range rows {
		*afterConversation, *afterQuestion = row.ConversationID, row.ID
		if !healthQuestionActive(row, now) {
			continue
		}
		var project corestore.Project
		if err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
			var err error
			project, err = reader.Project(ctx, row.ProjectID)
			return err
		}); err != nil {
			return err
		}
		if project.Profile != "native" || project.DeletedAt != nil {
			continue
		}
		wait := healthWait{Project: string(row.ProjectID), Item: string(row.WorkItemID), Attempt: row.AttemptID, Since: row.CreatedAt, Active: true}
		finding, active := humanWaitFinding(now, wait)
		if !active {
			continue
		}
		if old, ok := byFingerprint[finding.Fingerprint]; !ok || finding.ConditionSince.Before(old.ConditionSince) {
			byFingerprint[finding.Fingerprint] = finding
		}
		projects[row.ProjectID] = true
	}
	if len(byFingerprint) == 0 {
		return nil
	}
	findings := make([]healthFinding, 0, len(byFingerprint))
	fingerprints := map[string]bool{}
	for fingerprint, finding := range byFingerprint {
		findings = append(findings, finding)
		fingerprints[fingerprint] = true
	}
	slices.SortFunc(findings, func(a, b healthFinding) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
	return s.publishHealthEvaluationScope(ctx, organization, now, findings, healthEvaluationScope{projects: projects, fingerprints: fingerprints})
}

func readHealthDependencySource(ctx context.Context, reader tenantstore.Reader, project projectstore.Record) (healthProject, bool, error) {
	out := healthProject{ID: string(project.Core.ID)}
	if !project.Core.RequireDependencies {
		return out, true, nil
	}
	transitions, ok := reader.(eventstore.WorkflowTransitionReader)
	if !ok {
		return out, false, hubapi.NativeWorkItemStorageUnavailable()
	}
	page, err := reader.WorkItemPage(ctx, project.Core.ID, corestore.Cursor{}, corestore.MaxPageSize)
	if err != nil {
		return out, false, err
	}
	items := page.Items
	complete := page.Next.ID == 0
	states := map[string]tracker.NativeState{}
	for _, state := range project.States {
		states[state.Name] = state
	}
	gated, held, listed := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, item := range items {
		state := states[hubapi.ValueOrEmpty(item.WorkflowStateName)]
		if item.Archived || state.Terminal || !state.Dispatchable {
			continue
		}
		relationships, err := reader.Relationships(ctx, item.ProjectID, item.NativeID, corestore.RelationshipCursor{}, corestore.MaxPageSize)
		if err != nil {
			return out, false, err
		}
		if relationships.Next.RelatedWorkItemID != "" {
			complete = false
		}
		dependentAt := item.NativeCreatedAt
		transition, err := transitions.LatestWorkflowTransition(ctx, item.ProjectID, item.NativeID)
		if err == nil {
			dependentAt = transition.RecordedAt
		} else if !errors.Is(err, corestore.ErrNotFound) {
			return out, false, err
		}
		for _, edge := range relationships.Items {
			if edge.Type != "dependency" || edge.Direction != "blocked_by" || edge.Terminal {
				continue
			}
			gated[string(item.NativeID)] = true
			if edge.Dispatchable {
				continue
			}
			held[string(edge.RelatedProjectID)+"/"+string(edge.RelatedWorkItemID)] = true
			if edge.RelatedProjectID == project.Core.ID {
				listed[string(edge.RelatedWorkItemID)] = true
			}
			blocker, err := reader.WorkItem(ctx, edge.RelatedProjectID, edge.RelatedWorkItemID)
			if err != nil {
				return out, false, err
			}
			blockerAt := blocker.NativeCreatedAt
			transition, err := transitions.LatestWorkflowTransition(ctx, edge.RelatedProjectID, edge.RelatedWorkItemID)
			if err == nil {
				blockerAt = transition.RecordedAt
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return out, false, err
			}
			since := edge.CreatedAt
			if dependentAt.After(since) {
				since = dependentAt
			}
			if blockerAt.After(since) {
				since = blockerAt
			}
			if out.HoldSince.IsZero() || since.Before(out.HoldSince) {
				out.HoldSince = since
			}
		}
	}
	out.DependencyGated, out.HeldCount = len(gated), len(held)
	for item := range listed {
		out.HeldBlockers = append(out.HeldBlockers, item)
	}
	slices.Sort(out.HeldBlockers)
	return out, complete, nil
}

func (s *Service) maintainRetiredHealthSubjects(ctx context.Context, organization tracker.OrganizationID, after *string, projectWait *healthProjectWaitCursor, limit int) error {
	if projectWait.finding.ID != "" {
		return s.maintainHealthProjectWait(ctx, organization, projectWait, limit)
	}
	now := s.config.now().UTC()
	var rows []postgres.HealthRecord
	err := s.database.postgres.ReadHealth(ctx, organization, corestore.Background, func(reader postgres.HealthReader) error {
		var err error
		rows, err = reader.HealthEvaluationRecords(ctx, postgres.HealthEvaluationRecordsInput{OrganizationID: organization, ID: *after, Limit: int32(limit)})
		return err
	})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		*after = ""
		return nil
	}
	var failures []error
	for _, row := range rows {
		*after = row.ID
		if row.LastSeenAt.After(now) || !slices.Contains([]string{"runner", "project", "work_item"}, row.SubjectKind) {
			continue
		}
		if row.SubjectKind == "project" && row.Signal == "unanswered_human_wait" {
			*projectWait = healthProjectWaitCursor{finding: row}
			if err := s.maintainHealthProjectWait(ctx, organization, projectWait, limit); err != nil {
				failures = append(failures, err)
			}
			break
		}
		if row.SubjectKind == "work_item" && row.Signal == "unanswered_human_wait" {
			var item corestore.WorkItem
			err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
				var err error
				item, err = reader.WorkItemByNativeID(ctx, tracker.NativeWorkItemID(row.SubjectID))
				return err
			})
			if err == nil {
				if err := s.maintainHealthWaitItem(ctx, organization, corestore.WorkItemReference{ProjectID: item.ProjectID, WorkItemID: item.NativeID}); err != nil {
					failures = append(failures, err)
				}
				continue
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				failures = append(failures, err)
				continue
			}
		}
		scope := healthEvaluationScope{projects: map[tracker.ProjectID]bool{tracker.ProjectID(row.SubjectID): true}, runners: map[string]bool{row.SubjectID: true}, fingerprints: map[string]bool{row.Fingerprint: true}, retiredSubjects: map[healthSubject]bool{{Kind: row.SubjectKind, ID: row.SubjectID}: true}, observedAt: now}
		covered := false
		if err := s.database.postgres.ReadHealth(ctx, organization, corestore.Background, func(reader postgres.HealthReader) error {
			var err error
			covered, err = scope.coversRecord(ctx, reader, organization, row)
			return err
		}); err != nil {
			failures = append(failures, err)
			continue
		}
		if covered {
			if err := s.resolveHealthRecord(ctx, organization, now, row, scope); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func (s *Service) maintainHealthProjectWait(ctx context.Context, organization tracker.OrganizationID, cursor *healthProjectWaitCursor, limit int) error {
	now := s.config.now().UTC()
	var questions []healthstore.QuestionSource
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		owner, ok := reader.(healthstore.SourceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		questions, err = owner.HealthSourceQuestions(ctx, cursor.conversation, cursor.question, limit)
		return err
	})
	if err != nil {
		return err
	}
	for _, question := range questions {
		cursor.conversation, cursor.question = question.ConversationID, question.ID
		if question.ProjectID != tracker.ProjectID(cursor.finding.SubjectID) || question.WorkItemID != "" || !healthQuestionActive(question, now) {
			continue
		}
		if _, active := humanWaitFinding(now, healthWait{Project: cursor.finding.SubjectID, Since: question.CreatedAt, Active: true}); active {
			*cursor = healthProjectWaitCursor{}
			return nil
		}
	}
	if len(questions) > 0 {
		return nil
	}
	row := cursor.finding
	scope := healthEvaluationScope{projects: map[tracker.ProjectID]bool{tracker.ProjectID(row.SubjectID): true}, fingerprints: map[string]bool{row.Fingerprint: true}}
	err = s.resolveHealthRecord(ctx, organization, now, row, scope)
	if errors.Is(err, corestore.ErrRevisionConflict) {
		err = nil
	}
	if err == nil {
		*cursor = healthProjectWaitCursor{}
	}
	return err
}
