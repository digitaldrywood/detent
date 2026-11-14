package accessrecords_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectPolicyAuthorityIsRevisionedPerTenantAndCASProtected(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	organizations := []tracker.OrganizationID{"org_policy_first", "org_policy_second"}
	for _, organization := range organizations {
		pgstoretest.SeedRepository(t, repository, organization)
	}
	now := time.Date(2026, 11, 1, 20, 0, 0, 0, time.UTC)
	for index, organization := range organizations {
		project := tracker.ProjectID("prj_first")
		policyID := fmt.Sprintf("policy_%d", index+1)
		row := pgstoretest.PolicyTestRevision(organization, project, policyID, now)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.ApproveProjectPolicy(t.Context(), row, "")
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := r.ProjectPolicy(t.Context(), project)
			if err != nil {
				return err
			}
			if !policyRevisionEqual(got, row) {
				t.Errorf("organization %s policy = %+v, want %+v", organization, got, row)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		stale := pgstoretest.PolicyTestRevision(organization, project, policyID+"_next", now.Add(time.Minute))
		err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.ApproveProjectPolicy(t.Context(), stale, "stale")
		})
		if !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("stale project-policy write error = %v, want revision conflict", err)
		}
	}

	policy := pgstoretest.PolicyTestRevision(organizations[0], "prj_first", "policy_1", now)
	review := eventstore.ReviewPolicy{OrganizationID: organizations[0], ProjectID: "prj_first", PolicyRevisionID: policy.PolicyID, ReviewPolicyID: "review_1", Revision: 1, UpdatedBy: "admin", UpdatedAt: now, PayloadObjectKey: policy.PayloadObjectKey, PayloadSHA256: policy.PayloadSHA256, PayloadBytes: policy.PayloadBytes, PayloadContentBytes: policy.PayloadContentBytes, Compatibility: &policystore.ReviewFacts{RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{{Name: "ci", PrincipalID: "ci", WorkflowID: "ci.yml", WorkflowSHA256: strings.Repeat("a", 64), Source: "independent", MaxAgeSeconds: 3600}}}}
	if err := repository.WriteTenant(t.Context(), organizations[0], corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.SetReviewPolicy(t.Context(), review, ""); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), organizations[0], corestore.Serving, func(r tenantstore.Reader) error {
		got, err := r.ReviewPolicy(t.Context(), "prj_first")
		if err != nil {
			return err
		}
		if !reviewPolicyEqual(got, review) {
			t.Errorf("review policy = %+v, want %+v", got, review)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	staleReview := review
	staleReview.ReviewPolicyID = "review_stale"
	staleReview.Revision++
	err := repository.WriteTenant(t.Context(), organizations[0], corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetReviewPolicy(t.Context(), staleReview, "review_stale")
	})
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("stale review-policy write error = %v, want revision conflict", err)
	}
	nextReview := review
	nextReview.ReviewPolicyID, nextReview.Revision = "review_next", 2
	if err := repository.WriteTenant(t.Context(), organizations[0], corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetReviewPolicy(t.Context(), nextReview, review.ReviewPolicyID)
	}); err != nil {
		t.Fatal(err)
	}
	tampered := review
	replacement := pgstoretest.PolicyTestRevision(organizations[0], "prj_first", "tampered", now)
	tampered.Revision = 3
	tampered.PayloadObjectKey, tampered.PayloadSHA256 = replacement.PayloadObjectKey, replacement.PayloadSHA256
	err = repository.WriteTenant(t.Context(), organizations[0], corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetReviewPolicy(t.Context(), tampered, nextReview.ReviewPolicyID)
	})
	if !errors.Is(err, corestore.ErrUniqueViolation) {
		t.Fatalf("changed immutable review content error=%v, want unique violation", err)
	}
	if err := repository.ReadTenant(t.Context(), organizations[0], corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.ReviewPolicy(t.Context(), "prj_first")
		if err != nil {
			return err
		}
		if current.ReviewPolicyID != nextReview.ReviewPolicyID || current.Revision != 2 {
			t.Fatalf("refused immutable content changed current review: %+v", current)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	type boardScope struct {
		organization tracker.OrganizationID
		project      tracker.ProjectID
	}
	readHeads := func(t *testing.T) map[boardScope]viewstore.BoardSequence {
		t.Helper()
		heads := make(map[boardScope]viewstore.BoardSequence)
		for _, organization := range organizations {
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
					head, err := r.BoardSequence(t.Context(), project)
					if err != nil {
						return err
					}
					heads[boardScope{organization, project}] = head
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		return heads
	}
	abort := errors.New("abort policy reset")
	for _, organization := range organizations {
		for _, test := range []struct {
			name                 string
			project, review      bool
			source, otherProject bool
			want                 error
		}{
			{name: "project reset", project: true},
			{name: "review reset", review: true},
			{name: "combined reset then source", project: true, review: true, source: true},
			{name: "two project resets", project: true, otherProject: true},
			{name: "same policy replay"},
			{name: "stale policy CAS", project: true, want: corestore.ErrRevisionConflict},
			{name: "rolled back combined reset", project: true, review: true, want: abort},
		} {
			t.Run(string(organization)+"/"+test.name, func(t *testing.T) {
				before := readHeads(t)
				key := boardScope{organization, "prj_first"}
				var currentPolicy policystore.Revision
				var currentReview eventstore.ReviewPolicy
				err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					var err error
					currentPolicy, err = w.ProjectPolicy(t.Context(), key.project)
					if err != nil {
						return err
					}
					currentReview, err = w.ReviewPolicy(t.Context(), key.project)
					if err != nil && !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					selected := currentPolicy
					if test.project {
						selected = pgstoretest.PolicyTestRevision(organization, key.project, "reset_"+strings.ReplaceAll(test.name, " ", "_"), now)
						expected := currentPolicy.PolicyID
						if errors.Is(test.want, corestore.ErrRevisionConflict) {
							expected = "stale"
						}
						if err := w.ApproveProjectPolicy(t.Context(), selected, expected); err != nil {
							return err
						}
					}
					if test.review {
						next := eventstore.ReviewPolicy{
							OrganizationID: organization, ProjectID: key.project, PolicyRevisionID: selected.PolicyID,
							ReviewPolicyID: "review_" + strings.ReplaceAll(test.name, " ", "_"), Revision: currentReview.Revision + 1,
							UpdatedBy: "admin", UpdatedAt: now, PayloadObjectKey: selected.PayloadObjectKey,
							PayloadSHA256: selected.PayloadSHA256, PayloadBytes: selected.PayloadBytes, PayloadContentBytes: selected.PayloadContentBytes,
						}
						if err := w.SetReviewPolicy(t.Context(), next, currentReview.ReviewPolicyID); err != nil {
							return err
						}
					}
					if !test.project && !test.review {
						if err := w.ApproveProjectPolicy(t.Context(), currentPolicy, currentPolicy.PolicyID); err != nil {
							return err
						}
						if err := w.SetReviewPolicy(t.Context(), currentReview, currentReview.ReviewPolicyID); err != nil {
							return err
						}
					}
					if test.otherProject {
						next := pgstoretest.PolicyTestRevision(organization, "prj_second", "second_project_policy", now)
						if err := w.ApproveProjectPolicy(t.Context(), next, ""); err != nil {
							return err
						}
					}
					if test.source {
						item, err := w.WorkItem(t.Context(), key.project, "wi_first_1")
						if err != nil {
							return err
						}
						item.Title = "Current policy source"
						if _, err := w.UpdateWorkItem(t.Context(), item); err != nil {
							return err
						}
					}
					head, err := w.BoardSequence(t.Context(), key.project)
					if err != nil {
						return err
					}
					if head != before[key] {
						t.Fatalf("policy notice allocated before callback completed: head=%+v before=%+v", head, before[key])
					}
					return test.want
				})
				if !errors.Is(err, test.want) {
					t.Fatalf("policy reset error=%v want=%v", err, test.want)
				}
				after := readHeads(t)
				for scope, head := range before {
					changed := test.want == nil && (test.project || test.review) && scope.organization == organization && (scope.project == key.project || test.otherProject)
					if !changed {
						if after[scope] != head {
							t.Errorf("unrelated/no-op/failed policy reset changed %s/%s head: before=%+v after=%+v", scope.organization, scope.project, head, after[scope])
						}
						continue
					}
					advance := int64(1)
					if test.source && scope == key {
						advance++
					}
					if after[scope].Sequence != head.Sequence+advance || after[scope].ReplayFloor != head.Sequence+1 {
						t.Fatalf("policy reset did not retain exact committed reset sequence for %s/%s: before=%+v after=%+v", scope.organization, scope.project, head, after[scope])
					}
					if err := repository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
						deltas, err := r.BoardDeltas(t.Context(), scope.project, head.Sequence, 3)
						if err != nil {
							return err
						}
						if len(deltas) != int(advance) || deltas[0].Sequence != head.Sequence+1 || deltas[0].Topic != "board" || deltas[0].Kind != "invalidate" || deltas[0].EntityID != string(scope.project) || deltas[0].Previous != nil || deltas[0].Current != nil {
							t.Fatalf("policy reset markers=%+v", deltas)
						}
						if advance == 2 && (deltas[1].Topic != "work_item" || deltas[1].EntityID != "wi_first_1" || deltas[1].Sequence != after[scope].Sequence) {
							t.Fatalf("policy reset reordered the later source event: %+v", deltas)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if test.want != nil {
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						stored, err := r.ProjectPolicy(t.Context(), key.project)
						if err != nil {
							return err
						}
						storedReview, err := r.ReviewPolicy(t.Context(), key.project)
						if err == nil && (!policyRevisionEqual(stored, currentPolicy) || !reviewPolicyEqual(storedReview, currentReview)) {
							t.Error("failed policy reset committed policy state")
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}

}

func TestProjectPolicyRevisionRetainsFrozenLandingGate(t *testing.T) {
	repository, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	organization := tracker.OrganizationID("org_policy_landing")
	pgstoretest.SeedRepository(t, repository, organization)
	project := tracker.ProjectID("prj_first")
	now := time.Date(2026, 11, 1, 20, 0, 0, 0, time.UTC)
	initial := pgstoretest.PolicyTestRevision(organization, project, "policy_non_github", now)
	next := pgstoretest.PolicyTestRevision(organization, project, "policy_github", now.Add(time.Minute))
	*next.GitHubPullRequest = true
	for _, write := range []struct {
		row      policystore.Revision
		expected string
	}{{initial, ""}, {next, initial.PolicyID}} {
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.ApproveProjectPolicy(t.Context(), write.row, write.expected)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		for _, want := range []policystore.Revision{initial, next} {
			got, err := r.ProjectPolicyRevision(t.Context(), project, want.PolicyID)
			if err != nil {
				return err
			}
			if !policyRevisionEqual(got, want) {
				t.Errorf("frozen policy revision = %+v, want %+v", got, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLeasePolicyPinSharesClaimTransactionAndTenantScope(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	var statement string
	organizations := []tracker.OrganizationID{"org_lease_policy_first", "org_lease_policy_second"}
	now := time.Date(2026, 11, 1, 20, 0, 0, 0, time.UTC)
	for _, organization := range organizations {
		pgstoretest.SeedLeaseFamily(t, repository, organization)
		policy := pgstoretest.PolicyTestRevision(organization, "prj_1", "policy_claim", now)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.ApproveProjectPolicy(t.Context(), policy, "")
		}); err != nil {
			t.Fatal(err)
		}
	}

	rollback := errors.New("rollback claim fixture")
	first := organizations[0]
	err := repository.WriteTenant(t.Context(), first, corestore.Serving, func(w tenantstore.Writer) error {
		lease, err := w.Claim(t.Context(), pgstoretest.LeaseClaim(1))
		if err != nil {
			return fmt.Errorf("claim lease: %w", err)
		}
		if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: first, ProjectID: "prj_1", LeaseID: "lease_1", PolicyID: "policy_claim", BoundAt: now}); err != nil {
			return fmt.Errorf("bind lease policy: %w", err)
		}
		if lease.LeaseID != "lease_1" {
			t.Fatalf("claimed lease id = %q", lease.LeaseID)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("claim transaction error = %v, want rollback fixture", err)
	}
	if err := repository.ReadTenant(t.Context(), first, corestore.Serving, func(r tenantstore.Reader) error {
		if _, err := r.Lease(t.Context(), "lease_1"); !errors.Is(err, corestore.ErrNotFound) {
			t.Errorf("rolled-back lease lookup error = %v, want not found", err)
		}
		if _, err := r.LeasePolicy(t.Context(), "lease_1"); !errors.Is(err, corestore.ErrNotFound) {
			t.Errorf("rolled-back policy lookup error = %v, want not found", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, organization := range organizations {
		leaseID := "lease_1"
		var lease leasestore.Lease
		err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			var err error
			lease, err = w.Claim(t.Context(), pgstoretest.LeaseClaim(1))
			if err != nil {
				return err
			}
			return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: organization, ProjectID: "prj_1", LeaseID: tracker.LeaseID(leaseID), PolicyID: "policy_claim", BoundAt: now})
		})
		if err != nil {
			t.Fatal(err)
		}
		if lease.LeaseID != leaseID || lease.IssueID != 1 {
			t.Fatalf("claim for %s = %+v", organization, lease)
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			pinned, err := r.LeasePolicy(t.Context(), tracker.LeaseID(leaseID))
			if err != nil {
				return err
			}
			if pinned.OrganizationID != organization || pinned.ProjectID != "prj_1" || pinned.LeaseID != tracker.LeaseID(leaseID) || pinned.PolicyID != "policy_claim" || !pinned.BoundAt.Equal(now) {
				t.Errorf("policy pin for %s = %+v", organization, pinned)
			}
			bySession, err := r.LeaseBySession(t.Context(), "session_1")
			if err != nil {
				return err
			}
			if bySession.LeaseID != lease.LeaseID || bySession.IssueID != lease.IssueID || bySession.MachineID != lease.MachineID {
				t.Errorf("session lookup for %s = %+v, want %+v", organization, bySession, lease)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		readMutationPolicies := func(want map[tracker.ProjectID]billingstore.MonthlyPolicy) {
			t.Helper()
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				owner := r.(interface {
					LeaseMutationPolicies(context.Context, tracker.ProjectID, tracker.LeaseID) (policystore.LeasePolicy, []billingstore.MonthlyPolicy, error)
				})
				observed := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, query string) {
					if strings.Contains(query, "FROM lease_policy_bindings pin") {
						statement = query
					}
				})
				pinned, policies, err := owner.LeaseMutationPolicies(observed, "prj_1", tracker.LeaseID(leaseID))
				if err != nil {
					return err
				}
				if pinned.OrganizationID != organization || pinned.ProjectID != "prj_1" || pinned.LeaseID != tracker.LeaseID(leaseID) || pinned.PolicyID != "policy_claim" || !pinned.BoundAt.Equal(now) {
					t.Fatalf("mutation pin for %s=%+v", organization, pinned)
				}
				if len(policies) != len(want) {
					t.Fatalf("mutation monthly policies=%+v, want %+v", policies, want)
				}
				seen := map[tracker.ProjectID]bool{}
				for _, row := range policies {
					expected, ok := want[row.ProjectID]
					if !ok || seen[row.ProjectID] || row.OrganizationID != organization || row.Revision != expected.Revision || row.Actor != expected.Actor || !row.UpdatedAt.Equal(expected.UpdatedAt) || !reflect.DeepEqual(row.Policy, expected.Policy) {
						t.Fatalf("mutation monthly policy=%+v, want %+v", row, expected)
					}
					seen[row.ProjectID] = true
				}
				for _, test := range []struct {
					project tracker.ProjectID
					lease   tracker.LeaseID
				}{{"prj_2", tracker.LeaseID(leaseID)}, {"prj_1", "lease_missing"}} {
					if _, _, err := owner.LeaseMutationPolicies(t.Context(), test.project, test.lease); !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("foreign/missing mutation policy lookup=%+v error=%v", test, err)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		readMutationPolicies(nil)
		orgLimit := int64(100)
		if organization != first {
			orgLimit = 200
		}
		organizationPolicy := billingstore.MonthlyPolicy{OrganizationID: organization, Revision: 1, Policy: budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, TotalMicros: new(orgLimit)}, Actor: "fixture", UpdatedAt: now}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.SetBillingMonthlyPolicy(t.Context(), organizationPolicy, 0)
		}); err != nil {
			t.Fatal(err)
		}
		readMutationPolicies(map[tracker.ProjectID]billingstore.MonthlyPolicy{"": organizationPolicy})
		organizationPolicy.Revision = 2
		organizationPolicy.UpdatedAt = now.Add(time.Minute)
		organizationPolicy.Policy.TotalMicros = new(orgLimit + 10)
		projectPolicy := billingstore.MonthlyPolicy{OrganizationID: organization, ProjectID: "prj_1", Revision: 1, Policy: budget.MonthlyPolicy{Enabled: true, Mode: budget.DrainIssue, RunnerMicros: new(orgLimit + 20)}, Actor: "fixture", UpdatedAt: now}
		foreignPolicy := billingstore.MonthlyPolicy{OrganizationID: organization, ProjectID: "prj_2", Revision: 1, Policy: budget.MonthlyPolicy{Enabled: true, Mode: budget.HardStop, TotalMicros: new(int64(999))}, Actor: "fixture", UpdatedAt: now}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, value := range []struct {
				row      billingstore.MonthlyPolicy
				expected tracker.Revision
			}{{organizationPolicy, 1}, {projectPolicy, 0}, {foreignPolicy, 0}} {
				if err := w.SetBillingMonthlyPolicy(t.Context(), value.row, value.expected); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		readMutationPolicies(map[tracker.ProjectID]billingstore.MonthlyPolicy{"": organizationPolicy, "prj_1": projectPolicy})
		organizationPolicy.Revision, projectPolicy.Revision = 1002, 1002
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, row := range []billingstore.MonthlyPolicy{organizationPolicy, projectPolicy} {
				if err := w.ImportBillingMonthlyPolicy(t.Context(), row); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO monthly_budget_policies(organization_id,project_id,revision,enabled,mode,actor_id,updated_at)
 SELECT $1,requested.project_id,revision,false,$2,'fixture_history',$3
 FROM (VALUES (''::text),('prj_1'::text)) requested(project_id) CROSS JOIN generate_series(3,1001) revision`, organization, budget.DrainIssue, now); err != nil {
			t.Fatal(err)
		}
		readMutationPolicies(map[tracker.ProjectID]billingstore.MonthlyPolicy{"": organizationPolicy, "prj_1": projectPolicy})
		if organization == first {
			err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: organization, ProjectID: "prj_1", LeaseID: tracker.LeaseID(leaseID), PolicyID: "policy_claim", BoundAt: now.Add(time.Minute)})
			})
			if !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("conflicting lease policy bind error = %v, want revision conflict", err)
			}
		}
	}
	if statement == "" {
		t.Fatal("lease policy metadata statement was not observed")
	}
	if _, err := admin.ExecContext(t.Context(), "ANALYZE monthly_budget_policies"); err != nil {
		t.Fatal(err)
	}
	connection, err := admin.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	tx, err := connection.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "SET LOCAL ROLE detent_runtime_v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", first); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement, first, "lease_1", "prj_1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan []struct{ Plan pgstoretest.SearchPlanNode }
	if err := json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
		t.Fatalf("decode lease policy plan: %v", err)
	}
	if !plan[0].Plan.UsesIndex("monthly_budget_policies_pkey") {
		t.Fatalf("latest policy reads did not use default index: %s", raw)
	}
	var visits func(pgstoretest.SearchPlanNode) float64
	visits = func(node pgstoretest.SearchPlanNode) float64 {
		count := 0.0
		if node.Relation == "monthly_budget_policies" {
			count = (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
		}
		for _, child := range node.Plans {
			count += visits(child)
		}
		return count
	}
	if count := visits(plan[0].Plan); count > 2 {
		t.Fatalf("latest policy reads visited %.0f rows: %s", count, raw)
	}
	t.Logf("default runtime-role lease policy plan=%s", raw)

}

func TestPolicyWritesWaitForSharedProjectAuthorityLock(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	now := time.Date(2026, 11, 1, 20, 0, 0, 0, time.UTC)
	for _, updateReview := range []bool{false, true} {
		name := "project policy"
		if updateReview {
			name = "review policy"
		}
		t.Run(name, func(t *testing.T) {
			organization := tracker.OrganizationID("org_policy_lock_" + map[bool]string{false: "project", true: "review"}[updateReview])
			project := tracker.ProjectID("prj_first")
			pgstoretest.SeedRepository(t, repository, organization)
			initial := pgstoretest.PolicyTestRevision(organization, project, "policy_initial", now)
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ApproveProjectPolicy(t.Context(), initial, "")
			}); err != nil {
				t.Fatal(err)
			}
			initialReview := eventstore.ReviewPolicy{OrganizationID: organization, ProjectID: project, PolicyRevisionID: initial.PolicyID, ReviewPolicyID: "review_initial", Revision: 1, UpdatedBy: "admin", UpdatedAt: now, PayloadObjectKey: initial.PayloadObjectKey, PayloadSHA256: initial.PayloadSHA256, PayloadBytes: initial.PayloadBytes, PayloadContentBytes: initial.PayloadContentBytes}
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.SetReviewPolicy(t.Context(), initialReview, "")
			}); err != nil {
				t.Fatal(err)
			}
			locked := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			first := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
				return repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.LockWorkItemProject(ctx, project); err != nil {
						return err
					}
					close(locked)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			})
			pgstoretest.WaitRepositorySignal(t, locked)
			started := make(chan struct{})
			second := pgstoretest.StartRepositoryTask(t, func(ctx context.Context) error {
				return repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
					close(started)
					if updateReview {
						next := initialReview
						next.ReviewPolicyID = "review_next"
						next.Revision++
						next.UpdatedAt = now.Add(time.Minute)
						return w.SetReviewPolicy(ctx, next, initialReview.ReviewPolicyID)
					}
					next := pgstoretest.PolicyTestRevision(organization, project, "policy_next", now.Add(time.Minute))
					return w.ApproveProjectPolicy(ctx, next, initial.PolicyID)
				})
			})
			pgstoretest.WaitRepositorySignal(t, started)
			pgstoretest.WaitLeaseLock(t, admin, 1, second)
			releaseOnce.Do(func() { close(release) })
			if err := first.Wait(t); err != nil {
				t.Fatal(err)
			}
			if err := second.Wait(t); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				if updateReview {
					current, err := r.ReviewPolicy(t.Context(), project)
					if err != nil {
						return err
					}
					if current.ReviewPolicyID != "review_next" || current.Revision != 2 {
						t.Errorf("review policy after lock = %+v", current)
					}
					return nil
				}
				current, err := r.ProjectPolicy(t.Context(), project)
				if err != nil {
					return err
				}
				if current.PolicyID != "policy_next" {
					t.Errorf("project policy after lock = %+v", current)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func policyRevisionEqual(left, right policystore.Revision) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.PolicyID == right.PolicyID && samePolicyGate(left.GitHubPullRequest, right.GitHubPullRequest) && left.ApprovedBy == right.ApprovedBy && left.ApprovedAt.Equal(right.ApprovedAt) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
}

func samePolicyGate(left, right *bool) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func reviewPolicyEqual(left, right eventstore.ReviewPolicy) bool {
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.PolicyRevisionID == right.PolicyRevisionID && left.ReviewPolicyID == right.ReviewPolicyID && left.Revision == right.Revision && left.UpdatedBy == right.UpdatedBy && left.UpdatedAt.Equal(right.UpdatedAt) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes && reflect.DeepEqual(left.Compatibility, right.Compatibility)
}

func TestPolicyRevisionCompatibilityFactsAreImmutableCompanions(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	organizations := []tracker.OrganizationID{"org_compat_first", "org_compat_second"}
	project := tracker.ProjectID("prj_first")
	now := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	check := func(name string) tracker.ChangeCheckSpec {
		return tracker.ChangeCheckSpec{Name: name, PrincipalID: "ci", WorkflowID: "ci.yml", WorkflowSHA256: strings.Repeat("a", 64), Source: "independent", MaxAgeSeconds: 3600}
	}
	write := func(organization tracker.OrganizationID, fn func(tenantstore.Writer) error) error {
		return repository.WriteTenant(t.Context(), organization, corestore.Serving, fn)
	}
	type written struct {
		policy  policystore.Revision
		checked eventstore.ReviewPolicy
		empty   eventstore.ReviewPolicy
		legacy  eventstore.ReviewPolicy
	}
	rows := map[tracker.OrganizationID]written{}
	for index, organization := range organizations {
		pgstoretest.SeedRepository(t, repository, organization)
		row := pgstoretest.PolicyTestRevision(organization, project, fmt.Sprintf("policy_compat_%d", index), now)
		row.Compatibility = &changerequest.CompatibilityGates{Kind: "command", SecurityAudit: index == 0, Validator: true, RequiredChecks: index + 1, AutomatedReview: "required", PlanEnabled: index == 1, PlanReview: "both"}
		review := func(id string, revision int64, facts *policystore.ReviewFacts) eventstore.ReviewPolicy {
			payload := pgstoretest.PolicyTestRevision(organization, project, id, now)
			return eventstore.ReviewPolicy{OrganizationID: organization, ProjectID: project, PolicyRevisionID: row.PolicyID, ReviewPolicyID: id, Revision: revision, UpdatedBy: "admin", UpdatedAt: now, PayloadObjectKey: payload.PayloadObjectKey, PayloadSHA256: payload.PayloadSHA256, PayloadBytes: 1, PayloadContentBytes: 1, Compatibility: facts}
		}
		w := written{
			policy:  row,
			checked: review("review_checked", 1, &policystore.ReviewFacts{RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{check("zeta"), check("alpha")}}),
			empty:   review("review_empty", 2, &policystore.ReviewFacts{RequiredChecks: []tracker.ChangeCheckSpec{}}),
			legacy:  review("review_legacy", 3, nil),
		}
		rows[organization] = w
		if err := write(organization, func(tw tenantstore.Writer) error {
			if err := tw.ApproveProjectPolicy(t.Context(), w.policy, ""); err != nil {
				return err
			}
			expected := ""
			for _, next := range []eventstore.ReviewPolicy{w.checked, w.empty, w.legacy} {
				if err := tw.SetReviewPolicy(t.Context(), next, expected); err != nil {
					return err
				}
				expected = next.ReviewPolicyID
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, organization := range organizations {
		w := rows[organization]
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			policies, err := r.PolicyCompatibility(t.Context(), project, []string{w.policy.PolicyID, rows[organizations[0]].policy.PolicyID, rows[organizations[1]].policy.PolicyID})
			if err != nil {
				return err
			}
			want := policystore.Compatibility{PayloadSHA256: w.policy.PayloadSHA256, Gates: *w.policy.Compatibility}
			if len(policies) != 1 || policies[w.policy.PolicyID] != want {
				t.Errorf("organization %s policy facts = %+v, want only %+v", organization, policies, want)
			}
			reviews, err := r.ReviewCompatibility(t.Context(), project, []string{"review_checked", "review_empty", "review_legacy"})
			if err != nil {
				return err
			}
			if got := reviews["review_checked"]; got.PolicyID != w.policy.PolicyID || got.PayloadSHA256 != w.checked.PayloadSHA256 || !got.RequireReview || !reflect.DeepEqual(got.RequiredChecks, w.checked.Compatibility.RequiredChecks) {
				t.Errorf("organization %s checked review facts = %+v, want ordered %+v", organization, got, w.checked.Compatibility)
			}
			if got, found := reviews["review_empty"]; !found || got.RequireReview || len(got.RequiredChecks) != 0 {
				t.Errorf("organization %s known-empty review facts = %+v found=%v", organization, got, found)
			}
			if got, found := reviews["review_legacy"]; found {
				t.Errorf("organization %s review without facts reported %+v", organization, got)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	first := rows[organizations[0]]
	conflicting := first.policy
	conflicting.Compatibility = new(*first.policy.Compatibility)
	conflicting.Compatibility.SecurityAudit = false
	if err := write(organizations[0], func(w tenantstore.Writer) error {
		return w.ApproveProjectPolicy(t.Context(), conflicting, first.policy.PolicyID)
	}); !errors.Is(err, corestore.ErrUniqueViolation) {
		t.Fatalf("conflicting policy facts replay error = %v, want unique violation", err)
	}
	disagreeing := pgstoretest.PolicyTestRevision(organizations[0], project, "policy_compat_gate", now)
	disagreeing.Compatibility = &changerequest.CompatibilityGates{GitHubPullRequest: true}
	if err := write(organizations[0], func(w tenantstore.Writer) error {
		return w.ApproveProjectPolicy(t.Context(), disagreeing, first.policy.PolicyID)
	}); !errors.Is(err, eventstore.ErrInvalidRecord) {
		t.Fatalf("facts disagreeing with the revision gate error = %v, want invalid record", err)
	}
	reordered := first.checked
	reordered.Revision = 4
	reordered.Compatibility = &policystore.ReviewFacts{RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{check("alpha"), check("zeta")}}
	if err := write(organizations[0], func(w tenantstore.Writer) error {
		return w.SetReviewPolicy(t.Context(), reordered, first.legacy.ReviewPolicyID)
	}); !errors.Is(err, corestore.ErrUniqueViolation) {
		t.Fatalf("reordered review facts replay error = %v, want unique violation", err)
	}
	if err := write(organizations[0], func(w tenantstore.Writer) error {
		return w.ApproveProjectPolicy(t.Context(), first.policy, first.policy.PolicyID)
	}); err != nil {
		t.Fatalf("identical facts replay: %v", err)
	}
	for _, statement := range []string{
		"UPDATE policy_revision_compatibility SET gate_validator=NOT gate_validator",
		"DELETE FROM policy_revision_compatibility",
		"UPDATE review_policy_revision_compatibility SET require_review=NOT require_review",
		"DELETE FROM review_policy_revision_checks",
	} {
		if _, err := admin.ExecContext(t.Context(), statement); err == nil || !strings.Contains(err.Error(), "history is immutable") {
			t.Errorf("%s error = %v, want immutable history refusal", statement, err)
		}
	}
}
