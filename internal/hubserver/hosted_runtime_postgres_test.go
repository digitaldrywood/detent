package hubserver

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresConfiguredPlanReplacement(t *testing.T) {
	for _, mode := range []string{"startup", "reload", "publication", "audit refusal"} {
		t.Run(mode, func(t *testing.T) {
			repo, admin := projectRecordPostgresWithAdmin(t, 4)
			var services []*Service
			plans := pilotHostedPlans()
			plans.Plans[0].ID = "free"
			plans.Base = plans.Plans[0].PlanReference
			plans.Plans[0].Allowances["api_mutations"] = 0
			plans.Plans[1].ID = "starter"
			plans.Plans[1].Features = []string{"collaboration"}
			plans.Plans[1].Allowances["api_mutations"] = 0
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: time.Now()})
				}); err != nil {
					t.Fatal(err)
				}
				d := &database{postgres: repo, billingRepository: repo, hostedOrganization: org, billingAuditStore: &memoryRunDetails{}, now: time.Now, logger: discardLogger()}
				s := &Service{database: d, config: Config{Hosted: &HostedConfig{OrganizationID: string(org), Plans: &plans, EntitlementAdministrator: "operator"}, now: time.Now, Logger: discardLogger()}}
				if err := d.configureHostedPlans(t.Context(), s.config.Hosted); err != nil {
					t.Fatal(err)
				}
				if err := d.writeBillingTenant(t.Context(), func(w tenantstore.Writer) error {
					return w.CreateBillingGrant(t.Context(), billingstore.Grant{OrganizationID: org, ID: "grant_" + string(org), Plan: billingstore.PlanReference{ID: plans.Plans[1].ID, Version: 1}, Scope: []string{"api_mutations", "ingested_events"}, StartsAt: time.Now().Add(-time.Hour)})
				}); err != nil {
					t.Fatal(err)
				}
				services = append(services, s)
			}
			denied := errors.New("usage refused")
			request := func(s *Service, id string) error {
				return s.database.writeBillingTenant(t.Context(), func(w tenantstore.Writer) error {
					entitlement, err := s.database.hostedEntitlementRecord(t.Context(), w, time.Now())
					if err != nil {
						return err
					}
					usage, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: s.database.hostedOrganization, ID: id, WindowStart: time.Now().UTC().Truncate(time.Hour), Metric: "api_mutations", Amount: 1}, entitlement.Allowances["api_mutations"])
					if err == nil && !usage.Allowed {
						return denied
					}
					return err
				})
			}
			for _, s := range services {
				if err := request(s, "before"); !errors.Is(err, denied) {
					t.Fatalf("old allowance error = %v", err)
				}
			}
			plans.Plans[0].Name = "Configured plan"
			plans.Plans[0].MonthlyUSDCents = new(int64(1234))
			plans.Plans[0].Features = []string{"collaboration", "model_choice"}
			plans.Plans[0].Allowances["api_mutations"] = 1000000
			plans.Plans[0].Allowances["ingested_events"] = 1
			plans.Plans[0].Allowances["concurrent_work"] = 17
			stage := "00000000-0000-4000-8000-000000000010"
			if mode == "audit refusal" {
				if _, err := admin.ExecContext(t.Context(), "ALTER TABLE hosted_plan_audit ADD CONSTRAINT refuse_update CHECK (action <> 'update_plan')"); err != nil {
					t.Fatal(err)
				}
			}
			for _, s := range services {
				if mode == "startup" {
					if err := s.database.configureHostedPlans(t.Context(), s.config.Hosted); err != nil {
						t.Fatal(err)
					}
				} else {
					update := HostedRuntimeUpdate{Plans: &plans, Reason: "configuration edit"}
					if mode == "publication" {
						s.config.Hosted.SharedEntry = &HostedSharedEntry{}
						update.StageID = stage
					}
					before := s.database.runtimeState()
					validation := update
					validation.ValidateOnly = true
					if err := s.applyHostedRuntime(t.Context(), &validation); err != nil {
						t.Fatal(err)
					}
					if err := request(s, "validation"); !errors.Is(err, denied) || s.database.runtimeState() != before {
						t.Fatalf("validation changed active definition: %v", err)
					}
					err := s.applyHostedRuntime(t.Context(), &update)
					if mode == "audit refusal" {
						if err == nil || s.database.runtimeState() != before {
							t.Fatalf("audit refusal activated runtime: %v", err)
						}
						if err := request(s, "refused"); !errors.Is(err, denied) {
							t.Fatalf("audit refusal committed allowance: %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "audit refusal" {
				if _, err := admin.ExecContext(t.Context(), "ALTER TABLE hosted_plan_audit DROP CONSTRAINT refuse_update"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "publication" {
				for _, s := range services {
					if err := request(s, "prepared"); !errors.Is(err, denied) {
						t.Fatalf("preparation changed allowance: %v", err)
					}
				}
				if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES($1)", stage); err != nil {
					t.Fatal(err)
				}
				for _, s := range services {
					if err := s.database.refreshRuntime(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, s := range services {
				if err := s.database.refreshRuntime(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := s.database.readBillingTenant(t.Context(), func(r tenantstore.Reader) error {
					entitlement, err := s.database.hostedEntitlementRecord(t.Context(), r, time.Now())
					if err != nil {
						return err
					}
					if entitlement.Allowances["concurrent_work"] != 17 || entitlement.Allowances["api_mutations"] != 1000000 || entitlement.Allowances["ingested_events"] != plans.Plans[1].Allowances["ingested_events"] || entitlement.Name != "Configured plan" || entitlement.MonthlyUSDCents == nil || *entitlement.MonthlyUSDCents != 1234 || !slices.Contains(entitlement.Features, "model_choice") || slices.Contains(entitlement.Features, "native_execution") {
						return fmt.Errorf("configured entitlement = %+v", entitlement)
					}
					audits, err := r.BillingPlanAudits(t.Context(), 0, 100)
					if err != nil {
						return err
					}
					updates := 0
					for _, audit := range audits {
						if audit.Action == "update_plan" && audit.ActorID == "configuration" {
							updates++
						}
					}
					if updates != 1 {
						return fmt.Errorf("configuration update audits = %d", updates)
					}
					var limited *hostedLimitError
					if err := s.database.requireHostedFeatureRecord(t.Context(), r, "native_execution", time.Now()); !errors.As(err, &limited) {
						return fmt.Errorf("removed feature allowed: %v", err)
					}
					return s.database.requireHostedFeatureRecord(t.Context(), r, "model_choice", time.Now())
				}); err != nil {
					t.Fatal(err)
				}
				if err := request(s, "after"); err != nil {
					t.Fatalf("new allowance refused request: %v", err)
				}
			}
		})
	}
}

func TestPostgresRuntimeValidationDoesNotPublish(t *testing.T) {
	repo, admin := projectRecordPostgresWithAdmin(t, 4)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		d := &database{billingRepository: repo, hostedOrganization: org, billingAuditStore: &memoryRunDetails{}, now: time.Now, logger: discardLogger()}
		s := &Service{database: d, config: Config{Hosted: &HostedConfig{OrganizationID: string(org), EntitlementAdministrator: "actor"}, now: time.Now, Logger: discardLogger()}}
		plans := pilotHostedPlans()
		update := HostedRuntimeUpdate{Plans: &plans, Reason: "Test runtime", ValidateOnly: true}
		if err := s.applyHostedRuntime(t.Context(), &update); err != nil {
			t.Fatal(err)
		}
		if d.plans() != nil {
			t.Fatal("validation published runtime")
		}
		if err := repo.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.BillingPlans(t.Context(), billingstore.PlanReference{}, 100)
			if len(rows) != 0 {
				t.Error("validation committed plans")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		update.ValidateOnly = false
		if org == "org_second" {
			s.config.Hosted.SharedEntry = &HostedSharedEntry{}
			update.StageID = "00000000-0000-4000-8000-000000000003"
		}
		if err := s.applyHostedRuntime(t.Context(), &update); err != nil {
			t.Fatal(err)
		}
		if update.StageID != "" {
			if d.plans() != nil {
				t.Fatal("first preparation was visible before publication")
			}
			if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES($1)", update.StageID); err != nil {
				t.Fatal(err)
			}
			if err := d.refreshRuntime(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if d.plans() == nil {
			t.Fatal("runtime not published")
		}
		if err := repo.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.BillingPlans(t.Context(), billingstore.PlanReference{}, 100)
			if len(rows) != len(plans.Plans) {
				t.Errorf("plans=%d", len(rows))
			}
			audits, err2 := r.BillingPlanAudits(t.Context(), 0, 100)
			if len(audits) != len(plans.Plans)+1 {
				t.Errorf("audits=%d", len(audits))
			}
			if err != nil {
				return err
			}
			return err2
		}); err != nil {
			t.Fatal(err)
		}
	}
}
