package postgres_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBillingPlansKeepHistoricalTermsAndTenantScope(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		start := time.Date(2024, 2, 29, 23, 59, 59, 123456000, time.UTC)
		end := start.AddDate(0, 1, 0)
		plan := billingstore.Plan{OrganizationID: org, PlanReference: billingstore.PlanReference{ID: "example", Version: 7}, Name: "Historical plan", MonthlyUSDCents: new(int64(1234)), RuntimeID: "runtime_example", Allowances: map[string]int64{"collaboration_bytes": 98765, "requests": 10}, Features: []string{"collaboration", "native_execution"}}
		assignment := billingstore.Assignment{OrganizationID: org, Base: plan.PlanReference, Subscription: &plan.PlanReference, ExpiresAt: &end, Revision: 1}
		grant := billingstore.Grant{OrganizationID: org, ID: "grant_example", Plan: plan.PlanReference, Scope: []string{"collaboration"}, StartsAt: start, ExpiresAt: &end}
		account := billingstore.Account{OrganizationID: org, Mode: "live", Binding: billing.Binding{OrganizationID: string(org), AccountID: "account_example", CustomerID: "customer_" + string(org)}, Status: "active", Plan: plan.PlanReference, PaidThrough: end, AccessUntil: end, Snapshot: billing.Snapshot{SubscriptionID: "subscription_example", PriceID: "price_example", PeriodStart: start, PeriodEnd: end, InvoiceCreatedAt: start, Status: "active"}}
		policy := billingstore.MonthlyPolicy{OrganizationID: org, Revision: 1, Policy: budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, TotalMicros: new(int64(100))}, Actor: "example", UpdatedAt: start}
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateBillingPlan(t.Context(), plan); err != nil {
				return err
			}
			if err := w.SetBillingAssignment(t.Context(), assignment, 0); err != nil {
				return err
			}
			if err := w.CreateBillingGrant(t.Context(), grant); err != nil {
				return err
			}
			if err := w.SetBillingAccount(t.Context(), account); err != nil {
				return err
			}
			return w.SetBillingMonthlyPolicy(t.Context(), policy, 0)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := r.BillingPlan(t.Context(), plan.PlanReference)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(got, plan) {
				t.Errorf("plan=%+v want %+v", got, plan)
			}
			assigned, err := r.BillingAssignment(t.Context())
			if err != nil {
				return err
			}
			if assigned.ExpiresAt != nil {
				assigned.ExpiresAt = new(assigned.ExpiresAt.UTC())
			}
			if !reflect.DeepEqual(assigned, assignment) {
				t.Errorf("assignment=%+v want %+v", assigned, assignment)
			}
			grants, err := r.BillingGrants(t.Context(), "", 20)
			if err != nil {
				return err
			}
			for i := range grants {
				grants[i].StartsAt = grants[i].StartsAt.UTC()
				if grants[i].ExpiresAt != nil {
					grants[i].ExpiresAt = new(grants[i].ExpiresAt.UTC())
				}
			}
			if !reflect.DeepEqual(grants, []billingstore.Grant{grant}) {
				t.Errorf("grants=%+v", grants)
			}
			bound, err := r.BillingAccount(t.Context())
			if err != nil {
				return err
			}
			bound.PaidThrough = bound.PaidThrough.UTC()
			bound.AccessUntil = bound.AccessUntil.UTC()
			bound.Snapshot.InvoiceCreatedAt = bound.Snapshot.InvoiceCreatedAt.UTC()
			bound.Snapshot.PeriodStart = bound.Snapshot.PeriodStart.UTC()
			bound.Snapshot.PeriodEnd = bound.Snapshot.PeriodEnd.UTC()
			if !reflect.DeepEqual(bound, account) {
				t.Errorf("account=%+v want %+v", bound, account)
			}
			settings, err := r.BillingMonthlyPolicy(t.Context(), "")
			if err != nil {
				return err
			}
			settings.UpdatedAt = settings.UpdatedAt.UTC()
			if !reflect.DeepEqual(settings, policy) {
				t.Errorf("policy=%+v want %+v", settings, policy)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name  string
			write func(tenantstore.Writer) error
		}{
			{"assignment", func(w tenantstore.Writer) error { return w.SetBillingAssignment(t.Context(), assignment, 2) }},
			{"policy", func(w tenantstore.Writer) error { return w.SetBillingMonthlyPolicy(t.Context(), policy, 2) }},
		} {
			t.Run(string(org)+"/stale_"+test.name, func(t *testing.T) {
				err := repository.WriteTenant(t.Context(), org, corestore.Serving, test.write)
				if !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("stale update=%v", err)
				}
			})
		}
	}
}

