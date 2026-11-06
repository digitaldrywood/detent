package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type releaseDeliveryReader interface {
	ReleaseDelivery(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (eventstore.ReleaseDelivery, error)
}
type releaseDeliveryWriter interface {
	PutReleaseDelivery(context.Context, eventstore.ReleaseDelivery) error
}

func TestRuntimeReleaseDeliveryIdentity(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		history := seedHistory(t, repository, org, string(org))
		for number := 1; number <= 2; number++ {
			record := eventstore.ReleaseDelivery{OrganizationID: org, ProjectID: "prj_first", WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_first_%d", number)), ReleaseDelivery: tracker.ReleaseDelivery{ReleasePickInput: tracker.ReleasePickInput{SourceIssueID: "wi_first_1", SourceChangeID: history.change.ID, SourceMergeSHA: "source", Repository: "https://github.com/owner/repository", PRNumber: number, HeadRef: "pick", HeadSHA: "head", BaseRef: "release/v1.0", BaseSHA: "base", FrozenSHA: "frozen"}, ChangeID: history.change.ID, Actor: tracker.Actor{Kind: "human", PrincipalID: "example", ViaKey: "example-key"}, AssociatedAt: historyAt()}}
			put := func(value eventstore.ReleaseDelivery) error {
				return repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					owner, ok := w.(releaseDeliveryWriter)
					if !ok {
						return errors.New("runtime release delivery writer missing")
					}
					return owner.PutReleaseDelivery(t.Context(), value)
				})
			}
			if err := put(record); err != nil {
				t.Fatal(err)
			}
			if err := put(record); err != nil {
				t.Fatalf("replay: %v", err)
			}
			changed := record
			changed.SourceMergeSHA = "different"
			if err := put(changed); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("identity rewrite: %v", err)
			}
			changed = record
			changed.HeadSHA = "new-head"
			if err := put(changed); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("partial refresh: %v", err)
			}
			changed.BaseSHA = "new-base"
			if err := put(changed); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				owner, ok := r.(releaseDeliveryReader)
				if !ok {
					return errors.New("runtime release delivery reader missing")
				}
				got, err := owner.ReleaseDelivery(t.Context(), record.ProjectID, record.WorkItemID)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(got, changed) {
					t.Fatalf("delivery = %#v, want %#v", got, changed)
				}
				_, err = owner.ReleaseDelivery(t.Context(), "prj_second", record.WorkItemID)
				if !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("cross-project read: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			changed.OrganizationID = "org_other"
			if err := put(changed); !errors.Is(err, corestore.ErrTenantMismatch) {
				t.Fatalf("cross-tenant write: %v", err)
			}
		}
	}
}
