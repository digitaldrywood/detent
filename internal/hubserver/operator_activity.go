package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) executeActivityRead(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	r, w, err := operatortool.DecodeActivity(call.Arguments, s.config.now().UTC())
	if err != nil {
		return operatortool.Result{}, err
	}
	ctx, err = operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: r.ProjectID})
	if err != nil {
		return operatortool.Result{}, err
	}
	credential, err := currentHubOperator(ctx)
	if err != nil {
		return operatortool.Result{}, operatortool.ErrAccessDenied
	}
	if s.database == nil || s.config.Hosted == nil {
		return operatortool.Result{}, errHubOperatorUnavailable
	}
	report, err := s.readActivityReport(ctx, credential, r, w)
	if err != nil {
		return operatortool.Result{}, safeWorkReadError(err)
	}
	return hubOperatorResult(report)
}

func (s *Service) hostedActivity(c echo.Context) error {
	r := operatortool.ActivityRequest{ProjectID: c.QueryParam("project_id"), RunnerID: c.QueryParam("runner_id"), From: c.QueryParam("from"), To: c.QueryParam("to")}
	if value := c.QueryParam("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > operatortool.MaxItemLimit {
			return s.nativeAPIError(c, nativeInvalid("Invalid activity limit"))
		}
		r.Limit = limit
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	result, err := s.executeActivityRead(c.Request().Context(), operatortool.Call{Name: operatortool.Activity, Arguments: raw})
	if errors.Is(err, operatortool.ErrInvalidArguments) {
		return s.nativeAPIError(c, nativeInvalid("Invalid activity filters or window"))
	}
	if errors.Is(err, operatortool.ErrAccessDenied) {
		return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "access_denied", Message: err.Error()})
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result.Content)
}

func (s *Service) readActivityReport(ctx context.Context, credential apiCredential, r operatortool.ActivityRequest, w operatortool.AnalyticsWindow) (operatortool.ActivityReport, error) {
	out := operatortool.ActivityReport{OrganizationID: s.config.Hosted.OrganizationID, ObservedAt: s.config.now().UTC(), Window: w, Running: []operatortool.ActivityAttempt{}, Finished: []operatortool.ActivityAttempt{}, TypicalDurations: []operatortool.ActivityTypicalDuration{}, PopulationLimit: maxAnalyticsPopulation}
	projects, err := s.usageReadableProjects(ctx, credential)
	if err != nil {
		return out, err
	}
	projects = slices.DeleteFunc(projects, func(id string) bool {
		if r.ProjectID != "" && r.ProjectID != id {
			return true
		}
		_, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: id})
		return err != nil
	})
	if r.ProjectID != "" && len(projects) == 0 {
		return out, operatortool.ErrAccessDenied
	}
	tx, err := s.database.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	for _, id := range projects {
		scope := nativeScope{organization: tracker.OrganizationID(out.OrganizationID), project: tracker.ProjectID(id), credential: credential}
		population, err := loadNativeAnalyticsAttempts(ctx, tx, scope, w, r.RunnerID, false)
		if err != nil {
			return out, err
		}
		out.Partial = out.Partial || population.Partial
		out.TypicalDurations = append(out.TypicalDurations, activityTypicalDurations(id, population.Items, population.Partial)...)
	}
	out.Running, err = readActivityAttempts(ctx, tx, out.OrganizationID, projects, r, w, out.ObservedAt, true)
	if err != nil {
		return out, err
	}
	out.Finished, err = readActivityAttempts(ctx, tx, out.OrganizationID, projects, r, w, out.ObservedAt, false)
	if err != nil {
		return out, err
	}
	for _, attempt := range append(slices.Clone(out.Running), out.Finished...) {
		out.Partial = out.Partial || attempt.Partial
	}
	return out, nil
}

func closedAttemptSeconds(phases []tracker.NativePhase) float64 {
	var seconds float64
	for _, phase := range phases {
		if !phase.FinishedAt.IsZero() {
			seconds += max(0, phase.FinishedAt.Sub(phase.StartedAt).Seconds())
		}
	}
	return seconds
}

func activityTypicalDurations(project string, attempts []nativeAnalyticsAttempt, partial bool) []operatortool.ActivityTypicalDuration {
	values := map[string][]float64{}
	for _, a := range attempts {
		if a.Identity.Role == "" || a.Status != "succeeded" && a.Status != "failed" {
			continue
		}
		if seconds := closedAttemptSeconds(a.Phases); seconds > 0 {
			values[a.Identity.Role] = append(values[a.Identity.Role], seconds)
		}
	}
	out := []operatortool.ActivityTypicalDuration{}
	for role, seconds := range values {
		out = append(out, operatortool.ActivityTypicalDuration{ProjectID: project, Stage: role, AnalyticsDuration: analyticsDuration(seconds), Partial: partial})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stage < out[j].Stage })
	return out
}

func activityStageTiming(a *operatortool.ActivityAttempt, phases []tracker.NativePhase, now time.Time, running bool) {
	if len(phases) > 0 {
		start := phases[len(phases)-1].StartedAt
		if !start.IsZero() && !start.After(now) {
			a.StageStartedAt = &start
			if running {
				seconds := now.Sub(start).Seconds()
				a.StageElapsedSeconds = &seconds
			}
		}
	}
	if !running {
		if seconds := closedAttemptSeconds(phases); seconds > 0 {
			a.StageDurationSeconds = &seconds
		}
	}
	if a.Stage == "" || a.StageStartedAt == nil || !running && a.StageDurationSeconds == nil {
		a.Partial = true
	}
}

