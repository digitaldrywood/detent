package hostedplans

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type PlanCommand struct {
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

type GrantRecord struct {
	HostedGrant
	Reason    string     `json:"reason"`
	GrantedBy string     `json:"granted_by"`
	GrantedAt *time.Time `json:"granted_at,omitempty"`
}

type EntitlementReport struct {
	OrganizationID   string              `json:"organization_id"`
	Base             PlanReference       `json:"base"`
	EffectiveBase    PlanReference       `json:"effective_base"`
	Source           string              `json:"source"`
	Revision         int64               `json:"revision"`
	Features         []string            `json:"features"`
	Grants           []GrantRecord `json:"grants"`
	GrantHistory     []GrantRecord `json:"grant_history"`
	NextGrantHistory string              `json:"next_grant_history,omitempty"`
	Plans            []HostedPlan        `json:"plans"`
}

func (d *Service) Entitlements(ctx context.Context, historyAfter int64, historyLimit int) (EntitlementReport, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	var report EntitlementReport
	if d.Plans() == nil {
		return report, hubapi.NotFound()
	}
	now, err := d.currentTime()
	if err != nil {
		return report, err
	}
	entitlement, err := d.Entitlement(ctx, nil, now)
	if err != nil {
		return report, err
	}
	report = EntitlementReport{OrganizationID: entitlement.OrganizationID, Base: entitlement.Base, EffectiveBase: entitlement.EffectiveBase, Source: entitlement.Source, Revision: entitlement.Revision, Features: entitlement.Features, Grants: []GrantRecord{}, GrantHistory: []GrantRecord{}, Plans: []HostedPlan{}}
	report.Plans, err = d.ReadPlans(ctx, nil)
	if err != nil {
		return report, err
	}

	if d.billingAuditStore == nil {
		return report, errors.New("hosted plan audit storage is unavailable")
	}
	history, next, err := d.ReadGrantAudits(ctx, historyAfter, historyLimit)
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

func (d *Service) finishHostedEntitlementReport(report EntitlementReport, entitlement HostedEntitlement, now time.Time) EntitlementReport {
	for _, grant := range entitlement.Grants {
		if grant.RevokedAt == nil && !grant.StartsAt.After(now) && (grant.ExpiresAt == nil || grant.ExpiresAt.After(now)) {
			report.Grants = append(report.Grants, GrantRecord{HostedGrant: grant})
		}
	}
	return report
}

func (d *Service) mergeHostedGrantAuditDetails(report *EntitlementReport, history []GrantRecord) {
	byID := make(map[string]GrantRecord, len(history))
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

func (d *Service) ReadGrantAudits(ctx context.Context, after int64, limit int) ([]GrantRecord, int64, error) {
	granted := make([]GrantRecord, 0, limit)
	var audits []billingstore.PlanAudit
	var hasMore bool
	err := d.ReadBillingTenant(ctx, func(reader tenantstore.Reader) error {
		var err error
		audits, err = reader.BillingPlanGrantAudits(ctx, after, limit)
		if err != nil || len(audits) < limit {
			return err
		}
		following, err := reader.BillingPlanGrantAudits(ctx, audits[len(audits)-1].ID, 1)
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
		var command PlanCommand
		if err := json.Unmarshal(archive, &command); err != nil {
			return nil, 0, err
		}
		grant, err := d.ReadGrant(ctx, command.GrantID)
		if err != nil {
			return nil, 0, err
		}
		granted = append(granted, GrantRecord{HostedGrant: grant, Reason: command.Reason, GrantedBy: audit.ActorID, GrantedAt: &audit.RecordedAt})
	}
	return granted, next, nil
}

func (d *Service) ReadGrant(ctx context.Context, id string) (HostedGrant, error) {

	var result HostedGrant
	err := d.ReadBillingTenant(ctx, func(reader tenantstore.Reader) error {
		grant, err := reader.BillingGrant(ctx, id)
		if err != nil {
			return err
		}
		result = HostedGrant{ID: grant.ID, Plan: PlanReference{ID: grant.Plan.ID, Version: grant.Plan.Version}, Scope: grant.Scope, StartsAt: grant.StartsAt, ExpiresAt: grant.ExpiresAt, RevokedAt: grant.RevokedAt}
		return nil
	})
	return result, err
}

func (d *Service) ApplyPlanCommand(ctx context.Context, actor string, command PlanCommand) error {
	if d.hostedBilling && (command.Action == "subscription" || command.Action == "end_subscription") {
		return hubapi.PublicInvalid("Stripe reconciliation owns subscription access; use separate complimentary grants for operator access")
	}
	if d.Plans() == nil || !SafeID(command.ID) || strings.TrimSpace(command.Reason) == "" || len(command.Reason) > 500 || command.ExpectedRevision < 1 || !slices.Contains([]string{"base", "subscription", "end_subscription", "grant", "revoke", "create_plan"}, command.Action) {
		return hubapi.PublicInvalid("A command ID, revision, action and bounded audit reason are required")
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
	return d.ApplyPlanCommandRecord(ctx, actor, command, hash, raw, now)
}

func (d *Service) ApplyPlanCommandRecord(ctx context.Context, actor string, command PlanCommand, hash string, raw []byte, now time.Time) error {
	if d.billingAuditStore == nil {
		return errors.New("hosted plan audit storage is unavailable")
	}
	archive, err := d.billingAuditStore.StoreDetail(ctx, string(d.hostedOrganization), raw)
	if err != nil {
		return err
	}
	reference := billingstore.AuditReferenceFromArchive(archive)
	return d.WriteBillingTenant(ctx, func(writer tenantstore.Writer) error {
		previous, err := writer.BillingPlanAudit(ctx, command.ID)
		if err == nil {
			if previous.RequestHash != hash {
				return hubapi.Conflict(tracker.Revision(command.ExpectedRevision))
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
			return hubapi.Conflict(tracker.Revision(assignment.Revision))
		}
		if slices.Contains([]string{"base", "subscription", "grant"}, command.Action) {
			if _, err := d.ReadPlanRecord(ctx, writer, command.Plan); err != nil {
				return hubapi.PublicInvalid("The plan version must exist")
			}
		}
		switch command.Action {
		case "create_plan":
			if command.Definition == nil || command.Plan != command.Definition.PlanReference || validateHostedPlan(*command.Definition) != nil {
				return hubapi.PublicInvalid("A valid new plan definition matching the plan reference is required")
			}
			plan := NormalizePlan(*command.Definition)
			var price *int64
			if plan.MonthlyUSDCents != nil {
				value := *plan.MonthlyUSDCents
				price = &value
			}
			if err := writer.CreateBillingPlan(ctx, billingstore.Plan{OrganizationID: d.hostedOrganization, PlanReference: billingstore.PlanReference{ID: plan.ID, Version: plan.Version}, Name: plan.Name, MonthlyUSDCents: price, Allowances: plan.Allowances, Features: plan.Features}); err != nil {
				if errors.Is(err, corestore.ErrUniqueViolation) {
					return hubapi.Conflict(tracker.Revision(assignment.Revision))
				}
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
				return hubapi.PublicInvalid("Subscription-derived access requires a future validity deadline")
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
			startsAt, err := ValidateGrantCommand(command, now)
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

func ValidateGrantCommand(command PlanCommand, now time.Time) (time.Time, error) {
	if !SafeID(command.GrantID) || len(command.Scope) == 0 || len(command.Scope) > len(AllowanceNames())+len(FeatureNames()) {
		return time.Time{}, hubapi.PublicInvalid("A grant ID and explicit allowance or feature scope are required")
	}
	for _, scope := range command.Scope {
		if !slices.Contains(AllowanceNames(), scope) && !slices.Contains(FeatureNames(), scope) {
			return time.Time{}, hubapi.PublicInvalid("Grant scope is unknown")
		}
	}
	startsAt := command.StartsAt
	if startsAt.IsZero() {
		startsAt = now.UTC()
	}
	if command.ExpiresAt != nil && (!command.ExpiresAt.After(now) || !command.ExpiresAt.After(startsAt)) {
		return time.Time{}, hubapi.PublicInvalid("Grant expiry must follow its start and the current time")
	}
	return startsAt, nil
}
