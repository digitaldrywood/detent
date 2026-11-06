package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type HostedBillingPlanSourceParity struct {
	Source                 billingstore.Plan
	SourceSHA256           string
	Expected               *billingstore.Plan
	ExpectedSHA256         string
	SeedAuditCommandID     string
	SeedAuditRequestHash   string
	SeedAuditArchiveSHA256 string
}

type HostedBillingSourceParityRepair struct {
	OrganizationID       tracker.OrganizationID
	SourceManifestSHA256 string
	SourceDatabaseSHA256 string
	SourceStateSHA256    string
	ExpectedStateSHA256  string
	SourceAssignment     billingstore.Assignment
	ExpectedAssignment   billingstore.Assignment
	Plans                []HostedBillingPlanSourceParity
	SourceGrants         []billingstore.Grant
	ExpectedGrants       []billingstore.Grant
	SourcePlanAudits     []billingstore.PlanAudit
}

type HostedBillingSourceParityResult struct {
	PlansCreated   int
	PlansRepaired  int
	AssignmentsSet bool
	GrantsCreated  int
	AuditsCreated  int
}

var ErrHostedBillingSourceParity = errors.New("hosted billing source parity is invalid")

func HostedBillingSourceParityDigests(repair HostedBillingSourceParityRepair) (string, string, error) {
	if err := validateHostedBillingSourceParityContent(repair); err != nil {
		return "", "", err
	}
	source, err := hostedBillingParitySourceDigest(repair)
	if err != nil {
		return "", "", err
	}
	expected, err := hostedBillingParityExpectedDigest(repair)
	if err != nil {
		return "", "", err
	}
	return source, expected, nil
}

func SealHostedBillingSourceParityRepair(repair *HostedBillingSourceParityRepair) error {
	if repair == nil {
		return ErrHostedBillingSourceParity
	}
	source, expected, err := HostedBillingSourceParityDigests(*repair)
	if err != nil {
		return err
	}
	repair.SourceStateSHA256 = source
	repair.ExpectedStateSHA256 = expected
	return nil
}

