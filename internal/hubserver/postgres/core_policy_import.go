package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/changerequest"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type CorePolicyImportSource struct {
	BundlePath     string
	neutral        *VerifiedNeutralImport
	CopyPath       string
	OrganizationID tracker.OrganizationID
	Objects        attachment.DetailStore
}

type CorePolicyImportResult struct {
	PolicyRevisions int64
	CurrentPolicies int64
	ReviewPolicies  int64
	MonthlyPolicies int64
}

type CoreMonthlyBudgetFactsResult struct {
	Admissions int64
	Leases     int64
}

type projectPolicyImportRow struct {
	Scope      string
	ProjectID  tracker.ProjectID
	PolicyID   string
	Descriptor policy.Descriptor
	ApprovedBy string
	ApprovedAt time.Time
	Current    string
}

type reviewPolicySourceReceipt struct {
	PolicyJSON string `json:"policy_json"`
}

var legacyReviewPolicyImportTime = time.Unix(0, 0).UTC()

func (r *Repository) ImportCorePolicies(ctx context.Context, source CorePolicyImportSource) (CorePolicyImportResult, error) {
	if (source.CopyPath == "") == (source.BundlePath == "") || source.OrganizationID == "" || source.Objects == nil {
		return CorePolicyImportResult{}, errors.New("invalid core policy import source")
	}
	var result CorePolicyImportResult
	if source.BundlePath != "" {
		err := ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *VerifiedNeutralImport) error {
			source.neutral = bundle
			if err := r.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
				return err
			}
			var err error
			result.PolicyRevisions, result.CurrentPolicies, err = r.importProjectPolicies(ctx, nil, source)
			if err != nil {
				return err
			}
			result.ReviewPolicies, err = r.importCurrentReviewPolicies(ctx, nil, source)
			if err != nil {
				return err
			}
			result.MonthlyPolicies, err = r.importMonthlyBudgetPolicies(ctx, nil, source)
			return err
		})
		return result, err
	}
	err := ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, copyHash string) error {
		if err := r.PinCoreImportSource(ctx, source.OrganizationID, copyHash, source.Objects); err != nil {
			return err
		}
		var err error
		result.PolicyRevisions, result.CurrentPolicies, err = r.importProjectPolicies(ctx, tx, source)
		if err != nil {
			return err
		}
		result.ReviewPolicies, err = r.importCurrentReviewPolicies(ctx, tx, source)
		if err != nil {
			return err
		}
		result.MonthlyPolicies, err = r.importMonthlyBudgetPolicies(ctx, tx, source)
		return err
	})
	return result, err
}

type coreReviewPolicyImportRow struct {
	project tracker.ProjectID
	raw     string
	rules   tracker.ChangeReviewPolicy
}

