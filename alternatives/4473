package web

import (
	"context"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

type projectReportData struct {
	ProjectID      string                          `json:"project_id"`
	Usage          []usageReportAPIResponse        `json:"usage"`
	Digest         templates.DailyDigestData       `json:"digest"`
	Efficiency     efficiency.Rollup               `json:"efficiency"`
	CostPerOutcome efficiency.CostPerOutcomeReport `json:"cost_per_outcome"`
	HasMore        bool                            `json:"has_more"`
	Unavailable    []string                        `json:"unavailable"`
}

type analyticsAttemptData struct {
	AttemptID               int64                     `json:"attempt_id"`
	AttemptNumber           int                       `json:"attempt_number"`
	ProjectID               string                    `json:"project_id"`
	IssueID                 string                    `json:"issue_id,omitempty"`
	Identifier              string                    `json:"identifier,omitempty"`
	URL                     string                    `json:"url,omitempty"`
	Status                  string                    `json:"status,omitempty"`
	TerminalState           string                    `json:"terminal_state,omitempty"`
	StartedAt               time.Time                 `json:"started_at"`
	CompletedAt             *time.Time                `json:"completed_at,omitempty"`
	HeartbeatAt             *time.Time                `json:"heartbeat_at,omitempty"`
	Phase                   string                    `json:"phase,omitempty"`
	StatusMessage           string                    `json:"status_message,omitempty"`
	StatusMessageTruncation *runtimeoutput.Truncation `json:"status_message_truncation,omitempty"`
	WaitReason              string                    `json:"wait_reason,omitempty"`
	ErrorClass              string                    `json:"error_class,omitempty"`
	ErrorMessage            string                    `json:"error_message,omitempty"`
	WorkerHost              string                    `json:"worker_host,omitempty"`
	Stale                   bool                      `json:"stale"`
}

func (s *Server) authorizedAnalyticsProjects(ctx context.Context, snapshot telemetry.Snapshot, selected string) ([]templates.ProjectSmallMultiple, error) {
	if credential, ok := apiCredentialFromContext(ctx); ok {
		snapshot = operatorScopedSnapshot(snapshot, credential.ProjectIDs)
	}
	snapshot, err := operatortool.ProjectSnapshot(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	projects := s.projectSmallMultiples(ctx, snapshot)
	projects = slices.DeleteFunc(slices.Clone(projects), func(p templates.ProjectSmallMultiple) bool {
		if selected != "" && p.ID != selected {
			return true
		}
		if p.ID == "" {
			return true
		}
		if !operatortool.ConnectionIdentity(ctx).Valid() {
			credential, ok := apiCredentialFromContext(ctx)
			return ok && !apikey.AllowsProject(credential.ProjectIDs, p.ID)
		}
		_, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: p.ID})
		return err != nil
	})
	return projects, nil
}

func (s *Server) readTimeSeries(ctx context.Context, selected string, window, bucket time.Duration, limit, offset int) (timeSeriesAPIResponse, []string, bool, error) {
	if s.hub == nil {
		return timeSeriesAPIResponse{}, nil, false, errOperatorCommandUnavailable
	}
	if _, available := s.hub.Latest(); !available {
		return timeSeriesAPIResponse{}, nil, false, errOperatorCommandUnavailable
	}
	snapshot := s.latestSnapshot(ctx)
	projects, err := s.authorizedAnalyticsProjects(ctx, snapshot, selected)
	if err != nil {
		return timeSeriesAPIResponse{}, nil, false, err
	}
	end := min(len(projects), offset+limit)
	page := projects[min(offset, len(projects)):end]
	ids := make([]string, 0, len(page))
	for _, p := range page {
		ids = append(ids, p.ID)
	}
	data := projectTimeSeriesResponse(page, selected, generatedAt(snapshot, s.now()), window, bucket)
	data.LastKnown, data.LastKnownUntil = snapshot.LastKnown, snapshot.LastKnownUntil
	return data, ids, end < len(projects), nil
}

func (s *Server) readReportGroups(ctx context.Context, ids []string, from, to time.Time, limit, offset int) ([]usageReportAPIResponse, bool, error) {
	groups := []usageReportAPIResponse{}
	more := false
	for _, by := range []store.UsageReportGroup{store.UsageReportByDay, store.UsageReportByProject, store.UsageReportByIssue, store.UsageReportByPR, store.UsageReportByModel} {
		report, err := s.readUsageReport(ctx, store.UsageReportQuery{By: by, From: from, To: to, ProjectIDs: ids})
		if err != nil {
			return nil, false, err
		}
		end := min(len(report.Rows), offset+limit)
		more = more || end < len(report.Rows)
		report.Rows = report.Rows[min(offset, len(report.Rows)):end]
		groups = append(groups, usageReportResponse(report, s.pricing))
	}
	return groups, more, nil
}

