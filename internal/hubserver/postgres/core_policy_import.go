package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
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
	if source.CopyPath != "" {
		return CorePolicyImportResult{},
			ErrNeutralImport
	}

	if source.BundlePath == "" || source.OrganizationID == "" || source.Objects == nil {
		return CorePolicyImportResult{}, errors.New("invalid core policy import source")
	}
	var result CorePolicyImportResult

	err := ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *VerifiedNeutralImport) error {
		if err := validateNeutralCoreSourceSchema(bundle, "policy_revisions", "project_policies", "change_review_policies", "monthly_budget_policies"); err != nil {
			return err
		}
		source.neutral = bundle
		if err := r.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
			return err
		}
		var err error
		result.PolicyRevisions, result.CurrentPolicies, err = r.importProjectPolicies(ctx, source)
		if err != nil {
			return err
		}
		result.ReviewPolicies, err = r.importCurrentReviewPolicies(ctx, source)
		if err != nil {
			return err
		}
		result.MonthlyPolicies, err = r.importMonthlyBudgetPolicies(ctx, source)
		return err
	})
	return result, err
}

type coreReviewPolicyImportRow struct {
	project tracker.ProjectID
	raw     string
	rules   tracker.ChangeReviewPolicy
}

func (r *Repository) importCurrentReviewPolicies(ctx context.Context, source CorePolicyImportSource) (int64, error) {
	content := eventstore.NewContent(source.Objects)
	var count int64
	var after tracker.ProjectID
	for {
		var page []coreReviewPolicyImportRow
		var err error

		page, err = neutralCoreReviewPolicies(ctx, source, after)

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
	if source.CopyPath != "" {
		return CoreMonthlyBudgetFactsResult{}, ErrNeutralImport

	}

	if source.BundlePath == "" || source.OrganizationID == "" || source.Objects == nil {
		return CoreMonthlyBudgetFactsResult{}, errors.New("invalid core monthly budget import source")
	}
	var result CoreMonthlyBudgetFactsResult

	err := ReadNeutralImportBundle(ctx, source.BundlePath, source.OrganizationID, func(bundle *VerifiedNeutralImport) error {
		if err := validateNeutralCoreSourceSchema(bundle, "issues", "leases", "monthly_budget_admissions", "monthly_budget_leases"); err != nil {
			return err
		}
		source.neutral = bundle
		if err := r.PinVerifiedNeutralImport(ctx, bundle, source.Objects); err != nil {
			return err
		}
		var err error
		result.Admissions, err = r.importMonthlyBudgetAdmissions(ctx, source)
		if err != nil {
			return err
		}
		result.Leases, err = r.importMonthlyBudgetLeases(ctx, source)
		return err
	})
	return result, err
}

func (r *Repository) importProjectPolicies(ctx context.Context, source CorePolicyImportSource) (int64, int64, error) {
	content := eventstore.NewContent(source.Objects)
	var revisionCount int64
	var afterScope, afterID string
	for {
		var page []projectPolicyImportRow
		var err error

		page, err = neutralCorePolicyRevisions(ctx, source, afterScope, afterID)

		if err != nil {
			return revisionCount, 0, err
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			var current string
			var pointerErr error

			current, pointerErr = neutralCoreCurrentPolicy(ctx, source, row.Scope)

			if errors.Is(pointerErr, corestore.ErrNotFound) {
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

		page, err = neutralCorePolicyPointers(ctx, source, after)

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

func (r *Repository) importMonthlyBudgetPolicies(ctx context.Context, source CorePolicyImportSource) (int64, error) {
	var count int64
	var afterProject string
	var afterRevision int64
	for {
		var page []billingstore.MonthlyPolicy
		var err error

		page, err = neutralCoreMonthlyPolicies(ctx, source, afterProject, afterRevision)

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

func (r *Repository) importMonthlyBudgetAdmissions(ctx context.Context, source CorePolicyImportSource) (int64, error) {
	var count int64
	var after int64
	for {
		var page []billingstore.MonthlyAdmission
		var err error

		page, err = neutralCoreMonthlyAdmissions(ctx, source, after)

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

func (r *Repository) importMonthlyBudgetLeases(ctx context.Context, source CorePolicyImportSource) (int64, error) {
	var count int64
	var after string
	for {
		var page []billingstore.MonthlyLease
		var err error

		page, err = neutralCoreMonthlyLeases(ctx, source, after)

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
