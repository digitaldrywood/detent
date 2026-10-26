package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const changeBase = nativeBase + "/work-items/:item/changes"

func changeCheckRequest(c echo.Context) bool {
	return c.Request().Method == http.MethodPost && c.Path() == changeBase+"/:change/versions/:version/checks"
}

func (s *Service) hostedChangeCheckPrincipal(c echo.Context, credential apiCredential) bool {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	if !changeCheckRequest(c) {
		return false
	}
	scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), project: tracker.ProjectID(c.Param("project")), credential: credential}
	return s.requireHostedChangeCheckPrincipal(ctx, s.database.reader, scope) == nil
}

func (s *Service) requireHostedChangeCheckPrincipal(ctx context.Context, query nativeQueryer, scope nativeScope) error {
	credential := scope.credential
	if credential.Scope != apiScopeOperator || credential.Hosted != nil || credential.Runner.RunnerID != "" || string(scope.organization) != s.config.Hosted.OrganizationID {
		return nativeNotFound()
	}
	var created string
	var expires sql.NullString
	err := query.QueryRowContext(ctx, `SELECT t.created_at, t.expires_at FROM api_tokens t
JOIN token_grants g ON g.token_id = t.id
WHERE t.id = ? AND t.token_hash = ? AND t.scope = 'operator' AND t.revoked_at IS NULL
AND g.organization_id = ? AND g.project_id = ?
AND NOT EXISTS (SELECT 1 FROM runner_identities r WHERE r.token_id = t.id)`, credential.ID, credential.Hash, scope.organization, scope.project).Scan(&created, &expires)
	if err != nil {
		return err
	}
	if expires.Valid && !runnerTimeValid(s.config.now(), created, expires.String) {
		return nativeNotFound()
	}
	rules, err := readChangePolicy(ctx, query, scope)
	if err != nil {
		return err
	}
	for _, check := range rules.RequiredChecks {
		if check.PrincipalID == credential.ID && check.Source == "independent" {
			return nil
		}
	}
	return nativeNotFound()
}

func (s *Service) registerChangeRoutes(e *echo.Echo) {
	read := s.requireNativeScope(apiScopeWorker, apiScopeOperator)
	write := s.requireNativeScope(apiScopeWorker, apiScopeOperator)
	operator := s.requireNativeScope(apiScopeOperator)
	e.GET(nativeBase+"/change-review-policy", s.getChangeReviewPolicy, read)
	e.PUT(nativeBase+"/change-review-policy", s.approveChangeReviewPolicy, s.requireChangeReviewPolicyAdmin())
	e.GET(changeBase, s.listChanges, read)
	e.POST(changeBase, s.createChange, write)
	e.GET(changeBase+"/:change", s.getChange, read)
	e.POST(changeBase+"/:change/versions", s.publishChangeVersion, write)
	e.GET(changeBase+"/:change/versions/:version/source", s.changeSource, read)
	e.POST(changeBase+"/:change/discussion", s.discussChange, write)
	e.POST(changeBase+"/:change/versions/:version/reviews", s.reviewChange, s.requireChangeReviewer())
	e.GET(changeBase+"/:change/versions/:version/viewed-files", s.changeViewedFiles, operator)
	e.POST(changeBase+"/:change/versions/:version/viewed-files", s.viewChangeFile, operator)
	e.POST(changeBase+"/:change/versions/:version/checks", s.submitChangeCheck, write)
	e.POST(changeBase+"/:change/versions/:version/landing", s.landChange, write)
}

// A hosted review is a project decision, so a write grant alone is not
// enough: the reviewer must also be an organization owner or administrator.
// Self-hosted operator tokens retain their existing review authority.
func (s *Service) requireChangeReviewer() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return s.requireNativeScope(apiScopeOperator, apiScopeWorker)(func(c echo.Context) error {
			scope := nativeRequestScope(c)
			if scope.credential.Hosted != nil && scope.credential.Scope != apiScopeWorker {
				if scope.credential.HostedRole != "owner" && scope.credential.HostedRole != "admin" {
					return s.nativeAPIError(c, nativeNotFound())
				}
				scope.requireHostedAdmin = true
				c.Set("native_scope", scope)
			}
			return next(c)
		})
	}
}