func (r *Repository) importCurrentReviewPolicies(ctx context.Context, tx *sql.Tx, source CorePolicyImportSource) (int64, error) {
	content := eventstore.NewContent(source.Objects)
	var count int64
	var after tracker.ProjectID
	for {
		var page []coreReviewPolicyImportRow
		var err error
		if source.neutral != nil {
			page, err = neutralCoreReviewPolicies(ctx, source, after)
		} else {
			var rows *sql.Rows
			rows, err = tx.QueryContext(ctx, `SELECT project_id,policy_json FROM change_review_policies
WHERE organization_id=? AND project_id>? ORDER BY project_id LIMIT 100`, source.OrganizationID, after)
			if err != nil {
				return count, err
			}
			page, err = readCoreImportPage(rows, func(rows *sql.Rows) (coreReviewPolicyImportRow, error) {
				var row coreReviewPolicyImportRow
				if err := rows.Scan(&row.project, &row.raw); err != nil {
					return coreReviewPolicyImportRow{}, err
				}
				if row.project == "" || json.Unmarshal([]byte(row.raw), &row.rules) != nil || row.rules.PolicyID == "" {
					return coreReviewPolicyImportRow{}, eventstore.ErrInvalidRecord
				}
				expectedID := changerequest.PolicyID(row.rules)
				if expectedID == "" || row.rules.ID != "" && row.rules.ID != expectedID {
					return coreReviewPolicyImportRow{}, mutation.ErrConflict
				}
				row.rules.ID = expectedID
				return row, nil
			})
		}
		if err != nil {
			return count, err
		}
		if len(page) == 0 {
			return count, nil
		}
		for _, sourceRow := range page {
			var policyRevision policystore.Revision
			if err := r.ReadTenant(ctx, source.OrganizationID, corestore.Background, func(reader tenantstore.Reader) error {
				var readErr error
				policyRevision, readErr = reader.ProjectPolicyRevision(ctx, sourceRow.project, sourceRow.rules.PolicyID)
				return readErr
			}); err != nil {
				return count, fmt.Errorf("read imported review policy dependency: %w", err)
			}
			descriptor, err := content.ProjectPolicy(ctx, policyRevision)
			if err != nil {
				return count, fmt.Errorf("verify imported review policy dependency: %w", err)
			}
			if err := changerequest.ValidatePolicy(sourceRow.rules, descriptor); err != nil {
				return count, fmt.Errorf("validate imported review policy: %w", err)
			}
			row, err := content.PrepareReviewPolicy(ctx, eventstore.ReviewPolicy{
				OrganizationID:   source.OrganizationID,
				ProjectID:        sourceRow.project,
				PolicyRevisionID: sourceRow.rules.PolicyID,
				ReviewPolicyID:   sourceRow.rules.ID,
				Revision:         1,
				UpdatedBy:        "production-import",
				UpdatedAt:        legacyReviewPolicyImportTime,
			}, sourceRow.rules)
			if err != nil {
				return count, err
			}
			receiptRaw, err := json.Marshal(reviewPolicySourceReceipt{PolicyJSON: sourceRow.raw})
			if err != nil {
				return count, err
			}
			receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{
				OrganizationID: source.OrganizationID,
				ActorID:        "production-import",
				Operation:      "core-review-policy-source",
				CommandKey:     string(sourceRow.project),
				RequestHash:    artifact.Digest([]byte(sourceRow.raw)),
				CreatedAt:      legacyReviewPolicyImportTime,
			}, receiptRaw)
			if err != nil {
				return count, err
			}
			if err := r.transaction(ctx, source.OrganizationID, corestore.Background, false, func(s *session) error {
				current, currentErr := s.queries.PolicyGetReviewCurrent(ctx, pgquery.PolicyGetReviewCurrentParams{OrganizationID: source.OrganizationID, ProjectID: sourceRow.project})
				expected := ""
				if currentErr == nil {
					currentPolicy := reviewPolicyRecord(pgquery.ChangeReviewPolicyRevision(current))
					if !sameReviewPolicy(currentPolicy, row) {
						return corestore.ErrRevisionConflict
					}
					expected = currentPolicy.ReviewPolicyID
				} else if !errors.Is(currentErr, pgx.ErrNoRows) {
					return mapError(ctx, currentErr)
				}
				if err := s.SetReviewPolicy(ctx, row, expected); err != nil {
					return err
				}
				storedReceipt, err := s.PutReceipt(ctx, receipt)
				if err != nil {
					return err
				}
				if storedReceipt.RequestHash != receipt.RequestHash {
					return corestore.ErrRevisionConflict
				}
				return nil
			}); err != nil {
				return count, fmt.Errorf("import current review policy: %w", err)
			}
			count++
			after = sourceRow.project
		}
	}
}

func (r *Repository) ImportCoreMonthlyBudgetFacts(ctx context.Context, source CorePolicyImportSource) (CoreMonthlyBudgetFactsResult, error) {
	if (source.CopyPath == "") == (source.BundlePath == "") || source.OrganizationID == "" || source.Objects == nil {
		return CoreMonthlyBudgetFactsResult{}, errors.New("invalid core monthly budget import source")
	}
	var result CoreMonthlyBudgetFactsResult
	if source.BundlePath != "" {
		err := ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *VerifiedNeutralImport) error {
			source.neutral = bundle
			if err := r.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
				return err
			}
			var err error
			result.Admissions, err = r.importMonthlyBudgetAdmissions(ctx, nil, source)
			if err != nil {
				return err
			}
			result.Leases, err = r.importMonthlyBudgetLeases(ctx, nil, source)
			return err
		})
		return result, err
	}
	err := ReadCoreImportCopy(ctx, source.CopyPath, func(tx *sql.Tx, copyHash string) error {
		if err := r.PinCoreImportSource(ctx, source.OrganizationID, copyHash, source.Objects); err != nil {
			return err
		}
		var err error
		result.Admissions, err = r.importMonthlyBudgetAdmissions(ctx, tx, source)
		if err != nil {
			return err
		}
		result.Leases, err = r.importMonthlyBudgetLeases(ctx, tx, source)
		return err
	})
	return result, err
}