func readActivityAttempts(ctx context.Context, q nativeQueryer, organization string, projects []string, r operatortool.ActivityRequest, w operatortool.AnalyticsWindow, now time.Time, running bool) ([]operatortool.ActivityAttempt, error) {
	encoded, err := json.Marshal(projects)
	if err != nil {
		return nil, err
	}
	predicate := "live=1"
	tail := " ORDER BY julianday(a.started_at),a.id"
	args := []any{formatHubTime(now), formatHubTime(now), organization, string(encoded), r.RunnerID, r.RunnerID}
	if !running {
		predicate = "live=0 AND julianday(finished_at)>=julianday(?) AND julianday(finished_at)<julianday(?)"
		tail = " ORDER BY julianday(a.finished_at) DESC,a.id DESC LIMIT ?"
		args = append(args, formatHubTime(w.From), formatHubTime(w.To), r.Limit)
	}
	rows, err := q.QueryContext(ctx, `WITH population AS (
 SELECT a.id,a.organization_id,a.project_id,a.work_item_id,a.status,a.started_at,a.updated_at,
 coalesce(lr.runner_id,json_extract(a.data_json,'$.runner_id'),'') AS runner_id,
 coalesce(nullif(json_extract(a.data_json,'$.runtime.identity.role'),''),json_extract(a.data_json,'$.identity.role'),'') AS stage,
 coalesce(json_extract(a.data_json,'$.runtime.phase'),'') AS phase,
 coalesce(json_extract(a.data_json,'$.runtime.phases'),'[]') AS phases,
 coalesce(json_extract(a.data_json,'$.runtime.phases_dropped'),0) AS dropped,
 coalesce(json_extract(a.data_json,'$.session_id'),l.session_id,'') AS session_id,
 coalesce(json_extract(a.data_json,'$.finalization.change_id'),json_extract(a.checkpoint_json,'$.change.change_id'),
 (SELECT c.id FROM change_requests c WHERE c.organization_id=a.organization_id AND c.project_id=a.project_id AND c.work_item_id=a.work_item_id ORDER BY julianday(json_extract(c.record_json,'$.created_at')) DESC,c.id DESC LIMIT 1),'') AS change_id,
 coalesce(json_extract(a.data_json,'$.finalization.publication.external.url'),'') AS publication_url,
 CASE WHEN a.status='running' AND l.released_at IS NULL AND julianday(l.renewed_at)<=julianday(?) AND julianday(l.expires_at)>julianday(?) THEN 1 ELSE 0 END AS live,
 CASE WHEN a.status!='running' THEN a.updated_at WHEN l.released_at IS NOT NULL AND julianday(l.released_at)<julianday(l.expires_at) THEN l.released_at ELSE l.expires_at END AS finished_at
 FROM native_attempts a JOIN leases l ON l.lease_id=a.lease_id LEFT JOIN lease_runners lr ON lr.lease_id=a.lease_id
 WHERE a.organization_id=? AND a.project_id IN (SELECT value FROM json_each(?))
 AND (?='' OR coalesce(lr.runner_id,json_extract(a.data_json,'$.runner_id'),'')=?)
)
SELECT a.id,a.work_item_id,i.number,i.title,a.project_id,p.name,a.runner_id,a.stage,a.phase,a.phases,a.dropped,a.started_at,a.session_id,a.change_id,
 coalesce(nullif(a.publication_url,''),(SELECT json_extract(v.record_json,'$.external.url') FROM change_requests c JOIN change_versions v ON v.id=json_extract(c.record_json,'$.current_version_id') AND v.change_id=c.id WHERE c.id=a.change_id AND c.organization_id=a.organization_id AND c.project_id=a.project_id),''),
 CASE WHEN a.status='running' THEN 'interrupted' ELSE a.status END,a.finished_at,
 (SELECT json_group_array(ws.id) FROM workspace_sessions ws WHERE ws.organization_id=a.organization_id AND ws.project_id=a.project_id AND ws.subject_work_item_id=a.work_item_id AND ws.attempt_id=a.id AND ws.state IN ('requested','starting','ready','idle'))
FROM population a JOIN issues i ON i.organization_id=a.organization_id AND i.project_id=a.project_id AND i.native_id=a.work_item_id
JOIN projects p ON p.organization_id=a.organization_id AND p.id=a.project_id WHERE `+predicate+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []operatortool.ActivityAttempt{}
	for rows.Next() {
		var a operatortool.ActivityAttempt
		var phasesRaw, started, finished, workspaceRaw string
		var dropped int
		if err := rows.Scan(&a.AttemptID, &a.WorkItemID, &a.Number, &a.Title, &a.ProjectID, &a.ProjectName, &a.RunnerID, &a.Stage, &a.Phase, &phasesRaw, &dropped, &started, &a.SessionID, &a.ChangeID, &a.PullRequestURL, &a.Outcome, &finished, &workspaceRaw); err != nil {
			return nil, err
		}
		a.StartedAt, err = parseTimeValue(started)
		if err != nil {
			return nil, err
		}
		var phases []tracker.NativePhase
		if json.Unmarshal([]byte(phasesRaw), &phases) != nil {
			a.Partial = true
		}
		if json.Unmarshal([]byte(workspaceRaw), &a.WorkspaceIDs) != nil {
			return nil, errHubOperatorUnavailable
		}
		a.Partial = a.Partial || dropped > 0
		if running {
			a.Outcome = ""
		} else {
			at, err := parseTimeValue(finished)
			if err != nil {
				return nil, err
			}
			a.FinishedAt = &at
		}
		activityStageTiming(&a, phases, now, running)
		out = append(out, a)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}
