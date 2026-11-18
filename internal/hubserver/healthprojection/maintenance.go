package healthprojection

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) MaintainPage(ctx context.Context, organization tracker.OrganizationID) (bool, error) {
	if err := hubmaintenance.RequireAllowance(ctx, string(organization), hubmaintenance.ByteLimit); err != nil {
		return false, err
	}
	var candidate healthstore.Finding
	err := s.Tenants.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
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
	err = s.Tenants.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
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
		raw, err := hubmaintenance.LimitDetails(s.Details).ReadDetail(ctx, string(organization), candidate.Evidence)
		if err != nil {
			return false, err
		}
		if err := json.Unmarshal(raw, &evidence); err != nil {
			return false, err
		}
	}
	published := false
	err = s.Tenants.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		owner, ok := w.(healthstore.PageMaintainer)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		published, err = owner.PublishHealthPage(ctx, candidate, evidence)
		return err
	})
	if published && s.Logger != nil {
		s.Logger.LogAttrs(ctx, slog.LevelDebug, "project health page", slog.String("organization_id", string(organization)), slog.Int64("generation", candidate.Generation), slog.Duration("projection_lag", s.Clock().Sub(candidate.SourceAt)))
	}
	return published, err
}

type WaitItemCursor struct {
	AfterProject tracker.ProjectID
	Project      tracker.ProjectID
	State        string
	ID           int64
}

func ScheduledHistory(history healthstore.ScheduledHistory) healthsignals.ScheduledValidation {
	out := healthsignals.ScheduledValidation{FirstReport: history.FirstReport, LastGreen: history.LastGreen}
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

func QuestionActive(row healthstore.QuestionSource, now time.Time) bool {
	return !row.ConversationDeleted && !row.ConversationArchived && row.ConversationStatus == "active" && slices.Contains([]string{"pending", "sending", "sent", "unknown"}, row.Status) && (row.ExpiresAt == nil || row.ExpiresAt.After(now))
}

func (s *Service) PublishBaselines(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, gaps []healthsignals.BaselineUnavailable) error {
	wanted := map[[2]string]bool{}
	for _, gap := range gaps {
		wanted[[2]string{gap.Signal, gap.Lane}] = true
	}
	current := map[[2]string]bool{}
	if err := s.Tenants.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
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

	return s.Postgres.WriteHealth(ctx, organization, corestore.Background, func(writer postgres.HealthWriter) error {
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

func (s *Service) MaintainWaitItems(ctx context.Context, organization tracker.OrganizationID, cursor *WaitItemCursor, limit int, maintain func(context.Context, tracker.OrganizationID, corestore.WorkItemReference) error) error {
	var items []healthstore.ItemSource
	repository, ok := s.Tenants.(tenantstore.SnapshotRepository)
	if !ok {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	err := repository.ReadTenantSnapshot(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
		var project projectstore.Record
		if cursor.Project == "" {
			projects, err := reader.Projects(ctx, cursor.AfterProject, 1)
			if err != nil {
				return err
			}
			if len(projects) == 0 {
				*cursor = WaitItemCursor{}
				return nil
			}
			cursor.Project, cursor.AfterProject = projects[0].ID, projects[0].ID
			if projects[0].Profile != "native" || projects[0].DeletedAt != nil {
				cursor.Project = ""
				return nil
			}
		}
		var err error
		project, err = reader.ProjectRecord(ctx, cursor.Project)
		if errors.Is(err, corestore.ErrNotFound) {
			cursor.Project = ""
			cursor.State = ""
			cursor.ID = 0
			return nil
		}
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil || project.Core.Profile != "native" {
			cursor.Project = ""
			cursor.State = ""
			cursor.ID = 0
			return nil
		}
		var states []string
		for _, state := range project.States {
			if !state.Terminal {
				states = append(states, state.Name)
			}
		}
		slices.Sort(states)
		if cursor.State == "" && len(states) > 0 {
			cursor.State = states[0]
		}
		if !slices.Contains(states, cursor.State) {
			next := slices.IndexFunc(states, func(state string) bool { return state > cursor.State })
			if next < 0 {
				cursor.Project = ""
				cursor.State = ""
				cursor.ID = 0
				return nil
			}
			cursor.State, cursor.ID = states[next], 0
		}
		owner, ok := reader.(healthstore.SourceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		items, err = owner.HealthSourceItems(ctx, cursor.Project, cursor.State, cursor.ID, limit)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			next := slices.IndexFunc(states, func(state string) bool { return state > cursor.State })
			cursor.ID = 0
			if next < 0 {
				cursor.Project, cursor.State = "", ""
			} else {
				cursor.State = states[next]
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, item := range items {
		cursor.ID = item.ID
		if err := maintain(ctx, organization, item.WorkItemReference); err != nil {
			failures = append(failures, err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(failures...)
}

func (s *Service) MaintainQuestions(ctx context.Context, organization tracker.OrganizationID, afterConversation, afterQuestion *string, limit int) error {
	now := s.Clock().UTC()
	var rows []healthstore.QuestionSource
	err := s.Tenants.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
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
	byFingerprint := map[string]healthsignals.Finding{}
	projects := map[tracker.ProjectID]bool{}
	for _, row := range rows {
		*afterConversation, *afterQuestion = row.ConversationID, row.ID
		if !QuestionActive(row, now) {
			continue
		}
		var project corestore.Project
		if err := s.Tenants.ReadTenant(ctx, organization, corestore.Background, func(reader tenantstore.Reader) error {
			var err error
			project, err = reader.Project(ctx, row.ProjectID)
			return err
		}); err != nil {
			return err
		}
		if project.Profile != "native" || project.DeletedAt != nil {
			continue
		}
		wait := healthsignals.Wait{Project: string(row.ProjectID), Item: string(row.WorkItemID), Attempt: row.AttemptID, Since: row.CreatedAt, Active: true}
		finding, active := healthsignals.HumanWaitFinding(now, wait)
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
	findings := make([]healthsignals.Finding, 0, len(byFingerprint))
	fingerprints := map[string]bool{}
	for fingerprint, finding := range byFingerprint {
		findings = append(findings, finding)
		fingerprints[fingerprint] = true
	}
	slices.SortFunc(findings, func(a, b healthsignals.Finding) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
	return s.Publish(ctx, organization, now, findings, Scope{Projects: projects, Fingerprints: fingerprints})
}

func ReadDependencies(ctx context.Context, reader tenantstore.Reader, project projectstore.Record) (healthsignals.Project, bool, error) {
	out := healthsignals.Project{ID: string(project.Core.ID)}
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
