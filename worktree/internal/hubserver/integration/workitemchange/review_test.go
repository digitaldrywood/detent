package workitemchange_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeInvalidVersionAndReviewPolicy(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewChange(t, nil)
	for _, test := range []struct {
		name string
		edit func(*tracker.ChangeVersionInput)
		want int
	}{
		{"base", func(v *tracker.ChangeVersionInput) { v.BaseSHA = "main" }, http.StatusUnprocessableEntity},
		{"head", func(v *tracker.ChangeVersionInput) { v.HeadSHA = "HEAD" }, http.StatusUnprocessableEntity},
		{"merge base", func(v *tracker.ChangeVersionInput) { v.MergeBaseSHA = "" }, http.StatusUnprocessableEntity},
		{"repository", func(v *tracker.ChangeVersionInput) { v.Repository = "file:///source" }, http.StatusUnprocessableEntity},
		{"code hash", func(v *tracker.ChangeVersionInput) { v.Code.SHA256 = "missing" }, http.StatusUnprocessableEntity},
		{"unknown configuration", func(v *tracker.ChangeVersionInput) { v.PolicyID = "other" }, http.StatusUnprocessableEntity},
		{"unpaired attempt", func(v *tracker.ChangeVersionInput) { v.RunID = hubapi.NewID("run") }, http.StatusUnprocessableEntity},
		{"stale review disposition", func(v *tracker.ChangeVersionInput) {
			v.ReviewDispositions = []tracker.ReviewDisposition{{HeadSHA: v.BaseSHA, ThreadID: "T1", Disposition: "fixed", Evidence: "regression passes"}}
		}, http.StatusUnprocessableEntity},
		{"duplicate review dispositions", func(v *tracker.ChangeVersionInput) {
			d := tracker.ReviewDisposition{HeadSHA: v.HeadSHA, ThreadID: "T1", Disposition: "fixed", Evidence: "regression passes"}
			v.ReviewDispositions = []tracker.ReviewDisposition{d, d}
		}, http.StatusUnprocessableEntity},
		{"missing review evidence", func(v *tracker.ChangeVersionInput) {
			v.ReviewDispositions = []tracker.ReviewDisposition{{HeadSHA: v.HeadSHA, ThreadID: "T1", Disposition: "fixed"}}
		}, http.StatusUnprocessableEntity},
		{"unknown review disposition", func(v *tracker.ChangeVersionInput) {
			v.ReviewDispositions = []tracker.ReviewDisposition{{HeadSHA: v.HeadSHA, ThreadID: "T1", Disposition: "surfaced", Evidence: "seen"}}
		}, http.StatusUnprocessableEntity},
		{"oversized review evidence", func(v *tracker.ChangeVersionInput) {
			v.ReviewDispositions = []tracker.ReviewDisposition{{HeadSHA: v.HeadSHA, ThreadID: "T1", Disposition: "fixed", Evidence: strings.Repeat("x", 8193)}}
		}, http.StatusUnprocessableEntity},
		{"too many review dispositions", func(v *tracker.ChangeVersionInput) {
			v.ReviewDispositions = make([]tracker.ReviewDisposition, 101)
		}, http.StatusUnprocessableEntity},
		{"forged PR identity", func(v *tracker.ChangeVersionInput) {
			v.External = &tracker.ChangeExternalReference{Provider: "github", ID: "2", URL: "https://github.com/example/repo/pull/1"}
		}, http.StatusUnprocessableEntity},
		{"unsafe PR URL", func(v *tracker.ChangeVersionInput) {
			v.External = &tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: "javascript:alert(1)"}
		}, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := hubfixture.ChangeInput()
			test.edit(&input)
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Path+"/versions", f.Token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ChangeVersionInput: input}), test.want)
		})
	}
	runner := hubfixture.PrepareRunner(t, f.Native, runnerauth.Read)
	runner.Enroll(t)
	for _, token := range []string{f.Token, runner.Redemption.Credential} {
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, f.Base+"/change-review-policy", token, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "forge-policy"}, Policy: f.Rules}), http.StatusForbidden)
	}
	request := tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "stale-policy"}, Policy: f.Rules}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, f.Base+"/change-review-policy", hubfixture.AdminToken, request), http.StatusConflict)
	request.IdempotencyKey, request.ExpectedID = "unknown-ci", f.Rules.ID
	request.Policy.RequiredChecks[0].PrincipalID = "unknown"
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, f.Base+"/change-review-policy", hubfixture.AdminToken, request), http.StatusUnprocessableEntity)
}
