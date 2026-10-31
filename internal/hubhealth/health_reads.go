package hubhealth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/hubsql"
)

type FindingsPage struct {
	ChangeSequence      int64                 `json:"change_sequence"`
	BaselineUnavailable []BaselineUnavailable `json:"baseline_unavailable"`
	Items               []Finding             `json:"items"`
	NextCursor          string                `json:"next_cursor,omitempty"`
	LastTickAt          *time.Time            `json:"last_tick_at"`
}

func (s *Store) ReadFindings(ctx context.Context, scope Scope, state, since, cursor string, limit int) (FindingsPage, error) {
	ctx, cancel := context.WithTimeout(ctx, s.hooks.ReadTimeout)
	defer cancel()
	page := FindingsPage{Items: []Finding{}, BaselineUnavailable: []BaselineUnavailable{}}
	if state == "" {
		state = "open"
	}
	if state != "open" && state != "resolved" && state != "all" {
		return page, s.hooks.Invalid("state must be open, resolved or all")
	}
	if limit < 1 || limit > 100 {
		return page, s.hooks.Invalid("limit must be between 1 and 100")
	}
	if since != "" {
		at, err := time.Parse(time.RFC3339Nano, since)
		if err != nil {
			return page, s.hooks.Invalid("since must be an RFC3339 timestamp")
		}
		since = formatHubTime(at)
	}
	after := int64(0)
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return page, s.hooks.Invalid("invalid findings cursor")
		}
		after, err = strconv.ParseInt(string(raw), 10, 64)
		if err != nil || after < 1 {
			return page, s.hooks.Invalid("invalid findings cursor")
		}
	}
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	page.ChangeSequence, _, err = s.hooks.Watermark(ctx, tx)
	if err != nil {
		return page, err
	}
	if err := s.hooks.ReadProject(ctx, tx); err != nil {
		return page, err
	}
	var query hubsql.Queryer = tx
	if scope.Reporter {
		prepared, prepareErr := s.hooks.PrepareQuery(ctx, tx)
		if prepareErr != nil {
			return page, prepareErr
		}
		query = prepared
	}
	resolved := state == "resolved"
	args := []any{scope.Organization, state, resolved, scope.Project, state, since, since, since, after}

	visibility := ""
	if scope.Reporter {
		condition, visibilityArgs := s.hooks.Visibility()
		visibility = " AND EXISTS(SELECT 1 FROM health_finding_issues fi JOIN issues i ON i.native_id=fi.work_item_id WHERE fi.finding_id=health_findings.id AND i.organization_id=? AND i.project_id=? AND (" + condition + "))"
		args = append(args, scope.Organization, scope.Project)
		args = append(args, visibilityArgs...)
	}
	args = append(args, ReadLimit+1)
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
		var f Finding
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
		if count > ReadLimit {
			return page, ErrReadLimit
		}
		if !projected.Valid {
			var ids []string
			if err := json.Unmarshal([]byte(projects), &ids); err != nil {
				return page, err
			}
			if !slices.Contains(ids, string(scope.Project)) {
				continue
			}
		}
		links, err := ReadFindingLinks(ctx, query, scope, f.ID)
		if err != nil {
			return page, err
		}
		f.Issues = links
		if scope.Reporter && len(links) == 0 {
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
		f.Evidence = scopeEvidence(f.Evidence, scope.Project)
		if slackFailed.Valid {
			at, err := parseTimeValue(slackFailed.String)
			if err != nil {
				return page, err
			}
			f.SlackUnavailable = &SlackFailure{Code: "slack_unavailable", StatusCode: int(slackStatus.Int64), At: at}
		}
		page.Items = append(page.Items, f)
		last = rowid
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return page, err
	}
	var tick, unavailable string
	err = tx.QueryRowContext(ctx, "SELECT last_tick_at,baseline_unavailable_json FROM health_detector_ticks WHERE organization_id=?", scope.Organization).Scan(&tick, &unavailable)
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
	var gaps []BaselineUnavailable
	if err := json.Unmarshal([]byte(unavailable), &gaps); err != nil {
		return page, err
	}
	for _, gap := range gaps {
		if gap.Project == string(scope.Project) {
			page.BaselineUnavailable = append(page.BaselineUnavailable, gap)
		}
	}
	page.LastTickAt = &at
	return page, nil
}