func (s *Service) requireChangeReviewPolicyAdmin() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		if s.config.Hosted == nil {
			return s.requireInstanceAdmin()(s.requireNativeScope(apiScopeAdmin)(next))
		}
		return s.requireOnboardingAdmin()(func(c echo.Context) error {
			scope := nativeRequestScope(c)
			scope.requireHostedAdmin = true
			c.Set("native_scope", scope)
			return next(c)
		})
	}
}

func readChangePolicy(ctx context.Context, query nativeQueryer, scope nativeScope) (tracker.ChangeReviewPolicy, error) {
	var result tracker.ChangeReviewPolicy
	var raw string
	if err := query.QueryRowContext(ctx, "SELECT policy_json FROM change_review_policies WHERE organization_id = ? AND project_id = ?", scope.organization, scope.project).Scan(&raw); err != nil {
		return result, err
	}
	err := decodeStorageJSON(ctx, []byte(raw), &result)
	return result, err
}

func (s *Service) getChangeReviewPolicy(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	result, err := readChangePolicy(ctx, s.database.reader, nativeRequestScope(c))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) approveChangeReviewPolicy(c echo.Context) error {
	var request tracker.ApproveChangeReviewPolicy
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return s.approveChangeReviewPolicyCommand(ctx, tx, scope, request)
	})
}

func readChange(ctx context.Context, query nativeQueryer, scope nativeScope, item, id string) (tracker.ChangeRequest, error) {
	if err := requireReporterItem(ctx, query, scope, item); err != nil {
		return tracker.ChangeRequest{}, err
	}
	var change tracker.ChangeRequest
	var raw string
	err := query.QueryRowContext(ctx, `SELECT c.record_json FROM change_requests c JOIN change_issue_links l ON l.change_id = c.id
WHERE c.organization_id = ? AND c.project_id = ? AND l.work_item_id = ? AND c.id = ?`, scope.organization, scope.project, item, id).Scan(&raw)
	if err != nil {
		return change, fmt.Errorf("read snapshot: %w", err)
	}
	err = decodeStorageJSON(ctx, []byte(raw), &change)
	if err != nil {
		return change, fmt.Errorf("decode snapshot: %w", err)
	}
	visibleChange, visible, err := reporterChange(ctx, query, scope, change)
	if err != nil {
		return tracker.ChangeRequest{}, err
	}
	if !visible {
		return tracker.ChangeRequest{}, nativeNotFound()
	}
	return visibleChange, nil
}

func readChangeVersionSnapshot(ctx context.Context, query nativeQueryer, changeID, versionID string) (tracker.ChangeVersion, error) {
	var version tracker.ChangeVersion
	var raw string
	if err := query.QueryRowContext(ctx, "SELECT record_json FROM change_versions WHERE change_id = ? AND id = ?", changeID, versionID).Scan(&raw); err != nil {
		return version, fmt.Errorf("read version snapshot: %w", err)
	}
	if err := decodeStorageJSON(ctx, []byte(raw), &version); err != nil {
		return version, fmt.Errorf("decode version snapshot: %w", err)
	}
	if err := hydrateChangeRecord(ctx, query, &version); err != nil {
		return version, err
	}
	return version, nil
}

func readChangeVersion(ctx context.Context, query nativeQueryer, changeID, versionID string) (tracker.ChangeVersion, error) {
	version, err := readChangeVersionSnapshot(ctx, query, changeID, versionID)
	if err != nil {
		return version, err
	}
	if err := resolveVersionPublication(ctx, query, changeID, &version); err != nil {
		return version, fmt.Errorf("resolve publication: %w", err)
	}
	quality, err := readQualityLanding(ctx, query, tracker.ChangeRequest{ID: changeID}, version.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return version, err
	}
	if quality != nil {
		version.LandingQuality = quality.Quality
	}
	receipt, err := readVersionLanding(ctx, query, version.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return version, nil
	}
	if err == nil {
		version.Landing = &receipt
	}
	if err != nil {
		return version, fmt.Errorf("read landing: %w", err)
	}
	return version, nil
}

