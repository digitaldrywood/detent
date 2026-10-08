package hubserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthFindingsPage struct {
	BaselineUnavailable []healthBaselineUnavailable `json:"baseline_unavailable"`
	Items               []healthFinding             `json:"items"`
	NextCursor          string                      `json:"next_cursor,omitempty"`
	LastTickAt          *time.Time                  `json:"last_tick_at"`
}

func (s *Service) getHealthFindings(c echo.Context) error {
	limit := 100
	if raw := c.QueryParam("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return s.nativeAPIError(c, nativeInvalid("limit must be between 1 and 100"))
		}
		limit = value
	}
	page, err := s.readHealthFindings(c.Request().Context(), nativeRequestScope(c), c.QueryParam("state"), c.QueryParam("since"), c.QueryParam("cursor"), limit)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readHealthFindings(ctx context.Context, scope nativeScope, state, since, cursor string, limit int) (healthFindingsPage, error) {
	page := healthFindingsPage{Items: []healthFinding{}, BaselineUnavailable: []healthBaselineUnavailable{}}
	if state == "" {
		state = "open"
	}
	if state != "open" && state != "resolved" {
		return page, nativeInvalid("state must be open or resolved")
	}
	if limit < 1 || limit > 100 {
		return page, nativeInvalid("limit must be between 1 and 100")
	}
	if since != "" {
		at, err := time.Parse(time.RFC3339Nano, since)
		if err != nil {
			return page, nativeInvalid("since must be an RFC3339 timestamp")
		}
		since = formatHubTime(at)
	}
	after := int64(0)
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return page, nativeInvalid("invalid findings cursor")
		}
		after, err = strconv.ParseInt(string(raw), 10, 64)
		if err != nil || after < 1 {
			return page, nativeInvalid("invalid findings cursor")
		}
	}
	if _, err := readNativeProject(ctx, s.database.reader, scope); err != nil {
		return page, err
	}
	resolved := state == "resolved"
	rows, err := s.database.reader.QueryContext(ctx, `SELECT rowid,id,fingerprint,signal,class,subject_json,opened_at,last_seen_at,resolved_at,severity,summary,next_action,evidence_json,email_unavailable,slack_unavailable_at,slack_status_code,
 COALESCE((SELECT work_item_id FROM health_finding_issues WHERE finding_id=health_findings.id AND project_id=?), '') FROM health_findings
 WHERE organization_id=? AND (resolved_at IS NOT NULL)=? AND EXISTS(SELECT 1 FROM json_each(projects_json) WHERE value=?)
 AND (?='' OR julianday(last_seen_at)>=julianday(?) OR julianday(resolved_at)>=julianday(?)) AND rowid>? ORDER BY rowid LIMIT ?`, scope.project, scope.organization, resolved, scope.project, since, since, since, after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		var f healthFinding
		var rowid int64
		var subject, opened, seen, evidence string
		var ended, slackFailed sql.NullString
		var slackStatus sql.NullInt64
		var item tracker.NativeWorkItemID
		if err := rows.Scan(&rowid, &f.ID, &f.Fingerprint, &f.Signal, &f.Class, &subject, &opened, &seen, &ended, &f.Severity, &f.Summary, &f.NextAction, &evidence, &f.EmailUnavailable, &slackFailed, &slackStatus, &item); err != nil {
			return page, err
		}
		if item != "" {
			f.FiledBy = "health_detector"
			f.Issues = []healthFindingIssue{{ProjectID: scope.project, WorkItemID: item}}
		}
		if len(page.Items) == limit {
			page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(last, 10)))
			break
		}
		if err := json.Unmarshal([]byte(subject), &f.Subject); err != nil {
			return page, err
		}
		if err := json.Unmarshal([]byte(evidence), &f.Evidence); err != nil {
			return page, err
		}
		if f.OpenedAt, err = parseTimeValue(opened); err != nil {
			return page, err
		}
		if f.LastSeenAt, err = parseTimeValue(seen); err != nil {
			return page, err
		}
		if ended.Valid {
			at, err := parseTimeValue(ended.String)
			if err != nil {
				return page, err
			}
			f.ResolvedAt = &at
		}
		f.Evidence = scopeHealthEvidence(f.Evidence, scope.project)
		if slackFailed.Valid {
			at, err := parseTimeValue(slackFailed.String)
			if err != nil {
				return page, err
			}
			f.SlackUnavailable = &healthSlackFailure{Code: "slack_unavailable", StatusCode: int(slackStatus.Int64), At: at}
		}
		page.Items = append(page.Items, f)
		last = rowid
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return page, err
	}
	var tick, unavailable string
	err = s.database.reader.QueryRowContext(ctx, "SELECT last_tick_at,baseline_unavailable_json FROM health_detector_ticks WHERE organization_id=?", scope.organization).Scan(&tick, &unavailable)
	if errors.Is(err, sql.ErrNoRows) {
		return page, nil
	}
	if err != nil {
		return page, err
	}
	at, err := parseTimeValue(tick)
	if err != nil {
		return page, err
	}
	var gaps []healthBaselineUnavailable
	if err := json.Unmarshal([]byte(unavailable), &gaps); err != nil {
		return page, err
	}
	for _, gap := range gaps {
		if gap.Project == string(scope.project) {
			page.BaselineUnavailable = append(page.BaselineUnavailable, gap)
		}
	}
	page.LastTickAt = &at
	return page, nil
}
