package hubserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type hostedPlanCommand struct {
	ID               string        `json:"idempotency_key"`
	Action           string        `json:"action"`
	ExpectedRevision int64         `json:"expected_revision"`
	Plan             PlanReference `json:"plan"`
	Definition       *HostedPlan   `json:"definition,omitempty"`
	GrantID          string        `json:"grant_id"`
	Scope            []string      `json:"scope"`
	StartsAt         time.Time     `json:"starts_at"`
	ExpiresAt        *time.Time    `json:"expires_at"`
	Reason           string        `json:"reason"`
}

func (s *Service) entitlementAdministrator(c echo.Context) bool {
	token, err := apiBearerToken(c)
	supplied := sha256.Sum256([]byte(token))
	configured := sha256.Sum256(s.config.Hosted.EntitlementAdminToken)
	return err == nil && len(s.config.Hosted.EntitlementAdminToken) >= 32 && subtle.ConstantTimeCompare(supplied[:], configured[:]) == 1
}

type hostedGrantRecord struct {
	HostedGrant
	Reason    string     `json:"reason"`
	GrantedBy string     `json:"granted_by"`
	GrantedAt *time.Time `json:"granted_at,omitempty"`
}

type hostedEntitlementReport struct {
	OrganizationID   string              `json:"organization_id"`
	Base             PlanReference       `json:"base"`
	EffectiveBase    PlanReference       `json:"effective_base"`
	Source           string              `json:"source"`
	Revision         int64               `json:"revision"`
	Features         []string            `json:"features"`
	Grants           []hostedGrantRecord `json:"grants"`
	GrantHistory     []hostedGrantRecord `json:"grant_history"`
	NextGrantHistory string              `json:"next_grant_history,omitempty"`
	Plans            []HostedPlan        `json:"plans"`
}

func (s *Service) hostedPlanReport(c echo.Context) error {
	if !s.entitlementAdministrator(c) || c.Param("organization") != s.config.Hosted.OrganizationID {
		return s.hostedJSONError(c, http.StatusForbidden, "Entitlement administrator access is required")
	}
	after := int64(0)
	if value := c.QueryParam("history_after"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			return s.invalidAPIRequest(c, errors.New("invalid grant history cursor"))
		}
		after = parsed
	}
	limit := 100
	if value := c.QueryParam("history_limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100 {
			return s.invalidAPIRequest(c, errors.New("grant history limit must be between 1 and 100"))
		}
		limit = parsed
	}
	report, err := s.database.hostedEntitlementReport(c.Request().Context(), after, limit)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, report)
}

func (d *database) hostedEntitlementReport(ctx context.Context, historyAfter int64, historyLimit int) (hostedEntitlementReport, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	var report hostedEntitlementReport
	if d.plans() == nil {
		return report, nativeNotFound()
	}
	now, err := d.currentTime()
	if err != nil {
		return report, err
	}
	entitlement, err := d.hostedEntitlement(ctx, nil, now)
	if err != nil {
		return report, err
	}
	report = hostedEntitlementReport{OrganizationID: entitlement.OrganizationID, Base: entitlement.Base, EffectiveBase: entitlement.EffectiveBase, Source: entitlement.Source, Revision: entitlement.Revision, Features: entitlement.Features, Grants: []hostedGrantRecord{}, GrantHistory: []hostedGrantRecord{}, Plans: []HostedPlan{}}
	report.Plans, err = d.readHostedPlans(ctx, d.reader)
	if err != nil {
		return report, err
	}

	if d.billingAuditStore == nil {
		return report, errors.New("hosted plan audit storage is unavailable")
	}
	history, next, err := d.readHostedGrantAudits(ctx, historyAfter, historyLimit)
	if err != nil {
		return report, err
	}
	report = d.finishHostedEntitlementReport(report, entitlement, now)
	report.GrantHistory = history
	d.mergeHostedGrantAuditDetails(&report, history)
	if next > 0 {
		report.NextGrantHistory = strconv.FormatInt(next, 10)
	}
	return report, nil
}

func (d *database) finishHostedEntitlementReport(report hostedEntitlementReport, entitlement HostedEntitlement, now time.Time) hostedEntitlementReport {
	for _, grant := range entitlement.Grants {
		if grant.RevokedAt == nil && !grant.StartsAt.After(now) && (grant.ExpiresAt == nil || grant.ExpiresAt.After(now)) {
			report.Grants = append(report.Grants, hostedGrantRecord{HostedGrant: grant})
		}
	}
	return report
}