func (r *Repository) importProjectPolicies(ctx context.Context, tx *sql.Tx, source CorePolicyImportSource) (int64, int64, error) {
	content := eventstore.NewContent(source.Objects)
	prefix := string(source.OrganizationID) + "/"
	var revisionCount int64
	var afterScope, afterID string
	for {
		var page []projectPolicyImportRow
		var err error
		if source.neutral != nil {
			page, err = neutralCorePolicyRevisions(ctx, source, afterScope, afterID)
		} else {
			var rows *sql.Rows
			rows, err = tx.QueryContext(ctx, `SELECT scope,policy_id,metadata_json,approved_by,approved_at,gate_github_pull_request
FROM policy_revisions
WHERE substr(scope,1,length(?))=? AND (scope>? OR (scope=? AND policy_id>?))
ORDER BY scope,policy_id LIMIT 100`, prefix, prefix, afterScope, afterScope, afterID)
			if err != nil {
				return revisionCount, 0, err
			}
			page, err = readCoreImportPage(rows, func(rows *sql.Rows) (projectPolicyImportRow, error) {
				var row projectPolicyImportRow
				var encoded, approvedAt string
				var sourceGate sql.NullBool
				if err := rows.Scan(&row.Scope, &row.PolicyID, &encoded, &row.ApprovedBy, &approvedAt, &sourceGate); err != nil {
					return projectPolicyImportRow{}, err
				}
				row.ProjectID, err = projectPolicyScope(source.OrganizationID, row.Scope)
				if err != nil {
					return projectPolicyImportRow{}, err
				}
				if err := json.Unmarshal([]byte(encoded), &row.Descriptor); err != nil {
					return projectPolicyImportRow{}, fmt.Errorf("decode imported project policy: %w", err)
				}
				if row.Descriptor.ID != row.PolicyID {
					return projectPolicyImportRow{}, mutation.ErrConflict
				}
				if _, err := workflowconfig.ResolveNativePolicy(row.Descriptor); err != nil {
					return projectPolicyImportRow{}, fmt.Errorf("validate imported project policy: %w", err)
				}
				if sourceGate.Valid && sourceGate.Bool != row.Descriptor.Gates.GitHubPullRequest {
					return projectPolicyImportRow{}, mutation.ErrConflict
				}
				row.ApprovedAt, err = time.Parse(time.RFC3339Nano, approvedAt)
				if err != nil || row.ApprovedBy == "" || row.ApprovedAt.IsZero() {
					return projectPolicyImportRow{}, eventstore.ErrInvalidRecord
				}
				return row, nil
			})
		}
		if err != nil {
			return revisionCount, 0, err
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			var current string
			var pointerErr error
			if source.neutral != nil {
				current, pointerErr = neutralCoreCurrentPolicy(ctx, source, row.Scope)
			} else {
				pointerErr = tx.QueryRowContext(ctx, `SELECT policy_id FROM project_policies WHERE scope=?`, row.Scope).Scan(&current)
			}
			if errors.Is(pointerErr, sql.ErrNoRows) || errors.Is(pointerErr, corestore.ErrNotFound) {
				current = ""
			} else if pointerErr != nil {
				return revisionCount, 0, pointerErr
			}
			prepared, err := content.PrepareProjectPolicy(ctx, source.OrganizationID, row.Descriptor)
			if err != nil {
				return revisionCount, 0, err
			}
			prepared.ProjectID = row.ProjectID
			prepared.ApprovedBy = row.ApprovedBy
			prepared.ApprovedAt = row.ApprovedAt
			approved := row.ApprovedBy != "runner-report" || current == row.PolicyID
			if err := r.transaction(ctx, source.OrganizationID, corestore.Background, false, func(s *session) error {
				return s.ImportProjectPolicyRevision(ctx, prepared, approved)
			}); err != nil {
				return revisionCount, 0, fmt.Errorf("import project policy revision: %w", err)
			}
			afterScope, afterID = row.Scope, row.PolicyID
			revisionCount++
		}
	}
	var currentCount int64
	var after string
	for {
		var page [][2]string
		var err error
		if source.neutral != nil {
			page, err = neutralCorePolicyPointers(ctx, source, after)
		} else {
			var rows *sql.Rows
			rows, err = tx.QueryContext(ctx, `SELECT scope,policy_id FROM project_policies
WHERE substr(scope,1,length(?))=? AND scope>? ORDER BY scope LIMIT 100`, prefix, prefix, after)
			if err != nil {
				return revisionCount, currentCount, err
			}
			page, err = readCoreImportPage(rows, func(rows *sql.Rows) ([2]string, error) {
				var scope, policyID string
				if err := rows.Scan(&scope, &policyID); err != nil {
					return [2]string{}, err
				}
				project, err := projectPolicyScope(source.OrganizationID, scope)
				if err != nil {
					return [2]string{}, err
				}
				return [2]string{string(project), policyID}, nil
			})
		}
		if err != nil {
			return revisionCount, currentCount, err
		}
		if len(page) == 0 {
			break
		}
		for _, pointer := range page {
			if err := r.transaction(ctx, source.OrganizationID, corestore.Background, false, func(s *session) error {
				return s.ImportProjectPolicyCurrent(ctx, tracker.ProjectID(pointer[0]), pointer[1])
			}); err != nil {
				return revisionCount, currentCount, fmt.Errorf("import current project policy: %w", err)
			}
			after = string(source.OrganizationID) + "/" + pointer[0]
			currentCount++
		}
	}
	return revisionCount, currentCount, nil
}