func (r *Repository) RepairHostedBillingSourceParity(ctx context.Context, repair HostedBillingSourceParityRepair) (HostedBillingSourceParityResult, error) {
	var result HostedBillingSourceParityResult
	if err := validateHostedBillingSourceParity(repair); err != nil {
		return result, err
	}
	sourceDigest, err := hostedBillingParitySourceDigest(repair)
	if err != nil || sourceDigest != repair.SourceStateSHA256 {
		return result, ErrHostedBillingSourceParity
	}
	expectedDigest, err := hostedBillingParityExpectedDigest(repair)
	if err != nil || expectedDigest != repair.ExpectedStateSHA256 {
		return result, ErrHostedBillingSourceParity
	}
	err = r.transactionIsolation(ctx, repair.OrganizationID, corestore.Background, false, pgx.Serializable, func(s *session) error {
		assignment, err := lockBillingAssignment(ctx, s, repair.OrganizationID)
		if err != nil {
			return err
		}
		currentGrants, err := lockBillingGrants(ctx, s, repair.OrganizationID)
		if err != nil {
			return err
		}
		if !billingGrantSetMatchesSourceOrExpected(currentGrants, repair.SourceGrants, repair.ExpectedGrants) {
			return corestore.ErrRevisionConflict
		}
		assignmentMatchesSource := sameBillingAssignmentValues(assignment, repair.SourceAssignment) && assignment.Revision >= repair.SourceAssignment.Revision
		if !assignmentMatchesSource && !sameBillingAssignmentExact(assignment, repair.ExpectedAssignment) {
			return corestore.ErrRevisionConflict
		}
		for _, planRepair := range repair.Plans {
			current, found, err := lockBillingPlan(ctx, s, repair.OrganizationID, planRepair.Source.PlanReference)
			if err != nil {
				return err
			}
			if found && sameBillingPlan(current, planRepair.Source) {
				continue
			}
			if planRepair.Expected == nil {
				if found {
					return corestore.ErrRevisionConflict
				}
				if err := s.CreateBillingPlan(ctx, planRepair.Source); err != nil {
					return err
				}
				result.PlansCreated++
				continue
			}
			if !found || !sameBillingPlan(current, *planRepair.Expected) {
				return corestore.ErrRevisionConflict
			}
			if err := verifySeedPlanAudit(ctx, s, repair.OrganizationID, planRepair); err != nil {
				return err
			}
			if err := replaceBillingPlan(ctx, s, planRepair.Source); err != nil {
				return err
			}
			result.PlansRepaired++
		}
		for _, grant := range repair.SourceGrants {
			current, exists := currentGrants[grant.ID]
			if exists {
				if !sameBillingGrant(current, grant) {
					return corestore.ErrRevisionConflict
				}
				continue
			}
			if _, expected := billingGrantMap(repair.ExpectedGrants)[grant.ID]; expected {
				return corestore.ErrRevisionConflict
			}
			grant = normalizeBillingParityGrantStorage(grant)
			if err := s.CreateBillingGrant(ctx, grant); err != nil {
				return err
			}
			result.GrantsCreated++
		}
		if !assignmentMatchesSource {
			if err := setBillingAssignmentSource(ctx, s, assignment, normalizeBillingParityAssignmentStorage(repair.SourceAssignment)); err != nil {
				return err
			}
			result.AssignmentsSet = true
		}
		for _, audit := range repair.SourcePlanAudits {
			previous, err := s.BillingPlanAudit(ctx, audit.CommandID)
			if err == nil {
				if !sameBillingPlanAudit(previous, audit) {
					return corestore.ErrRevisionConflict
				}
				continue
			}
			if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			created, err := s.CreateBillingPlanAudit(ctx, normalizeBillingParityAuditStorage(audit))
			if err != nil {
				return err
			}
			if created {
				result.AuditsCreated++
			}
		}
		return nil
	})
	return result, err
}

func validateHostedBillingSourceParity(repair HostedBillingSourceParityRepair) error {
	if err := validateHostedBillingSourceParityContent(repair); err != nil {
		return ErrHostedBillingSourceParity
	}
	if !isSHA256(repair.SourceStateSHA256) || !isSHA256(repair.ExpectedStateSHA256) {
		return ErrHostedBillingSourceParity
	}
	return nil
}

func validateHostedBillingSourceParityContent(repair HostedBillingSourceParityRepair) error {
	if repair.OrganizationID == "" || repair.SourceAssignment.OrganizationID != repair.OrganizationID || repair.ExpectedAssignment.OrganizationID != repair.OrganizationID || repair.SourceAssignment.Revision < 1 || repair.ExpectedAssignment.Revision < 1 || !isSHA256(repair.SourceManifestSHA256) || !isSHA256(repair.SourceDatabaseSHA256) || len(repair.Plans) > 100 || len(repair.SourceGrants) > 1000 || len(repair.ExpectedGrants) > 1000 || len(repair.SourcePlanAudits) > 1000 {
		return ErrHostedBillingSourceParity
	}
	planRefs := make(map[billingstore.PlanReference]struct{}, len(repair.Plans))
	for _, plan := range repair.Plans {
		if plan.Source.OrganizationID != repair.OrganizationID || !validPlanParityRecord(plan.Source) {
			return ErrHostedBillingSourceParity
		}
		sourceDigest, err := billingPlanParityDigest(plan.Source)
		if err != nil || sourceDigest != plan.SourceSHA256 {
			return ErrHostedBillingSourceParity
		}
		if _, ok := planRefs[plan.Source.PlanReference]; ok {
			return ErrHostedBillingSourceParity
		}
		planRefs[plan.Source.PlanReference] = struct{}{}
		if plan.Expected == nil {
			if plan.ExpectedSHA256 != "" || plan.SeedAuditCommandID != "" || plan.SeedAuditRequestHash != "" || plan.SeedAuditArchiveSHA256 != "" {
				return ErrHostedBillingSourceParity
			}
			continue
		}
		if plan.Expected.OrganizationID != repair.OrganizationID || plan.Expected.PlanReference != plan.Source.PlanReference || !validPlanParityRecord(*plan.Expected) {
			return ErrHostedBillingSourceParity
		}
		expectedDigest, err := billingPlanParityDigest(*plan.Expected)
		if err != nil || expectedDigest != plan.ExpectedSHA256 {
			return ErrHostedBillingSourceParity
		}
		if plan.SeedAuditCommandID != "seed_"+plan.Source.ID+"_"+billingVersion(plan.Source.Version) || !isSHA256(plan.SeedAuditRequestHash) || !isSHA256(plan.SeedAuditArchiveSHA256) {
			return ErrHostedBillingSourceParity
		}
	}
	if !validAssignmentParity(repair.SourceAssignment) || !validAssignmentParity(repair.ExpectedAssignment) || !validGrantParitySet(repair.OrganizationID, repair.SourceGrants) || !validGrantParitySet(repair.OrganizationID, repair.ExpectedGrants) || !validPlanAuditParitySet(repair.OrganizationID, repair.SourcePlanAudits) {
		return ErrHostedBillingSourceParity
	}
	return nil
}

