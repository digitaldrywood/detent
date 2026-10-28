package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const teamUnreadPageSize = 50
const teamUnreadJobsPerTick = 4

type teamUnreadSummary struct {
	Count   *int64 `json:"count"`
	Pending bool   `json:"pending"`
}

func (s *Service) registerTeamUnreadRoute(e *echo.Echo) {
	e.GET("/api/v2/organizations/:organization/threads/unread", func(c echo.Context) error {
		ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
		defer cancel()
		ctx, scope, err := s.teamThreadAuthority(ctx, "list_threads", apikey.ScopeRead)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		projects := slices.Clone(c.QueryParams()["project_id"])
		if len(projects) > 100 || c.QueryParam("refresh") != "" && c.QueryParam("refresh") != "true" {
			return s.nativeAPIError(c, threadInvalid())
		}
		slices.Sort(projects)
		projects = slices.Compact(projects)
		for _, project := range projects {
			if project == "" || len(project) > 256 {
				return s.nativeAPIError(c, threadInvalid())
			}
			if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, Action: "list_threads", ProjectID: project}); err != nil {
				return s.nativeAPIError(c, err)
			}
			check := scope
			check.project = tracker.ProjectID(project)
			if err := s.requireHostedProject(ctx, s.database.reader, check, false); err != nil {
				return s.nativeAPIError(c, err)
			}
			condition, args := scope.credential.projectGrantSQL("p.organization_id", "p.id")
			args = append([]any{scope.organization, project}, args...)
			var allowed int
			if err := s.database.reader.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM projects p WHERE p.organization_id=? AND p.id=? AND ("+condition+"))", args...).Scan(&allowed); err != nil {
				return s.nativeAPIError(c, fmt.Errorf("authorize unread project: %w", err))
			}
			if allowed == 0 {
				return s.nativeAPIError(c, nativeNotFound())
			}
		}
		result, err := s.requestTeamUnread(ctx, scope, projects, c.QueryParam("refresh") == "true")
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if result.Pending {
			select {
			case s.boardRefresh <- struct{}{}:
			default:
			}
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, result)
	}, s.operatorAuthority)
}

func (s *Service) requestTeamUnread(ctx context.Context, scope nativeScope, projects []string, refresh bool) (teamUnreadSummary, error) {
	var result teamUnreadSummary
	principal := operatorIdentity(scope.credential, string(scope.organization)).PrincipalID
	id, err := nativeCommandHash(struct {
		Organization, Principal string
	}{string(scope.organization), principal})
	if err != nil {
		return result, err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("begin unread request: %w", err)
	}
	defer tx.Rollback()
	if err := s.recheckHostedAuthority(ctx, tx, &scope, apikey.ScopeRead); err != nil {
		return result, err
	}
	reporter := ""
	if scope.credential.HostedRole == "reporter" {
		reporter = scope.actor().PrincipalID
		if reporter == "" {
			return result, operatortool.ErrAccessDenied
		}
	}
	scopeHash, err := nativeCommandHash(struct {
		Projects []string
		Reporter string
	}{projects, reporter})
	if err != nil {
		return result, err
	}
	created, err := tx.ExecContext(ctx, "INSERT INTO team_thread_unread_summaries(id,organization_id,principal_id,scope_hash,reporter_principal,turn) VALUES(?,?,?,?,?,(SELECT COALESCE(MAX(turn),0)+1 FROM team_thread_unread_summaries)) ON CONFLICT(organization_id,principal_id) DO NOTHING", id, scope.organization, principal, scopeHash, reporter)
	if err != nil {
		return result, fmt.Errorf("request unread summary: %w", err)
	}
	n, err := created.RowsAffected()
	if err != nil {
		return result, fmt.Errorf("read unread request result: %w", err)
	}
	changed, err := tx.ExecContext(ctx, "UPDATE team_thread_unread_summaries SET scope_hash=?,reporter_principal=?,requested_revision=requested_revision+1,scan_revision=0,after_thread='',through_thread='',partial_count=0,unread_count=NULL WHERE id=? AND scope_hash<>?", scopeHash, reporter, id, scopeHash)
	if err != nil {
		return result, fmt.Errorf("replace unread scope: %w", err)
	}
	replaced, err := changed.RowsAffected()
	if err != nil {
		return result, fmt.Errorf("read unread scope result: %w", err)
	}
	if n == 1 || replaced == 1 {
		if _, err := tx.ExecContext(ctx, "DELETE FROM team_thread_unread_projects WHERE summary_id=?", id); err != nil {
			return result, fmt.Errorf("clear unread scope: %w", err)
		}
		for _, project := range projects {
			if _, err := tx.ExecContext(ctx, "INSERT INTO team_thread_unread_projects(summary_id,project_id) VALUES(?,?)", id, project); err != nil {
				return result, fmt.Errorf("store unread scope: %w", err)
			}
		}
	} else if refresh {
		if _, err := tx.ExecContext(ctx, "UPDATE team_thread_unread_summaries SET requested_revision=MAX(requested_revision,CASE WHEN scan_revision=0 THEN completed_revision+1 ELSE scan_revision+1 END) WHERE id=?", id); err != nil {
			return result, fmt.Errorf("request unread refresh: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit unread request: %w", err)
	}
	var count sql.NullInt64
	if err := s.database.reader.QueryRowContext(ctx, "SELECT unread_count,requested_revision>completed_revision FROM team_thread_unread_summaries WHERE id=? AND scope_hash=?", id, scopeHash).Scan(&count, &result.Pending); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			result.Pending = true
			return result, nil
		}
		return result, fmt.Errorf("read unread summary: %w", err)
	}
	if count.Valid {
		result.Count = &count.Int64
	}
	return result, nil
}

