package postgres_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/gotestreceiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestGoTestReceiptsAreBoundedAndTenantScoped(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	type scope struct {
		organization tracker.OrganizationID
		project      tracker.ProjectID
		attestation  gotestreceiptstore.Attestation
	}
	scopes := []scope{}
	key := fmt.Sprintf("%064x", 801)
	for index := range 2 {
		suffix := fmt.Sprintf("%032x", index+1)
		organization := tracker.OrganizationID("org_" + suffix)
		project := tracker.ProjectID("prj_shared")
		seedTenantAccess(t, repository, organization)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Slug: "example-project", Profile: "native"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		attestation := gotestreceiptstore.Attestation{OrganizationID: organization, ProjectID: project, ID: "gtra_" + suffix, CredentialID: "credential_" + suffix, RunnerID: "runner_" + suffix, CreatedAt: now, ExpiresAt: now.Add(12 * time.Hour)}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateGoTestReceiptAttestation(t.Context(), attestation)
		}); err != nil {
			t.Fatal(err)
		}
		scopes = append(scopes, scope{organization: organization, project: project, attestation: attestation})
	}
	for _, testScope := range scopes {
		if err := repository.WriteTenant(t.Context(), testScope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			for i := 0; i < 9; i++ {
				input := fmt.Sprintf("open\tm\tfile_%d.go", i)
				receipt := gotestreceiptstore.Receipt{OrganizationID: testScope.organization, ProjectID: testScope.project, BinaryKey: key, InputsDigest: fmt.Sprintf("%064x", i+1), Inputs: []string{input}, Package: "example/package", RunnerID: testScope.attestation.RunnerID, RecordedAt: now.Add(time.Duration(i) * time.Minute), AttestationID: testScope.attestation.ID}
				if err := w.PutGoTestReceipt(t.Context(), receipt, gotestreceipt.MaxCandidates); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for index, testScope := range scopes {
		if err := repository.ReadTenant(t.Context(), testScope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			attestation, err := r.GoTestReceiptAttestation(t.Context(), testScope.project, testScope.attestation.ID, testScope.attestation.CredentialID)
			if err != nil {
				return err
			}
			if attestation.RunnerID != testScope.attestation.RunnerID {
				t.Fatalf("runner=%q, want %q", attestation.RunnerID, testScope.attestation.RunnerID)
			}
			if _, err := r.GoTestReceiptAttestation(t.Context(), testScope.project, testScope.attestation.ID, "other_credential"); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("other credential read attestation: %v", err)
			}
			candidates, err := r.GoTestReceipts(t.Context(), testScope.project, key, now.Add(-time.Hour), gotestreceipt.MaxCandidates)
			if err != nil {
				return err
			}
			if len(candidates) != gotestreceipt.MaxCandidates || candidates[0].Digest != fmt.Sprintf("%064x", 9) || candidates[len(candidates)-1].Digest != fmt.Sprintf("%064x", 2) {
				t.Fatalf("organization %d candidates=%+v", index, candidates)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, testScope := range scopes {
		var receipts, attestations int64
		if err := repository.WriteTenant(t.Context(), testScope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			r, a, err := w.PruneGoTestReceipts(t.Context(), now.Add(time.Hour), now.Add(13*time.Hour), 64)
			receipts, attestations = r, a
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if receipts != gotestreceipt.MaxCandidates || attestations != 1 {
			t.Fatalf("pruned receipts=%d attestations=%d", receipts, attestations)
		}
	}
}
