package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOwnedMachineRegistrationRejectsCredentialTakeover(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		seedRunnerIdentity(t, repository, org, "owner", "enrolled", historyAt(), true)
		seedRunnerIdentity(t, repository, org, "other", "other_enrolled", historyAt(), true)
		for _, tc := range []struct {
			name, machine, owner string
			capacity             int64
			denied               bool
		}{
			{"create", "new", "owner", 2, false},
			{"refresh", "new", "owner", 3, false},
			{"takeover", "new", "other", 9, true},
			{"unowned", "enrolled", "owner", 9, true},
		} {
			t.Run(string(org)+"/"+tc.name, func(t *testing.T) {
				err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					owner, ok := w.(interface {
						RegisterOwnedMachine(context.Context, leasestore.Machine, string) error
					})
					if !ok {
						t.Fatal("typed credential-owned registration is unavailable")
					}
					return owner.RegisterOwnedMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: tc.machine, Hostname: "example", Capacity: tc.capacity, Version: "example"}, tc.owner)
				})
				if tc.denied {
					if !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("takeover error = %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			})
		}
		if err := repository.ReadLeases(t.Context(), org, corestore.Serving, func(r leasestore.Reader) error {
			machine, err := r.Machine(t.Context(), "new")
			if err == nil && machine.Capacity != 3 {
				t.Errorf("capacity = %d, want 3", machine.Capacity)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