func (d *database) mergeHostedGrantAuditDetails(report *hostedEntitlementReport, history []hostedGrantRecord) {
	byID := make(map[string]hostedGrantRecord, len(history))
	for _, record := range history {
		byID[record.ID] = record
	}
	for i := range report.Grants {
		if record, ok := byID[report.Grants[i].ID]; ok {
			report.Grants[i].Reason = record.Reason
			report.Grants[i].GrantedBy = record.GrantedBy
			report.Grants[i].GrantedAt = record.GrantedAt
		}
	}
}

func (d *database) readHostedGrantAudits(ctx context.Context, after int64, limit int) ([]hostedGrantRecord, int64, error) {
	granted := make([]hostedGrantRecord, 0, limit)
	var audits []billingstore.PlanAudit
	var hasMore bool
	err := d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
		var err error
		audits, err = reader.BillingPlanAudits(ctx, after, limit)
		if err != nil || len(audits) < limit {
			return err
		}
		following, err := reader.BillingPlanAudits(ctx, audits[len(audits)-1].ID, 1)
		hasMore = len(following) > 0
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	var next int64
	if hasMore {
		next = audits[limit-1].ID
	}
	for _, audit := range audits {
		if audit.Action != "grant" {
			continue
		}
		archive, err := d.billingAuditStore.ReadDetail(ctx, string(d.hostedOrganization), audit.Reference.ArchiveReference())
		if err != nil {
			return nil, 0, err
		}
		var command hostedPlanCommand
		if err := json.Unmarshal(archive, &command); err != nil {
			return nil, 0, err
		}
		grant, err := d.readHostedGrant(ctx, command.GrantID)
		if err != nil {
			return nil, 0, err
		}
		granted = append(granted, hostedGrantRecord{HostedGrant: grant, Reason: command.Reason, GrantedBy: audit.ActorID, GrantedAt: &audit.RecordedAt})
	}
	return granted, next, nil
}

func (d *database) readHostedGrant(ctx context.Context, id string) (HostedGrant, error) {

	var result HostedGrant
	err := d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
		grant, err := reader.BillingGrant(ctx, id)
		if err != nil {
			return err
		}
		result = HostedGrant{ID: grant.ID, Plan: PlanReference{ID: grant.Plan.ID, Version: grant.Plan.Version}, Scope: grant.Scope, StartsAt: grant.StartsAt, ExpiresAt: grant.ExpiresAt, RevokedAt: grant.RevokedAt}
		return nil
	})
	return result, err
}

