package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func policyMismatch(message string) error {
	return &nativeError{Code: "policy_mismatch", Message: message, status: http.StatusConflict}
}

func (s *Service) policyScope(c echo.Context) (string, error) {
	if c.Param("organization") != "" {
		scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization")), project: tracker.ProjectID(c.Param("project"))}
		var count int
		if err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT count(*) FROM projects WHERE organization_id = ? AND id = ?", scope.organization, scope.project).Scan(&count); err != nil {
			return "", err
		}
		if count != 1 {
			return "", nativeNotFound()
		}
		return string(scope.organization) + "/" + string(scope.project), nil
	}
	repository := c.Param("owner") + "/" + c.Param("repo")
	var count int
	if err := s.database.db.QueryRowContext(c.Request().Context(), "SELECT count(*) FROM repositories WHERE github_owner || '/' || github_name = ? COLLATE NOCASE", repository).Scan(&count); err != nil {
		return "", err
	}
	if count != 1 {
		return "", nativeNotFound()
	}
	return "repository:" + strings.ToLower(repository), nil
}

func (s *Service) getProjectPolicy(c echo.Context) error {
	scope, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	approval, err := readProjectPolicy(c.Request().Context(), s.database.db, scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, approval)
}

func (s *Service) approveProjectPolicy(c echo.Context) error {
	var change policy.Change
	if err := decodeAPIJSON(c, &change); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := change.Policy.Validate(); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	scope, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.ID == "" {
		return s.nativeAPIError(c, nativeNotFound())
	}
	approval, err := s.database.approvePolicy(c.Request().Context(), scope, credential.ID, change)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, approval)
}

func (s *Service) revokeProjectPolicy(c echo.Context) error {
	var request struct {
		ExpectedID string `json:"expected_policy_id"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	scope, err := s.policyScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	result, err := s.database.db.ExecContext(c.Request().Context(), "DELETE FROM project_policies WHERE scope = ? AND policy_id = ?", scope, request.ExpectedID)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if count != 1 {
		return s.nativeAPIError(c, policyMismatch("Policy changed or is already revoked; inspect the current approval before revoking it"))
	}
	return c.NoContent(http.StatusNoContent)
}

func (d *database) approvePolicy(ctx context.Context, scope, actor string, change policy.Change) (result policy.Approval, resultErr error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, tx.Rollback())
		}
	}()
	var current string
	err = tx.QueryRowContext(ctx, "SELECT policy_id FROM project_policies WHERE scope = ?", scope).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if current != change.ExpectedID && current != change.Policy.ID {
		return result, policyMismatch("Approved policy changed; inspect the current policy and supply its expected_policy_id")
	}
	now, err := d.currentTime()
	if err != nil {
		return result, err
	}
	if current != change.Policy.ID {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM lease_policies p JOIN leases l ON l.lease_id = p.lease_id
WHERE p.scope = ? AND l.released_at IS NULL AND julianday(l.expires_at) > julianday(?)`, scope, formatHubTime(now)).Scan(&active); err != nil {
			return result, err
		}
		if active != 0 {
			return result, policyMismatch("Active leases retain their approved policy; finish or cancel them before approving a different revision")
		}
	}
	raw, err := json.Marshal(change.Policy)
	if err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO policy_revisions (scope, policy_id, metadata_json, approved_by, approved_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING", scope, change.Policy.ID, string(raw), actor, formatHubTime(now)); err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO project_policies (scope, policy_id) VALUES (?, ?) ON CONFLICT (scope) DO UPDATE SET policy_id = excluded.policy_id", scope, change.Policy.ID); err != nil {
		return result, err
	}
	result, err = readProjectPolicy(ctx, tx, scope)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

type policyQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readProjectPolicy(ctx context.Context, db policyQuerier, scope string) (policy.Approval, error) {
	var result policy.Approval
	var raw string
	err := db.QueryRowContext(ctx, `SELECT r.metadata_json, r.approved_by, r.approved_at
FROM project_policies p JOIN policy_revisions r ON r.scope = p.scope AND r.policy_id = p.policy_id WHERE p.scope = ?`, scope).Scan(&raw, &result.ApprovedBy, &result.ApprovedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return result, policyMismatch("No approved repository policy; resolve the customer definition and ask an administrator to approve its descriptor")
	}
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(raw), &result.Policy); err != nil {
		return result, err
	}
	return result, result.Policy.Validate()
}