func (s *Server) readProjectReport(ctx context.Context, id string, snapshot telemetry.Snapshot, projects []templates.ProjectSmallMultiple, from, to time.Time, outcomeFrom, outcomeTo time.Time, bucket time.Duration, location *time.Location, limit, offset int) (projectReportData, error) {
	report := projectReportData{ProjectID: id, Unavailable: []string{}}
	if !from.Equal(from.UTC().Truncate(24*time.Hour)) || !to.Equal(to.UTC().Truncate(24*time.Hour)) {
		report.Unavailable = append(report.Unavailable, "usage_subday_attribution")
	}
	ids := []string{id}
	var err error
	report.Usage, report.HasMore, err = s.readReportGroups(ctx, ids, from, to.Add(-time.Nanosecond), limit, offset)
	if err != nil {
		return report, err
	}
	scoped := projectScopedSnapshotForProject(snapshot, telemetry.Project{ID: id})
	report.Digest, err = s.dailyDigestDataForWindows(ctx, scoped, projects, location, reportDigestWindows(from, to, location, ids))
	if err != nil {
		return report, err
	}
	report.Efficiency, report.CostPerOutcome, err = s.readReportOutcomes(ctx, id, from, to, outcomeFrom, outcomeTo, bucket)
	return report, err
}

func (s *Server) executeAnalyticsRead(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	now := s.now().UTC()
	r, w, err := operatortool.DecodeAnalytics(call.Name, call.Arguments, now)
	if err != nil {
		return operatortool.Result{}, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: r.ProjectID})
	if err != nil {
		return operatortool.Result{}, err
	}
	if s.hub == nil || call.Name == operatortool.Reports && s.store == nil {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	if call.Name == operatortool.TimeSeries {
		data, ids, more, err := s.readTimeSeries(ctx, r.ProjectID, w.To.Sub(w.From), w.Bucket, r.Limit, r.Offset)
		if err != nil {
			return operatortool.Result{}, err
		}
		duration := w.To.Sub(w.From)
		w.To = data.GeneratedAt
		w.From = w.To.Add(-duration)
		return operatorResult(struct {
			Series         timeSeriesAPIResponse        `json:"series"`
			ProjectIDs     []string                     `json:"project_ids"`
			Window         operatortool.AnalyticsWindow `json:"window"`
			ObservedAt     time.Time                    `json:"observed_at"`
			HasMore        bool                         `json:"has_more"`
			Source         string                       `json:"source"`
			LastKnown      bool                         `json:"snapshot_last_known"`
			LastKnownUntil time.Time                    `json:"snapshot_last_known_until,omitzero"`
		}{data, ids, w, now, more, "dashboard_project_samples", data.LastKnown, data.LastKnownUntil})
	}
	rawSnapshot := s.latestSnapshot(ctx)
	snapshot, err := operatortool.ProjectSnapshot(ctx, rawSnapshot)
	if err != nil {
		return operatortool.Result{}, err
	}
	if r.ProjectID != "" {
		snapshot = projectScopedSnapshotForProject(snapshot, telemetry.Project{ID: r.ProjectID})
	}
	if call.Name == operatortool.AnalyticsDashboard {
		if s.hub == nil {
			return operatortool.Result{}, errOperatorCommandUnavailable
		}
		if _, available := s.hub.Latest(); !available {
			return operatortool.Result{}, errOperatorCommandUnavailable
		}
		snapshot = s.analyticsDashboardData(ctx, snapshot).Snapshot
		visibleAttempts := []analyticsAttemptData{}
		for _, attempt := range rawSnapshot.WorkAttempts {
			if attempt.ProjectID == "" || r.ProjectID != "" && attempt.ProjectID != r.ProjectID {
				continue
			}
			if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: attempt.ProjectID}); err == nil {
				visibleAttempts = append(visibleAttempts, analyticsAttemptData{AttemptID: attempt.AttemptID, AttemptNumber: attempt.AttemptNumber, ProjectID: attempt.ProjectID, IssueID: attempt.IssueID, Identifier: attempt.Identifier, URL: attempt.IssueURL, Status: attempt.Status, TerminalState: attempt.TerminalState, StartedAt: attempt.StartedAt, CompletedAt: attempt.CompletedAt, HeartbeatAt: attempt.HeartbeatAt, Phase: attempt.Phase, StatusMessage: attempt.StatusMessage, StatusMessageTruncation: attempt.StatusMessageTruncation, WaitReason: attempt.WaitReason, ErrorClass: attempt.ErrorClass, ErrorMessage: attempt.ErrorMessage, WorkerHost: attempt.WorkerHost, Stale: attempt.Stale})
			}
		}
		unavailable := []string{}
		if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ResourceKind: "instance"}); err != nil || r.ProjectID != "" {
			snapshot.Events = nil
			unavailable = append(unavailable, "unattributed_activity")
		}
		attempts := operatortool.OffsetPage(visibleAttempts, r.Offset, r.Limit)
		events := operatortool.OffsetPage(snapshot.Events, r.Offset, r.Limit)
		return operatorResult(struct {
			Attempts       operatortool.ReadPage[analyticsAttemptData]    `json:"attempts"`
			Activity       operatortool.ReadPage[telemetry.ActivityEvent] `json:"activity"`
			GeneratedAt    time.Time                                      `json:"generated_at"`
			ObservedAt     time.Time                                      `json:"observed_at"`
			Source         string                                         `json:"source"`
			Unavailable    []string                                       `json:"unavailable"`
			LastKnown      bool                                           `json:"snapshot_last_known"`
			LastKnownUntil time.Time                                      `json:"snapshot_last_known_until,omitzero"`
		}{attempts, events, snapshot.GeneratedAt, now, "dashboard_snapshot", unavailable, rawSnapshot.LastKnown, rawSnapshot.LastKnownUntil})
	}
	if s.store == nil {
		return operatortool.Result{}, errOperatorCommandUnavailable
	}
	projects, err := s.authorizedAnalyticsProjects(ctx, snapshot, r.ProjectID)
	if err != nil {
		return operatortool.Result{}, err
	}
	end := min(len(projects), r.Offset+r.Limit)
	location := time.UTC
	if r.Timezone != "" {
		location, err = time.LoadLocation(r.Timezone)
		if err != nil {
			return operatortool.Result{}, operatortool.ErrInvalidArguments
		}
	}
	reports := []projectReportData{}
	for _, p := range projects[min(r.Offset, len(projects)):end] {
		report, err := s.readProjectReport(ctx, p.ID, snapshot, projects, w.From, w.To, w.From, w.To, w.Bucket, location, r.Limit, r.RowOffset)
		if err != nil {
			return operatortool.Result{}, errOperatorCommandUnavailable
		}
		reports = append(reports, report)
	}
	return operatorResult(struct {
		Projects       []projectReportData          `json:"projects"`
		Window         operatortool.AnalyticsWindow `json:"window"`
		GeneratedAt    time.Time                    `json:"generated_at"`
		ObservedAt     time.Time                    `json:"observed_at"`
		HasMore        bool                         `json:"has_more"`
		Source         string                       `json:"source"`
		LastKnown      bool                         `json:"snapshot_last_known"`
		LastKnownUntil time.Time                    `json:"snapshot_last_known_until,omitzero"`
	}{reports, w, snapshot.GeneratedAt, now, end < len(projects), "runtime_store", rawSnapshot.LastKnown, rawSnapshot.LastKnownUntil})
}

func reportDigestWindows(from, to time.Time, location *time.Location, ids []string) []store.DailyDigestWindow {
	local := from.In(location)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	windows := []store.DailyDigestWindow{}
	for start.Before(to) {
		end := start.AddDate(0, 0, 1)
		windows = append(windows, store.DailyDigestWindow{Date: start.Format(time.DateOnly), From: maxReportTime(start, from), To: minReportTime(end, to), ProjectIDs: ids})
		start = end
	}
	return windows
}
func maxReportTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func minReportTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (s *Server) readReportOutcomes(ctx context.Context, id string, from, to, outcomeFrom, outcomeTo time.Time, bucket time.Duration) (efficiency.Rollup, efficiency.CostPerOutcomeReport, error) {
	rollup, err := s.store.EfficiencyRollup(ctx, efficiency.Query{ProjectID: id, From: from, To: to})
	if err != nil {
		return rollup, efficiency.CostPerOutcomeReport{}, err
	}
	outcomes, err := s.store.CostPerOutcome(ctx, efficiency.CostPerOutcomeQuery{ProjectID: id, From: outcomeFrom, To: outcomeTo, Bucket: bucket})
	return rollup, outcomes, err
}
