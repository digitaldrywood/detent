package hubserver

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestPostgresCatalogRemovalChecksReferences(t *testing.T) {
	repo, admin := projectRecordPostgresWithAdmin(t, 4)
	for _, mode := range []string{"startup", "validation", "reload", "prepare", "publication"} {
		for _, kind := range []string{"base", "subscription", "grant", "future grant", "expired grant", "revoked grant", "unreferenced", "administrator", "payment pending", "disable catalog"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				plans := pilotHostedPlans()
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
						if err := s.database.applyHostedPlanCommand(t.Context(), "operator", hostedPlanCommand{ID: "admin_create", Action: "create_plan", ExpectedRevision: 1, Plan: plan.PlanReference, Definition: &plan, Reason: "administrator plan"}); err != nil {
							t.Fatal(err)
						}
					}
					setReference := func() {
						t.Helper()
						if err := s.database.writeBillingTenant(t.Context(), func(w tenantstore.Writer) error {
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
						}); err != nil {
							t.Fatal(err)
						}
					}
					update := HostedRuntimeUpdate{Plans: &candidate, Reason: "remove configured plan", ValidateOnly: mode == "validation"}
					if mode == "prepare" || mode == "publication" {
						s.config.Hosted.SharedEntry = &HostedSharedEntry{}
						update.StageID = stageID
					}
					before := s.database.runtimeState()
					setReference()
					var err error
					switch mode {
					case "startup":
						cfg := *s.config.Hosted
						cfg.Plans = &candidate
						err = s.database.configureHostedPlans(t.Context(), &cfg)
					case "publication":
						if kind == "disable catalog" {
							err = s.applyHostedRuntime(t.Context(), &update)
						} else {
							if s == services[0] {
								if _, err := admin.ExecContext(t.Context(), "INSERT INTO hosted_runtime_publications(id) VALUES($1)", update.StageID); err != nil {
									t.Fatal(err)
								}
							}
							err = s.database.refreshRuntime(t.Context())
						}
					default:
						err = s.applyHostedRuntime(t.Context(), &update)
					}
					wantError := kind == "base" || kind == "subscription" || kind == "grant" || kind == "future grant" || kind == "disable catalog"
					if wantError {
						if err == nil || !strings.Contains(err.Error(), "configuration omits referenced plan") || s.database.runtimeState() != before {
							t.Fatalf("%s removal error=%v; snapshot retained=%t", s.database.hostedOrganization, err, s.database.runtimeState() == before)
						}
					} else if err != nil {
						t.Fatalf("%s exempt/unreferenced plan blocked: %v", s.database.hostedOrganization, err)
					}
				}
			})
		}
	}
}
