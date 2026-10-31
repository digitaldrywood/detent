package hubgithub

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPublishReleasePick(t *testing.T) {
	for _, name := range []string{"publish", "refresh", "refresh hold", "lost response", "wrong patch", "unmerged source", "frozen identity", "head moved", "branch moved", "truncated patch", "prepared missing trailer", "prepared wrong trailer", "prepared wrong parent", "head moves before create", "hold during verification", "different existing PR", "ambiguous", "closed PR", "different native owner"} {
		t.Run(name, func(t *testing.T) {
			input := tracker.ReleasePickInput{SourceIssueID: "wi_source", SourceChangeID: "change_source", Repository: "https://github.com/example-org/example-project", SourceMergeSHA: strings.Repeat("a", 40), HeadRef: "pick/fix", HeadSHA: strings.Repeat("b", 40), BaseRef: "release/v1.2", BaseSHA: strings.Repeat("c", 40), FrozenSHA: strings.Repeat("d", 40)}
			if strings.HasPrefix(name, "refresh") {
				input.PRNumber = 7
			}
			fixtureInput := input
			fixtureInput.PRNumber = 7
			f := &releasePickTransportFixture{input: fixtureInput}
			r := NewReconciler(f)
			authorize := func(context.Context) error { return nil }
			item := tracker.NativeWorkItemID("wi_delivery")
			if name == "different existing PR" || name == "ambiguous" || name == "closed PR" || name == "different native owner" {
				if _, err := r.PublishReleasePick(t.Context(), item, input, authorize); err != nil {
					t.Fatal(err)
				}
				if name == "different existing PR" {
					f.existing.Base.Ref = "develop"
				}
				if name == "closed PR" {
					f.existing.State = "closed"
				}
				if name == "different native owner" {
					item = "wi_other"
				}
			}
			f.alter = name
			if name == "head moves before create" {
				f.beforeRead = func() { f.alter = "head moved" }
			}

			if name == "hold during verification" || name == "refresh hold" {
				authorize = func(context.Context) error { return errors.New("human hold") }
			}
			f.lostResponse = name == "lost response"
			result, err := r.PublishReleasePick(t.Context(), item, input, authorize)
			if name == "lost response" {
				if err == nil || f.creates != 1 {
					t.Fatalf("lost response err=%v creates=%d", err, f.creates)
				}
				result, err = r.PublishReleasePick(t.Context(), item, input, authorize)
			}
			if name == "publish" || name == "lost response" || name == "refresh" {
				if err != nil || result.PRNumber != 7 || result.HeadSHA != input.HeadSHA || result.WorkItemID != item || result.URL != "https://github.com/example-org/example-project/pull/7" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if _, err := r.PublishReleasePick(t.Context(), item, input, authorize); err != nil {
					t.Fatal(err)
				}
				wantCreates := 1
				if name == "refresh" {
					wantCreates = 0
				}
				if f.creates != wantCreates {
					t.Fatalf("created %d PRs", f.creates)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe publication accepted")
				}
				want := 0
				if f.existing != nil {
					want = 1
				}
				if f.creates != want {
					t.Fatalf("refusal created PR: %d", f.creates)
				}
			}
		})
	}
}
