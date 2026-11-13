package hostedplans

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/google/uuid"
)

func TestPostgresConfiguredPlanReplacement(t *testing.T) {
	for _, mode := range []string{"startup", "startup unpublished", "startup unbound", "reload", "publication", "audit refusal"} {
		t.Run(mode, func(t *testing.T) {
			plans := PilotPlans()
			plans.Plans[0].ID = "configured_free"
			plans.Base = plans.Plans[0].PlanReference
			plans.Plans[0].Allowances["api_mutations"] = 0
			plans.Plans[1].ID = "configured_grant"
			plans.Plans[1].Features = []string{"collaboration"}
			plans.Plans[1].Allowances["api_mutations"] = 0
			repo, admin := postgresWithAdmin(t, 4)
			services := configuredPlanPostgresServices(t, plans, repo)
			for _, s := range services {
				if err := hubapi.FromFeatureError(s.database.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
					return w.CreateBillingGrant(t.Context(), billingstore.Grant{OrganizationID: s.database.hostedOrganization, ID: "grant_" + string(s.database.hostedOrganization), Plan: billingstore.PlanReference{ID: plans.Plans[1].ID, Version: 1}, Scope: []string{"api_mutations", "ingested_events"}, StartsAt: time.Now().Add(-time.Hour)})
				})); err != nil {
					t.Fatal(err)
				}
			}
			denied := errors.New("usage refused")
			request := func(s *hubService, id string) error {
				return hubapi.FromFeatureError(s.database.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
					entitlement, err := hubapi.FeatureResult(s.database.plansService().EntitlementRecord(t.Context(), w, time.Now()))
					if err != nil {
						return err
					}
					usage, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: s.database.hostedOrganization, ID: id, WindowStart: time.Now().UTC().Truncate(time.Hour), Metric: "api_mutations", Amount: 1}, entitlement.Allowances["api_mutations"])
					if err == nil && !usage.Allowed {
						return denied
					}
					return err
				}))
			}
			for _, s := range services {
				if err := request(s, "before"); !errors.Is(err, denied) {
					t.Fatalf("old allowance error = %v", err)
				}
			}
			if mode == "startup unpublished" || mode == "startup unbound" {
				configured := plans.Plans[0]
				configured.Version++
				configured.Name = "Authorized startup version"
				configured.MonthlyUSDCents = new(int64(500))
				configured.Features = []string{"collaboration"}
				configured.Allowances = maps.Clone(configured.Allowances)
				configured.Allowances["api_mutations"] = 0
				configured.Allowances["collaboration_bytes"] = 64 << 20
				plans.Plans = append(plans.Plans, configured)
				stored := configured
				stored.Name = "Retained legacy version"
				stored.MonthlyUSDCents = new(int64(900))
				stored.Features = []string{"collaboration", "model_choice"}
				stored.Allowances = maps.Clone(configured.Allowances)
				stored.Allowances["api_mutations"] = 999
				stored.Allowances["collaboration_bytes"] = 500 << 30
				for _, s := range services {
					s.config.Hosted.Plans = &plans
					if mode == "startup unpublished" {
						staged := plans
						staged.Plans = slices.Clone(plans.Plans)
						staged.Plans[len(staged.Plans)-1] = stored
						s.config.Hosted.SharedEntry = &HostedSharedEntry{}
						update := HostedRuntimeUpdate{Plans: &staged, Reason: "unpublished conflicting version", StageID: "00000000-0000-4000-8000-000000000011"}
						if err := s.applyHostedRuntime(t.Context(), &update); err != nil {
							t.Fatal(err)
						}
						s.config.Hosted.SharedEntry = nil
					} else if err := hubapi.FromFeatureError(s.database.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
						return w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: s.database.hostedOrganization, PlanReference: billingstore.PlanReference{ID: stored.ID, Version: stored.Version}, Name: stored.Name, MonthlyUSDCents: stored.MonthlyUSDCents, Features: stored.Features, Allowances: stored.Allowances})
					})); err != nil {
						t.Fatal(err)
					}
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
			readStoredPlans := func(s *hubService) []billingstore.Plan {
				t.Helper()
				stored := make([]billingstore.Plan, 0, len(plans.Plans))
				if err := hubapi.FromFeatureError(s.database.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
					for _, configured := range plans.Plans {
						plan, err := r.BillingPlan(t.Context(), billingstore.PlanReference{ID: configured.ID, Version: configured.Version})
						if err != nil {
							return err
						}
						stored = append(stored, plan)
					}
					return nil
				})); err != nil {
					t.Fatal(err)
				}
				return stored
			}
			for _, s := range services {
				if strings.HasPrefix(mode, "startup") {
					stored := readStoredPlans(s)
					if mode == "startup unpublished" || mode == "startup unbound" {
						last := &stored[len(stored)-1]
						if last.Name != "Retained legacy version" || last.MonthlyUSDCents == nil || *last.MonthlyUSDCents != 900 || last.Allowances["api_mutations"] != 999 || last.Allowances["collaboration_bytes"] != 500<<30 {
							t.Fatalf("conflicting startup fixture lost its stored terms: %+v", last)
						}
						if mode == "startup unpublished" {
							var published bool
							if err := hubapi.FromFeatureError(s.database.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
								var err error
								published, err = r.RuntimePublished(t.Context(), last.RuntimeID)
								return err
							})); err != nil {
								t.Fatal(err)
							}
							if published || last.RuntimeID != "00000000-0000-4000-8000-000000000011" {
								t.Fatal("conflicting startup fixture was already published")
							}
							last.Name, last.MonthlyUSDCents = "Authorized startup version", new(int64(500))
							last.Features = []string{"collaboration"}
							last.Allowances["api_mutations"] = 0
							last.Allowances["collaboration_bytes"] = 64 << 20
						} else if last.RuntimeID != "" {
							t.Fatal("legacy startup fixture did not retain an empty runtime binding")
						}
					}
					if err := hubapi.FromFeatureError(s.database.plansService().ConfigurePlans(t.Context(), PlansInput(s.config.Hosted))); err != nil {
						t.Fatal(err)
					}
					for read := range 2 {
						if read != 0 {
							s.database.hostedRuntime.Store(nil)
							if err := hubapi.FromFeatureError(s.database.plansService().RefreshRuntime(t.Context())); err != nil {
								t.Fatal(err)
							}
						}
						retained := readStoredPlans(s)
						state := s.database.plansService().RuntimeState()
						if state == nil {
							t.Fatal("startup did not retain its runtime snapshot")
						}
						for index := range retained {
							if retained[index].RuntimeID != state.Revision {
								t.Fatal("startup did not bind the retained plan to its runtime")
							}
							retained[index].RuntimeID = stored[index].RuntimeID
						}
						if !reflect.DeepEqual(retained, stored) {
							t.Fatalf("startup or cold refresh changed retained plans: %+v want %+v", retained, stored)
						}
					}
					if err := request(s, "startup-preserved"); !errors.Is(err, denied) {
						t.Fatalf("startup replaced the existing allowance: %v", err)
					}
					update := HostedRuntimeUpdate{Plans: &plans, Reason: "explicit startup catalog update"}
					if err := s.applyHostedRuntime(t.Context(), &update); err != nil {
						t.Fatal(err)
					}
				} else {
					update := HostedRuntimeUpdate{Plans: &plans, Reason: "configuration edit"}
					if mode == "publication" {
						s.config.Hosted.SharedEntry = &HostedSharedEntry{}
						update.StageID = stage
					}
					before := s.database.plansService().RuntimeState()
					validation := update
					validation.ValidateOnly = true
					if err := s.applyHostedRuntime(t.Context(), &validation); err != nil {
						t.Fatal(err)
					}
					if err := request(s, "validation"); !errors.Is(err, denied) || s.database.plansService().RuntimeState() != before {
						t.Fatalf("validation changed active definition: %v", err)
					}
					err := s.applyHostedRuntime(t.Context(), &update)
					if mode == "audit refusal" {
						if err == nil || s.database.plansService().RuntimeState() != before {
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
			if mode == "audit refusal" {
				for _, s := range services {
					update := HostedRuntimeUpdate{Plans: &plans, Reason: "retry configuration edit"}
					if err := s.applyHostedRuntime(t.Context(), &update); err != nil {
						t.Fatal(err)
					}
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
					if err := hubapi.FromFeatureError(s.database.plansService().RefreshRuntime(t.Context())); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, s := range services {
				if err := hubapi.FromFeatureError(s.database.plansService().RefreshRuntime(t.Context())); err != nil {
					t.Fatal(err)
				}
				if err := hubapi.FromFeatureError(s.database.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
					entitlement, err := hubapi.FeatureResult(s.database.plansService().EntitlementRecord(t.Context(), r, time.Now()))
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
					wantUpdates := 1
					if mode == "startup unpublished" || mode == "startup unbound" {
						wantUpdates = 2
					}
					if updates != wantUpdates {
						return fmt.Errorf("configuration update audits = %d", updates)
					}
					if mode == "startup unpublished" || mode == "startup unbound" {
						configured := plans.Plans[len(plans.Plans)-1]
						plan, err := r.BillingPlan(t.Context(), billingstore.PlanReference{ID: configured.ID, Version: configured.Version})
						if err != nil {
							return err
						}
						if plan.Name != "Authorized startup version" || plan.MonthlyUSDCents == nil || *plan.MonthlyUSDCents != 500 || plan.Allowances["api_mutations"] != 0 || plan.Allowances["collaboration_bytes"] != 64<<20 || !slices.Equal(plan.Features, []string{"collaboration"}) {
							return fmt.Errorf("explicit reload did not apply configured startup version: %+v", plan)
						}
					}
					var limited *LimitError
					if err := hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(t.Context(), r, "native_execution", time.Now())); !errors.As(err, &limited) {
						return fmt.Errorf("removed feature allowed: %v", err)
					}
					return hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(t.Context(), r, "model_choice", time.Now()))
				})); err != nil {
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
	repo, admin := postgresWithAdmin(t, 4)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		d := &hubDatabase{billingRepository: repo, hostedOrganization: org, billingAuditStore: hubtest.NewDetails(), now: time.Now, logger: discardLogger()}
		s := &hubService{database: d, config: hubConfig{Hosted: &HostedConfig{OrganizationID: string(org), EntitlementAdministrator: "actor"}, now: time.Now, Logger: discardLogger()}}
		plans := PilotPlans()
		update := HostedRuntimeUpdate{Plans: &plans, Reason: "Test runtime", ValidateOnly: true}
		if err := s.applyHostedRuntime(t.Context(), &update); err != nil {
			t.Fatal(err)
		}
		if d.plansService().Plans() != nil {
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
			if d.plansService().Plans() != nil {
				t.Fatal("first preparation was visible before publication")
			}
			if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES($1)", update.StageID); err != nil {
				t.Fatal(err)
			}
			if err := hubapi.FromFeatureError(d.plansService().RefreshRuntime(t.Context())); err != nil {
				t.Fatal(err)
			}
		}
		if d.plansService().Plans() == nil {
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

func TestPostgresCatalogRemovalChecksReferences(t *testing.T) {
	repo, admin := postgresWithAdmin(t, 4)
	for _, mode := range []string{"startup", "validation", "reload", "prepare", "publication"} {
		for _, kind := range []string{"base", "subscription", "grant", "future grant", "expired grant", "revoked grant", "unreferenced", "administrator", "payment pending", "disable catalog"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				plans := PilotPlans()
				if kind == "payment pending" {
					plans = *CheckoutHostedPlans(&plans)
				}
				services := configuredPlanPostgresServices(t, plans, repo)
				stageID := uuid.NewString()
				candidate := plans
				candidate.Plans = candidate.Plans[:1]
				candidate.Base = candidate.Plans[0].PlanReference
				if kind == "disable catalog" {
					candidate = HostedPlansConfig{}
				}
				ref := billingstore.PlanReference{ID: "comp_team", Version: 1}
				if kind == "administrator" {
					ref.ID = "administrator_plan"
				}
				if kind == "payment pending" {
					ref.ID = "payment_pending"
				}
				if mode == "publication" && kind != "disable catalog" {
					for _, s := range services {
						s.config.Hosted.SharedEntry = &HostedSharedEntry{}
						update := HostedRuntimeUpdate{Plans: &candidate, Reason: "prepare removal", StageID: stageID}
						if err := s.applyHostedRuntime(t.Context(), &update); err != nil {
							t.Fatal(err)
						}
					}
				}
				for _, s := range services {
					if kind == "administrator" {
						plan := HostedPlan{PlanReference: PlanReference{ID: ref.ID, Version: ref.Version}, Features: []string{"collaboration"}, Allowances: map[string]int64{"api_mutations": 7}}
						if err := hubapi.FromFeatureError(s.database.plansService().ApplyPlanCommand(t.Context(), "operator", PlanCommand{ID: "admin_create", Action: "create_plan", ExpectedRevision: 1, Plan: plan.PlanReference, Definition: &plan, Reason: "administrator plan"})); err != nil {
							t.Fatal(err)
						}
					}
					setReference := func() {
						t.Helper()
						if err := hubapi.FromFeatureError(s.database.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
							assignment, err := w.BillingAssignment(t.Context())
							if err != nil {
								return err
							}
							switch kind {
							case "base", "administrator", "payment pending":
								assignment.Base = ref
							case "subscription":
								assignment.Subscription, assignment.ExpiresAt = &ref, new(time.Now().Add(time.Hour))
							case "grant", "future grant", "expired grant", "revoked grant":
								grant := billingstore.Grant{OrganizationID: s.database.hostedOrganization, ID: "grant", Plan: ref, Scope: []string{"api_mutations"}, StartsAt: time.Now().Add(-time.Hour)}
								if kind == "future grant" {
									grant.StartsAt = time.Now().Add(time.Hour)
								}
								if kind == "expired grant" {
									grant.ExpiresAt = new(time.Now().Add(-time.Minute))
								}
								if kind == "revoked grant" {
									grant.RevokedAt = new(time.Now().Add(-time.Minute))
								}
								return w.CreateBillingGrant(t.Context(), grant)
							}
							return w.SetBillingAssignment(t.Context(), assignment, assignment.Revision)
						})); err != nil {
							t.Fatal(err)
						}
					}
					update := HostedRuntimeUpdate{Plans: &candidate, Reason: "remove configured plan", ValidateOnly: mode == "validation"}
					if mode == "prepare" || mode == "publication" {
						s.config.Hosted.SharedEntry = &HostedSharedEntry{}
						update.StageID = stageID
					}
					before := s.database.plansService().RuntimeState()
					setReference()
					var err error
					switch mode {
					case "startup":
						cfg := *s.config.Hosted
						cfg.Plans = &candidate
						err = hubapi.FromFeatureError(s.database.plansService().ConfigurePlans(t.Context(), PlansInput(&cfg)))
					case "publication":
						if kind == "disable catalog" {
							err = s.applyHostedRuntime(t.Context(), &update)
						} else {
							if s == services[0] {
								if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES($1)", update.StageID); err != nil {
									t.Fatal(err)
								}
							}
							err = hubapi.FromFeatureError(s.database.plansService().RefreshRuntime(t.Context()))
						}
					default:
						err = s.applyHostedRuntime(t.Context(), &update)
					}
					wantError := kind == "base" || kind == "subscription" || kind == "grant" || kind == "future grant"
					if wantError {
						if err == nil || !strings.Contains(err.Error(), "configuration omits referenced plan") || s.database.plansService().RuntimeState() != before {
							t.Fatalf("%s removal error=%v; snapshot retained=%t", s.database.hostedOrganization, err, s.database.plansService().RuntimeState() == before)
						}
					} else if err != nil {
						t.Fatalf("%s exempt/unreferenced plan blocked: %v", s.database.hostedOrganization, err)
					}
					if kind == "disable catalog" && (mode == "startup" || mode == "reload") {
						if state := s.database.plansService().RuntimeState(); state == nil || state.Plans.Base != Tiers()[0].PlanReference {
							t.Fatalf("%s catalog without plans did not default to the free plan: %+v", s.database.hostedOrganization, state)
						}
					}
				}
			})
		}
	}
}