func projectPolicyScope(organization tracker.OrganizationID, scope string) (tracker.ProjectID, error) {
	prefix := string(organization) + "/"
	if !strings.HasPrefix(scope, prefix) {
		return "", mutation.ErrConflict
	}
	project := strings.TrimPrefix(scope, prefix)
	if project == "" || strings.Contains(project, "/") {
		return "", mutation.ErrConflict
	}
	return tracker.ProjectID(project), nil
}

func (r *Repository) importMonthlyBudgetPolicies(ctx context.Context, tx *sql.Tx, source CorePolicyImportSource) (int64, error) {
	var count int64
	var afterProject string
	var afterRevision int64
	for {
		var page []billingstore.MonthlyPolicy
		var err error
		if source.neutral != nil {
			page, err = neutralCoreMonthlyPolicies(ctx, source, afterProject, afterRevision)
		} else {
			var rows *sql.Rows
			rows, err = tx.QueryContext(ctx, `SELECT project_id,revision,policy_json,actor_id,updated_at FROM monthly_budget_policies
WHERE organization_id=? AND (project_id>? OR (project_id=? AND revision>?)) ORDER BY project_id,revision LIMIT 100`, source.OrganizationID, afterProject, afterProject, afterRevision)
			if err != nil {
				return count, err
			}
			page, err = readCoreImportPage(rows, func(rows *sql.Rows) (billingstore.MonthlyPolicy, error) {
				var row billingstore.MonthlyPolicy
				var encoded, updatedAt string
				if err := rows.Scan(&row.ProjectID, &row.Revision, &encoded, &row.Actor, &updatedAt); err != nil {
					return billingstore.MonthlyPolicy{}, err
				}
				row.OrganizationID = source.OrganizationID
				if err := json.Unmarshal([]byte(encoded), &row.Policy); err != nil {
					return billingstore.MonthlyPolicy{}, fmt.Errorf("decode imported monthly budget policy: %w", err)
				}
				row.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
				if err != nil || row.Revision < 1 {
					return billingstore.MonthlyPolicy{}, eventstore.ErrInvalidRecord
				}
				if err := row.Policy.Validate(); err != nil {
					return billingstore.MonthlyPolicy{}, err
				}
				return row, nil
			})
		}
		if err != nil {
			return count, err
		}
		if len(page) == 0 {
			return count, nil
		}
		for _, row := range page {
			if err := r.transaction(ctx, source.OrganizationID, corestore.Background, false, func(s *session) error {
				return s.ImportBillingMonthlyPolicy(ctx, row)
			}); err != nil {
				return count, fmt.Errorf("import monthly budget policy: %w", err)
			}
			afterProject, afterRevision = string(row.ProjectID), int64(row.Revision)
			count++
		}
	}
}