func (s *Service) maintainTeamUnread(ctx context.Context) error {
	for range teamUnreadJobsPerTick {
		more, _, err := s.database.advanceTeamUnread(ctx)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	return nil
}

func (d *database) advanceTeamUnread(ctx context.Context) (bool, int, error) {
	var id, organization, principal, reporter, after, through string
	var requested, scan, partial int64
	err := d.reader.QueryRowContext(ctx, `SELECT id,organization_id,principal_id,reporter_principal,requested_revision,scan_revision,after_thread,through_thread,partial_count FROM team_thread_unread_summaries WHERE requested_revision>completed_revision ORDER BY turn,id LIMIT 1`).Scan(&id, &organization, &principal, &reporter, &requested, &scan, &after, &through, &partial)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("next unread summary: %w", err)
	}
	originalScan, originalAfter := scan, after
	if scan == 0 {
		scan = requested
		if err := d.reader.QueryRowContext(ctx, "SELECT COALESCE(MAX(id),'') FROM team_threads WHERE organization_id=?", organization).Scan(&through); err != nil {
			return false, 0, fmt.Errorf("bound unread scan: %w", err)
		}
	}
	rows, err := d.reader.QueryContext(ctx, `SELECT t.id,CASE WHEN t.scope='general' OR EXISTS(SELECT 1 FROM team_thread_unread_projects p WHERE p.summary_id=? AND p.project_id=t.project_id) THEN t.post_count>COALESCE(r.last_seq,0) ELSE 0 END,COALESCE(t.project_id,''),COALESCE(t.work_item_id,'') FROM (SELECT id,scope,project_id,post_count,work_item_id FROM team_threads WHERE organization_id=? AND id>? AND id<=? ORDER BY id LIMIT ?) t LEFT JOIN team_thread_reads r ON r.thread_id=t.id AND r.principal_id=? ORDER BY t.id`, id, organization, after, through, teamUnreadPageSize+1, principal)
	if err != nil {
		return false, 0, fmt.Errorf("scan unread page: %w", err)
	}
	defer rows.Close()
	type row struct {
		id, project, issue string
		unread             bool
	}
	page := make([]row, 0, teamUnreadPageSize+1)
	for rows.Next() {
		var item row
		if err := rows.Scan(&item.id, &item.unread, &item.project, &item.issue); err != nil {
			return false, 0, fmt.Errorf("decode unread page: %w", err)
		}
		page = append(page, item)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, 0, fmt.Errorf("read unread page: %w", err)
	}
	complete := len(page) <= teamUnreadPageSize
	if !complete {
		page = page[:teamUnreadPageSize]
	}
	for _, item := range page {
		after = item.id
		if item.unread {
			if reporter != "" && item.issue != "" {
				scope := nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(item.project), credential: apiCredential{HostedRole: "reporter"}, sourceActor: &tracker.Actor{PrincipalID: reporter}}
				if err := requireReporterItem(ctx, d.reader, scope, item.issue); err != nil {
					if errors.Is(err, sql.ErrNoRows) || isNativeNotFound(err) {
						continue
					}
					return false, 0, fmt.Errorf("authorize unread issue: %w", err)
				}
			}
			partial++
		}
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, fmt.Errorf("begin unread projection: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE team_thread_unread_summaries SET turn=(SELECT COALESCE(MAX(turn),0)+1 FROM team_thread_unread_summaries) WHERE id=?", id); err != nil {
		return false, 0, fmt.Errorf("rotate unread summary: %w", err)
	}
	var query strings.Builder
	query.WriteString("UPDATE team_thread_unread_summaries SET ")
	var args []any
	if complete {
		query.WriteString("unread_count=?,completed_revision=?,scan_revision=0,after_thread='',through_thread='',partial_count=0")
		args = []any{partial, scan}
	} else {
		query.WriteString("scan_revision=?,after_thread=?,through_thread=?,partial_count=?")
		args = []any{scan, after, through, partial}
	}
	query.WriteString(" WHERE id=? AND requested_revision=? AND scan_revision=? AND after_thread=?")
	args = append(args, id, requested, originalScan, originalAfter)
	if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
		return false, 0, fmt.Errorf("project unread page: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("commit unread projection: %w", err)
	}
	return true, len(page), nil
}
