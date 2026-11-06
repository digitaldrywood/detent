package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type scheduledDecisions interface {
	ReadScheduledDecision(context.Context, tracker.ProjectID, progresscheck.CIFailure) (progresscheck.CIDecision, error)
	StoreScheduledDecision(context.Context, tracker.ProjectID, progresscheck.CIFailure, progresscheck.CIDecision, time.Time) error
}

func TestScheduledDecisionTenantReplay(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		input := progresscheck.CIFailure{Repository: "owner/repository", HeadSHA: "head", RunID: "run", RunAttempt: "1", JobID: "job", JobName: "test", ExitStatus: "failure"}
		want := progresscheck.CIDecision{Outcome: progresscheck.CISource, Target: "test", Reason: string(org)}
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			owner, ok := w.(scheduledDecisions)
			if !ok {
				t.Fatal("PostgreSQL scheduled decision owner is missing")
			}
			if err := owner.StoreScheduledDecision(t.Context(), "prj_first", input, want, time.Now()); err != nil {
				return err
			}
			other := want
			other.Reason = "replacement"
			if err := owner.StoreScheduledDecision(t.Context(), "prj_first", input, other, time.Now()); err != nil {
				return err
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
	}
}