func validAssignmentParity(row billingstore.Assignment) bool {
	if row.OrganizationID == "" || row.Revision < 1 || !validPlanReference(row.Base) {
		return false
	}
	if row.Subscription == nil {
		return row.ExpiresAt == nil
	}
	return validPlanReference(*row.Subscription) && row.ExpiresAt != nil && !row.ExpiresAt.IsZero()
}

func validPlanParityRecord(row billingstore.Plan) bool {
	if row.OrganizationID == "" || !validPlanReference(row.PlanReference) || strings.TrimSpace(row.Name) != row.Name || len(row.Name) > 80 || row.MonthlyUSDCents != nil && *row.MonthlyUSDCents < 0 {
		return false
	}
	for name, amount := range row.Allowances {
		if strings.TrimSpace(name) == "" || amount < 0 {
			return false
		}
	}
	return true
}

func validPlanReference(ref billingstore.PlanReference) bool {
	return strings.TrimSpace(ref.ID) == ref.ID && ref.ID != "" && len(ref.ID) <= 128 && ref.Version > 0
}

func validGrantParitySet(org tracker.OrganizationID, grants []billingstore.Grant) bool {
	seen := make(map[string]struct{}, len(grants))
	for _, grant := range grants {
		if grant.OrganizationID != org || strings.TrimSpace(grant.ID) != grant.ID || grant.ID == "" || len(grant.ID) > 128 || !validPlanReference(grant.Plan) || grant.StartsAt.IsZero() || grant.RevokedAt != nil && grant.RevokedAt.IsZero() || grant.ExpiresAt != nil && grant.ExpiresAt.IsZero() {
			return false
		}
		if _, ok := seen[grant.ID]; ok {
			return false
		}
		seen[grant.ID] = struct{}{}
		if len(grant.Scope) == 0 {
			return false
		}
		for _, scope := range grant.Scope {
			if strings.TrimSpace(scope) != scope || scope == "" {
				return false
			}
		}
	}
	return true
}