func resolveVersionPublication(ctx context.Context, query nativeQueryer, changeID string, version *tracker.ChangeVersion) error {
	if version.External != nil {
		return nil
	}
	var confirmed tracker.NativePRPublication
	err := query.QueryRowContext(ctx, `SELECT p.change_id, p.version_id, p.repository, p.base_ref, p.branch, p.head_sha,
p.policy_id, p.source_attempt_id, p.provider, p.external_id, p.external_url
FROM native_pr_publications p JOIN change_requests c ON c.id=p.change_id
JOIN native_attempts a ON a.id=p.recorder_attempt_id
AND a.organization_id=c.organization_id AND a.project_id=c.project_id AND a.work_item_id=c.work_item_id
WHERE p.change_id=? AND p.version_id=?`, changeID, version.ID).Scan(&confirmed.ChangeID, &confirmed.VersionID, &confirmed.Repository,
		&confirmed.BaseRef, &confirmed.Branch, &confirmed.HeadSHA, &confirmed.PolicyID, &confirmed.SourceAttemptID,
		&confirmed.External.Provider, &confirmed.External.ID, &confirmed.External.URL)
	if err == nil {
		confirmed.SourceVersion = tracker.NativeChangeReference{ChangeID: confirmed.ChangeID, VersionID: confirmed.VersionID, HeadSHA: confirmed.HeadSHA}
		if confirmed.Matches(changeID, *version) {
			version.External = &confirmed.External
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var raw string
	err = query.QueryRowContext(ctx, `SELECT a.data_json
FROM native_attempts a JOIN change_requests c ON c.id=?
AND c.organization_id=a.organization_id AND c.project_id=a.project_id AND c.work_item_id=a.work_item_id
WHERE a.finalization_version_id=?
ORDER BY a.fencing_token DESC LIMIT 1`, changeID, version.ID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var run tracker.NativeRunData
	if err := decodeRunDetail(ctx, query, raw, &run); err != nil {
		return err
	}
	if run.Finalization != nil && run.Finalization.Publication != nil && run.Finalization.Publication.Matches(changeID, *version) {
		version.External = &run.Finalization.Publication.External
	}
	return nil
}

// Limit the indexed event lookup before joining provenance, so malformed
// evidence cannot cause a scan of older attempts. Checkpoints remain available
// after event compaction and after the recorder replaces its latest handoff.
const versionPublicationPushSQL = `WITH checkpoint AS (
 SELECT data_json FROM collaboration_events
 WHERE organization_id=? AND project_id=? AND work_item_id=?
 AND type='run.checkpointed'
 AND json_extract(data_json,'$.run.handoff.external_effect')='git_push'
 AND json_extract(data_json,'$.run.handoff.effect_state')='confirmed'
 AND json_extract(data_json,'$.run.handoff.change.change_id')=?
 AND json_extract(data_json,'$.run.handoff.change.version_id')=?
 AND json_extract(data_json,'$.run.handoff.change.head_sha')=?
 ORDER BY sequence DESC LIMIT 1
)
SELECT json_extract(e.data_json,'$.run') FROM checkpoint e
JOIN native_attempts a ON a.id=json_extract(e.data_json,'$.run.attempt_id')
 AND a.organization_id=? AND a.project_id=? AND a.work_item_id=?
 AND a.run_id=json_extract(e.data_json,'$.run.run_id')
 AND a.lease_id=json_extract(e.data_json,'$.run.lease_id')
 AND a.fencing_token=json_extract(e.data_json,'$.run.fencing_token')
 AND json_extract(a.data_json,'$.policy_id')=json_extract(e.data_json,'$.run.policy_id')
JOIN native_attempt_events ae ON ae.attempt_id=a.id AND ae.sequence=json_extract(e.data_json,'$.run.sequence')
JOIN leases l ON l.lease_id=a.lease_id AND l.fencing_token=a.fencing_token
JOIN issues i ON i.id=l.issue_id AND i.organization_id=a.organization_id AND i.project_id=a.project_id AND i.native_id=a.work_item_id
JOIN lease_policies p ON p.lease_id=a.lease_id AND p.scope=a.organization_id || '/' || a.project_id
 AND p.policy_id=json_extract(e.data_json,'$.run.policy_id')`

func resolveVersionPublicationPush(ctx context.Context, query nativeQueryer, scope nativeScope, item string, version tracker.ChangeVersion) (tracker.ChangePublicationPush, bool, error) {
	if version.External != nil || version.AttemptID == "" || !version.Policy.Gates.GitHubPullRequest || version.Landing != nil && version.Landing.Landed {
		return tracker.ChangePublicationPush{}, false, nil
	}
	var raw string
	err := query.QueryRowContext(ctx, versionPublicationPushSQL, scope.organization, scope.project, item, version.ChangeID, version.ID, version.HeadSHA, scope.organization, scope.project, item).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return tracker.ChangePublicationPush{}, false, nil
	}
	if err != nil {
		return tracker.ChangePublicationPush{}, false, err
	}
	var run tracker.NativeRunData
	if err := decodeRunDetail(ctx, query, raw, &run); err != nil {
		return tracker.ChangePublicationPush{}, false, err
	}
	ref := tracker.NativeChangeReference{ChangeID: version.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA}
	checkpoint := run.Handoff
	if checkpoint == nil || checkpoint.Change == nil || *checkpoint.Change != ref || checkpoint.HeadSHA != version.HeadSHA || !validNativeID(checkpoint.EffectID, "effect") {
		return tracker.ChangePublicationPush{}, false, nil
	}
	return tracker.ChangePublicationPush{Change: ref, RecordingAttemptID: run.AttemptID, EffectID: checkpoint.EffectID}, true, nil
}

func changeRows[T any](ctx context.Context, query nativeQueryer, statement string, args ...any) ([]T, error) {
	rows, err := query.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query snapshot: %w", err)
	}
	defer rows.Close()
	result := []T{}
	for rows.Next() {
		var raw string
		var item T
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan snapshot: %w", err)
		}
		if err := decodeStorageJSON(ctx, []byte(raw), &item); err != nil {
			return nil, fmt.Errorf("decode snapshot: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read snapshot rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range result {
		if err := hydrateChangeRecord(ctx, query, &result[i]); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *Service) listChanges(c echo.Context) error {
	result, err := s.readChanges(c.Request().Context(), nativeRequestScope(c), c.Param("item"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) readChanges(ctx context.Context, scope nativeScope, item string) ([]tracker.ChangeRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if _, _, err := readNativeIssue(ctx, s.database.reader, scope, item); err != nil {
		return nil, err
	}
	result, err := changeRows[tracker.ChangeRequest](ctx, s.database.reader, `SELECT c.record_json FROM change_requests c JOIN change_issue_links l ON l.change_id = c.id
WHERE c.organization_id = ? AND c.project_id = ? AND l.work_item_id = ? ORDER BY c.rowid`, scope.organization, scope.project, item)
	if err != nil {
		return nil, err
	}
	if scope.credential.HostedRole == "reporter" {
		filtered := []tracker.ChangeRequest{}
		for _, change := range result {
			value, visible, err := reporterChange(ctx, s.database.reader, scope, change)
			if err != nil {
				return nil, err
			}
			if visible {
				filtered = append(filtered, value)
			}
		}
		result = filtered
	}
	return result, nil
}

func (s *Service) createChange(c echo.Context) error {
	var request tracker.CreateChange
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return s.createChangeCommand(ctx, tx, scope, c.Param("item"), request, now)
	})
}

func (s *Service) getChange(c echo.Context) error {
	if handled, err := s.archiveView(c, c.Param("item"), "change:"+c.Param("change"), nil); handled {
		return err
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope, ctx := nativeRequestScope(c), ctx
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	defer tx.Rollback()
	if c.QueryParam("view") == "bounded" {
		args := operatortool.ChangeArguments{ProjectID: string(scope.project), ItemID: c.Param("item"), ChangeID: c.Param("change"), VersionID: c.QueryParam("version_id"), Section: c.QueryParam("section"), Cursor: c.QueryParam("cursor")}
		if c.QueryParam("limit") != "" {
			args.Limit, err = strconv.Atoi(c.QueryParam("limit"))
			if err != nil {
				return s.nativeAPIError(c, nativePublicInvalid("limit must be an integer"))
			}
		}
		raw, err := json.Marshal(args)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		args, err = operatortool.DecodeChangeArguments(operatortool.GetChange, raw)
		if err != nil {
			return s.nativeAPIError(c, nativePublicInvalid("invalid Change read selector"))
		}
		result := (hubChangeApplication{service: s}).result(scope, args)
		result, err = readBoundedChange(ctx, tx, scope, args, result)
		if err != nil {
			var refusal *operatortool.RequestError
			if errors.As(err, &refusal) {
				return s.nativeAPIError(c, refusal)
			}
			if errors.Is(err, mutation.ErrConflict) {
				return s.nativeAPIError(c, nativeConflict(0))
			}
			if errors.Is(err, operatortool.ErrInvalidArguments) {
				return s.nativeAPIError(c, nativePublicInvalid("invalid Change read cursor"))
			}
			return s.nativeAPIError(c, err)
		}
		if err := tx.Commit(); err != nil {
			return s.nativeAPIError(c, err)
		}
		bounded, err := operatortool.BoundedChangeResult(result)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.Blob(http.StatusOK, "application/json", bounded.Content)
	}

	result, err := readChangeDetail(ctx, tx, scope, c.Param("item"), c.Param("change"), s.config.now())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := tx.Commit(); err != nil {
		return s.nativeAPIError(c, err)
	}
	return nativeArchiveJSON(c, http.StatusOK, result)
}

func readChangeDetail(ctx context.Context, query nativeQueryer, scope nativeScope, item, id string, now time.Time) (tracker.ChangeDetail, error) {
	if explanationRead(ctx) {
		change, err := readChange(ctx, query, scope, item, id)
		if err != nil {
			return tracker.ChangeDetail{}, err
		}
		return readRuntimeChangeDetail(ctx, query, scope, change, now)
	}
	return readChangeDetailView(ctx, query, scope, item, id, now, false)
}

func readChangeDetailView(ctx context.Context, query nativeQueryer, scope nativeScope, item, id string, now time.Time, current bool) (tracker.ChangeDetail, error) {
	var result tracker.ChangeDetail
	var err error
	result.Change, err = readChange(ctx, query, scope, item, id)
	if err != nil {
		return result, fmt.Errorf("load change: %w", err)
	}
	query, err = archiveQuery(ctx, query, item, "change", id, 0)
	if err != nil {
		return result, err
	}
	result.SourceIssues, err = readChangeSourceIssues(ctx, query, scope, id)
	if err != nil {
		return result, fmt.Errorf("load source issues: %w", err)
	}
	versionFilter, evidenceFilter := "", ""
	args := []any{id}
	if current {
		versionFilter = " AND id = ?"
		evidenceFilter = " AND version_id = ?"
		args = append(args, result.Change.CurrentVersion)
	}
	result.Versions, err = changeRows[tracker.ChangeVersion](ctx, query, "SELECT record_json FROM change_versions WHERE change_id = ?"+versionFilter+" ORDER BY number", args...)
	if err != nil {
		return result, fmt.Errorf("load versions: %w", err)
	}
	for i := range result.Versions {
		if err := resolveVersionPublication(ctx, query, id, &result.Versions[i]); err != nil {
			return result, fmt.Errorf("resolve publication: %w", err)
		}
		receipt, err := readVersionLanding(ctx, query, result.Versions[i].ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, fmt.Errorf("load landing: %w", err)
		}
		if err == nil {
			result.Versions[i].Landing = &receipt
		}
		push, found, err := resolveVersionPublicationPush(ctx, query, scope, item, result.Versions[i])
		if err != nil {
			return result, fmt.Errorf("resolve publication push: %w", err)
		}
		if found {
			result.PublicationPushes = append(result.PublicationPushes, push)
		}
	}
	result.Reviews, err = changeRows[tracker.ChangeReview](ctx, query, "SELECT record_json FROM change_evidence WHERE change_id = ? AND kind = 'review'"+evidenceFilter+" ORDER BY sequence", args...)
	if err != nil {
		return result, fmt.Errorf("load reviews: %w", err)
	}
	result.Checks, err = changeRows[tracker.ChangeCheck](ctx, query, "SELECT record_json FROM change_evidence WHERE change_id = ? AND kind = 'check'"+evidenceFilter+" ORDER BY sequence", args...)
	if err != nil {
		return result, fmt.Errorf("load checks: %w", err)
	}
	if current {
		evidenceFilter = " AND (version_id = ? OR version_id IS NULL OR version_id = '')"
	}
	result.Discussion, err = changeRows[tracker.ChangeDiscussion](ctx, query, "SELECT record_json FROM change_evidence WHERE change_id = ? AND kind = 'discussion'"+evidenceFilter+" ORDER BY sequence", args...)
	if err != nil {
		return result, fmt.Errorf("load discussion: %w", err)
	}
	var staleApproval bool
	if current {
		err = query.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM change_evidence WHERE change_id = ? AND kind = 'review'
AND json_extract(record_json, '$.decision') = 'approved' AND json_extract(record_json, '$.validator') IS NULL AND json_extract(record_json, '$.implementation_review') IS NULL AND version_id != ?)`, id, result.Change.CurrentVersion).Scan(&staleApproval)
		if err != nil {
			return result, fmt.Errorf("load stale approval: %w", err)
		}
	}
	result.Summary, err = readChangeSummary(ctx, query, scope, result, now, staleApproval)
	if err != nil {
		return result, fmt.Errorf("load summary: %w", err)
	}
	err = loadChangeExternal(ctx, query, scope, &result)
	if err != nil {
		return result, fmt.Errorf("load external evidence: %w", err)
	}
	return result, nil
}

func readCurrentChangeDetail(ctx context.Context, query nativeQueryer, scope nativeScope, change tracker.ChangeRequest, now time.Time) (tracker.ChangeDetail, error) {
	result, err := readCurrentChangeDetailWithVersion(ctx, query, scope, change, now)
	result.Versions = nil
	return result, err
}

func readCurrentChangeDetailWithVersion(ctx context.Context, query nativeQueryer, scope nativeScope, change tracker.ChangeRequest, now time.Time) (tracker.ChangeDetail, error) {
	return readCurrentChangeDetailWithPolicy(ctx, query, scope, change, now, nil)
}

func readCurrentChangeDetailWithPolicy(ctx context.Context, query nativeQueryer, scope nativeScope, change tracker.ChangeRequest, now time.Time, currentPolicy *changeSummaryPolicy) (tracker.ChangeDetail, error) {
	result := tracker.ChangeDetail{Change: change}
	var staleApproval bool
	version, found, err := readNativeChangeVersion(ctx, query, change)
	if err != nil {
		return result, err
	}
	if found {
		result.Versions = []tracker.ChangeVersion{version}
		result.Reviews, err = changeRows[tracker.ChangeReview](ctx, query, "SELECT record_json FROM change_evidence WHERE change_id = ? AND version_id = ? AND kind = 'review' ORDER BY sequence", change.ID, version.ID)
		if err != nil {
			return result, err
		}
		err = query.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM change_evidence WHERE change_id = ? AND kind = 'review'
AND json_extract(record_json, '$.decision') = 'approved' AND json_extract(record_json, '$.validator') IS NULL AND json_extract(record_json, '$.implementation_review') IS NULL AND version_id != ?)`, change.ID, version.ID).Scan(&staleApproval)
		if err != nil {
			return result, err
		}
		result.Checks, err = changeRows[tracker.ChangeCheck](ctx, query, "SELECT record_json FROM change_evidence WHERE change_id = ? AND version_id = ? AND kind = 'check' ORDER BY sequence", change.ID, version.ID)
		if err != nil {
			return result, err
		}
	}
	if currentPolicy == nil {
		loaded, loadErr := readChangeSummaryPolicy(ctx, query, scope)
		if loadErr != nil {
			return result, loadErr
		}
		currentPolicy = &loaded
	}
	result.Summary = currentPolicy.summarize(result, now, staleApproval)
	err = loadChangeExternal(ctx, query, scope, &result)
	result.Reviews, result.Checks = nil, nil
	return result, err
}

type changeSummaryPolicy struct {
	approval policy.Approval
	rules    tracker.ChangeReviewPolicy
}

func readChangeSummaryPolicy(ctx context.Context, query nativeQueryer, scope nativeScope) (changeSummaryPolicy, error) {
	approval, _, err := readProjectPolicy(ctx, query, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return changeSummaryPolicy{}, err
	}
	rules, err := readChangePolicy(ctx, query, scope)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return changeSummaryPolicy{}, err
	}
	return changeSummaryPolicy{approval: approval, rules: rules}, nil
}

func readChangeSummary(ctx context.Context, query nativeQueryer, scope nativeScope, detail tracker.ChangeDetail, now time.Time, priorApproval bool) (tracker.ChangeSummary, error) {
	current, err := readChangeSummaryPolicy(ctx, query, scope)
	if err != nil {
		return tracker.ChangeSummary{}, err
	}
	return current.summarize(detail, now, priorApproval), nil
}

func (p changeSummaryPolicy) identities(version *tracker.ChangeVersion) (string, string) {
	return changerequest.CurrentPolicyIdentities(version, p.approval.Policy, p.rules)
}

func (p changeSummaryPolicy) summarize(detail tracker.ChangeDetail, now time.Time, priorApproval bool) tracker.ChangeSummary {
	var current *tracker.ChangeVersion
	for i := range detail.Versions {
		if detail.Versions[i].ID == detail.Change.CurrentVersion {
			current = &detail.Versions[i]
			break
		}
	}
	policyID, reviewID := p.identities(current)
	return changerequest.SummarizeCurrentVersion(detail, policyID, reviewID, now, priorApproval)
}

func loadChangeExternal(ctx context.Context, query nativeQueryer, scope nativeScope, detail *tracker.ChangeDetail) error {
	for _, version := range detail.Versions {
		if version.ID != detail.Change.CurrentVersion || version.External == nil {
			continue
		}
		var issueID tracker.WorkItemID
		err := query.QueryRowContext(ctx, `SELECT pr.issue_id FROM pull_requests pr JOIN projects p ON p.repository_id = pr.repository_id
WHERE p.organization_id = ? AND p.id = ? AND pr.url = ? AND pr.issue_id IS NOT NULL`, scope.organization, scope.project, version.External.URL).Scan(&issueID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		record := &tracker.Record{}
		if err := loadWorkItemPullRequests(ctx, query, map[tracker.WorkItemID]*tracker.Record{issueID: record}, []tracker.WorkItemID{issueID}); err != nil {
			return err
		}
		for _, pr := range record.PullRequests {
			if pr.URL == version.External.URL {
				detail.External = &pr
				if pr.HeadSHA != version.HeadSHA {
					detail.Summary.ExternalReview = "stale_head"
				} else if pr.Reviews.Decision != "" {
					detail.Summary.ExternalReview = "snapshot: " + pr.Reviews.Decision
				}
				return nil
			}
		}
	}
	return nil
}

func (s *Service) createChangeCommand(ctx context.Context, tx *sql.Tx, scope nativeScope, item string, request tracker.CreateChange, now time.Time) (any, error) {
	if strings.TrimSpace(request.Title) == "" || len(request.Title) > 512 || len(request.Body) > 64<<10 || len(request.LinkedIssues) > 32 {
		return nil, nativePublicInvalid("Change requires a title up to 512 bytes, discussion up to 64 KiB, and at most 32 linked issues")
	}
	change := tracker.ChangeRequest{ID: newNativeID("change"), OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: tracker.NativeWorkItemID(item), Title: request.Title, Body: request.Body, Revision: 1, CreatedAt: now, UpdatedAt: now}
	change.LinkedIssues = append([]tracker.NativeWorkItemID{change.WorkItemID}, request.LinkedIssues...)
	slices.Sort(change.LinkedIssues)
	change.LinkedIssues = slices.Compact(change.LinkedIssues)
	for _, id := range change.LinkedIssues {
		if _, _, err := readNativeIssue(ctx, tx, scope, string(id)); err != nil {
			return nil, err
		}
	}
	raw, err := marshalNative(change)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO change_requests (id, organization_id, project_id, work_item_id, record_json, claim_version_id, claim_head_sha) VALUES (?, ?, ?, ?, ?, '', '')", change.ID, scope.organization, scope.project, change.WorkItemID, raw); err != nil {
		return nil, err
	}
	for _, id := range change.LinkedIssues {
		if _, err := tx.ExecContext(ctx, "INSERT INTO change_issue_links VALUES (?, ?, ?, ?)", change.ID, scope.organization, scope.project, id); err != nil {
			return nil, err
		}
		if err := appendNativeHistory(ctx, tx, scope, string(id), "change.created", tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: change.ID}}, now); err != nil {
			return nil, err
		}
	}
	return change, nil
}

