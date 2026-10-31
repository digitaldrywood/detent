package hubserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

type healthFindingsPage struct {
	ChangeSequence      int64                       `json:"change_sequence"`
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
			return s.nativeAPIError(c, nativePublicInvalid("limit must be between 1 and 100"))
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
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	page := healthFindingsPage{Items: []healthFinding{}, BaselineUnavailable: []healthBaselineUnavailable{}}
	if state == "" {
		state = "open"
	}
	if state != "open" && state != "resolved" && state != "all" {
		return page, nativePublicInvalid("state must be open, resolved or all")
	}
	if limit < 1 || limit > 100 {
		return page, nativePublicInvalid("limit must be between 1 and 100")
	}
	if since != "" {
		at, err := time.Parse(time.RFC3339Nano, since)
		if err != nil {
			return page, nativePublicInvalid("since must be an RFC3339 timestamp")
		}
		since = formatHubTime(at)
	}
	after := int64(0)
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return page, nativePublicInvalid("invalid findings cursor")
		}
		after, err = strconv.ParseInt(string(raw), 10, 64)
		if err != nil || after < 1 {
			return page, nativePublicInvalid("invalid findings cursor")
		}
	}
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	page.ChangeSequence, _, err = readDashboardWatermark(ctx, tx, scope)
	if err != nil {
		return page, err
	}
	if _, err := readNativeProject(ctx, tx, scope); err != nil {
		return page, err
	}
	var query nativeQueryer = tx
	if scope.credential.HostedRole == "reporter" {
		prepared, prepareErr := prepareNativePageQuery(ctx, tx, scope, "", "native_issue_actors")
		if prepareErr != nil {
			return page, prepareErr
		}
		query = prepared
	}
	resolved := state == "resolved"
	args := []any{scope.organization, state, resolved, scope.project, state, since, since, since, after}

	visibility := ""
	if scope.credential.HostedRole == "reporter" {
		condition, visibilityArgs := scope.issueVisibilitySQL("i")
		visibility = " AND EXISTS(SELECT 1 FROM health_finding_issues fi JOIN issues i ON i.native_id=fi.work_item_id WHERE fi.finding_id=health_findings.id AND i.organization_id=? AND i.project_id=? AND (" + condition + "))"
		args = append(args, scope.organization, scope.project)
		args = append(args, visibilityArgs...)
	}
	args = append(args, healthReadLimit+1)
	rows, err := query.QueryContext(ctx, `SELECT rowid,id,fingerprint,signal,class,subject_json,opened_at,last_seen_at,resolved_at,severity,summary,next_action,evidence_json,email_unavailable,slack_unavailable_at,slack_status_code,projects_json,projects_projected FROM health_findings
 WHERE organization_id=? AND (?='all' OR (resolved_at IS NOT NULL)=?) AND (projects_projected IS NULL OR EXISTS(SELECT 1 FROM health_finding_projects fp WHERE fp.finding_id=health_findings.id AND fp.project_id=?))
 AND ((?='all' AND resolved_at IS NULL) OR ?='' OR julianday(last_seen_at)>=julianday(?) OR julianday(resolved_at)>=julianday(?)) AND rowid>?`+visibility+` ORDER BY rowid LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	var last int64
	count := 0
	for rows.Next() {
		var f healthFinding
		var rowid int64
		var subject, opened, seen, evidence string
		var ended, slackFailed sql.NullString
		var slackStatus sql.NullInt64
		var projects string
		var projected sql.NullInt64
		if err := rows.Scan(&rowid, &f.ID, &f.Fingerprint, &f.Signal, &f.Class, &subject, &opened, &seen, &ended, &f.Severity, &f.Summary, &f.NextAction, &evidence, &f.EmailUnavailable, &slackFailed, &slackStatus, &projects, &projected); err != nil {
			return page, err
		}
		count++
		if count > healthReadLimit {
			return page, errHealthReadLimit
		}
		if !projected.Valid {
			var ids []string
			if err := json.Unmarshal([]byte(projects), &ids); err != nil {
				return page, err
			}
			if !slices.Contains(ids, string(scope.project)) {
				continue
			}
		}
		links, err := readHealthFindingLinks(ctx, query, scope, f.ID)
		if err != nil {
			return page, err
		}
		f.Issues = links
		if scope.credential.HostedRole == "reporter" && len(links) == 0 {
			continue
		}
		if len(links) > 0 {
			f.FiledBy = "health_detector"
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
	err = tx.QueryRowContext(ctx, "SELECT last_tick_at,baseline_unavailable_json FROM health_detector_ticks WHERE organization_id=?", scope.organization).Scan(&tick, &unavailable)
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
