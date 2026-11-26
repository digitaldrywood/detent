package postgres_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestScheduledDecisionTenantReplay(t *testing.T) {
	t.Parallel()
	repository, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, repository, org)
		input := progresscheck.CIFailure{Repository: "owner/repository", HeadSHA: "head", RunID: "run", RunAttempt: "1", JobID: "job", JobName: "test", ExitStatus: "failure"}
		want := progresscheck.CIDecision{Outcome: progresscheck.CISource, Target: "test", Reason: string(org)}
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			owner, ok := w.(policystore.ScheduledWriter)
			if !ok {
				t.Fatal("PostgreSQL scheduled decision owner is missing")
			}
			before, err := w.CollaborationBytes(t.Context())
			if err != nil {
				return err
			}
			if usage, err := owner.StoreScheduledDecision(t.Context(), "prj_first", input, want, time.Now(), math.MaxInt64); err != nil || !usage.Applied {
				t.Fatalf("new decision accounting = %+v, %v", usage, err)
			}
			other := want
			other.Reason = "replacement"
			if usage, err := owner.StoreScheduledDecision(t.Context(), "prj_first", input, other, time.Now(), math.MaxInt64); err != nil || !usage.Allowed || usage.Applied {
				t.Fatalf("replayed decision accounting = %+v, %v", usage, err)
			}
			after, err := w.CollaborationBytes(t.Context())
			if err != nil {
				return err
			}
			wantBytes := int64(72)
			if org == "org_second" {
				wantBytes = 74
			}
			if after-before != wantBytes {
				t.Fatalf("decision bytes = %d, want %d", after-before, wantBytes)
			}
			got, err := owner.ReadScheduledDecision(t.Context(), "prj_first", input)
			if err != nil {
				return err
			}
			if got != want {
				t.Fatalf("decision = %+v, want %+v", got, want)
			}
			if usage, err := owner.UpdateScheduledDecision(t.Context(), "prj_first", input, other, math.MaxInt64); err != nil || !usage.Applied {
				t.Fatalf("updated decision accounting = %+v, %v", usage, err)
			}
			updated, err := w.CollaborationBytes(t.Context())
			if err != nil {
				return err
			}
			wantBytes = 74
			if org == "org_second" {
				wantBytes = 75
			}
			if updated-before != wantBytes {
				t.Fatalf("updated decision bytes = %d, want %d", updated-before, wantBytes)
			}
			input.HeadSHA = "changed"
			if _, err := owner.ReadScheduledDecision(t.Context(), "prj_first", input); err == nil {
				t.Fatal("changed head replay accepted")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			owner := w.(policystore.ScheduledWriter)
			for _, id := range []string{"original", "replacement"} {
				if _, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: org, ID: id, WindowStart: now.Add(-time.Hour), Metric: "api_mutations", Amount: 1}, 10); err != nil {
					return err
				}
			}
			for _, step := range []struct {
				id   string
				at   time.Time
				want bool
			}{
				{"original", now, true},
				{"replacement", now, false},
				{"replacement", now.Add(time.Minute), true},
			} {
				got, err := owner.ClaimScheduledClassification(t.Context(), "prj_first", "job", step.id, step.at, step.at.Add(time.Minute))
				if err != nil || got != step.want {
					t.Fatalf("claim %s = %t, %v; want %t", step.id, got, err, step.want)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		err = repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
			return w.PruneRequestBillingUsage(t.Context(), now, 128)
		})
		if err != nil {
			t.Fatal(err)
		}
		err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			owner := w.(policystore.ScheduledWriter)
			if released, err := owner.ReleaseScheduledClassification(t.Context(), "prj_first", "job", "original", nil); err != nil || released {
				t.Fatalf("expired owner release = %t, %v", released, err)
			}
			at := now.Add(time.Minute)
			if released, err := owner.ReleaseScheduledClassification(t.Context(), "prj_first", "job", "replacement", &at); err != nil || !released {
				t.Fatalf("live owner release after retention = %t, %v", released, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		input.HeadSHA, input.JobID = "head", "denied"
		denied := errors.New("storage allowance exhausted")
		err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			usage, err := w.(policystore.ScheduledWriter).StoreScheduledDecision(t.Context(), "prj_first", input, want, time.Now(), 0)
			if err != nil || usage.Allowed {
				t.Fatalf("decision over storage allowance = %+v, %v", usage, err)
			}
			return denied
		})
		if !errors.Is(err, denied) {
			t.Fatal(err)
		}
		err = repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.(policystore.ScheduledReader).ReadScheduledDecision(t.Context(), "prj_first", input)
			return err
		})
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("decision over storage allowance persisted: %v", err)
		}
	}
}
