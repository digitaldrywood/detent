package postgres_test

import (
	"errors"
	"math"
	"testing"
	"time"

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
			input.HeadSHA = "changed"
			if _, err := owner.ReadScheduledDecision(t.Context(), "prj_first", input); err == nil {
				t.Fatal("changed head replay accepted")
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
