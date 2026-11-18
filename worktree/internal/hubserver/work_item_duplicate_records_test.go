package hubserver

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestValidateWorkItemReceiptIssue(t *testing.T) {
	t.Parallel()
	scope := hubscope.NativeScope{Organization: "org_example", Project: "prj_example"}
	native := tracker.NativeWorkItemID(hubapi.NewID("wi"))
	for _, test := range []struct {
		name    string
		id      tracker.NativeWorkItemID
		profile string
		item    string
		want    error
	}{
		{name: "native item", id: native, profile: "native", item: string(native)},
		{name: "native create", id: native, profile: "native"},
		{name: "compatible imported item", id: "wi_imported_example", profile: "github_compatible", item: "wi_imported_example"},
		{name: "compatible create without native identity", id: "wi_imported_example", profile: "github_compatible", want: receiptstore.ErrExpired},
		{name: "another item", id: native, profile: "native", item: "wi_imported_example", want: receiptstore.ErrExpired},
		{name: "unknown profile", id: native, profile: "example", item: string(native), want: receiptstore.ErrExpired},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: test.id, Number: 1, Revision: 2, Profile: test.profile}, Title: "Example", State: "Cancelled"}
			raw, err := json.Marshal(hubrecords.RelationshipOmittedIssue(issue))
			if err != nil {
				t.Fatal(err)
			}
			if err := validateWorkItemReceiptIssue(scope, test.item, raw); !errors.Is(err, test.want) {
				t.Fatalf("validate receipt issue error=%v want %v", err, test.want)
			}
		})
	}
}