func TestBillingEffectiveGrantsExcludeHistoryAndKeepAllActiveScopes(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	org := tracker.OrganizationID("org_grants")
	seedTenantAccess(t, repository, org)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	revokedAt := now.Add(-time.Minute)
	plan := billingstore.Plan{OrganizationID: org, PlanReference: billingstore.PlanReference{ID: "example", Version: 1}, Name: "Example", RuntimeID: "runtime_example", Allowances: map[string]int64{}, Features: []string{}}
	grants := []billingstore.Grant{
		{OrganizationID: org, ID: "active_a", Plan: plan.PlanReference, Scope: []string{"collaboration", "model_choice"}, StartsAt: now.Add(-time.Hour)},
		{OrganizationID: org, ID: "expired", Plan: plan.PlanReference, Scope: []string{"native_execution"}, StartsAt: now.Add(-time.Hour), ExpiresAt: new(now)},
		{OrganizationID: org, ID: "future", Plan: plan.PlanReference, Scope: []string{"native_execution"}, StartsAt: now.Add(time.Hour)},
		{OrganizationID: org, ID: "revoked", Plan: plan.PlanReference, Scope: []string{"native_execution"}, StartsAt: now.Add(-time.Hour), RevokedAt: &revokedAt},
	}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateBillingPlan(t.Context(), plan); err != nil {
			return err
		}
		for _, grant := range grants {
			if err := w.CreateBillingGrant(t.Context(), grant); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		active, err := r.BillingEffectiveGrants(t.Context(), now)
		if err != nil {
			return err
		}
		if len(active) != 1 || active[0].ID != "active_a" || !reflect.DeepEqual(active[0].Scope, []string{"collaboration", "model_choice"}) {
			return fmt.Errorf("effective grants=%+v", active)
		}
		revoked, err := r.BillingGrant(t.Context(), "revoked")
		if err != nil {
			return err
		}
		if revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(revokedAt) || !reflect.DeepEqual(revoked.Scope, []string{"native_execution"}) {
			return fmt.Errorf("revoked grant=%+v", revoked)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBillingFeatureGateUsesCurrentAssignmentsAndGrantScope(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	org := tracker.OrganizationID("org_feature_gate")
	seedTenantAccess(t, repository, org)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	expiredAt := now
	revokedAt := now.Add(-time.Minute)
	base := billingstore.Plan{OrganizationID: org, PlanReference: billingstore.PlanReference{ID: "base", Version: 1}, Name: "Base", RuntimeID: "base", Features: []string{"collaboration"}, Allowances: map[string]int64{}}
	subscriptionPlan := billingstore.Plan{OrganizationID: org, PlanReference: billingstore.PlanReference{ID: "subscription", Version: 1}, Name: "Subscription", RuntimeID: "", Features: []string{}, Allowances: map[string]int64{}}
	grantPlan := billingstore.Plan{OrganizationID: org, PlanReference: billingstore.PlanReference{ID: "grant", Version: 1}, Name: "Grant", RuntimeID: "grant", Features: []string{"github_integration"}, Allowances: map[string]int64{}}
	retiredPlan := billingstore.Plan{OrganizationID: org, PlanReference: billingstore.PlanReference{ID: "retired", Version: 1}, Name: "Retired", RuntimeID: "", Features: []string{"hosted_artifacts"}, Allowances: map[string]int64{}}
	stalePlan := billingstore.Plan{OrganizationID: org, PlanReference: billingstore.PlanReference{ID: "stale", Version: 1}, Name: "Stale", RuntimeID: "runtime_stale", Features: []string{"hosted_artifacts"}, Allowances: map[string]int64{}}
	capacityPlan := billingstore.Plan{OrganizationID: org, PlanReference: billingstore.PlanReference{ID: "starter", Version: 1}, Name: "Capacity", RuntimeID: "starter", Features: []string{"native_execution"}, Allowances: map[string]int64{}}
	assignment := billingstore.Assignment{OrganizationID: org, Base: base.PlanReference, Revision: 1}
	grants := []billingstore.Grant{
		{OrganizationID: org, ID: "active_feature", Plan: grantPlan.PlanReference, Scope: []string{"github_integration"}, StartsAt: now.Add(-time.Hour)},
		{OrganizationID: org, ID: "expired_feature", Plan: retiredPlan.PlanReference, Scope: []string{"hosted_artifacts"}, StartsAt: now.Add(-time.Hour), ExpiresAt: &expiredAt},
		{OrganizationID: org, ID: "revoked_feature", Plan: retiredPlan.PlanReference, Scope: []string{"hosted_artifacts"}, StartsAt: now.Add(-time.Hour), RevokedAt: &revokedAt},
		{OrganizationID: org, ID: "stale_runtime", Plan: stalePlan.PlanReference, Scope: []string{"hosted_artifacts"}, StartsAt: now.Add(-time.Hour)},
		{OrganizationID: org, ID: "capacity", Plan: capacityPlan.PlanReference, Scope: []string{"projects"}, StartsAt: now.Add(-time.Hour)},
	}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		for _, plan := range []billingstore.Plan{base, subscriptionPlan, grantPlan, retiredPlan, stalePlan, capacityPlan} {
			if err := w.CreateBillingPlan(t.Context(), plan); err != nil {
				return err
			}
		}
		for _, id := range []string{"grant", "starter"} {
			if err := w.StageRuntime(t.Context(), billingstore.RuntimeRevision{ID: id, Published: true}); err != nil {
				return err
			}
		}

		if err := w.SetBillingAssignment(t.Context(), assignment, 0); err != nil {
			return err
		}
		for _, grant := range grants {
			if err := w.CreateBillingGrant(t.Context(), grant); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		feature string
		want    bool
	}{
		{feature: "collaboration", want: false},
		{feature: "github_integration", want: true},
		{feature: "hosted_artifacts", want: false},
		{feature: "native_execution", want: true},
	} {
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := r.BillingFeatureEnabled(t.Context(), test.feature, now, []string{"starter:1"})
			if err != nil {
				return err
			}
			if got != test.want {
				return fmt.Errorf("feature %s enabled=%t want %t", test.feature, got, test.want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.StageRuntime(t.Context(), billingstore.RuntimeRevision{ID: "base", Published: true})
	}); err != nil {
		t.Fatal(err)
	}

	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		enabled, err := r.BillingFeatureEnabled(t.Context(), "collaboration", now, []string{"starter:1"})
		if err != nil {
			return err
		}
		if !enabled {
			return fmt.Errorf("base collaboration feature was not enabled")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	expires := now.Add(time.Hour)
	assignment.Subscription = &subscriptionPlan.PlanReference
	assignment.ExpiresAt = &expires
	assignment.Revision++
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetBillingAssignment(t.Context(), assignment, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		enabled, err := r.BillingFeatureEnabled(t.Context(), "collaboration", now, []string{"starter:1"})
		if err != nil {
			return err
		}
		if enabled {
			return fmt.Errorf("base collaboration leaked through active subscription")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBillingPlanAuditRetainsArchiveReferenceAndCommandReplay(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	org := tracker.OrganizationID("org_plan_audit")
	seedTenantAccess(t, repository, org)
	archive := attachment.ArchiveReference{BindingID: "binding_example", CustomerOwned: true, Key: "billing/example", SHA256: "sha256_example", CompressedSize: 18, UncompressedSize: 31, Codec: "gzip", SchemaVersion: 1, Kind: "detail", Retention: "retained"}
	audit := billingstore.PlanAudit{OrganizationID: org, CommandID: "command_example", RequestHash: "hash_example", ActorID: "actor_example", Action: "grant", RecordedAt: historyAt(), Reference: billingstore.AuditReferenceFromArchive(archive)}
	for _, applied := range []bool{true, false} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			created, err := w.CreateBillingPlanAudit(t.Context(), audit)
			if created != applied {
				t.Errorf("created=%v want %v", created, applied)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		got, err := r.BillingPlanAudit(t.Context(), audit.CommandID)
		if err != nil {
			return err
		}
		if got.RequestHash != audit.RequestHash || got.ActorID != audit.ActorID || got.Action != audit.Action || got.Reference.ArchiveReference() != archive {
			return fmt.Errorf("audit metadata or archive reference changed: %+v", got)
		}
		rows, err := r.BillingPlanAudits(t.Context(), 0, 10)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].ID != got.ID {
			return fmt.Errorf("audit page=%+v", rows)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHostedBillingSourceParityRestoresSourceWithoutReplacingCurrentTenantData(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	org := tracker.OrganizationID("org_source_parity")
	otherOrg := tracker.OrganizationID("org_source_parity_other")
	seedTenantAccess(t, repository, org)
	seedTenantAccess(t, repository, otherOrg)
	now := time.Date(2026, 11, 4, 2, 0, 0, 0, time.UTC)
	free := billingstore.Plan{OrganizationID: org, PlanReference: billingstore.PlanReference{ID: "free", Version: 1}, Name: "Free", Features: []string{}, Allowances: map[string]int64{"collaboration_bytes": 1 << 20}}
	monthlyPrice := int64(4900)
	currentStarter := billingstore.Plan{OrganizationID: org, PlanReference: billingstore.PlanReference{ID: "starter", Version: 1}, Name: "Starter", MonthlyUSDCents: &monthlyPrice, Features: []string{"collaboration"}, Allowances: map[string]int64{"collaboration_bytes": 1 << 30}}
	otherFree := billingstore.Plan{OrganizationID: otherOrg, PlanReference: free.PlanReference, Name: free.Name, Features: []string{}, Allowances: map[string]int64{"collaboration_bytes": 1 << 20}}
	otherMonthlyPrice := int64(4900)
	otherStarter := billingstore.Plan{OrganizationID: otherOrg, PlanReference: currentStarter.PlanReference, Name: currentStarter.Name, MonthlyUSDCents: &otherMonthlyPrice, Features: []string{"collaboration"}, Allowances: map[string]int64{"collaboration_bytes": 2 << 30}}
	operatorPlan := billingstore.Plan{OrganizationID: org, PlanReference: billingstore.PlanReference{ID: "operator", Version: 1}, Name: "Operator", Features: []string{}, Allowances: map[string]int64{}}
	currentAssignment := billingstore.Assignment{OrganizationID: org, Base: free.PlanReference, Revision: 1}
	otherAssignment := billingstore.Assignment{OrganizationID: otherOrg, Base: free.PlanReference, Revision: 1}
	operatorGrant := billingstore.Grant{OrganizationID: org, ID: "current_operator_grant", Plan: operatorPlan.PlanReference, Scope: []string{"collaboration_bytes"}, StartsAt: now.Add(-time.Hour)}
	seedArchive := attachment.ArchiveReference{BindingID: "binding_source_parity", Key: "billing/source-parity-seed", SHA256: strings.Repeat("a", 64), CompressedSize: 10, UncompressedSize: 20, Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
	seedAudit := billingstore.PlanAudit{OrganizationID: org, CommandID: "seed_starter_1", RequestHash: strings.Repeat("b", 64), ActorID: "configuration", Action: "create_plan", RecordedAt: now.Add(-time.Hour), Reference: billingstore.AuditReferenceFromArchive(seedArchive)}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		for _, plan := range []billingstore.Plan{free, currentStarter, operatorPlan} {
			if err := w.CreateBillingPlan(t.Context(), plan); err != nil {
				return err
			}
		}
		if err := w.SetBillingAssignment(t.Context(), currentAssignment, 0); err != nil {
			return err
		}
		if err := w.CreateBillingGrant(t.Context(), operatorGrant); err != nil {
			return err
		}
		_, err := w.CreateBillingPlanAudit(t.Context(), seedAudit)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), otherOrg, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateBillingPlan(t.Context(), otherFree); err != nil {
			return err
		}
		if err := w.CreateBillingPlan(t.Context(), otherStarter); err != nil {
			return err
		}
		return w.SetBillingAssignment(t.Context(), otherAssignment, 0)
	}); err != nil {
		t.Fatal(err)
	}
	expires := now.Add(-time.Hour).Add(57_298_070)
	expiredGrant := billingstore.Grant{OrganizationID: org, ID: "source_expired_starter", Plan: currentStarter.PlanReference, Scope: []string{"collaboration_bytes"}, StartsAt: now.Add(-48 * time.Hour).Add(487_589_706), ExpiresAt: &expires}
	sourceAssignmentExpiry := now.Add(10 * 24 * time.Hour).Add(34_657)
	sourceAssignment := billingstore.Assignment{OrganizationID: org, Base: free.PlanReference, Subscription: &currentStarter.PlanReference, ExpiresAt: &sourceAssignmentExpiry, Revision: 2}
	sourceStarter := currentStarter
	sourceStarter.Allowances = map[string]int64{"collaboration_bytes": 25 << 30}
	sourceAuditArchive := attachment.ArchiveReference{BindingID: "binding_source_parity", Key: "billing/source-parity-grant", SHA256: strings.Repeat("c", 64), CompressedSize: 12, UncompressedSize: 22, Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
	sourceAudit := billingstore.PlanAudit{OrganizationID: org, CommandID: "source_expired_starter_grant", RequestHash: strings.Repeat("d", 64), ActorID: "detent-operator", Action: "grant", RecordedAt: now.Add(-48 * time.Hour).Add(487_589_706), Reference: billingstore.AuditReferenceFromArchive(sourceAuditArchive)}
	sourcePlanSHA, err := postgres.HostedBillingPlanParityDigest(sourceStarter)
	if err != nil {
		t.Fatal(err)
	}
	expectedPlanSHA, err := postgres.HostedBillingPlanParityDigest(currentStarter)
	if err != nil {
		t.Fatal(err)
	}
	repair := postgres.HostedBillingSourceParityRepair{
		OrganizationID:       org,
		SourceManifestSHA256: strings.Repeat("e", 64),
		SourceDatabaseSHA256: strings.Repeat("f", 64),
		SourceAssignment:     sourceAssignment,
		ExpectedAssignment:   currentAssignment,
		Plans: []postgres.HostedBillingPlanSourceParity{{
			Source:                 sourceStarter,
			SourceSHA256:           sourcePlanSHA,
			Expected:               &currentStarter,
			ExpectedSHA256:         expectedPlanSHA,
			SeedAuditCommandID:     seedAudit.CommandID,
			SeedAuditRequestHash:   seedAudit.RequestHash,
			SeedAuditArchiveSHA256: seedAudit.Reference.SHA256,
		}},
		SourceGrants:     []billingstore.Grant{expiredGrant},
		ExpectedGrants:   []billingstore.Grant{operatorGrant},
		SourcePlanAudits: []billingstore.PlanAudit{sourceAudit},
	}
	if err := postgres.SealHostedBillingSourceParityRepair(&repair); err != nil {
		t.Fatal(err)
	}
	result, err := repository.RepairHostedBillingSourceParity(t.Context(), repair)
	if err != nil {
		t.Fatal(err)
	}
	if result.PlansRepaired != 1 || result.AssignmentsSet != true || result.GrantsCreated != 1 || result.AuditsCreated != 1 || result.PlansCreated != 0 {
		t.Fatalf("repair result=%+v", result)
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		assignment, err := r.BillingAssignment(t.Context())
		if err != nil {
			return err
		}
		if assignment.Base != sourceAssignment.Base || assignment.Subscription == nil || *assignment.Subscription != *sourceAssignment.Subscription || assignment.ExpiresAt == nil || !assignment.ExpiresAt.Equal(sourceAssignmentExpiry.Truncate(time.Microsecond)) || assignment.Revision != 2 {
			return fmt.Errorf("source assignment=%+v", assignment)
		}
		plan, err := r.BillingPlan(t.Context(), sourceStarter.PlanReference)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(plan, sourceStarter) {
			return fmt.Errorf("source plan=%+v", plan)
		}
		grant, err := r.BillingGrant(t.Context(), expiredGrant.ID)
		if err != nil {
			return err
		}
		if grant.OrganizationID != expiredGrant.OrganizationID || grant.ID != expiredGrant.ID || grant.Plan != expiredGrant.Plan || !reflect.DeepEqual(grant.Scope, expiredGrant.Scope) || !grant.StartsAt.Equal(expiredGrant.StartsAt.Truncate(time.Microsecond)) || grant.RevokedAt != nil || grant.ExpiresAt == nil || !grant.ExpiresAt.Equal(expires.Truncate(time.Microsecond)) || grant.ExpiresAt.After(now) {
			return fmt.Errorf("expired source grant=%+v", grant)
		}
		grants, err := r.BillingGrants(t.Context(), "", 20)
		if err != nil {
			return err
		}
		if len(grants) != 2 {
			return fmt.Errorf("grant count=%d", len(grants))
		}
		active, err := r.BillingEffectiveGrants(t.Context(), now)
		if err != nil {
			return err
		}
		if len(active) != 1 || active[0].ID != operatorGrant.ID {
			return fmt.Errorf("effective grants=%+v", active)
		}
		gotAudit, err := r.BillingPlanAudit(t.Context(), sourceAudit.CommandID)
		if err != nil {
			return err
		}
		if gotAudit.OrganizationID != sourceAudit.OrganizationID || gotAudit.CommandID != sourceAudit.CommandID || gotAudit.RequestHash != sourceAudit.RequestHash || gotAudit.ActorID != sourceAudit.ActorID || gotAudit.Action != sourceAudit.Action || !gotAudit.RecordedAt.Equal(sourceAudit.RecordedAt.Truncate(time.Microsecond)) || gotAudit.Reference != sourceAudit.Reference {
			return fmt.Errorf("source audit=%+v", gotAudit)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result, err = repository.RepairHostedBillingSourceParity(t.Context(), repair)
	if err != nil || result != (postgres.HostedBillingSourceParityResult{}) {
		t.Fatalf("idempotent repair=%+v err=%v", result, err)
	}
	if err := repository.ReadTenant(t.Context(), otherOrg, corestore.Serving, func(r tenantstore.Reader) error {
		assignment, err := r.BillingAssignment(t.Context())
		if err != nil {
			return err
		}
		plan, err := r.BillingPlan(t.Context(), otherStarter.PlanReference)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(assignment, otherAssignment) || !reflect.DeepEqual(plan, otherStarter) {
			return fmt.Errorf("other tenant changed: assignment=%+v plan=%+v", assignment, plan)
		}
		grants, err := r.BillingGrants(t.Context(), "", 10)
		if err != nil {
			return err
		}
		if len(grants) != 0 {
			return fmt.Errorf("other tenant grants=%+v", grants)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHostedBillingSourceParityRefusesChangedSeedPlan(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	org := tracker.OrganizationID("org_source_parity_cas")
	seedTenantAccess(t, repository, org)
	now := time.Date(2026, 11, 4, 2, 0, 0, 0, time.UTC)
	base := billingstore.Plan{OrganizationID: org, PlanReference: billingstore.PlanReference{ID: "base", Version: 1}, Name: "Base", Features: []string{}, Allowances: map[string]int64{}}
	current := billingstore.Plan{OrganizationID: org, PlanReference: billingstore.PlanReference{ID: "starter", Version: 1}, Name: "Starter", Features: []string{}, Allowances: map[string]int64{"collaboration_bytes": 1 << 30}}
	changed := current
	changed.RuntimeID = "runtime_current"
	assignment := billingstore.Assignment{OrganizationID: org, Base: base.PlanReference, Revision: 1}
	archive := attachment.ArchiveReference{BindingID: "binding_source_parity", Key: "billing/source-parity-seed", SHA256: strings.Repeat("a", 64), CompressedSize: 10, UncompressedSize: 20, Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
	audit := billingstore.PlanAudit{OrganizationID: org, CommandID: "seed_starter_1", RequestHash: strings.Repeat("b", 64), ActorID: "configuration", Action: "create_plan", RecordedAt: now.Add(-time.Hour), Reference: billingstore.AuditReferenceFromArchive(archive)}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateBillingPlan(t.Context(), base); err != nil {
			return err
		}
		if err := w.CreateBillingPlan(t.Context(), current); err != nil {
			return err
		}
		if err := w.SetBillingAssignment(t.Context(), assignment, 0); err != nil {
			return err
		}
		_, err := w.CreateBillingPlanAudit(t.Context(), audit)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetBillingPlanRuntime(t.Context(), changed.PlanReference, changed.RuntimeID)
	}); err != nil {
		t.Fatal(err)
	}
	source := current
	source.Allowances = map[string]int64{"collaboration_bytes": 25 << 30}
	sourceSHA, err := postgres.HostedBillingPlanParityDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	currentSHA, err := postgres.HostedBillingPlanParityDigest(current)
	if err != nil {
		t.Fatal(err)
	}
	repair := postgres.HostedBillingSourceParityRepair{
		OrganizationID:       org,
		SourceManifestSHA256: strings.Repeat("c", 64),
		SourceDatabaseSHA256: strings.Repeat("d", 64),
		SourceAssignment:     assignment,
		ExpectedAssignment:   assignment,
		Plans: []postgres.HostedBillingPlanSourceParity{{
			Source:                 source,
			SourceSHA256:           sourceSHA,
			Expected:               &current,
			ExpectedSHA256:         currentSHA,
			SeedAuditCommandID:     audit.CommandID,
			SeedAuditRequestHash:   audit.RequestHash,
			SeedAuditArchiveSHA256: audit.Reference.SHA256,
		}},
	}
	if err := postgres.SealHostedBillingSourceParityRepair(&repair); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RepairHostedBillingSourceParity(t.Context(), repair); !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("repair error=%v", err)
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		got, err := r.BillingPlan(t.Context(), changed.PlanReference)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, changed) {
			return fmt.Errorf("changed plan overwritten: %+v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBillingMonthlyBudgetParticipantsAreTypedAndTenantScoped(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	org := tracker.OrganizationID("org_budget")
	seedTenantAccess(t, repository, org)
	project := tracker.ProjectID("prj_budget")
	err := repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
		if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: project, Name: "Budget example", Profile: "native"}); err != nil {
			return err
		}
		for id := int64(1); id <= 2; id++ {
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ID: id, ProjectID: project, NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", id)), Number: id, Title: "Budget example"}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
		for id := int64(1); id <= 2; id++ {
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: fmt.Sprintf("machine_%d", id), Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	leases := []leasestore.Lease{}
	for id := int64(1); id <= 2; id++ {
		var lease leasestore.Lease
		err := repository.WriteLeases(t.Context(), org, corestore.Serving, func(w leasestore.Writer) error {
			var err error
			lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: id, ExpectedRevision: 1, MachineID: fmt.Sprintf("machine_%d", id), SessionID: fmt.Sprintf("session_%d", id), LeaseID: fmt.Sprintf("lease_%d", id), TTL: time.Hour})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	firstAt := historyAt()
	for i, lease := range leases {
		row := billingstore.MonthlyLease{OrganizationID: org, LeaseID: tracker.LeaseID(lease.LeaseID), LunaAPI: i == 0, RunnerAPI: i == 1, SpriteInfrastructure: i == 0}
		admission := billingstore.MonthlyAdmission{OrganizationID: org, IssueID: tracker.WorkItemID(i + 1), AdmittedAt: firstAt.Add(time.Duration(i) * time.Minute)}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.RecordBillingMonthlyAdmission(t.Context(), admission); err != nil {
				return err
			}
			replay := admission
			replay.AdmittedAt = replay.AdmittedAt.Add(time.Hour)
			if err := w.RecordBillingMonthlyAdmission(t.Context(), replay); err != nil {
				return err
			}
			return w.RecordBillingMonthlyLease(t.Context(), row)
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			gotAdmission, err := r.BillingMonthlyAdmission(t.Context(), admission.IssueID)
			if err != nil {
				return err
			}
			if !gotAdmission.AdmittedAt.Equal(admission.AdmittedAt) {
				return fmt.Errorf("admission=%+v want %+v", gotAdmission, admission)
			}
			gotLease, err := r.BillingMonthlyLease(t.Context(), row.LeaseID)
			if err != nil {
				return err
			}
			if gotLease != row {
				return fmt.Errorf("lease exposure=%+v want %+v", gotLease, row)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
