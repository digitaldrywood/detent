package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
)

const (
	goTestReceiptRetention   = 30 * 24 * time.Hour
	goTestReceiptPruneRounds = 20
)

func (s *Service) lookupGoTestReceipts(c echo.Context) error {
	key := c.QueryParam("key")
	if !gotestreceipt.ValidKey(key) {
		return s.nativeAPIError(c, nativeInvalid("key must be a lowercase sha256 hex digest"))
	}
	scope := nativeRequestScope(c)
	candidates, err := readGoTestReceipts(c.Request().Context(), s.database.reader, string(scope.organization), string(scope.project), key)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{"candidates": candidates})
}

func (s *Service) recordGoTestReceipt(c echo.Context) error {
	var receipt gotestreceipt.Receipt
	if err := decodeAPIJSON(c, &receipt); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := receipt.Validate(); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.ID == "" || credential.Runner.RunnerID == "" && credential.Scope != apiScopeOperator && credential.Scope != apiScopeAdmin {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope := nativeRequestScope(c)
	var reporter string
	err := s.database.reader.QueryRowContext(c.Request().Context(), `SELECT runner_id FROM go_test_receipt_executions WHERE id=? AND organization_id=? AND project_id=? AND credential_id=?`, receipt.ExecutionID, scope.organization, scope.project, credential.ID).Scan(&reporter)
	if err != nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if err := storeGoTestReceipt(c.Request().Context(), s.database.db, string(scope.organization), string(scope.project), reporter, receipt, s.config.now()); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func readGoTestReceipts(ctx context.Context, query nativeQueryer, organization, project, key string) ([]gotestreceipt.Candidate, error) {
	rows, err := query.QueryContext(ctx, `SELECT r.inputs, r.inputs_digest FROM (
 SELECT inputs, inputs_digest, execution_id, organization_id, project_id, recorded_at_ns
 FROM go_test_receipts WHERE organization_id=? AND project_id=? AND binary_key=? ORDER BY recorded_at_ns DESC LIMIT ?
) r JOIN go_test_receipt_executions e ON e.id=r.execution_id AND e.organization_id=r.organization_id AND e.project_id=r.project_id
ORDER BY r.recorded_at_ns DESC`, organization, project, key, gotestreceipt.MaxCandidates)
	if err != nil {
		return nil, fmt.Errorf("read go test receipts: %w", err)
	}
	defer rows.Close()
	candidates := []gotestreceipt.Candidate{}
	for rows.Next() {
		var inputs, digest string
		if err := rows.Scan(&inputs, &digest); err != nil {
			return nil, fmt.Errorf("scan go test receipt: %w", err)
		}
		candidate := gotestreceipt.Candidate{Inputs: []string{}, Digest: digest}
		if inputs != "" {
			candidate.Inputs = strings.Split(inputs, "\n")
		}
		if err := gotestreceipt.ValidateInputs(candidate.Inputs); err != nil {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read go test receipts: %w", err)
	}
	return candidates, nil
}

func storeGoTestReceipt(ctx context.Context, exec hostedExecer, organization, project, reporter string, receipt gotestreceipt.Receipt, now time.Time) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO go_test_receipts (organization_id, project_id, binary_key, inputs_digest, inputs, package, runner_id, recorded_at_ns, execution_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(organization_id, project_id, binary_key, inputs_digest) DO UPDATE SET package=excluded.package, runner_id=excluded.runner_id, recorded_at_ns=excluded.recorded_at_ns, execution_id=excluded.execution_id`,
		organization, project, receipt.Key, receipt.Digest, strings.Join(receipt.Inputs, "\n"), receipt.Package, reporter, now.UnixNano(), receipt.ExecutionID)
	if err != nil {
		return fmt.Errorf("store go test receipt: %w", err)
	}
	return nil
}

func (d *database) expireGoTestReceipts(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-goTestReceiptRetention).UnixNano()
	for range goTestReceiptPruneRounds {
		result, err := d.db.ExecContext(ctx, "DELETE FROM go_test_receipts WHERE rowid IN (SELECT rowid FROM go_test_receipts WHERE recorded_at_ns < ? ORDER BY recorded_at_ns LIMIT ?)", cutoff, commandReceiptPruneBatch)
		if err != nil {
			return fmt.Errorf("expire go test receipts: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count expired go test receipts: %w", err)
		}
		if deleted < commandReceiptPruneBatch {
			_, err := d.db.ExecContext(ctx, "DELETE FROM go_test_receipt_executions WHERE id IN (SELECT id FROM go_test_receipt_executions WHERE created_at_ns < ? ORDER BY created_at_ns LIMIT ?)", cutoff, commandReceiptPruneBatch)
			if err != nil {
				return fmt.Errorf("expire receipt executions: %w", err)
			}
			return nil
		}
	}
	return nil
}

func (s *Service) beginGoTestReceiptExecution(c echo.Context) error {
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.ID == "" || credential.Runner.RunnerID == "" && credential.Scope != apiScopeOperator && credential.Scope != apiScopeAdmin {
		return s.nativeAPIError(c, nativeNotFound())
	}
	scope := nativeRequestScope(c)
	key, err := s.config.SecretKeys.Derive("detent:go-test-receipts:v2:" + string(scope.organization) + ":" + string(scope.project))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	execution := gotestreceipt.Execution{ID: newNativeID("gtex"), DigestKey: key}
	runner := credential.Runner.RunnerID
	if runner == "" {
		runner = credential.ID
	}
	_, err = s.database.db.ExecContext(c.Request().Context(), `INSERT INTO go_test_receipt_executions (id,organization_id,project_id,credential_id,runner_id,created_at_ns) VALUES (?,?,?,?,?,?)`, execution.ID, scope.organization, scope.project, credential.ID, runner, s.config.now().UnixNano())
	if err != nil {
		return s.nativeAPIError(c, fmt.Errorf("create receipt execution: %w", err))
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusCreated, execution)
}
