package workitemchange_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeLandingPreservesCompatiblePolicyEvidence(t *testing.T) {
	t.Parallel()
	h := hubfixture.NewHostedSecurity(t)
	owner := h.User(t, "review_admin", "owner", "review-admin@example.test", "", "")
	for _, test := range []struct {
		name         string
		changePolicy func(*policy.Descriptor)
		wantStatus   int
	}{
		{name: "unchanged", wantStatus: http.StatusOK},
		{name: "non-gate configuration", changePolicy: func(p *policy.Descriptor) { p.ConfigDigest = policy.Digest([]byte("updated configuration")) }, wantStatus: http.StatusOK},
		{name: "merge method", changePolicy: func(p *policy.Descriptor) { p.Gates.MergeMethod = "merge" }, wantStatus: http.StatusOK},
		{name: "validator requirement", changePolicy: func(p *policy.Descriptor) { p.Gates.Validator = true }, wantStatus: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := hubtest.Project(t, h.Repository, h.Primary, hubtest.ProjectOptions{Name: test.name, States: hubfixture.States()})
			hubtest.Policy(t, h.Repository, h.Config.RunDetails, project, hubtest.PolicyOptions{Now: h.Now()})
			h.Grant(t, owner, project.ID, true, false)
			base := hubfixture.ProjectBase(h.Primary, project.ID)
			created := h.Request(t, owner, http.MethodPost, base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "item"}, Title: "Example source", Body: hubfixture.IssueBody(), State: "Todo"})
			hubfixture.RequireStatus(t, created, http.StatusOK)
			var item tracker.NativeIssue
			hubfixture.Decode(t, created, &item)
			path := base + "/work-items/" + string(item.WorkItemID) + "/changes"
			created = h.Request(t, owner, http.MethodPost, path, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Example source"})
			hubfixture.RequireStatus(t, created, http.StatusOK)
			var change tracker.ChangeRequest
			hubfixture.Decode(t, created, &change)
			path += "/" + change.ID
			input := hubfixture.ChangeInput()
			published := h.Request(t, owner, http.MethodPost, path+"/versions", tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "version"}, ChangeVersionInput: input})
			hubfixture.RequireStatus(t, published, http.StatusOK)
			var version tracker.ChangeVersion
			hubfixture.Decode(t, published, &version)
			if test.changePolicy != nil {
				next := hubfixture.Policy()
				test.changePolicy(&next)
				next = next.WithID()
				row, err := eventstore.NewContent(h.Config.RunDetails).PrepareProjectPolicy(t.Context(), item.OrganizationID, next)
				if err != nil {
					t.Fatal(err)
				}
				row.ProjectID, row.ApprovedBy, row.ApprovedAt = item.ProjectID, "example-owner", h.Now()
				if err := h.Repository.WriteTenant(t.Context(), item.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					return w.ApproveProjectPolicy(t.Context(), row, input.PolicyID)
				}); err != nil {
					t.Fatal(err)
				}
			}
			landed := h.Request(t, owner, http.MethodPost, path+"/versions/"+version.ID+"/landing", tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "landing"}, MergeSHA: strings.Repeat("d", 40), BaseRef: "main", Method: "squash"})
			hubfixture.RequireStatus(t, landed, test.wantStatus)
			if test.wantStatus == http.StatusOK {
				var result tracker.ChangeRequest
				hubfixture.Decode(t, landed, &result)
				if result.CurrentVersion != version.ID || result.Landed == nil || result.Landed.VersionID != version.ID {
					t.Fatalf("landing changed reviewed source: %+v", result)
				}
			}
		})
	}
}