func validPlanAuditParitySet(org tracker.OrganizationID, audits []billingstore.PlanAudit) bool {
	seen := make(map[string]struct{}, len(audits))
	for _, audit := range audits {
		if audit.OrganizationID != org || strings.TrimSpace(audit.CommandID) != audit.CommandID || audit.CommandID == "" || !isSHA256(audit.RequestHash) || audit.ActorID == "" || audit.Action == "" || audit.RecordedAt.IsZero() || audit.Reference.Key == "" || !isSHA256(audit.Reference.SHA256) || audit.Reference.CompressedBytes <= 0 || audit.Reference.ContentBytes < 0 || audit.Reference.Codec == "" || audit.Reference.FormatVersion <= 0 || audit.Reference.Kind == "" || audit.Reference.Retention == "" {
			return false
		}
		if _, ok := seen[audit.CommandID]; ok {
			return false
		}
		seen[audit.CommandID] = struct{}{}
	}
	return true
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func HostedBillingPlanParityDigest(plan billingstore.Plan) (string, error) {
	return billingPlanParityDigest(plan)
}

func billingPlanParityDigest(plan billingstore.Plan) (string, error) {
	plan = normalizeBillingParityPlan(plan)
	raw, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	return digestBillingParity(raw), nil
}

func hostedBillingParitySourceDigest(repair HostedBillingSourceParityRepair) (string, error) {
	plans := make([]billingstore.Plan, 0, len(repair.Plans))
	for _, plan := range repair.Plans {
		plans = append(plans, normalizeBillingParityPlan(plan.Source))
	}
	slices.SortFunc(plans, func(left, right billingstore.Plan) int {
		return comparePlanReference(left.PlanReference, right.PlanReference)
	})
	grants := normalizeBillingParityGrants(repair.SourceGrants)
	audits := normalizeBillingParityAudits(repair.SourcePlanAudits)
	value := struct {
		Organization tracker.OrganizationID
		Manifest     string
		Database     string
		Assignment   billingstore.Assignment
		Plans        []billingstore.Plan
		Grants       []billingstore.Grant
		Audits       []billingstore.PlanAudit
	}{repair.OrganizationID, repair.SourceManifestSHA256, repair.SourceDatabaseSHA256, normalizeBillingParityAssignment(repair.SourceAssignment), plans, grants, audits}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return digestBillingParity(raw), nil
}

func hostedBillingParityExpectedDigest(repair HostedBillingSourceParityRepair) (string, error) {
	plans := make([]struct {
		Reference billingstore.PlanReference
		Expected  *billingstore.Plan
		Digest    string
		SeedID    string
		SeedHash  string
		SeedSHA   string
	}, 0, len(repair.Plans))
	for _, plan := range repair.Plans {
		var expected *billingstore.Plan
		if plan.Expected != nil {
			normalized := normalizeBillingParityPlan(*plan.Expected)
			expected = &normalized
		}
		plans = append(plans, struct {
			Reference billingstore.PlanReference
			Expected  *billingstore.Plan
			Digest    string
			SeedID    string
			SeedHash  string
			SeedSHA   string
		}{plan.Source.PlanReference, expected, plan.ExpectedSHA256, plan.SeedAuditCommandID, plan.SeedAuditRequestHash, plan.SeedAuditArchiveSHA256})
	}
	slices.SortFunc(plans, func(left, right struct {
		Reference billingstore.PlanReference
		Expected  *billingstore.Plan
		Digest    string
		SeedID    string
		SeedHash  string
		SeedSHA   string
	}) int {
		return comparePlanReference(left.Reference, right.Reference)
	})
	grants := normalizeBillingParityGrants(repair.ExpectedGrants)
	value := struct {
		Organization tracker.OrganizationID
		Assignment   billingstore.Assignment
		Plans        []struct {
			Reference billingstore.PlanReference
			Expected  *billingstore.Plan
			Digest    string
			SeedID    string
			SeedHash  string
			SeedSHA   string
		}
		Grants []billingstore.Grant
	}{repair.OrganizationID, normalizeBillingParityAssignment(repair.ExpectedAssignment), plans, grants}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return digestBillingParity(raw), nil
}

func digestBillingParity(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func normalizeBillingParityPlan(row billingstore.Plan) billingstore.Plan {
	row.Features = slices.Clone(row.Features)
	slices.Sort(row.Features)
	if row.MonthlyUSDCents != nil {
		value := *row.MonthlyUSDCents
		row.MonthlyUSDCents = &value
	}
	return row
}

func normalizeBillingParityAssignment(row billingstore.Assignment) billingstore.Assignment {
	if row.Subscription != nil {
		value := *row.Subscription
		row.Subscription = &value
	}
	if row.ExpiresAt != nil {
		value := row.ExpiresAt.UTC()
		row.ExpiresAt = &value
	}
	return row
}

func normalizeBillingParityAssignmentStorage(row billingstore.Assignment) billingstore.Assignment {
	row = normalizeBillingParityAssignment(row)
	row.ExpiresAt = recordTimePointer(row.ExpiresAt)
	return row
}

func normalizeBillingParityGrants(rows []billingstore.Grant) []billingstore.Grant {
	result := slices.Clone(rows)
	for index := range result {
		result[index].Scope = slices.Clone(result[index].Scope)
		slices.Sort(result[index].Scope)
		if result[index].ExpiresAt != nil {
			value := result[index].ExpiresAt.UTC()
			result[index].ExpiresAt = &value
		}
		if result[index].RevokedAt != nil {
			value := result[index].RevokedAt.UTC()
			result[index].RevokedAt = &value
		}
		result[index].StartsAt = result[index].StartsAt.UTC()
	}
	slices.SortFunc(result, func(left, right billingstore.Grant) int { return strings.Compare(left.ID, right.ID) })
	return result
}

func normalizeBillingParityGrantStorage(row billingstore.Grant) billingstore.Grant {
	row.Scope = slices.Clone(row.Scope)
	slices.Sort(row.Scope)
	row.StartsAt = recordTime(row.StartsAt)
	row.ExpiresAt = recordTimePointer(row.ExpiresAt)
	row.RevokedAt = recordTimePointer(row.RevokedAt)
	return row
}

func normalizeBillingParityAuditStorage(row billingstore.PlanAudit) billingstore.PlanAudit {
	row.RecordedAt = recordTime(row.RecordedAt)
	return row
}

func normalizeBillingParityAudits(rows []billingstore.PlanAudit) []billingstore.PlanAudit {
	result := slices.Clone(rows)
	slices.SortFunc(result, func(left, right billingstore.PlanAudit) int { return strings.Compare(left.CommandID, right.CommandID) })
	return result
}

func comparePlanReference(left, right billingstore.PlanReference) int {
	if comparison := strings.Compare(left.ID, right.ID); comparison != 0 {
		return comparison
	}
	if left.Version < right.Version {
		return -1
	}
	if left.Version > right.Version {
		return 1
	}
	return 0
}

func sameBillingAssignmentExact(left, right billingstore.Assignment) bool {
	return sameBillingAssignmentValues(left, right) && left.Revision == right.Revision
}

func sameBillingAssignmentValues(left, right billingstore.Assignment) bool {
	if left.OrganizationID != right.OrganizationID || left.Base != right.Base || (left.Subscription == nil) != (right.Subscription == nil) {
		return false
	}
	if left.Subscription != nil && *left.Subscription != *right.Subscription {
		return false
	}
	return sameOptionalBillingTime(left.ExpiresAt, right.ExpiresAt)
}

func sameBillingPlan(left, right billingstore.Plan) bool {
	left, right = normalizeBillingParityPlan(left), normalizeBillingParityPlan(right)
	if left.OrganizationID != right.OrganizationID || left.PlanReference != right.PlanReference || left.Name != right.Name || left.RuntimeID != right.RuntimeID || !maps.Equal(left.Allowances, right.Allowances) || !slices.Equal(left.Features, right.Features) {
		return false
	}
	if left.MonthlyUSDCents == nil || right.MonthlyUSDCents == nil {
		return left.MonthlyUSDCents == nil && right.MonthlyUSDCents == nil
	}
	return *left.MonthlyUSDCents == *right.MonthlyUSDCents
}

func sameBillingGrant(left, right billingstore.Grant) bool {
	leftRows, rightRows := normalizeBillingParityGrants([]billingstore.Grant{left}), normalizeBillingParityGrants([]billingstore.Grant{right})
	left, right = leftRows[0], rightRows[0]
	return left.OrganizationID == right.OrganizationID && left.ID == right.ID && left.Plan == right.Plan && slices.Equal(left.Scope, right.Scope) && recordTime(left.StartsAt).Equal(recordTime(right.StartsAt)) && sameOptionalBillingTime(left.ExpiresAt, right.ExpiresAt) && sameOptionalBillingTime(left.RevokedAt, right.RevokedAt)
}

func sameBillingPlanAudit(left, right billingstore.PlanAudit) bool {
	return left.OrganizationID == right.OrganizationID && left.CommandID == right.CommandID && left.RequestHash == right.RequestHash && left.ActorID == right.ActorID && left.Action == right.Action && recordTime(left.RecordedAt).Equal(recordTime(right.RecordedAt)) && left.Reference == right.Reference
}

func sameOptionalBillingTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return recordTime(*left).Equal(recordTime(*right))
}

func lockBillingAssignment(ctx context.Context, s *session, organization tracker.OrganizationID) (billingstore.Assignment, error) {
	var row billingstore.Assignment
	row.OrganizationID = organization
	var subscription pgtype.Text
	var version pgtype.Int8
	err := s.db.QueryRow(ctx, `SELECT base_id,base_version,subscription_id,subscription_version,subscription_expires_at,revision FROM hosted_plan_assignments WHERE organization_id=$1 FOR UPDATE`, organization).Scan(&row.Base.ID, &row.Base.Version, &subscription, &version, &row.ExpiresAt, &row.Revision)
	if err != nil {
		return billingstore.Assignment{}, mapError(ctx, err)
	}
	if subscription.Valid && version.Valid {
		row.Subscription = &billingstore.PlanReference{ID: subscription.String, Version: version.Int64}
	}
	return row, nil
}

func lockBillingPlan(ctx context.Context, s *session, organization tracker.OrganizationID, reference billingstore.PlanReference) (billingstore.Plan, bool, error) {
	var found string
	err := s.db.QueryRow(ctx, `SELECT id FROM hosted_plans WHERE organization_id=$1 AND id=$2 AND version=$3 FOR UPDATE`, organization, reference.ID, reference.Version).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return billingstore.Plan{}, false, nil
	}
	if err != nil {
		return billingstore.Plan{}, false, mapError(ctx, err)
	}
	row, err := s.BillingPlan(ctx, reference)
	return row, err == nil, err
}

