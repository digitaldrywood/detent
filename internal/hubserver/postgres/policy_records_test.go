package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectPolicyAuthorityIsRevisionedPerTenantAndCASProtected(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	organizations := []tracker.OrganizationID{"org_policy_first", "org_policy_second"}
	for _, organization := range organizations {
		seedRepository(t, repository, organization)
	}
	now := time.Date(2026, 11, 1, 20, 0, 0, 0, time.UTC)
	for index, organization := range organizations {
		project := tracker.ProjectID("prj_first")
		policyID := fmt.Sprintf("policy_%d", index+1)
		row := policyTestRevision(organization, project, policyID, now)
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
		stale := policyTestRevision(organization, project, policyID+"_next", now.Add(time.Minute))
		err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.ApproveProjectPolicy(t.Context(), stale, "stale")
		})
		if !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("stale project-policy write error = %v, want revision conflict", err)
		}
	}

	policy := policyTestRevision(organizations[0], "prj_first", "policy_1", now)
	review := eventstore.ReviewPolicy{OrganizationID: organizations[0], ProjectID: "prj_first", PolicyRevisionID: policy.PolicyID, ReviewPolicyID: "review_1", Revision: 1, UpdatedBy: "admin", UpdatedAt: now, PayloadObjectKey: policy.PayloadObjectKey, PayloadSHA256: policy.PayloadSHA256, PayloadBytes: policy.PayloadBytes, PayloadContentBytes: policy.PayloadContentBytes}
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
	replacement := policyTestRevision(organizations[0], "prj_first", "tampered", now)
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

}

func TestProjectPolicyRevisionRetainsFrozenLandingGate(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	organization := tracker.OrganizationID("org_policy_landing")
	seedRepository(t, repository, organization)
	project := tracker.ProjectID("prj_first")
	now := time.Date(2026, 11, 1, 20, 0, 0, 0, time.UTC)
	initial := policyTestRevision(organization, project, "policy_non_github", now)
	next := policyTestRevision(organization, project, "policy_github", now.Add(time.Minute))
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
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	var statement string
	organizations := []tracker.OrganizationID{"org_lease_policy_first", "org_lease_policy_second"}
	now := time.Date(2026, 11, 1, 20, 0, 0, 0, time.UTC)
	for _, organization := range organizations {
		seedLeaseFamily(t, repository, organization)
		policy := policyTestRevision(organization, "prj_1", "policy_claim", now)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.ApproveProjectPolicy(t.Context(), policy, "")
		}); err != nil {
			t.Fatal(err)
		}
	}

	rollback := errors.New("rollback claim fixture")
	first := organizations[0]
	err := repository.WriteTenant(t.Context(), first, corestore.Serving, func(w tenantstore.Writer) error {
		lease, err := w.Claim(t.Context(), leaseClaim(1))
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
			lease, err = w.Claim(t.Context(), leaseClaim(1))
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
	var plan []struct{ Plan searchPlanNode }
	if err := json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
		t.Fatalf("decode lease policy plan: %v", err)
	}
	if !plan[0].Plan.usesIndex("monthly_budget_policies_pkey") {
		t.Fatalf("latest policy reads did not use default index: %s", raw)
	}
	var visits func(searchPlanNode) float64
	visits = func(node searchPlanNode) float64 {
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
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
	now := time.Date(2026, 11, 1, 20, 0, 0, 0, time.UTC)
	for _, updateReview := range []bool{false, true} {
		name := "project policy"
		if updateReview {
			name = "review policy"
		}
		t.Run(name, func(t *testing.T) {
			organization := tracker.OrganizationID("org_policy_lock_" + map[bool]string{false: "project", true: "review"}[updateReview])
			project := tracker.ProjectID("prj_first")
			seedRepository(t, repository, organization)
			initial := policyTestRevision(organization, project, "policy_initial", now)
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
			first := startRepositoryTask(t, func(ctx context.Context) error {
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
			waitRepositorySignal(t, locked)
			started := make(chan struct{})
			second := startRepositoryTask(t, func(ctx context.Context) error {
				return repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
					close(started)
					if updateReview {
						next := initialReview
						next.ReviewPolicyID = "review_next"
						next.Revision++
						next.UpdatedAt = now.Add(time.Minute)
						return w.SetReviewPolicy(ctx, next, initialReview.ReviewPolicyID)
					}
					next := policyTestRevision(organization, project, "policy_next", now.Add(time.Minute))
					return w.ApproveProjectPolicy(ctx, next, initial.PolicyID)
				})
			})
			waitRepositorySignal(t, started)
			waitLeaseLock(t, admin, 1, second)
			releaseOnce.Do(func() { close(release) })
			if err := first.wait(t); err != nil {
				t.Fatal(err)
			}
			if err := second.wait(t); err != nil {
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

func policyTestRevision(organization tracker.OrganizationID, project tracker.ProjectID, policyID string, at time.Time) policystore.Revision {
	digestBytes := sha256.Sum256([]byte(string(organization) + "/" + string(project) + "/" + policyID))
	digest := hex.EncodeToString(digestBytes[:])
	return policystore.Revision{OrganizationID: organization, ProjectID: project, PolicyID: policyID, GitHubPullRequest: new(false), ApprovedBy: "admin", ApprovedAt: at, PayloadObjectKey: "orgs/" + string(organization) + "/blobs/sha256/" + digest[:2] + "/" + digest, PayloadSHA256: digest, PayloadBytes: 1, PayloadContentBytes: 1}
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
	return left.OrganizationID == right.OrganizationID && left.ProjectID == right.ProjectID && left.PolicyRevisionID == right.PolicyRevisionID && left.ReviewPolicyID == right.ReviewPolicyID && left.Revision == right.Revision && left.UpdatedBy == right.UpdatedBy && left.UpdatedAt.Equal(right.UpdatedAt) && left.PayloadObjectKey == right.PayloadObjectKey && left.PayloadSHA256 == right.PayloadSHA256 && left.PayloadBytes == right.PayloadBytes && left.PayloadContentBytes == right.PayloadContentBytes
}
