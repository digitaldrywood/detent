package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func dependencyBasis(ids []string) string {
	slices.Sort(ids)
	digest := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return hex.EncodeToString(digest[:])
}

func readNativeDependencyDetails(ctx context.Context, q nativeQueryer, scope nativeScope, issue *tracker.NativeIssue) error {
	return readNativeDependencyDetailsBatch(ctx, q, scope, []*tracker.NativeIssue{issue})
}

func readNativeDependencyDetailsBatch(ctx context.Context, q nativeQueryer, scope nativeScope, issues []*tracker.NativeIssue) error {
	if scope.credential.HostedRole == "reporter" {
		pageQuery, pageErr := prepareNativePageQuery(ctx, q, nativeScope{organization: scope.organization}, "", "native_issue_actors")
		if pageErr != nil {
			return pageErr
		}
		q = pageQuery
	}
	if len(issues) == 0 {
		return nil
	}
	type dependencyEdges struct {
		blocked, unblocks               []string
		privateBlocked, privateUnblocks bool
	}
	items := make(map[tracker.NativeWorkItemID]*tracker.NativeIssue, len(issues))
	edges := make(map[tracker.NativeWorkItemID]*dependencyEdges, len(issues))
	ids := make([]tracker.NativeWorkItemID, 0, len(issues))
	for _, issue := range issues {
		issue.Dependencies = []tracker.NativeWorkItemID{}
		issue.Blockers = []tracker.NativeDependency{}
		issue.Dependents = []tracker.NativeDependency{}
		issue.DependencyReady = true
		items[issue.WorkItemID] = issue
		edges[issue.WorkItemID] = &dependencyEdges{}
		ids = append(ids, issue.WorkItemID)
	}
	rawIDs, err := sqlList(ids)
	if err != nil {
		return err
	}
	selection := rawIDs
	condition, grantArgs := scope.credential.projectGrantSQL("i.organization_id", "i.project_id")
	visibility, visibilityArgs := scope.issueVisibilitySQL("i")
	condition = "(" + condition + ") AND (" + visibility + ")"
	grantArgs = append(grantArgs, visibilityArgs...)
	args := append(grantArgs, selection, scope.organization, scope.project)
	rows, err := querySQL(ctx, q, `SELECT source.native_id, i.native_id, i.project_id, i.number, i.title, COALESCE(ws.detent_state,''), COALESCE(ws.terminal,0), o.slug, p.slug,
 d.dependent_issue_id = source.id, (`+condition+`), COALESCE(ws.dispatchable,0), CASE WHEN ws.terminal=1 THEN `+nativeTerminalEnteredAt+` ELSE NULL END,
 (SELECT a.started_at FROM native_attempts a
 WHERE a.work_item_id=i.native_id AND a.organization_id=i.organization_id AND a.project_id=i.project_id AND a.status='running'
 AND EXISTS (SELECT 1 FROM leases l WHERE l.lease_id=a.lease_id AND l.released_at IS NULL AND l.expires_at>strftime('%Y-%m-%dT%H:%M:%fZ','now')) ORDER BY a.fencing_token DESC LIMIT 1)
 FROM issues source CROSS JOIN issue_dependencies d ON d.dependent_issue_id=source.id OR d.blocker_issue_id=source.id
 JOIN issues i ON i.id=CASE WHEN d.dependent_issue_id=source.id THEN d.blocker_issue_id ELSE d.dependent_issue_id END
 JOIN organizations o ON o.id=i.organization_id JOIN projects p ON p.id=i.project_id
 LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id
 WHERE source.native_id IN (SELECT value FROM sql_rows(?)) AND source.organization_id=? AND source.project_id=?`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id tracker.NativeWorkItemID
		var link tracker.NativeDependency
		var organizationSlug, projectSlug string
		var isBlocker, visible bool
		var started, closed sql.NullString
		if err := rows.Scan(&id, &link.ID, &link.ProjectID, &link.Number, &link.Title, &link.State, &link.Terminal, &organizationSlug, &projectSlug, &isBlocker, &visible, &link.Dispatchable, &closed, &started); err != nil {
			return err
		}
		issue, edge := items[id], edges[id]
		if issue == nil || edge == nil {
			return sql.ErrNoRows
		}
		link.Identifier = string(link.ProjectID) + "#" + strconv.Itoa(link.Number)
		link.WebURL = issueBrowserPath(organizationSlug, projectSlug, link.Number)
		if closed.Valid {
			at, err := parseTimeValue(closed.String)
			if err != nil {
				return err
			}
			link.ClosedAt = &at
		}
		if started.Valid {
			at, err := parseTimeValue(started.String)
			if err != nil {
				return err
			}
			link.StartedAt = &at
		}
		if isBlocker {
			edge.blocked = append(edge.blocked, string(link.ID))
			edge.privateBlocked = edge.privateBlocked || link.ProjectID != issue.ProjectID
		} else {
			edge.unblocks = append(edge.unblocks, string(link.ID))
			edge.privateUnblocks = edge.privateUnblocks || link.ProjectID != issue.ProjectID
		}
		if !visible {
			edge.privateBlocked = edge.privateBlocked || isBlocker
			edge.privateUnblocks = edge.privateUnblocks || !isBlocker
			continue
		}
		if isBlocker {
			issue.DependencyReady = issue.DependencyReady && link.Terminal
			issue.Dependencies = append(issue.Dependencies, link.ID)
			issue.Blockers = append(issue.Blockers, link)
		} else {
			issue.Dependents = append(issue.Dependents, link)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, issue := range items {
		slices.Sort(issue.Dependencies)
		slices.SortFunc(issue.Blockers, func(a, b tracker.NativeDependency) int {
			return strings.Compare(string(a.ID), string(b.ID))
		})
		slices.SortFunc(issue.Dependents, func(a, b tracker.NativeDependency) int {
			return strings.Compare(string(a.ID), string(b.ID))
		})
	}
	reasons, err := querySQL(ctx, q, `SELECT native_id,blocked_reason,blocked_reason_basis,blocked_reason_revision,unblocks_reason,unblocks_reason_basis,unblocks_reason_revision FROM issues WHERE native_id IN (SELECT value FROM sql_rows(?)) AND organization_id=? AND project_id=?`, selection, scope.organization, scope.project)
	if err != nil {
		return err
	}
	defer reasons.Close()
	loaded := 0
	for reasons.Next() {
		var id tracker.NativeWorkItemID
		var blocked, unblocks tracker.DependencyReason
		if err := reasons.Scan(&id, &blocked.Text, &blocked.Basis, &blocked.Revision, &unblocks.Text, &unblocks.Basis, &unblocks.Revision); err != nil {
			return err
		}
		issue, edge := items[id], edges[id]
		if issue == nil || edge == nil {
			return sql.ErrNoRows
		}
		blocked.CurrentBasis = dependencyBasis(edge.blocked)
		unblocks.CurrentBasis = dependencyBasis(edge.unblocks)
		if blocked.Basis != blocked.CurrentBasis || edge.privateBlocked {
			blocked.Text = ""
		}
		if unblocks.Basis != unblocks.CurrentBasis || edge.privateUnblocks {
			unblocks.Text = ""
		}
		if edge.privateBlocked {
			blocked.CurrentBasis = ""
		}
		if edge.privateUnblocks {
			unblocks.CurrentBasis = ""
		}
		issue.BlockedReason, issue.UnblocksReason = blocked, unblocks
		loaded++
	}
	if err := reasons.Err(); err != nil {
		return err
	}
	if err := reasons.Close(); err != nil {
		return err
	}
	if loaded != len(items) {
		return sql.ErrNoRows
	}
	return readNativeRelatedBatch(ctx, q, scope, items)
}

func validDependencyReason(text string) bool {
	for index, char := range text {
		if (char == '.' || char == '!' || char == '?') && strings.HasPrefix(text[index+1:], " ") && strings.TrimSpace(text[index+1:]) != "" {
			return false
		}
	}
	return text == strings.TrimSpace(text) && utf8.ValidString(text) && !strings.ContainsFunc(text, unicode.IsControl) && utf8.RuneCountInString(text) <= 140 && !strings.ContainsAny(text, "\r\n\t<>[]*_`") && !strings.Contains(text, "://") && !strings.Contains(strings.ToLower(text), "mailto:") && !strings.Contains(strings.ToLower(text), "www.")
}

func (s *Service) updateNativeDependencyReason(c echo.Context) error {
	var request tracker.UpdateDependencyReason
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	options := nativeCommandOptions{OperationID: "dependency-reason/" + c.Param("item"), Item: c.Param("item"), RequireLease: true, Feature: "collaboration"}
	result, err := s.executeNativeMutation(c.Request().Context(), scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, _ time.Time) (any, error) {
		if !validDependencyReason(request.Text) || (request.Kind != "blocked" && request.Kind != "unblocks") {
			return nil, nativePublicInvalid("Reason must be plain text of at most 140 characters and kind blocked or unblocks")
		}
		if _, _, err := readNativeIssue(ctx, tx, scope, c.Param("item")); err != nil {
			return nil, err
		}
		issue, _, err := readNativeIssue(ctx, tx, scope, string(request.WorkItemID))
		if err != nil {
			return nil, err
		}
		reason := issue.BlockedReason
		column := "blocked_reason"
		if request.Kind == "unblocks" {
			reason = issue.UnblocksReason
			column = "unblocks_reason"
		}
		if reason.CurrentBasis == "" || request.Basis != reason.CurrentBasis || request.ExpectedRevision != reason.Revision {
			return nil, nativeConflict(tracker.Revision(reason.Revision))
		}
		_, err = tx.ExecContext(ctx, "UPDATE issues SET "+column+"=?,"+column+"_basis=?,"+column+"_revision="+column+"_revision+1 WHERE native_id=? AND organization_id=? AND project_id=?", request.Text, request.Basis, request.WorkItemID, scope.organization, scope.project)
		if err != nil {
			return nil, err
		}
		reason.Text, reason.Basis, reason.Revision = request.Text, request.Basis, reason.Revision+1
		return reason, nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}

func readNativeDependencyReference(ctx context.Context, q nativeQueryer, scope nativeScope, id tracker.NativeWorkItemID) (*tracker.NativeDependency, error) {
	if scope.credential.HostedRole == "reporter" {
		pageQuery, pageErr := prepareNativePageQuery(ctx, q, nativeScope{organization: scope.organization}, "", "native_issue_actors")
		if pageErr != nil {
			return nil, pageErr
		}
		q = pageQuery
	}
	condition, grantArgs := scope.credential.projectGrantSQL("i.organization_id", "i.project_id")
	visibility, visibilityArgs := scope.issueVisibilitySQL("i")
	condition = "(" + condition + ") AND (" + visibility + ")"
	grantArgs = append(grantArgs, visibilityArgs...)
	args := append([]any{scope.organization, id}, grantArgs...)
	var link tracker.NativeDependency
	var organizationSlug, projectSlug string
	err := q.QueryRowContext(ctx, `SELECT i.native_id,i.project_id,i.number,i.title,COALESCE(ws.detent_state,''),COALESCE(ws.terminal,0),o.slug,p.slug FROM issues i JOIN organizations o ON o.id=i.organization_id JOIN projects p ON p.id=i.project_id LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.native_id=? AND (`+condition+`)`, args...).Scan(&link.ID, &link.ProjectID, &link.Number, &link.Title, &link.State, &link.Terminal, &organizationSlug, &projectSlug)
	if err != nil {
		return nil, err
	}
	link.WebURL = issueBrowserPath(organizationSlug, projectSlug, link.Number)
	return &link, nil
}