func lockBillingGrants(ctx context.Context, s *session, organization tracker.OrganizationID) (map[string]billingstore.Grant, error) {
	rows, err := s.db.Query(ctx, `SELECT id FROM hosted_complimentary_grants WHERE organization_id=$1 ORDER BY id LIMIT 1001 FOR UPDATE`, organization)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	count := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, mapError(ctx, err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, mapError(ctx, err)
	}
	rows.Close()
	if count > 1000 {
		return nil, ErrHostedBillingSourceParity
	}
	result := make(map[string]billingstore.Grant)
	for after := ""; ; {
		page, err := s.BillingGrants(ctx, after, 100)
		if err != nil {
			return nil, err
		}
		for _, grant := range page {
			result[grant.ID] = grant
		}
		if len(page) < 100 {
			return result, nil
		}
		after = page[len(page)-1].ID
	}
}

func billingGrantMap(rows []billingstore.Grant) map[string]billingstore.Grant {
	result := make(map[string]billingstore.Grant, len(rows))
	for _, row := range rows {
		result[row.ID] = row
	}
	return result
}

func billingGrantSetMatchesSourceOrExpected(current map[string]billingstore.Grant, sources, expected []billingstore.Grant) bool {
	sourceMap := billingGrantMap(sources)
	expectedMap := billingGrantMap(expected)
	for id, row := range current {
		if source, ok := sourceMap[id]; ok && sameBillingGrant(row, source) {
			continue
		}
		want, ok := expectedMap[id]
		if !ok || !sameBillingGrant(row, want) {
			return false
		}
	}
	for id := range expectedMap {
		if _, ok := current[id]; !ok {
			return false
		}
	}
	for id, row := range sourceMap {
		if currentRow, ok := current[id]; ok && !sameBillingGrant(currentRow, row) {
			return false
		}
	}
	return true
}

