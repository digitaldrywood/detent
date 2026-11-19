package runnerfleet_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerDiagnosticsWithoutReports(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	f := hubfixture.NewNative(t, h, h.Primary, "Diagnostics without reports")
	response := f.Request(t, http.MethodGet, f.Base+"/runner-diagnostics?limit=10", nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var page runnerauth.DiagnosticPage
	hubfixture.Decode(t, response, &page)
	if page.Unavailable["runner"] != "not_authorized_or_unreported" {
		t.Fatalf("unavailable=%v", page.Unavailable)
	}
}

func TestRunnerRemovalUsesTenantAuthorityAndActiveLeases(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	repository, now := h.Repository, h.Now()
	for _, org := range []tracker.OrganizationID{hubfixture.Primary, hubfixture.Secondary} {
		err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "project_example", Name: "Example", Slug: "example", Profile: "native"}, States: hubfixture.States()}); err != nil {
				return err
			}
			for _, id := range []string{"idle", "busy"} {
				expires := now.Add(time.Hour)
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: id, Name: id, Hash: apikey.HashToken(string(org) + id), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
					return err
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: id, Hostname: id, Capacity: 1, Version: "example"}); err != nil {
					return err
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: id, RunnerID: id, MachineID: tracker.MachineID(id), TokenHash: apikey.HashToken(string(org) + id + "enrollment"), Scope: "projects", Operations: []string{runnerauth.Read}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
					return err
				}
				if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: org, RunnerID: id, MachineID: tracker.MachineID(id), TokenID: id, EnrollmentID: id, Scope: "projects", State: "active", Operations: []string{runnerauth.Read}, CapacityLimit: 1, ReportedCapacity: 1, OS: "linux", Architecture: "arm64", CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
					return err
				}
			}
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "project_example", NativeID: "item_example", Number: 1, Title: "Example"})
			if err != nil {
				return err
			}
			lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: 1, MachineID: "busy", SessionID: "example", LeaseID: "lease_example", TTL: time.Hour})
			if err != nil {
				return err
			}
			return w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, RunnerID: "busy", LeaseID: lease.LeaseID, AssignedAt: now})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{hubfixture.Primary, hubfixture.Secondary} {
		t.Run(string(org), func(t *testing.T) {
			path := hubfixture.OrganizationBase(org) + "/runners/"
			for _, id := range []string{"busy", "idle"} {
				want := http.StatusNoContent
				if id == "busy" {
					want = http.StatusUnprocessableEntity
				}
				hubfixture.RequireStatus(t, h.Request(t, http.MethodDelete, path+id, h.AdminToken(), nil), want)
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					identity, err := r.RunnerIdentity(t.Context(), id)
					if err != nil {
						return err
					}
					credential, err := r.AccessCredential(t.Context(), id)
					if err != nil {
						return err
					}
					if (identity.RemovedAt != nil) != (id == "idle") || (credential.RevokedAt != nil) != (id == "idle") {
						t.Fatalf("runner %s removed=%v credential revoked=%v", id, identity.RemovedAt, credential.RevokedAt)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			hubfixture.RequireStatus(t, h.Request(t, http.MethodDelete, path+"idle", h.AdminToken(), nil), http.StatusNoContent)
		})
	}
}
