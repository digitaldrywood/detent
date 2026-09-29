package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// TestBackfillChangeReviewPolicies is hub migration 40: every project with an
// approved repository policy ends with a review policy a runner can publish
// under. A project approved before seeding existed gets the default, a
// project seeded while the default required review follows the repository
// gate, and a policy that pins checks is an administrator's decision and is
// kept. Running it again changes nothing.
func TestBackfillChangeReviewPolicies(t *testing.T) {
	t.Parallel()
	missing := newNativeFixture(t, nil, "", "missing")
	service := missing.service
	legacy := newNativeFixture(t, service, "", "legacy")
	current := newNativeFixture(t, service, "", "current")
	pinned := newNativeFixture(t, service, "", "pinned")
	unapproved := newNativeFixture(t, service, "", "unapproved")
	descriptor := hubTestPolicy()
	for _, f := range []nativeFixture{missing, legacy, current, pinned} {
		approveHubTestPolicy(t, service, f.base+"/policy", descriptor)
	}
	scopeOf := func(f nativeFixture) nativeScope {
		return nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	}
	store := func(f nativeFixture, rules tracker.ChangeReviewPolicy) {
		t.Helper()
		rules.ID = changerequest.PolicyID(rules)
		raw, err := json.Marshal(rules)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.database.db.ExecContext(t.Context(), "UPDATE change_review_policies SET policy_json = ? WHERE organization_id = ? AND project_id = ?", string(raw), f.project.OrganizationID, f.project.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.database.db.ExecContext(t.Context(), "DELETE FROM change_review_policies WHERE organization_id = ? AND project_id = ?", missing.project.OrganizationID, missing.project.ID); err != nil {
		t.Fatal(err)
	}
	store(legacy, tracker.ChangeReviewPolicy{PolicyID: descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}})
	var principal string
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE name = 'operator-pinned'").Scan(&principal); err != nil {
		t.Fatal(err)
	}
	shaped := tracker.ChangeReviewPolicy{PolicyID: descriptor.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{{Name: "test", PrincipalID: principal, WorkflowID: "ci.yml", WorkflowSHA256: policy.Digest([]byte("trusted CI")), Source: "independent", MaxAgeSeconds: 3600}}}
	store(pinned, shaped)
	shaped.ID = changerequest.PolicyID(shaped)

	want := defaultChangeReviewPolicy(descriptor)
	if want.RequireReview {
		t.Fatalf("default under a command gate = %#v, want no review requirement", want)
	}
	for _, run := range []string{"migrated", "applied again"} {
		t.Run(run, func(t *testing.T) {
			tx, err := service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := backfillChangeReviewPolicies(t.Context(), tx); err != nil {
				t.Fatal(errors.Join(err, tx.Rollback()))
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name string
				f    nativeFixture
				want tracker.ChangeReviewPolicy
			}{
				{name: "approved before seeding", f: missing, want: want},
				{name: "seeded under the old default", f: legacy, want: want},
				{name: "already the default", f: current, want: want},
				{name: "pinned checks", f: pinned, want: shaped},
			} {
				got, err := readChangePolicy(t.Context(), service.database.db, scopeOf(test.f))
				if err != nil {
					t.Fatalf("%s: %v", test.name, err)
				}
				if got.ID != test.want.ID || got.RequireReview != test.want.RequireReview || got.PolicyID != test.want.PolicyID {
					t.Fatalf("%s: review policy = %#v, want %#v", test.name, got, test.want)
				}
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, unapproved.base+"/change-review-policy", unapproved.token, nil), http.StatusNotFound)
		})
	}
}