func claimPolicyScope(query claimCandidateQuery) (string, error) {
	if query.NativeScope != nil {
		return string(query.NativeScope.organization) + "/" + string(query.NativeScope.project), nil
	}
	if len(query.Repositories) != 1 {
		return "", policyMismatch("A repository policy claim requires exactly one repository")
	}
	return "repository:" + strings.ToLower(strings.TrimSpace(query.Repositories[0])), nil
}

func validateClaimPolicy(ctx context.Context, tx *sql.Tx, query claimCandidateQuery, machine tracker.MachineID) (string, error) {
	scope, err := claimPolicyScope(query)
	if err != nil {
		return "", err
	}
	approval, err := readProjectPolicy(ctx, tx, scope)
	if err != nil {
		return "", err
	}
	if query.PolicyID == "" || query.PolicyID != approval.Policy.ID {
		return "", policyMismatch("Runner policy is missing or stale; load the approved repository definition and permitted local overrides before claiming work")
	}
	var runnerID string
	if query.NativeScope != nil {
		runnerID = query.NativeScope.credential.Runner.RunnerID
	}
	var tags []string
	if runnerID == "" && (approval.Policy.Requirements.RunnerID != "" || approval.Policy.Requirements.MachineID != "" || len(approval.Policy.Requirements.RequiredTags) != 0) {
		return "", &nativeError{Code: "selector_no_match", Message: "Constrained routing requires an administrator-enrolled runner; legacy registration cannot assert trusted host identity or tags", status: http.StatusConflict}
	}
	if runnerID != "" {
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT tags_json FROM runner_identities WHERE id = ? AND machine_id = ? AND organization_id = ?", runnerID, machine, query.NativeScope.organization).Scan(&raw); err != nil {
			return "", err
		}
		if err := json.Unmarshal([]byte(raw), &tags); err != nil {
			return "", err
		}
	}
	if err := approval.Policy.Requirements.Match(runnerID, string(machine), tags); err != nil {
		return "", &nativeError{Code: "selector_no_match", Message: err.Error(), status: http.StatusConflict}
	}
	return scope, nil
}

func (d *database) leasePolicyID(ctx context.Context, lease tracker.LeaseID) (string, error) {
	var id string
	err := d.db.QueryRowContext(ctx, "SELECT policy_id FROM lease_policies WHERE lease_id = ?", lease).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func requireApprovedLeasePolicy(ctx context.Context, tx *sql.Tx, lease tracker.LeaseID, required bool) error {
	var pinned, approved string
	err := tx.QueryRowContext(ctx, `SELECT l.policy_id, coalesce(p.policy_id, '') FROM lease_policies l
LEFT JOIN project_policies p ON p.scope = l.scope WHERE l.lease_id = ?`, lease).Scan(&pinned, &approved)
	if errors.Is(err, sql.ErrNoRows) {
		if required {
			return policyMismatch("Legacy lease has no pinned policy; release it and request a new approved claim")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if pinned != approved {
		return policyMismatch("Pinned policy has been revoked; stop the attempt and obtain administrator approval before restarting")
	}
	return requireLeaseRouting(ctx, tx, lease)
}

func requireLeaseRouting(ctx context.Context, tx *sql.Tx, lease tracker.LeaseID) error {
	var runner, machine, tags, state, raw string
	var access bool
	err := tx.QueryRowContext(ctx, `SELECT r.id, r.machine_id, r.tags_json, r.state, p.metadata_json,
EXISTS (SELECT 1 FROM token_grants g WHERE g.token_id = r.token_id AND g.organization_id = i.organization_id AND g.project_id = i.project_id)
FROM lease_runners lr JOIN runner_identities r ON r.id = lr.runner_id JOIN leases l ON l.lease_id = lr.lease_id
JOIN issues i ON i.id = l.issue_id JOIN lease_policies lp ON lp.lease_id = l.lease_id
JOIN policy_revisions p ON p.scope = lp.scope AND p.policy_id = lp.policy_id WHERE lr.lease_id = ?`, lease).Scan(&runner, &machine, &tags, &state, &raw, &access)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !access {
		return nativeNotFound()
	}
	if state == "disabled" {
		return &nativeError{Code: "runner_disabled", Message: "Runner is disabled", status: http.StatusConflict}
	}
	var descriptor policy.Descriptor
	var authorizedTags []string
	if err := json.Unmarshal([]byte(raw), &descriptor); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(tags), &authorizedTags); err != nil {
		return err
	}
	if err := descriptor.Requirements.Match(runner, machine, authorizedTags); err != nil {
		return &nativeError{Code: "selector_no_match", Message: err.Error(), status: http.StatusConflict}
	}
	return nil
}