func (r *Repository) importMonthlyBudgetAdmissions(ctx context.Context, tx *sql.Tx, source CorePolicyImportSource) (int64, error) {
	var count int64
	var after int64
	for {
		var page []billingstore.MonthlyAdmission
		var err error
		if source.neutral != nil {
			page, err = neutralCoreMonthlyAdmissions(ctx, source, after)
		} else {
			var rows *sql.Rows
			rows, err = tx.QueryContext(ctx, `SELECT a.issue_id,a.admitted_at FROM monthly_budget_admissions a
JOIN issues i ON i.id=a.issue_id WHERE i.organization_id=? AND a.issue_id>? ORDER BY a.issue_id LIMIT 100`, source.OrganizationID, after)
			if err != nil {
				return count, err
			}
			page, err = readCoreImportPage(rows, func(rows *sql.Rows) (billingstore.MonthlyAdmission, error) {
				var row billingstore.MonthlyAdmission
				var admittedAt string
				if err := rows.Scan(&row.IssueID, &admittedAt); err != nil {
					return billingstore.MonthlyAdmission{}, err
				}
				row.OrganizationID = source.OrganizationID
				row.AdmittedAt, err = time.Parse(time.RFC3339Nano, admittedAt)
				if err != nil || row.IssueID < 1 || row.AdmittedAt.IsZero() {
					return billingstore.MonthlyAdmission{}, eventstore.ErrInvalidRecord
				}
				return row, nil
			})
		}
		if err != nil {
			return count, err
		}
		if len(page) == 0 {
			return count, nil
		}
		for _, row := range page {
			if err := r.transaction(ctx, source.OrganizationID, corestore.Background, false, func(s *session) error {
				return s.ImportBillingMonthlyAdmission(ctx, row)
			}); err != nil {
				return count, fmt.Errorf("import monthly budget admission: %w", err)
			}
			after = int64(row.IssueID)
			count++
		}
	}
}

func (r *Repository) importMonthlyBudgetLeases(ctx context.Context, tx *sql.Tx, source CorePolicyImportSource) (int64, error) {
	var count int64
	var after string
	for {
		var page []billingstore.MonthlyLease
		var err error
		if source.neutral != nil {
			page, err = neutralCoreMonthlyLeases(ctx, source, after)
		} else {
			var rows *sql.Rows
			rows, err = tx.QueryContext(ctx, `SELECT b.lease_id,b.exposure_json FROM monthly_budget_leases b
JOIN leases l ON l.lease_id=b.lease_id JOIN issues i ON i.id=l.issue_id
WHERE i.organization_id=? AND b.lease_id>? ORDER BY b.lease_id LIMIT 100`, source.OrganizationID, after)
			if err != nil {
				return count, err
			}
			page, err = readCoreImportPage(rows, func(rows *sql.Rows) (billingstore.MonthlyLease, error) {
				var row billingstore.MonthlyLease
				var encoded string
				if err := rows.Scan(&row.LeaseID, &encoded); err != nil {
					return billingstore.MonthlyLease{}, err
				}
				var exposure budget.CostExposure
				if err := json.Unmarshal([]byte(encoded), &exposure); err != nil {
					return billingstore.MonthlyLease{}, fmt.Errorf("decode monthly budget lease exposure: %w", err)
				}
				row.OrganizationID = source.OrganizationID
				row.LunaAPI, row.RunnerAPI, row.SpriteInfrastructure = exposure.LunaAPI, exposure.RunnerAPI, exposure.SpriteInfrastructure
				if row.LeaseID == "" {
					return billingstore.MonthlyLease{}, eventstore.ErrInvalidRecord
				}
				return row, nil
			})
		}
		if err != nil {
			return count, err
		}
		if len(page) == 0 {
			return count, nil
		}
		for _, row := range page {
			if err := r.transaction(ctx, source.OrganizationID, corestore.Background, false, func(s *session) error {
				return s.RecordBillingMonthlyLease(ctx, row)
			}); err != nil {
				return count, fmt.Errorf("import monthly budget lease: %w", err)
			}
			after = string(row.LeaseID)
			count++
		}
	}
}

var _ policystore.Writer = (*session)(nil)

func readCoreImportPage[T any](rows *sql.Rows, read func(*sql.Rows) (T, error)) ([]T, error) {
	defer rows.Close()
	page := make([]T, 0, 100)
	for rows.Next() {
		row, err := read(rows)
		if err != nil {
			return nil, err
		}
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return page, nil
}