func (s *Service) updateHostedPlan(c echo.Context) error {
	if !s.entitlementAdministrator(c) || c.Param("organization") != s.config.Hosted.OrganizationID {
		return s.hostedJSONError(c, http.StatusForbidden, "Entitlement administrator access is required")
	}

	var command hostedPlanCommand
	if err := decodeAPIJSON(c, &command); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := s.database.applyHostedPlanCommand(c.Request().Context(), s.config.Hosted.EntitlementAdministrator, command); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (d *database) applyHostedPlanCommand(ctx context.Context, actor string, command hostedPlanCommand) error {
	if d.hostedBilling && (command.Action == "subscription" || command.Action == "end_subscription") {
		return nativePublicInvalid("Stripe reconciliation owns subscription access; use separate complimentary grants for operator access")
	}
	if d.plans() == nil || !hostedSafeID(command.ID) || strings.TrimSpace(command.Reason) == "" || len(command.Reason) > 500 || command.ExpectedRevision < 1 || !slices.Contains([]string{"base", "subscription", "end_subscription", "grant", "revoke", "create_plan"}, command.Action) {
		return nativePublicInvalid("A command ID, revision, action and bounded audit reason are required")
	}
	raw, err := json.Marshal(command)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	hash := hex.EncodeToString(digest[:])

	now, err := d.currentTime()
	if err != nil {
		return err
	}
	return d.applyHostedPlanCommandRecord(ctx, actor, command, hash, raw, now)
}

func (d *database) applyHostedPlanCommandRecord(ctx context.Context, actor string, command hostedPlanCommand, hash string, raw []byte, now time.Time) error {
	if d.billingAuditStore == nil {
		return errors.New("hosted plan audit storage is unavailable")
	}
	archive, err := d.billingAuditStore.StoreDetail(ctx, string(d.hostedOrganization), raw)
	if err != nil {
		return err
	}
	reference := billingstore.AuditReferenceFromArchive(archive)
	return d.writeBillingTenant(ctx, func(writer tenantstore.Writer) error {
		previous, err := writer.BillingPlanAudit(ctx, command.ID)
		if err == nil {
			if previous.RequestHash != hash {
				return nativeConflict(tracker.Revision(command.ExpectedRevision))
			}
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		assignment, err := writer.BillingAssignment(ctx)
		if err != nil {
			return err
		}
		if assignment.Revision != command.ExpectedRevision {
			return nativeConflict(tracker.Revision(assignment.Revision))
		}
		if slices.Contains([]string{"base", "subscription", "grant"}, command.Action) {
			if _, err := d.readHostedPlanRecord(ctx, writer, command.Plan); err != nil {
				return nativePublicInvalid("The plan version must exist")
			}
		}
		switch command.Action {
		case "create_plan":
			if command.Definition == nil || command.Plan != command.Definition.PlanReference || validateHostedPlan(*command.Definition) != nil {
				return nativePublicInvalid("A valid new plan definition matching the plan reference is required")
			}
			plan := normalizeHostedPlan(*command.Definition)
			var price *int64
			if plan.MonthlyUSDCents != nil {
				value := *plan.MonthlyUSDCents
				price = &value
			}
			if err := writer.CreateBillingPlan(ctx, billingstore.Plan{OrganizationID: d.hostedOrganization, PlanReference: billingstore.PlanReference{ID: plan.ID, Version: plan.Version}, Name: plan.Name, MonthlyUSDCents: price, Allowances: plan.Allowances, Features: plan.Features}); err != nil {
				return err
			}
			assignment.Revision++
			if err := writer.SetBillingAssignment(ctx, assignment, command.ExpectedRevision); err != nil {
				return err
			}
		case "base":
			assignment.Base = billingstore.PlanReference{ID: command.Plan.ID, Version: command.Plan.Version}
			assignment.Revision++
			if err := writer.SetBillingAssignment(ctx, assignment, command.ExpectedRevision); err != nil {
				return err
			}
		case "subscription":
			if command.ExpiresAt == nil || !command.ExpiresAt.After(now) {
				return nativePublicInvalid("Subscription-derived access requires a future validity deadline")
			}
			assignment.Subscription = &billingstore.PlanReference{ID: command.Plan.ID, Version: command.Plan.Version}
			assignment.ExpiresAt = command.ExpiresAt
			assignment.Revision++
			if err := writer.SetBillingAssignment(ctx, assignment, command.ExpectedRevision); err != nil {
				return err
			}
		case "end_subscription":
			assignment.Subscription = nil
			assignment.ExpiresAt = nil
			assignment.Revision++
			if err := writer.SetBillingAssignment(ctx, assignment, command.ExpectedRevision); err != nil {
				return err
			}
		case "grant":
			startsAt, err := validateHostedGrantCommand(command, now)
			if err != nil {
				return err
			}
			if err := writer.CreateBillingGrant(ctx, billingstore.Grant{OrganizationID: d.hostedOrganization, ID: command.GrantID, Plan: billingstore.PlanReference{ID: command.Plan.ID, Version: command.Plan.Version}, Scope: command.Scope, StartsAt: startsAt, ExpiresAt: command.ExpiresAt}); err != nil {
				return err
			}
			assignment.Revision++
			if err := writer.SetBillingAssignment(ctx, assignment, command.ExpectedRevision); err != nil {
				return err
			}
		case "revoke":
			if err := writer.RevokeBillingGrant(ctx, command.GrantID, now.UTC()); err != nil {
				return err
			}
			assignment.Revision++
			if err := writer.SetBillingAssignment(ctx, assignment, command.ExpectedRevision); err != nil {
				return err
			}
		}
		_, err = writer.CreateBillingPlanAudit(ctx, billingstore.PlanAudit{OrganizationID: d.hostedOrganization, CommandID: command.ID, RequestHash: hash, ActorID: actor, Action: command.Action, RecordedAt: now.UTC(), Reference: reference})
		return err
	})
}

func validateHostedGrantCommand(command hostedPlanCommand, now time.Time) (time.Time, error) {
	if !hostedSafeID(command.GrantID) || len(command.Scope) == 0 || len(command.Scope) > len(hostedAllowanceNames())+len(hostedFeatureNames()) {
		return time.Time{}, nativePublicInvalid("A grant ID and explicit allowance or feature scope are required")
	}
	for _, scope := range command.Scope {
		if !slices.Contains(hostedAllowanceNames(), scope) && !slices.Contains(hostedFeatureNames(), scope) {
			return time.Time{}, nativePublicInvalid("Grant scope is unknown")
		}
	}
	startsAt := command.StartsAt
	if startsAt.IsZero() {
		startsAt = now.UTC()
	}
	if command.ExpiresAt != nil && (!command.ExpiresAt.After(now) || !command.ExpiresAt.After(startsAt)) {
		return time.Time{}, nativePublicInvalid("Grant expiry must follow its start and the current time")
	}
	return startsAt, nil
}

func (d *database) auditHostedPlanCommand(ctx context.Context, tx *sql.Tx, actor string, command hostedPlanCommand, now time.Time) error {
	raw, err := json.Marshal(command)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	_, err = tx.ExecContext(ctx, "INSERT INTO hosted_plan_audit(command_id,request_hash,actor_id,reason,action,recorded_at,record_json) VALUES(?,?,?,?,?,?,?)", command.ID, hex.EncodeToString(digest[:]), actor, command.Reason, command.Action, formatHubTime(now), string(raw))
	return err
}

func insertHostedComplimentaryGrant(ctx context.Context, tx *sql.Tx, command hostedPlanCommand, now time.Time) error {
	if !hostedSafeID(command.GrantID) || len(command.Scope) == 0 || len(command.Scope) > len(hostedAllowanceNames())+len(hostedFeatureNames()) {
		return nativePublicInvalid("A grant ID and explicit allowance or feature scope are required")
	}
	for _, scope := range command.Scope {
		if !slices.Contains(hostedAllowanceNames(), scope) && !slices.Contains(hostedFeatureNames(), scope) {
			return nativePublicInvalid("Grant scope is unknown")
		}
	}
	if command.StartsAt.IsZero() {
		command.StartsAt = now.UTC()
	}
	if command.ExpiresAt != nil && (!command.ExpiresAt.After(now) || !command.ExpiresAt.After(command.StartsAt)) {
		return nativePublicInvalid("Grant expiry must follow its start and the current time")
	}
	raw, err := json.Marshal(HostedGrant{ID: command.GrantID, Plan: command.Plan, Scope: command.Scope, StartsAt: command.StartsAt, ExpiresAt: command.ExpiresAt})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO hosted_complimentary_grants(id,record_json) VALUES(?,?)", command.GrantID, string(raw))
	return err
}

func hostedRunnerTransactionMetrics(path string) []string {
	switch path {
	case nativeBase + "/leases/:lease/validate", nativeBase + "/claims/preview":
		return []string{"events_total", "usage_windows"}
	case nativeBase + "/machines/:machine/heartbeat":
		return []string{"connected_runners", "events_total", "usage_windows", "collaboration_bytes"}
	}
	return nil
}

func hostedNativeMutationMetrics(input any, completion bool) []string {
	metrics := []string{"events_total", "usage_windows"}
	if _, runEvent := input.(tracker.NativeRunEvent); !runEvent {
		metrics = append(metrics, "unarchived_issues", "projects", "repositories", "registered_runners")
		if !completion {
			metrics = append(metrics, "connected_runners")
		}
	}
	if !completion {
		metrics = append(metrics, "collaboration_bytes", "history_records")
	}
	return metrics
}

func hostedCompletionMutation(c echo.Context, input any) bool {
	if strings.HasSuffix(c.Path(), "/work-items/:item/archive") {
		return true
	}
	if event, ok := input.(tracker.NativeRunEvent); ok {
		return event.Type == "run.finished" || event.Type == "run.checkpointed"
	}
	return false
}

func hostedMutationFeature(c echo.Context) string {
	if strings.Contains(c.Path(), "/integration") || strings.Contains(c.Path(), "/imports") {
		return "github_integration"
	}
	return "collaboration"
}
