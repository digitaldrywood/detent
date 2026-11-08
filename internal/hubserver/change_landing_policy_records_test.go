package hubserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeLandingPreservesCompatiblePolicyEvidence(t *testing.T) {
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
			repository, f, owner := newChangePolicyPostgresFixture(t)
			created := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "item"}, Title: "Example source", Body: workItemRecordBody, State: "Todo"})
			requireNativeStatus(t, created, http.StatusOK)
			var item tracker.NativeIssue
			decodeHubResponse(t, created, &item)
			path := f.base + "/work-items/" + string(item.WorkItemID) + "/changes"
			created = f.request(t, owner, http.MethodPost, path, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Example source"})
			requireNativeStatus(t, created, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, created, &change)
			path += "/" + change.ID
			input := changeTestInput()
			published := f.request(t, owner, http.MethodPost, path+"/versions", tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "version"}, ChangeVersionInput: input})
			requireNativeStatus(t, published, http.StatusOK)
			var version tracker.ChangeVersion
			decodeHubResponse(t, published, &version)
			if test.changePolicy != nil {
				next := hubTestPolicy()
				test.changePolicy(&next)
				next = next.WithID()
				row, err := eventstore.NewContent(f.service.runDetailStore()).PrepareProjectPolicy(t.Context(), item.OrganizationID, next)
				if err != nil {
					t.Fatal(err)
				}
				row.ProjectID, row.ApprovedBy, row.ApprovedAt = item.ProjectID, "example-owner", f.service.config.now()
				if err := repository.WriteTenant(t.Context(), item.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					return w.ApproveProjectPolicy(t.Context(), row, input.PolicyID)
				}); err != nil {
					t.Fatal(err)
				}
			}
			landed := f.request(t, owner, http.MethodPost, path+"/versions/"+version.ID+"/landing", tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "landing"}, MergeSHA: strings.Repeat("d", 40), BaseRef: "main", Method: "squash"})
			requireNativeStatus(t, landed, test.wantStatus)
			if test.wantStatus == http.StatusOK {
				var result tracker.ChangeRequest
				decodeHubResponse(t, landed, &result)
				if result.CurrentVersion != version.ID || result.Landed == nil || result.Landed.VersionID != version.ID {
					t.Fatalf("landing changed reviewed source: %+v", result)
				}
			}
		})
	}
}