func (s *Service) approveChangeReviewPolicyCommand(ctx context.Context, tx *sql.Tx, scope nativeScope, request tracker.ApproveChangeReviewPolicy) (any, error) {
	approved, err := requireProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return nil, err
	}
	if err := changerequest.ValidatePolicy(request.Policy, approved.Policy); err != nil {
		return nil, nativeInvalid(err.Error())
	}
	for _, check := range request.Policy.RequiredChecks {
		var role string
		err := tx.QueryRowContext(ctx, `SELECT scope FROM api_tokens t WHERE id = ? AND revoked_at IS NULL
AND EXISTS (SELECT 1 FROM token_grants g WHERE g.token_id = t.id AND g.organization_id = ? AND g.project_id = ?)`, check.PrincipalID, scope.organization, scope.project).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) || err == nil && check.Source == "independent" && role == string(apiScopeWorker) {
			return nil, nativePublicInvalid("Expected CI principals must have a project grant; independent validation requires an operator credential")
		}
		if err != nil {
			return nil, err
		}
	}
	previous, err := readChangePolicy(ctx, tx, scope)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// A policy that pins no checks follows the repository gate and is
	// nobody's separate decision, so an approval that expects no review
	// policy may replace it; any other policy must be named by the
	// identity the approver read.
	seeded := followsRepositoryGate(previous)
	if previous.ID != request.ExpectedID && (request.ExpectedID != "" || !seeded) {
		return nil, policyMismatch("Review policy changed; supply its current identity")
	}
	rules := request.Policy
	rules.ID = changerequest.PolicyID(rules)
	raw, err := marshalNative(rules)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO change_review_policies (organization_id, project_id, policy_json) VALUES (?, ?, ?)
ON CONFLICT (organization_id, project_id) DO UPDATE SET policy_json = excluded.policy_json`, scope.organization, scope.project, raw)
	if err != nil {
		return nil, err
	}
	if previous.ID != rules.ID {
		if err := routeStaleNativeLandingChanges(ctx, tx, string(scope.organization)+"/"+string(scope.project), scope.actor().PrincipalID, "", false, s.config.now()); err != nil {
			return nil, err
		}
		if err := resetNativeBoardPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project)); err != nil {
			return nil, err
		}
	}
	return rules, nil
}