func verifySeedPlanAudit(ctx context.Context, s *session, organization tracker.OrganizationID, plan HostedBillingPlanSourceParity) error {
	audit, err := s.BillingPlanAudit(ctx, plan.SeedAuditCommandID)
	if err != nil {
		return err
	}
	if audit.OrganizationID != organization || audit.ActorID != "configuration" || audit.Action != "create_plan" || audit.RequestHash != plan.SeedAuditRequestHash || audit.Reference.SHA256 != plan.SeedAuditArchiveSHA256 {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func replaceBillingPlan(ctx context.Context, s *session, plan billingstore.Plan) error {
	var monthly pgtype.Int8
	if plan.MonthlyUSDCents != nil {
		monthly = pgtype.Int8{Int64: *plan.MonthlyUSDCents, Valid: true}
	}
	if _, err := s.db.Exec(ctx, `UPDATE hosted_plans SET name=$1,monthly_usd_cents=$2,runtime_id=$3 WHERE organization_id=$4 AND id=$5 AND version=$6`, plan.Name, monthly, plan.RuntimeID, s.organization, plan.ID, plan.Version); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM hosted_plan_allowances WHERE organization_id=$1 AND plan_id=$2 AND plan_version=$3`, s.organization, plan.ID, plan.Version); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM hosted_plan_features WHERE organization_id=$1 AND plan_id=$2 AND plan_version=$3`, s.organization, plan.ID, plan.Version); err != nil {
		return mapError(ctx, err)
	}
	for metric, amount := range plan.Allowances {
		if _, err := s.db.Exec(ctx, `INSERT INTO hosted_plan_allowances(organization_id,plan_id,plan_version,metric,amount) VALUES($1,$2,$3,$4,$5)`, s.organization, plan.ID, plan.Version, metric, amount); err != nil {
			return mapError(ctx, err)
		}
	}
	for _, feature := range plan.Features {
		if _, err := s.db.Exec(ctx, `INSERT INTO hosted_plan_features(organization_id,plan_id,plan_version,feature) VALUES($1,$2,$3,$4)`, s.organization, plan.ID, plan.Version, feature); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func setBillingAssignmentSource(ctx context.Context, s *session, current, source billingstore.Assignment) error {
	var subscription *string
	var version pgtype.Int8
	if source.Subscription != nil {
		subscription = &source.Subscription.ID
		version = pgtype.Int8{Int64: source.Subscription.Version, Valid: true}
	}
	nextRevision := max(current.Revision+1, source.Revision)
	result, err := s.db.Exec(ctx, `UPDATE hosted_plan_assignments SET base_id=$1,base_version=$2,subscription_id=$3,subscription_version=$4,subscription_expires_at=$5,revision=$6 WHERE organization_id=$7 AND revision=$8 AND base_id=$9 AND base_version=$10 AND subscription_id IS NOT DISTINCT FROM $11 AND subscription_version IS NOT DISTINCT FROM $12 AND subscription_expires_at IS NOT DISTINCT FROM $13`, source.Base.ID, source.Base.Version, subscription, version, source.ExpiresAt, nextRevision, s.organization, current.Revision, current.Base.ID, current.Base.Version, nullableBillingReferenceID(current.Subscription), nullableBillingReferenceVersion(current.Subscription), current.ExpiresAt)
	if err != nil {
		return mapError(ctx, err)
	}
	count := result.RowsAffected()
	if err != nil || count != 1 {
		if err != nil {
			return mapError(ctx, err)
		}
		return corestore.ErrRevisionConflict
	}
	return nil
}

func nullableBillingReferenceID(value *billingstore.PlanReference) any {
	if value == nil {
		return nil
	}
	return value.ID
}

func nullableBillingReferenceVersion(value *billingstore.PlanReference) any {
	if value == nil {
		return nil
	}
	return value.Version
}

func billingVersion(version int64) string {
	return strconv.FormatInt(version, 10)
}
