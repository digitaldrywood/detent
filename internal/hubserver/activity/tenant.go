package activity

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/activitystore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) Read(ctx context.Context, caller Caller, request operatortool.ActivityRequest, window operatortool.AnalyticsWindow) (operatortool.ActivityReport, error) {
	credential := caller.Credential
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	now := s.deps.Now().UTC()
	out := operatortool.ActivityReport{OrganizationID: s.deps.Organization(), ObservedAt: now, Window: window, PopulationLimit: activitystore.PopulationLimit, Running: []operatortool.ActivityAttempt{}, Finished: []operatortool.ActivityAttempt{}, RunningIssues: []operatortool.ActivityIssue{}, FinishedIssues: []operatortool.ActivityIssue{}, TypicalDurations: []operatortool.ActivityTypicalDuration{}, IssueTypicalDurations: []operatortool.ActivityIssueTypical{}}
	filter := activitystore.Filter{RunnerID: request.RunnerID, From: window.From, To: window.To, Now: now, Limit: activitystore.IssuePopulationLimit}
	if credential.HostedRole == "reporter" {
		filter.Principal = caller.Scope.Actor.PrincipalID
		if filter.Principal == "" {
			return out, operatortool.ErrAccessDenied
		}
	}
	var snapshot activitystore.Snapshot
	read := func(r tenantstore.Reader) error {
		access, ok := r.(accessstore.ProjectAccessPageReader)
		if !ok {
			return s.deps.MissingParticipants
		}
		projects, err := access.ProjectAccessPage(ctx, hubauth.ProjectFilter(credential, tracker.ProjectID(request.ProjectID), "", 100))
		if err != nil {
			return err
		}
		if len(projects) > 100 {
			out.Partial = true
			out.IssuesPartial = true
			projects = projects[:100]
		}
		for _, project := range projects {
			if request.ProjectID != "" && request.ProjectID != string(project.ProjectID) {
				continue
			}
			if credential.EntryKey == nil && credential.HostedKeyKind == "service" {
				err := caller.Scope.ForProject(project.ProjectID).Authorize(ctx, r, false)
				if errors.Is(err, accessstore.ErrDenied) || caller.IsNotFound != nil && caller.IsNotFound(err) {
					continue
				}
				if err != nil {
					return err
				}
			}
			filter.Projects = append(filter.Projects, project.ProjectID)
		}
		if request.ProjectID != "" && len(filter.Projects) == 0 {
			return operatortool.ErrAccessDenied
		}
		reader, ok := r.(activitystore.Reader)
		if !ok {
			return errors.New("activity reader unavailable")
		}
		snapshot, err = reader.ActivitySnapshot(ctx, filter)
		return err
	}
	var err error
	if repository, ok := s.deps.Repository.(tenantstore.SnapshotRepository); ok {
		err = repository.ReadTenantSnapshot(ctx, tracker.OrganizationID(out.OrganizationID), corestore.Serving, read)
	} else {
		err = s.deps.Repository.ReadTenant(ctx, tracker.OrganizationID(out.OrganizationID), corestore.Serving, read)
	}
	if err != nil {
		return out, err
	}
	out.Partial = out.Partial || snapshot.Partial || snapshot.DurationsPartial
	out.IssuesPartial = out.IssuesPartial || snapshot.Partial || snapshot.IssuesPartial
	out.ChangeSequences = snapshot.Sequences
	events := []eventstore.Event{}
	for _, attempt := range snapshot.Attempts {
		if attempt.Event != nil {
			events = append(events, *attempt.Event)
		}
	}
	values, failures := reportdata.ReadObjects(events, func(event eventstore.Event) (tracker.CollaborationEvent, error) {
		return eventstore.NewContent(s.deps.Details()).Event(ctx, event)
	})
	runs := map[string]tracker.NativeRunData{}
	for index, value := range values {
		if err := failures[index]; err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, artifact.ErrMissing) {
				out.IssuesPartial = true
				continue
			}
			return out, fmt.Errorf("read activity run: %w", err)
		}
		if value.Data.Run != nil {
			runs[events[index].AttemptID] = *value.Data.Run
		}
	}
	factsByIssue := map[int64]activitystore.Issue{}
	factsByIssueByReference := map[[3]string]activitystore.Issue{}
	for _, fact := range snapshot.Issues {
		factsByIssue[fact.ID] = fact
		factsByIssueByReference[[3]string{out.OrganizationID, string(fact.ProjectID), string(fact.WorkItemID)}] = fact
	}
	attemptsByIssue := map[int64][]activityIssueAttempt{}
	missingAttemptTiming := map[int64]bool{}
	typical := map[string][]activityDurationSample{}
	timingPartial := map[string]bool{}
	for _, fact := range snapshot.Durations {
		project := string(fact.ProjectID)
		typical[project] = append(typical[project], activityDurationSample{ID: fact.AttemptID, Status: fact.Status, Role: fact.Role, Seconds: fact.Seconds, Partial: fact.Partial})
		timingPartial[project] = timingPartial[project] || fact.Partial
		out.Partial = out.Partial || fact.Partial
	}
	for _, fact := range snapshot.Attempts {
		run, hydrated := runs[fact.ID]
		missingAttemptTiming[fact.IssueID] = missingAttemptTiming[fact.IssueID] || !hydrated
		role := fact.Role
		if role == "" {
			role = RunRole(run)
		}
		runner, session := fact.RunnerID, fact.SessionID
		if runner == "" {
			runner = run.RunnerID
		}
		if run.SessionID != "" {
			session = run.SessionID
		}
		end := fact.FinishedAt
		if fact.Live {
			end = now
		}
		attempt := activityIssueAttempt{run: run, started: fact.StartedAt, ended: end, role: role, runner: runner, session: session, live: fact.Live}
		attemptsByIssue[fact.IssueID] = append(attemptsByIssue[fact.IssueID], attempt)
		if !fact.Selected {
			continue
		}
		row := operatortool.ActivityAttempt{WorkItemID: string(fact.WorkItemID), Number: fact.Number, Title: fact.Title, ProjectID: string(fact.ProjectID), ProjectName: fact.ProjectName, RunnerID: runner, AttemptID: fact.ID, Stage: role, StartedAt: fact.StartedAt, SessionID: session, WorkspaceIDs: append([]string{}, factsByIssue[fact.IssueID].WorkspaceIDs...), Partial: !hydrated}
		if run.Runtime != nil {
			row.Phase = run.Runtime.Phase
		}
		if run.Finalization != nil {
			row.ChangeID = run.Finalization.ChangeID
			if run.Finalization.Publication != nil {
				row.PullRequestURL = run.Finalization.Publication.External.URL
			}
		}
		if row.ChangeID == "" && run.Handoff != nil && run.Handoff.Change != nil {
			row.ChangeID = run.Handoff.Change.ChangeID
		}
		issue := factsByIssue[fact.IssueID]
		if row.ChangeID == "" {
			row.ChangeID = issue.ChangeID
		}
		if row.PullRequestURL == "" && row.ChangeID == issue.ChangeID {
			row.PullRequestURL = issue.PublicationURL
		}
		phases, dropped := activityRunPhases(run)
		row.Partial = row.Partial || dropped > 0
		activityStageTiming(&row, phases, now, fact.Live)
		if fact.Live {
			out.Running = append(out.Running, row)
		} else {
			if !fact.FinishedAt.Before(window.From) && fact.FinishedAt.Before(window.To) {
				row.Outcome = fact.Status
				if row.Outcome == "running" {
					row.Outcome = "interrupted"
				}
				finished := fact.FinishedAt
				row.FinishedAt = &finished
				out.Finished = append(out.Finished, row)
			}
		}
		out.Partial = out.Partial || row.Partial
	}
	transitions := map[int64][]activitystore.Transition{}
	for _, transition := range snapshot.Transitions {
		transitions[transition.IssueID] = append(transitions[transition.IssueID], transition)
	}
	totals := map[string][]float64{}
	laneValues := map[string]map[string][]float64{}
	for _, fact := range snapshot.Issues {
		history := transitions[fact.ID]
		slices.SortFunc(history, func(a, b activitystore.Transition) int { return cmp.Compare(a.Sequence, b.Sequence) })
		initial := fact.State
		if len(history) > 0 {
			initial = history[0].From
		}
		samples := []reportdata.ResidenceSample{{Sequence: 0, At: fact.CreatedAt, Created: fact.CreatedAt, Initial: initial, State: initial, Entered: fact.CreatedAt, Partial: fact.Partial || !snapshot.Created[fact.ID]}}
		for _, transition := range history {
			samples = append(samples, reportdata.ResidenceSample{Sequence: transition.Sequence, At: transition.At, State: transition.To, Entered: transition.At})
		}
		issue := activityIssueResidence(operatortool.ActivityIssue{WorkItemID: string(fact.WorkItemID), Number: fact.Number, Title: fact.Title, ProjectID: string(fact.ProjectID), ProjectName: fact.ProjectName, CurrentLane: fact.State, Partial: fact.Partial, ChangeID: fact.ChangeID, PullRequestURL: fact.PublicationURL}, snapshot.States[fact.ProjectID], samples, now)
		if issue.CurrentLane != fact.State {
			issue.CurrentLane = fact.State
			issue.Partial = true
		}
		live := activityIssueWork(&issue, attemptsByIssue[fact.ID], now)
		issue.Partial = issue.Partial || missingAttemptTiming[fact.ID]
		if request.RunnerID != "" && !slices.Contains(issue.RunnerIDs, request.RunnerID) {
			continue
		}
		finished := issue.FinishedAt != nil && !issue.FinishedAt.Before(window.From) && issue.FinishedAt.Before(window.To)
		if live {
			out.RunningIssues = append(out.RunningIssues, issue)
		} else if finished {
			out.FinishedIssues = append(out.FinishedIssues, issue)
		} else {
			continue
		}
		out.IssuesPartial = out.IssuesPartial || issue.Partial
		if finished {
			totals[issue.ProjectID] = append(totals[issue.ProjectID], issue.TotalSeconds)
			if laneValues[issue.ProjectID] == nil {
				laneValues[issue.ProjectID] = map[string][]float64{}
			}
			for _, lane := range issue.Lanes {
				if lane.Entries > 0 {
					laneValues[issue.ProjectID][lane.Name] = append(laneValues[issue.ProjectID][lane.Name], lane.Seconds)
				}
			}
		}
	}
	for project, samples := range typical {
		out.TypicalDurations = append(out.TypicalDurations, activityTypicalDurations(project, samples, snapshot.DurationsPartial || timingPartial[project])...)
	}
	for project, values := range totals {
		item := operatortool.ActivityIssueTypical{ProjectID: project, Total: reportdata.Duration(values), Lanes: map[string]operatortool.AnalyticsDuration{}, Partial: out.IssuesPartial || snapshot.Partial}
		for name, seconds := range laneValues[project] {
			item.Lanes[name] = reportdata.Duration(seconds)
		}
		out.IssueTypicalDurations = append(out.IssueTypicalDurations, item)
	}
	slices.SortFunc(out.Running, func(a, b operatortool.ActivityAttempt) int {
		return cmp.Or(a.StartedAt.Compare(b.StartedAt), cmp.Compare(a.AttemptID, b.AttemptID))
	})
	slices.SortFunc(out.Finished, func(a, b operatortool.ActivityAttempt) int {
		return cmp.Or(b.FinishedAt.Compare(*a.FinishedAt), cmp.Compare(a.AttemptID, b.AttemptID))
	})
	slices.SortFunc(out.RunningIssues, func(a, b operatortool.ActivityIssue) int {
		return cmp.Or(factsByIssueByReference[[3]string{out.OrganizationID, b.ProjectID, b.WorkItemID}].UpdatedAt.Compare(factsByIssueByReference[[3]string{out.OrganizationID, a.ProjectID, a.WorkItemID}].UpdatedAt), cmp.Compare(a.WorkItemID, b.WorkItemID))
	})
	slices.SortFunc(out.FinishedIssues, func(a, b operatortool.ActivityIssue) int {
		return cmp.Or(b.FinishedAt.Compare(*a.FinishedAt), cmp.Compare(a.WorkItemID, b.WorkItemID))
	})
	slices.SortFunc(out.TypicalDurations, func(a, b operatortool.ActivityTypicalDuration) int {
		return cmp.Or(cmp.Compare(a.ProjectID, b.ProjectID), cmp.Compare(a.Stage, b.Stage))
	})
	slices.SortFunc(out.IssueTypicalDurations, func(a, b operatortool.ActivityIssueTypical) int { return cmp.Compare(a.ProjectID, b.ProjectID) })
	if len(out.Finished) > request.Limit {
		out.Finished = out.Finished[:request.Limit]
		out.Partial = true
	}
	if len(out.FinishedIssues) > request.Limit {
		out.FinishedIssues = out.FinishedIssues[:request.Limit]
		out.IssuesPartial = true
	}
	for {
		raw, err := json.Marshal(out)
		if err != nil {
			return out, err
		}
		if len(raw) <= operatortool.MaxResultBytes {
			return out, nil
		}
		switch {
		case len(out.FinishedIssues) > 0:
			out.IssuesPartial = true
			out.FinishedIssues = out.FinishedIssues[:len(out.FinishedIssues)/2]
		case len(out.Finished) > 0:
			out.Partial = true
			out.Finished = out.Finished[:len(out.Finished)/2]
		case len(out.RunningIssues) > 0:
			out.IssuesPartial = true
			out.RunningIssues = out.RunningIssues[:len(out.RunningIssues)/2]
		case len(out.Running) > 0:
			out.Partial = true
			out.Running = out.Running[:len(out.Running)/2]
		case len(out.IssueTypicalDurations) > 0:
			out.IssuesPartial = true
			out.IssueTypicalDurations = out.IssueTypicalDurations[:len(out.IssueTypicalDurations)/2]
		case len(out.TypicalDurations) > 0:
			out.Partial = true
			out.TypicalDurations = out.TypicalDurations[:len(out.TypicalDurations)/2]
		default:
			return out, s.deps.Unavailable
		}
	}
}
