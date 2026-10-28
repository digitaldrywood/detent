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
	"strings"
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
			return s.nativeAPIError(c, nativePublicInvalid("Invalid activity limit"))
		}
		r.Limit = limit
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	result, err := s.executeActivityRead(c.Request().Context(), operatortool.Call{Name: operatortool.Activity, Arguments: raw})
	if errors.Is(err, operatortool.ErrInvalidArguments) {
		return s.nativeAPIError(c, nativePublicInvalid("Invalid activity filters or window"))
	}
	if errors.Is(err, operatortool.ErrAccessDenied) {
		return s.refusedAPIError(c, http.StatusForbidden, "access_denied", operatortool.ErrAccessDenied.Error(), apiErrorResponse{Code: "access_denied", Message: operatortool.ErrAccessDenied.Error()})
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result.Content)
}

func (s *Service) readActivityReport(ctx context.Context, credential apiCredential, r operatortool.ActivityRequest, w operatortool.AnalyticsWindow) (operatortool.ActivityReport, error) {
	ctx, reporterErr := prepareReporterReadContext(ctx, s.database.reader, nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), credential: credential})
	if reporterErr != nil {
		return operatortool.ActivityReport{}, reporterErr
	}
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
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
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	out.ChangeSequences = map[string]int64{}
	for _, id := range projects {
		scope := nativeScope{organization: tracker.OrganizationID(out.OrganizationID), project: tracker.ProjectID(id)}
		head, _, err := readDashboardWatermark(ctx, tx, scope)
		if err != nil {
			return out, err
		}
		out.ChangeSequences[id] = head
		typical, partial, err := s.readActivityTypicalDurations(ctx, tx, scope, w, r.RunnerID)
		if err != nil {
			return out, err
		}
		out.Partial = out.Partial || partial
		out.TypicalDurations = append(out.TypicalDurations, typical...)
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
	if err := readActivityIssues(ctx, tx, projects, r, &out); err != nil {
		return out, err
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

func activityTypicalDurations(project string, attempts []activityDurationSample, partial bool) []operatortool.ActivityTypicalDuration {
	values := map[string][]float64{}
	for _, a := range attempts {
		if a.Role == "" || a.Status != "succeeded" && a.Status != "failed" {
			continue
		}
		if a.Seconds > 0 {
			values[a.Role] = append(values[a.Role], a.Seconds)
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
	out := []operatortool.ActivityAttempt{}
	offset := 0
	unknown := 0
	for {
		statement, args, err := activityAttemptsQuery(organization, projects, r, w, now, running)
		if err != nil {
			return nil, err
		}
		if visibility, ok := reportsScopeFrom(ctx); ok && visibility.reporterPrincipal != "" {
			statement = strings.ReplaceAll(statement, "n.organization_id=:org", "n.organization_id=:org AND n.work_item_id IN (SELECT ri.native_id FROM issues ri WHERE ri.organization_id=:org AND (ri.activity_actor_principal_id=:reporter OR ri.activity_actor_principal_id IS NULL))")
			args = append(args, sql.Named("reporter", visibility.reporterPrincipal))
		}
		if !running {
			statement = strings.Replace(statement, "LIMIT :limit", "LIMIT :limit OFFSET :offset", 1)
			args = append(args, sql.Named("offset", offset))
		}
		candidates, err := readActivityAttemptCandidates(ctx, q, statement, args)
		if err != nil {
			return nil, err
		}
		for _, c := range candidates {
			a := c.attempt
			visibility, scoped := reportsScopeFrom(ctx)
			if r.RunnerID != "" && !c.ready.Valid || scoped && visibility.reporterPrincipal != "" && !c.actor.Valid {
				if unknown == changeLegacyReadLimit {
					return nil, errChangeColumnsIncomplete
				}
				unknown++
			}
			if visibility, ok := reportsScopeFrom(ctx); ok && visibility.reporterPrincipal != "" {
				principal := c.actor.String
				if !c.actor.Valid {
					var actor tracker.Actor
					if err := json.Unmarshal([]byte(c.actorRaw.String), &actor); err != nil {
						return nil, err
					}
					principal = actor.PrincipalID
				}
				if principal == "" || principal != visibility.reporterPrincipal {
					continue
				}
			}
			var phases []tracker.NativePhase
			var err error
			if !c.ready.Valid {
				var data tracker.NativeRunData
				if err := decodeActivityRun(ctx, q, c.raw, &data); err != nil {
					return nil, err
				}
				if data.RunnerID != "" && a.RunnerID == "" {
					a.RunnerID = data.RunnerID
				}
				a.Stage = activityRunRole(data)
				phases, c.dropped = activityRunPhases(data)
				if data.Runtime != nil {
					a.Phase = data.Runtime.Phase
				}
				if data.SessionID != "" {
					a.SessionID = data.SessionID
				}
				if data.Finalization != nil {
					a.ChangeID = data.Finalization.ChangeID
					if data.Finalization.Publication != nil {
						a.PullRequestURL = data.Finalization.Publication.External.URL
					}
				}
				if a.ChangeID == "" && c.checkpoint.Valid {
					cp, err := decodeRunCheckpoint(ctx, q, c.checkpoint.String)
					if err != nil {
						return nil, err
					}
					if cp != nil && cp.Change != nil {
						a.ChangeID = cp.Change.ChangeID
					}
				}
			} else {
				if !c.publicationURL.Valid {
					var data tracker.NativeRunData
					if err := decodeActivityRun(ctx, q, c.raw, &data); err != nil {
						return nil, err
					}
					if data.Finalization != nil && data.Finalization.Publication != nil {
						a.PullRequestURL = data.Finalization.Publication.External.URL
					}
				}
				phases, err = readActivityPhases(ctx, q, a.AttemptID)
				if err != nil {
					return nil, err
				}
			}
			if r.RunnerID != "" && a.RunnerID != r.RunnerID {
				continue
			}
			a.StartedAt, err = parseTimeValue(c.started)
			if err != nil {
				return nil, err
			}
			if running {
				a.Outcome = ""
			} else {
				at, err := parseTimeValue(c.finished)
				if err != nil {
					return nil, err
				}
				a.FinishedAt = &at
			}
			if err := readActivityChangeLink(ctx, q, organization, &a); err != nil {
				return nil, err
			}
			a.WorkspaceIDs, err = readActivityWorkspaces(ctx, q, organization, a)
			if err != nil {
				return nil, err
			}
			a.Partial = c.dropped > 0
			activityStageTiming(&a, phases, now, running)
			out = append(out, a)
			if !running && len(out) == r.Limit {
				return out, nil
			}
		}
		if running || len(candidates) < r.Limit {
			return out, nil
		}
		offset += len(candidates)
		if offset >= maxAnalyticsPopulation {
			return nil, errHubOperatorUnavailable
		}
	}
}

func activityAttemptsQuery(organization string, projects []string, r operatortool.ActivityRequest, w operatortool.AnalyticsWindow, now time.Time, running bool) (string, []any, error) {

	args := []any{sql.Named("org", organization), sql.Named("runner", r.RunnerID), sql.Named("now", formatHubTime(now)), sql.Named("from", formatHubTime(w.From)), sql.Named("to", formatHubTime(w.To)), sql.Named("limit", r.Limit)}
	var projectArgs []string
	for i, project := range projects {
		name := "project" + strconv.Itoa(i)
		projectArgs = append(projectArgs, ":"+name)
		args = append(args, sql.Named(name, project))
	}
	scope := `n.organization_id=:org AND n.project_id IN (` + strings.Join(projectArgs, ",") + `)`
	runner := `(:runner='' OR (n.activity_projection_version IS NULL OR coalesce(lr.runner_id,n.runner_id,'')=:runner))`
	leases := ` JOIN leases l ON l.lease_id=n.lease_id LEFT JOIN lease_runners lr ON lr.lease_id=n.lease_id `
	live := `l.released_at IS NULL AND julianday(l.renewed_at)<=julianday(:now) AND julianday(l.expires_at)>julianday(:now)`
	finish := `CASE WHEN l.released_at IS NOT NULL AND julianday(l.released_at)<julianday(l.expires_at) THEN l.released_at ELSE l.expires_at END`
	candidates := `SELECT n.id,n.activity_started_at AS sort_at,n.started_at AS finished_at FROM native_attempts n` + leases + `WHERE ` + scope + ` AND n.status='running' AND ` + live + ` AND ` + runner
	order := ` ORDER BY sort_at,id`
	if !running {
		candidates = `SELECT n.id,n.activity_updated_at AS sort_at,n.finished_at FROM native_attempts n LEFT JOIN lease_runners lr ON lr.lease_id=n.lease_id WHERE ` + scope + ` AND n.status!='running' AND n.activity_updated_at>=julianday(:from) AND (n.activity_updated_at,n.id)<(julianday(:to),'') AND ` + runner + `
UNION ALL SELECT n.id,julianday(` + finish + `) AS sort_at,` + finish + ` AS finished_at FROM native_attempts n` + leases + `WHERE ` + scope + ` AND n.status='running' AND NOT (` + live + `) AND julianday(` + finish + `)>=julianday(:from) AND julianday(` + finish + `)<julianday(:to) AND ` + runner
		order = ` ORDER BY sort_at DESC,id DESC LIMIT :limit`
	}
	statement := `WITH selected AS MATERIALIZED (` + candidates + order + `),
linked AS MATERIALIZED (
 SELECT a.*,selected.finished_at AS effective_finished_at,selected.sort_at,
 coalesce(lr.runner_id,CASE WHEN a.activity_projection_version IS NOT NULL THEN a.runner_id END,'') AS resolved_runner_id,
 CASE WHEN a.activity_projection_version IS NOT NULL THEN coalesce(nullif(a.finalization_change_id,''),a.activity_checkpoint_change_id,'') ELSE '' END AS change_id
 FROM selected JOIN native_attempts a ON a.id=selected.id LEFT JOIN lease_runners lr ON lr.lease_id=a.lease_id
)
SELECT a.id,a.work_item_id,i.number,i.title,a.project_id,p.name,a.resolved_runner_id,
 coalesce(nullif(a.role,''),a.execution_role,''),CASE WHEN a.activity_projection_version IS NOT NULL THEN coalesce(a.runtime_phase,'') ELSE '' END,coalesce(a.activity_phases_dropped,0),a.started_at,
 coalesce(CASE WHEN a.activity_projection_version IS NOT NULL THEN nullif(a.session_id,'') END,l.session_id,''),a.change_id,CASE WHEN a.activity_projection_version IS NOT NULL THEN a.activity_publication_url END,
 CASE WHEN a.status='running' THEN 'interrupted' ELSE a.status END,a.effective_finished_at,
 CASE WHEN a.activity_projection_version IS NULL OR a.activity_publication_url IS NULL THEN a.data_json ELSE '' END,CASE WHEN a.activity_projection_version IS NULL THEN a.checkpoint_json END,a.activity_projection_version,i.activity_actor_principal_id,CASE WHEN i.activity_actor_principal_id IS NULL THEN i.actor_json ELSE '' END
FROM linked a JOIN leases l ON l.lease_id=a.lease_id
JOIN issues i ON i.organization_id=a.organization_id AND i.project_id=a.project_id AND i.native_id=a.work_item_id
JOIN projects p ON p.organization_id=a.organization_id AND p.id=a.project_id`
	if running {
		statement += ` ORDER BY a.sort_at,a.id`
	} else {
		statement += ` ORDER BY a.sort_at DESC,a.id DESC`
	}
	return statement, args, nil
}

type activityAttemptCandidate struct {
	attempt                operatortool.ActivityAttempt
	ready                  sql.NullInt64
	started, finished, raw string
	checkpoint             sql.NullString
	publicationURL         sql.NullString
	actor, actorRaw        sql.NullString
	dropped                int
}

func readActivityAttemptCandidates(ctx context.Context, q nativeQueryer, statement string, args []any) ([]activityAttemptCandidate, error) {
	rows, err := q.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}

	defer rows.Close()
	var candidates []activityAttemptCandidate
	for rows.Next() {
		var c activityAttemptCandidate
		a := &c.attempt
		if err := rows.Scan(&a.AttemptID, &a.WorkItemID, &a.Number, &a.Title, &a.ProjectID, &a.ProjectName, &a.RunnerID, &a.Stage, &a.Phase, &c.dropped, &c.started, &a.SessionID, &a.ChangeID, &c.publicationURL, &a.Outcome, &c.finished, &c.raw, &c.checkpoint, &c.ready, &c.actor, &c.actorRaw); err != nil {
			rows.Close()
			return nil, err
		}
		a.PullRequestURL = c.publicationURL.String
		candidates = append(candidates, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return candidates, nil
}
