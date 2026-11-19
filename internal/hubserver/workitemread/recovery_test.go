package workitemread

import (
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoundedRecoveryReviewsKeepsCurrentResults(t *testing.T) {
	t.Parallel()
	source := func(id string) tracker.ChangeReview {
		return tracker.ChangeReview{ID: id, Decision: "approved", ImplementationReview: &gate.ValidatorResult{Verdict: "pass"}}
	}
	acceptance := func(id string) tracker.ChangeReview {
		return tracker.ChangeReview{ID: id, Decision: "approved", Validator: &gate.ValidatorResult{Verdict: "pass"}}
	}
	audit := func(id string) tracker.ChangeReview {
		return tracker.ChangeReview{ID: id, Decision: "changes_requested", SecurityAudit: &tracker.SecurityAuditReview{}}
	}
	human := tracker.ChangeReview{ID: "human", Decision: "changes_requested", Body: "Keep the behavior."}
	for _, test := range []struct {
		name    string
		reviews []tracker.ChangeReview
		want    []string
	}{
		{name: "acceptance before newer source", reviews: []tracker.ChangeReview{acceptance("old_acceptance"), acceptance("acceptance"), source("old_source"), human, source("source")}, want: []string{"acceptance", "human", "source"}},
		{name: "ordinary reviews", reviews: []tracker.ChangeReview{human, {ID: "bot", Decision: "commented"}}, want: []string{"human", "bot"}},
		{name: "earlier audits remain visible", reviews: []tracker.ChangeReview{audit("old_audit"), audit("audit")}, want: []string{"old_audit", "audit"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, _, err := boundedRecoveryReviews(test.reviews, 256<<10)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 0, len(got))
			for _, review := range got {
				ids = append(ids, review.ID)
			}
			if !slices.Equal(ids, test.want) {
				t.Fatalf("reviews = %v, want %v", ids, test.want)
			}
		})
	}
}
